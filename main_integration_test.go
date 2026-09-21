//go:build integration

// 這個檔案的測試需要真的 Postgres，透過 DATABASE_URL 環境變數連線。
// 本機執行：
//
//	DATABASE_URL="postgres://postgres:postgres@localhost:5432/linkpulse?sslmode=disable" \
//	  go test -tags=integration ./...
//
// CI 裡由 .github/workflows/test.yml 的 postgres service container 提供資料庫。
package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func setupIntegrationDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("failed to open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if err := db.Ping(); err != nil {
		t.Fatalf("failed to ping db: %v", err)
	}

	schema, err := os.ReadFile("schema.sql")
	if err != nil {
		t.Fatalf("failed to read schema.sql: %v", err)
	}
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatalf("failed to apply schema: %v", err)
	}

	// 每個測試開始前清空資料，確保測試之間互不干擾
	if _, err := db.Exec(`TRUNCATE links RESTART IDENTITY`); err != nil {
		t.Fatalf("failed to truncate links table: %v", err)
	}

	return db
}

func TestIntegration_ShortenThenRedirect(t *testing.T) {
	db := setupIntegrationDB(t)
	srv := NewServer(db, "http://localhost:8080")
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	// 1. 建立短網址
	resp, err := http.Post(ts.URL+"/shorten", "application/json",
		strings.NewReader(`{"url":"https://example.com/integration-test"}`))
	if err != nil {
		t.Fatalf("shorten request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var shortenResp shortenResponse
	if err := json.NewDecoder(resp.Body).Decode(&shortenResp); err != nil {
		t.Fatalf("failed to decode shorten response: %v", err)
	}

	// 2. 資料真的落地
	var originalURL string
	var hitCount int
	err = db.QueryRow(`SELECT original_url, hit_count FROM links WHERE short_code = $1`, shortenResp.ShortCode).
		Scan(&originalURL, &hitCount)
	if err != nil {
		t.Fatalf("failed to query inserted row: %v", err)
	}
	if originalURL != "https://example.com/integration-test" {
		t.Errorf("expected original_url to match, got %q", originalURL)
	}
	if hitCount != 0 {
		t.Errorf("expected initial hit_count 0, got %d", hitCount)
	}

	// 3. 用短碼查詢，確認 302 + Location
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	redirectResp, err := client.Get(ts.URL + "/" + shortenResp.ShortCode)
	if err != nil {
		t.Fatalf("redirect request failed: %v", err)
	}
	defer redirectResp.Body.Close()
	if redirectResp.StatusCode != http.StatusFound {
		t.Fatalf("expected 302, got %d", redirectResp.StatusCode)
	}
	if loc := redirectResp.Header.Get("Location"); loc != "https://example.com/integration-test" {
		t.Errorf("expected Location to match original url, got %q", loc)
	}

	// 4. hit_count 應該累加到 1
	if err := db.QueryRow(`SELECT hit_count FROM links WHERE short_code = $1`, shortenResp.ShortCode).Scan(&hitCount); err != nil {
		t.Fatalf("failed to query hit_count: %v", err)
	}
	if hitCount != 1 {
		t.Errorf("expected hit_count 1 after one redirect, got %d", hitCount)
	}
}

func TestIntegration_HitCountIncrementsOnEachRedirect(t *testing.T) {
	db := setupIntegrationDB(t)
	srv := NewServer(db, "http://localhost:8080")
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	if _, err := db.Exec(`INSERT INTO links (short_code, original_url) VALUES ($1, $2)`,
		"hitcnt1", "https://example.com/hitcount"); err != nil {
		t.Fatalf("failed to seed link: %v", err)
	}

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	for i := 0; i < 3; i++ {
		resp, err := client.Get(ts.URL + "/hitcnt1")
		if err != nil {
			t.Fatalf("redirect request %d failed: %v", i, err)
		}
		resp.Body.Close()
	}

	var hitCount int
	if err := db.QueryRow(`SELECT hit_count FROM links WHERE short_code = 'hitcnt1'`).Scan(&hitCount); err != nil {
		t.Fatalf("failed to query hit_count: %v", err)
	}
	if hitCount != 3 {
		t.Errorf("expected hit_count 3 after three redirects, got %d", hitCount)
	}
}

func TestIntegration_RedirectNotFound(t *testing.T) {
	db := setupIntegrationDB(t)
	srv := NewServer(db, "http://localhost:8080")
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/doesnotexist")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

// TestIntegration_RedirectExpiredLink 驗證超過 linkTTL(3天)的短碼視同不存在，
// 不用等背景 CronJob 跑過就會立即生效 -- 這是查詢時的 created_at 過濾條件在做的事。
func TestIntegration_RedirectExpiredLink(t *testing.T) {
	db := setupIntegrationDB(t)
	srv := NewServer(db, "http://localhost:8080")
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	// 手動塞一筆 4 天前建立的資料,模擬已過期的短碼
	fourDaysAgo := time.Now().Add(-4 * 24 * time.Hour)
	if _, err := db.Exec(
		`INSERT INTO links (short_code, original_url, created_at) VALUES ($1, $2, $3)`,
		"old0001", "https://example.com/expired", fourDaysAgo,
	); err != nil {
		t.Fatalf("failed to seed expired link: %v", err)
	}

	resp, err := http.Get(ts.URL + "/old0001")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for expired link, got %d", resp.StatusCode)
	}
}

// TestIntegration_RedirectFreshLinkWithinTTL 確認沒過期的連結不會被誤判成過期
func TestIntegration_RedirectFreshLinkWithinTTL(t *testing.T) {
	db := setupIntegrationDB(t)
	srv := NewServer(db, "http://localhost:8080")
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	oneHourAgo := time.Now().Add(-1 * time.Hour)
	if _, err := db.Exec(
		`INSERT INTO links (short_code, original_url, created_at) VALUES ($1, $2, $3)`,
		"new0001", "https://example.com/fresh", oneHourAgo,
	); err != nil {
		t.Fatalf("failed to seed fresh link: %v", err)
	}

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(ts.URL + "/new0001")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("expected 302 for link within TTL, got %d", resp.StatusCode)
	}
}

// TestIntegration_ShortCodeCollision 驗證資料庫本身的 UNIQUE constraint 是
// app 端重試機制的安全網：就算 app 邏輯有 bug 讓兩個相同短碼都送進 INSERT，
// Postgres 這一層還是會擋下來，不會真的存進兩筆衝突的資料。
func TestIntegration_ShortCodeCollision(t *testing.T) {
	db := setupIntegrationDB(t)

	if _, err := db.Exec(`INSERT INTO links (short_code, original_url) VALUES ($1, $2)`,
		"dupe001", "https://example.com/first"); err != nil {
		t.Fatalf("failed to seed link: %v", err)
	}

	_, err := db.Exec(`INSERT INTO links (short_code, original_url) VALUES ($1, $2)`,
		"dupe001", "https://example.com/second")
	if err == nil {
		t.Fatal("expected unique constraint violation, got nil error")
	}
	if !strings.Contains(err.Error(), "unique") && !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("expected a unique constraint error, got: %v", err)
	}
}

