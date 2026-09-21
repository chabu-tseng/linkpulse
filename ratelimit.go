package main

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// ipRateLimiter 每個來源 IP 各自維護一個 token bucket。
//
// 已知限制：這個狀態只存在單一 Pod 的記憶體裡，兩個 replica 之間不共享，
// 所以同一個使用者實際上可以拿到大約 2 倍設定值的額度（打到不同 replica）。
// 要做到叢集層級的統一限流，需要換成 Redis 之類的共享儲存——這裡先用
// 單機版本，足夠擋掉單一來源的暴力打法（例如我們自己拿 hey 測試時那種）。
type ipRateLimiter struct {
	mu              sync.Mutex
	limiters        map[string]*rateLimiterEntry
	rps             rate.Limit
	burst           int
	cleanupInterval time.Duration
	staleAfter      time.Duration
}

type rateLimiterEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func newIPRateLimiter(rps rate.Limit, burst int) *ipRateLimiter {
	rl := &ipRateLimiter{
		limiters:        make(map[string]*rateLimiterEntry),
		rps:             rps,
		burst:           burst,
		cleanupInterval: 5 * time.Minute,
		staleAfter:      10 * time.Minute,
	}
	go rl.cleanupLoop()
	return rl
}

func (rl *ipRateLimiter) getLimiter(ip string) *rate.Limiter {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	entry, exists := rl.limiters[ip]
	if !exists {
		entry = &rateLimiterEntry{limiter: rate.NewLimiter(rl.rps, rl.burst)}
		rl.limiters[ip] = entry
	}
	entry.lastSeen = time.Now()
	return entry.limiter
}

// cleanupLoop 定期清掉太久沒動靜的 IP，避免長時間運行下 map 隨著不重複的
// 來源 IP 數量無限成長（例如被大量不同 IP 各打一兩次請求）。
func (rl *ipRateLimiter) cleanupLoop() {
	ticker := time.NewTicker(rl.cleanupInterval)
	defer ticker.Stop()
	for range ticker.C {
		rl.cleanupOnce()
	}
}

// cleanupOnce 跑一次清理，抽出來讓測試可以直接呼叫，不用真的等 ticker 觸發。
func (rl *ipRateLimiter) cleanupOnce() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	for ip, entry := range rl.limiters {
		if time.Since(entry.lastSeen) > rl.staleAfter {
			delete(rl.limiters, ip)
		}
	}
}

// clientIP 優先取 X-Forwarded-For 的第一個位址（真正的原始客戶端），
// 沒有的話退回 RemoteAddr。
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if idx := strings.Index(xff, ","); idx != -1 {
			return strings.TrimSpace(xff[:idx])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (rl *ipRateLimiter) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limiter := rl.getLimiter(clientIP(r))
		if !limiter.Allow() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":"rate limit exceeded, please slow down"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}
