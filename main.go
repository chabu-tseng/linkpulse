package main

import (
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

//go:embed static/index.html
var staticFiles embed.FS

// readIndexHTML 包成一個變數而不是直接呼叫 staticFiles.ReadFile，讓測試
// 可以替換掉它去模擬讀取失敗（embed.FS 的內容在編譯期就固定了，沒有這層
// 間接就沒辦法從外部觸發這個錯誤分支）。
var readIndexHTML = func() ([]byte, error) {
	return staticFiles.ReadFile("static/index.html")
}

const shortCodeChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// maxURLLength 限制使用者可以送入的網址長度，避免異常大的輸入打進資料庫
const maxURLLength = 2048

// allowedURLSchemes 只允許導向 http/https，避免 javascript:、file: 等危險 scheme
var allowedURLSchemes = map[string]bool{
	"http":  true,
	"https": true,
}

// linkTTL 短網址的存活時間。超過這個時間的短碼在 redirectHandler 查詢時
// 會直接被排除（視同不存在），不用等背景的清理 CronJob 跑過才失效。
const linkTTL = 72 * time.Hour

// Server 把外部依賴（DB、base URL、亂數來源）包起來，讓 handler 可以被注入假的依賴以利測試
type Server struct {
	db         *sql.DB
	baseURL    string
	randSource io.Reader
}

func NewServer(db *sql.DB, baseURL string) *Server {
	return &Server{db: db, baseURL: baseURL, randSource: rand.Reader}
}

// generateShortCode 產生一個 7 個字元的隨機短碼。source 通常傳
// crypto/rand.Reader，測試時可以換成一個會回錯誤的假 reader。
func generateShortCode(source io.Reader) (string, error) {
	b := make([]byte, 7)
	if _, err := io.ReadFull(source, b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = shortCodeChars[int(b[i])%len(shortCodeChars)]
	}
	return string(b), nil
}

// isValidURL 檢查使用者輸入是不是一個安全、可導向的網址
func isValidURL(raw string) bool {
	if strings.TrimSpace(raw) == "" {
		return false
	}
	if len(raw) > maxURLLength {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if !allowedURLSchemes[u.Scheme] {
		return false
	}
	if u.Host == "" {
		return false
	}
	return true
}

// indexHandler 提供前端頁面
func (s *Server) indexHandler(w http.ResponseWriter, r *http.Request) {
	data, err := readIndexHTML()
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

// healthzHandler 給 K8s liveness/readiness probe 用
func (s *Server) healthzHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

type shortenRequest struct {
	URL string `json:"url"`
}

type shortenResponse struct {
	ShortCode string `json:"short_code"`
	ShortURL  string `json:"short_url"`
}

// maxShortCodeRetries 短碼撞到 UNIQUE constraint 時最多重試幾次。
// 62^7 ≈ 3.5e12 種組合，正常情況下碰撞機率極低，這個上限只是防止
// 極端運氣不好時無限重試卡住請求，不是預期會被打滿的量。
const maxShortCodeRetries = 5

// isUniqueViolation 判斷錯誤是不是真的撞到 short_code 的 UNIQUE constraint，
// 而不是隨便什麼 DB 錯誤都拿去重試（例如連線斷掉重試也沒用，只會浪費時間）。
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	// Postgres 的 SQLSTATE 23505 = unique_violation
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// shortenHandler 接收原始網址，回傳短碼
func (s *Server) shortenHandler(w http.ResponseWriter, r *http.Request) {
	var req shortenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body, expected {\"url\":\"...\"}"}`, http.StatusBadRequest)
		return
	}

	if !isValidURL(req.URL) {
		http.Error(w, `{"error":"invalid or unsafe url"}`, http.StatusBadRequest)
		return
	}

	var code string
	for attempt := 0; ; attempt++ {
		var err error
		code, err = generateShortCode(s.randSource)
		if err != nil {
			log.Printf("failed to generate short code: %v", err)
			http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
			return
		}

		_, err = s.db.ExecContext(r.Context(),
			`INSERT INTO links (short_code, original_url) VALUES ($1, $2)`,
			code, req.URL,
		)
		if err == nil {
			break
		}

		if isUniqueViolation(err) && attempt < maxShortCodeRetries {
			log.Printf("short code collision on %q, retrying (attempt %d/%d)", code, attempt+1, maxShortCodeRetries)
			continue
		}

		log.Printf("failed to insert link: %v", err)
		http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(shortenResponse{
		ShortCode: code,
		ShortURL:  s.baseURL + "/" + code,
	})
}

// redirectHandler 用短碼查回原始網址並跳轉
func (s *Server) redirectHandler(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")

	var originalURL string
	err := s.db.QueryRowContext(r.Context(),
		`SELECT original_url FROM links WHERE short_code = $1 AND created_at > $2`,
		code, time.Now().Add(-linkTTL),
	).Scan(&originalURL)

	if err == sql.ErrNoRows {
		http.Error(w, `{"error":"short code not found"}`, http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("failed to query link: %v", err)
		http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
		return
	}

	// 更新點擊次數，失敗不影響 redirect 本身
	_, _ = s.db.ExecContext(r.Context(),
		`UPDATE links SET hit_count = hit_count + 1 WHERE short_code = $1`, code,
	)

	http.Redirect(w, r, originalURL, http.StatusFound)
}

func (s *Server) routes() *chi.Mux {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(metricsMiddleware)

	r.Get("/", s.indexHandler)
	r.Get("/healthz", s.healthzHandler)
	r.Handle("/metrics", promhttp.Handler())
	r.Post("/shorten", s.shortenHandler)
	r.Get("/{code}", s.redirectHandler)

	return r
}

func initDB() (*sql.DB, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres:postgres@localhost:5432/linkpulse?sslmode=disable"
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	// 沒有上限的連線池讓並發請求量可以直接轉換成對 Postgres 的並發連線數,
	// 高並發下會把 DB 的記憶體吃爆(2026-09-16 的負載測試就是這樣把 Postgres
	// OOMKilled 的)。這裡限制住,讓 app 自己在連線池打滿時排隊等待,而不是
	// 让 Postgres 自己被打爆。
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)
	return db, nil
}

func main() {
	db, err := initDB()
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		log.Fatalf("database ping failed: %v", err)
	}
	log.Println("connected to database")

	baseURL := os.Getenv("BASE_URL")
	if baseURL == "" {
		baseURL = "http://localhost:8080"
	}

	srv := NewServer(db, baseURL)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("server starting on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, srv.routes()))
}