// onceThenReader 第一次 Read 回傳固定的 bytes（用來製造一次可預期的短碼碰撞），
// 之後交給真正的亂數來源，讓重試那一次可以正常產生一個新的、不會撞的短碼。
type onceThenReader struct {
	first []byte
	after io.Reader
}

func (r *onceThenReader) Read(p []byte) (int, error) {
	if r.first != nil {
		n := copy(p, r.first)
		r.first = nil
		return n, nil
	}
	return r.after.Read(p)
}

// TestIntegration_ShortenRetriesOnRealCollision 端到端驗證 app 層的重試機制：
// 先塞一筆已知短碼(對應固定的隨機 bytes)，讓 shortenHandler 第一次產生的短碼
// 真的撞上，確認它會自動重試並最終成功，而不是直接回 500。
func TestIntegration_ShortenRetriesOnRealCollision(t *testing.T) {
	db := setupIntegrationDB(t)

	// shortCodeChars 的前 7 個字元是 "abcdefg"，對應到 raw bytes 0~6
	const collidingCode = "abcdefg"
	if _, err := db.Exec(`INSERT INTO links (short_code, original_url) VALUES ($1, $2)`,
		collidingCode, "https://example.com/pre-existing"); err != nil {
		t.Fatalf("failed to seed colliding link: %v", err)
	}

	srv := NewServer(db, "http://localhost:8080")
	srv.randSource = &onceThenReader{
		first: []byte{0, 1, 2, 3, 4, 5, 6}, // 產生出 "abcdefg"，會跟上面那筆撞號
		after: rand.Reader,
	}
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/shorten", "application/json",
		strings.NewReader(`{"url":"https://example.com/should-retry-and-succeed"}`))
	if err != nil {
		t.Fatalf("shorten request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after automatic retry, got %d", resp.StatusCode)
	}

	var shortenResp shortenResponse
	if err := json.NewDecoder(resp.Body).Decode(&shortenResp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if shortenResp.ShortCode == collidingCode {
		t.Fatalf("expected a different short code after retry, still got the colliding one %q", collidingCode)
	}
}

func TestIntegration_IndexPageServed(t *testing.T) {
	db := setupIntegrationDB(t)
	srv := NewServer(db, "http://localhost:8080")
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := make([]byte, 4096)
	n, _ := resp.Body.Read(body)
	if !strings.Contains(string(body[:n]), "LinkPulse") {
		t.Errorf("expected index page to contain 'LinkPulse'")
	}
}

func TestIntegration_HealthzOK(t *testing.T) {
	db := setupIntegrationDB(t)
	srv := NewServer(db, "http://localhost:8080")
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}
