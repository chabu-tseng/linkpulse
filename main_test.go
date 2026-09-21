package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// failingReader 永遠回傳錯誤,用來測試 generateShortCode 讀不到隨機資料時的行為
type failingReader struct{}

func (failingReader) Read(p []byte) (int, error) {
	return 0, errors.New("simulated rand read failure")
}

// ---------- generateShortCode ----------

func TestGenerateShortCode_Length(t *testing.T) {
	code, err := generateShortCode(rand.Reader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(code) != 7 {
		t.Errorf("expected length 7, got %d (%q)", len(code), code)
	}
}

func TestGenerateShortCode_CharsetValid(t *testing.T) {
	code, err := generateShortCode(rand.Reader)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, c := range code {
		if !strings.ContainsRune(shortCodeChars, c) {
			t.Errorf("character %q not in allowed charset", c)
		}
	}
}

func TestGenerateShortCode_NoError(t *testing.T) {
	for i := 0; i < 100; i++ {
		if _, err := generateShortCode(rand.Reader); err != nil {
			t.Fatalf("unexpected error on iteration %d: %v", i, err)
		}
	}
}

func TestGenerateShortCode_LowCollisionRate(t *testing.T) {
	const n = 10000
	seen := make(map[string]bool, n)
	collisions := 0
	for i := 0; i < n; i++ {
		code, err := generateShortCode(rand.Reader)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if seen[code] {
			collisions++
		}
		seen[code] = true
	}
	// 62^7 ≈ 3.5e12種組合，10000次抽樣預期碰撞數趨近於0，容許極少數以避免測試 flaky
	if collisions > 2 {
		t.Errorf("unexpectedly high collision count: %d out of %d draws", collisions, n)
	}
}

func TestGenerateShortCode_RandReadError(t *testing.T) {
	_, err := generateShortCode(failingReader{})
	if err == nil {
		t.Fatal("expected error when random source fails, got nil")
	}
}

// ---------- isValidURL ----------

func TestIsValidURL(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{"valid https", "https://example.com", true},
		{"valid http", "http://example.com", true},
		{"empty string", "", false},
		{"missing scheme", "example.com", false},
		{"javascript scheme", "javascript:alert(1)", false},
		{"file scheme", "file:///etc/passwd", false},
		{"whitespace only", "   ", false},
		{"too long", "https://example.com/" + strings.Repeat("a", maxURLLength), false},
		{"scheme without host", "https://", false},
		{"invalid percent-encoding", "http://%zz", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isValidURL(tc.input)
			if got != tc.want {
				t.Errorf("isValidURL(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

// ---------- test helpers ----------

func newTestServer(t *testing.T) (*Server, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewServer(db, "http://localhost:8080"), mock
}

// ---------- healthzHandler ----------

func TestHealthzHandler(t *testing.T) {
	srv, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()

	srv.routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response body: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("expected status=ok, got %q", body["status"])
	}
}

// ---------- shortenHandler ----------

func TestShortenHandler_Success(t *testing.T) {
	srv, mock := newTestServer(t)
	mock.ExpectExec("INSERT INTO links").
		WithArgs(sqlmock.AnyArg(), "https://example.com").
		WillReturnResult(sqlmock.NewResult(1, 1))

	body := strings.NewReader(`{"url":"https://example.com"}`)
	req := httptest.NewRequest(http.MethodPost, "/shorten", body)
	w := httptest.NewRecorder()

	srv.routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d, body=%s", w.Code, w.Body.String())
	}
	var resp shortenResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(resp.ShortCode) != 7 {
		t.Errorf("expected short_code length 7, got %d", len(resp.ShortCode))
	}
	wantURL := "http://localhost:8080/" + resp.ShortCode
	if resp.ShortURL != wantURL {
		t.Errorf("expected short_url %q, got %q", wantURL, resp.ShortURL)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

func TestShortenHandler_EmptyURL(t *testing.T) {
	srv, _ := newTestServer(t)
	body := strings.NewReader(`{"url":""}`)
	req := httptest.NewRequest(http.MethodPost, "/shorten", body)
	w := httptest.NewRecorder()

	srv.routes().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d", w.Code)
	}
}

func TestShortenHandler_InvalidScheme(t *testing.T) {
	srv, _ := newTestServer(t)
	body := strings.NewReader(`{"url":"javascript:alert(1)"}`)
	req := httptest.NewRequest(http.MethodPost, "/shorten", body)
	w := httptest.NewRecorder()

	srv.routes().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d", w.Code)
	}
}

func TestShortenHandler_MalformedJSON(t *testing.T) {
	srv, _ := newTestServer(t)
	body := strings.NewReader(`not json`)
	req := httptest.NewRequest(http.MethodPost, "/shorten", body)
	w := httptest.NewRecorder()

	srv.routes().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d", w.Code)
	}
}

func TestShortenHandler_RandSourceError(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	srv := &Server{db: db, baseURL: "http://localhost:8080", randSource: failingReader{}}

	body := strings.NewReader(`{"url":"https://example.com"}`)
	req := httptest.NewRequest(http.MethodPost, "/shorten", body)
	w := httptest.NewRecorder()

	srv.routes().ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected status 500, got %d", w.Code)
	}
}

func TestShortenHandler_DBInsertError(t *testing.T) {
	srv, mock := newTestServer(t)
	mock.ExpectExec("INSERT INTO links").
		WithArgs(sqlmock.AnyArg(), "https://example.com").
		WillReturnError(sql.ErrConnDone)

	body := strings.NewReader(`{"url":"https://example.com"}`)
	req := httptest.NewRequest(http.MethodPost, "/shorten", body)
	w := httptest.NewRecorder()

	srv.routes().ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected status 500, got %d", w.Code)
	}
}

