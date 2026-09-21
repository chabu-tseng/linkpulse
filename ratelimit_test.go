package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIPRateLimiter_AllowsWithinBurst(t *testing.T) {
	rl := newIPRateLimiter(1, 3)
	for i := 0; i < 3; i++ {
		if !rl.getLimiter("1.2.3.4").Allow() {
			t.Fatalf("request %d should be allowed within burst of 3", i+1)
		}
	}
}

func TestIPRateLimiter_RejectsBeyondBurst(t *testing.T) {
	rl := newIPRateLimiter(1, 3)
	for i := 0; i < 3; i++ {
		rl.getLimiter("1.2.3.4").Allow()
	}
	if rl.getLimiter("1.2.3.4").Allow() {
		t.Fatal("4th request should be rejected, burst of 3 already consumed")
	}
}

func TestIPRateLimiter_TracksIPsSeparately(t *testing.T) {
	rl := newIPRateLimiter(1, 1)
	if !rl.getLimiter("1.1.1.1").Allow() {
		t.Fatal("first request from 1.1.1.1 should be allowed")
	}
	if !rl.getLimiter("2.2.2.2").Allow() {
		t.Fatal("first request from a different IP (2.2.2.2) should be allowed independently")
	}
}

func TestClientIP_UsesXForwardedForFirstEntry(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.5, 10.0.0.1")
	req.RemoteAddr = "10.0.0.1:12345"

	if got := clientIP(req); got != "203.0.113.5" {
		t.Errorf("expected 203.0.113.5, got %q", got)
	}
}

func TestClientIP_FallsBackToRemoteAddr(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.0.2.1:54321"

	if got := clientIP(req); got != "192.0.2.1" {
		t.Errorf("expected 192.0.2.1, got %q", got)
	}
}

func TestClientIP_XForwardedForWithoutComma(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	req.RemoteAddr = "10.0.0.1:12345"

	if got := clientIP(req); got != "203.0.113.9" {
		t.Errorf("expected 203.0.113.9, got %q", got)
	}
}

func TestClientIP_MalformedRemoteAddrFallsBackToRawValue(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	// 沒有 port 的畸形 RemoteAddr,SplitHostPort 會失敗
	req.RemoteAddr = "not-a-valid-host-port"

	if got := clientIP(req); got != "not-a-valid-host-port" {
		t.Errorf("expected raw RemoteAddr as fallback, got %q", got)
	}
}

// ---------- cleanup ----------

func TestIPRateLimiter_CleanupOnceRemovesStaleEntries(t *testing.T) {
	rl := newIPRateLimiter(1, 1)
	rl.limiters["stale.ip"] = &rateLimiterEntry{
		limiter:  rl.getLimiter("stale.ip"),
		lastSeen: time.Now().Add(-1 * time.Hour), // 早就過了 staleAfter(10分鐘)
	}
	rl.limiters["fresh.ip"] = &rateLimiterEntry{
		limiter:  rl.getLimiter("fresh.ip"),
		lastSeen: time.Now(),
	}

	rl.cleanupOnce()

	rl.mu.Lock()
	defer rl.mu.Unlock()
	if _, exists := rl.limiters["stale.ip"]; exists {
		t.Error("stale entry should have been removed by cleanupOnce")
	}
	if _, exists := rl.limiters["fresh.ip"]; !exists {
		t.Error("fresh entry should not have been removed")
	}
}

func TestIPRateLimiter_CleanupLoopRunsOnTicker(t *testing.T) {
	rl := &ipRateLimiter{
		limiters:        make(map[string]*rateLimiterEntry),
		rps:             1,
		burst:           1,
		cleanupInterval: 10 * time.Millisecond,
		staleAfter:      0, // 任何存在的 entry 下一次 tick 就會被判定成過期
	}
	rl.getLimiter("goes.away")

	go rl.cleanupLoop()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rl.mu.Lock()
		_, exists := rl.limiters["goes.away"]
		rl.mu.Unlock()
		if !exists {
			return // 清理成功,測試通過
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("cleanupLoop did not remove the stale entry within 2s")
}

// ---------- middleware 串進真實 handler 的行為 ----------

func TestRateLimitMiddleware_RejectsOverLimit(t *testing.T) {
	srv, _ := newTestServer(t)
	// 換成一個容量只有 1 的 limiter，讓行為可預測，不用等真實時間流逝
	srv.rateLimiter = newIPRateLimiter(1, 1)

	router := srv.routes()

	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.RemoteAddr = "5.5.5.5:1111"
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("first request should pass, got %d", w2.Code)
	}

	req3 := httptest.NewRequest(http.MethodGet, "/", nil)
	req3.RemoteAddr = "5.5.5.5:2222" // 同一個 IP,不同 port,應該還是算同一個來源
	w3 := httptest.NewRecorder()
	router.ServeHTTP(w3, req3)
	if w3.Code != http.StatusTooManyRequests {
		t.Fatalf("second request from same IP should be rate limited, got %d", w3.Code)
	}
}

func TestRateLimitMiddleware_HealthzNeverLimited(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.rateLimiter = newIPRateLimiter(1, 1) // 容量極小,如果 /healthz 有被限流,第二次一定會 429
	router := srv.routes()

	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.RemoteAddr = "9.9.9.9:1234"
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("/healthz request %d should never be rate limited, got %d", i+1, w.Code)
		}
	}
}

func TestRateLimitMiddleware_MetricsNeverLimited(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.rateLimiter = newIPRateLimiter(1, 1)
	router := srv.routes()

	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		req.RemoteAddr = "9.9.9.9:1234"
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("/metrics request %d should never be rate limited, got %d", i+1, w.Code)
		}
	}
}