// ---------- redirectHandler ----------

func TestRedirectHandler_Found(t *testing.T) {
	srv, mock := newTestServer(t)
	rows := sqlmock.NewRows([]string{"original_url"}).AddRow("https://example.com")
	mock.ExpectQuery("SELECT original_url FROM links").
		WithArgs("abc1234", sqlmock.AnyArg()).
		WillReturnRows(rows)
	mock.ExpectExec("UPDATE links SET hit_count").
		WithArgs("abc1234").
		WillReturnResult(sqlmock.NewResult(0, 1))

	req := httptest.NewRequest(http.MethodGet, "/abc1234", nil)
	w := httptest.NewRecorder()

	srv.routes().ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("expected status 302, got %d", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "https://example.com" {
		t.Errorf("expected Location header https://example.com, got %q", loc)
	}
}

func TestRedirectHandler_NotFound(t *testing.T) {
	srv, mock := newTestServer(t)
	mock.ExpectQuery("SELECT original_url FROM links").
		WithArgs("missing", sqlmock.AnyArg()).
		WillReturnError(sql.ErrNoRows)

	req := httptest.NewRequest(http.MethodGet, "/missing", nil)
	w := httptest.NewRecorder()

	srv.routes().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected status 404, got %d", w.Code)
	}
}

func TestRedirectHandler_QueryError(t *testing.T) {
	srv, mock := newTestServer(t)
	mock.ExpectQuery("SELECT original_url FROM links").
		WithArgs("boom0001", sqlmock.AnyArg()).
		WillReturnError(sql.ErrConnDone)

	req := httptest.NewRequest(http.MethodGet, "/boom0001", nil)
	w := httptest.NewRecorder()

	srv.routes().ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected status 500, got %d", w.Code)
	}
}

// TestRedirectHandler_HitCountUpdateFailureDoesNotBlockRedirect 驗證即使
// hit_count 更新失敗，redirect 仍然要正常發生 -- 這是刻意設計，不是巧合。
func TestRedirectHandler_HitCountUpdateFailureDoesNotBlockRedirect(t *testing.T) {
	srv, mock := newTestServer(t)
	rows := sqlmock.NewRows([]string{"original_url"}).AddRow("https://example.com")
	mock.ExpectQuery("SELECT original_url FROM links").
		WithArgs("abc1234", sqlmock.AnyArg()).
		WillReturnRows(rows)
	mock.ExpectExec("UPDATE links SET hit_count").
		WithArgs("abc1234").
		WillReturnError(sql.ErrConnDone)

	req := httptest.NewRequest(http.MethodGet, "/abc1234", nil)
	w := httptest.NewRecorder()

	srv.routes().ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("expected redirect to still happen (302) even if hit_count update fails, got %d", w.Code)
	}
}

// ---------- /metrics ----------

func TestMetricsEndpoint(t *testing.T) {
	srv, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	srv.routes().ServeHTTP(httptest.NewRecorder(), req)

	req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	srv.routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "linkpulse_http_requests_total") {
		t.Errorf("expected /metrics body to contain linkpulse_http_requests_total")
	}
}

// ---------- initDB ----------

func TestInitDB_DefaultDSN(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	db, err := initDB()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if db == nil {
		t.Fatal("expected non-nil db")
	}
}

func TestInitDB_CustomDSN(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://user:pass@example.com:5432/db?sslmode=disable")
	db, err := initDB()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if db == nil {
		t.Fatal("expected non-nil db")
	}
}

// ---------- metricsMiddleware ----------

func TestMetricsMiddleware_UnmatchedRoute(t *testing.T) {
	srv, _ := newTestServer(t)
	// /{code} 只吃單一路徑片段,多一層路徑才會真的落到 chi 的 NotFound、
	// RoutePattern() 回空字串那個分支
	req := httptest.NewRequest(http.MethodGet, "/a/b", nil)
	w := httptest.NewRecorder()

	srv.routes().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// ---------- indexHandler ----------

func TestIndexHandler(t *testing.T) {
	srv, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	srv.routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "LinkPulse") {
		t.Errorf("expected body to contain 'LinkPulse', got: %s", w.Body.String()[:200])
	}
}

func TestIndexHandler_ReadError(t *testing.T) {
	original := readIndexHTML
	readIndexHTML = func() ([]byte, error) {
		return nil, errors.New("simulated embedded file read failure")
	}
	t.Cleanup(func() { readIndexHTML = original })

	srv, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	srv.routes().ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected status 500, got %d", w.Code)
	}
}
