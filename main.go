package main

import (
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/json"
	"log"
	"net/http"
	"os"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	_ "github.com/jackc/pgx/v5/stdlib"
)

var db *sql.DB

//go:embed static/index.html
var staticFiles embed.FS

const shortCodeChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// generateShortCode 產生一個 7 個字元的隨機短碼
func generateShortCode() (string, error) {
	b := make([]byte, 7)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = shortCodeChars[int(b[i])%len(shortCodeChars)]
	}
	return string(b), nil
}

// indexHandler 提供前端頁面
func indexHandler(w http.ResponseWriter, r *http.Request) {
	data, err := staticFiles.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

// healthzHandler 給 K8s liveness/readiness probe 用
func healthzHandler(w http.ResponseWriter, r *http.Request) {
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

// shortenHandler 接收原始網址，回傳短碼
func shortenHandler(w http.ResponseWriter, r *http.Request) {
	var req shortenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.URL == "" {
		http.Error(w, `{"error":"invalid request body, expected {\"url\":\"...\"}"}`, http.StatusBadRequest)
		return
	}

	code, err := generateShortCode()
	if err != nil {
		log.Printf("failed to generate short code: %v", err)
		http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
		return
	}

	_, err = db.ExecContext(r.Context(),
		`INSERT INTO links (short_code, original_url) VALUES ($1, $2)`,
		code, req.URL,
	)
	if err != nil {
		log.Printf("failed to insert link: %v", err)
		http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
		return
	}

	baseURL := os.Getenv("BASE_URL")
	if baseURL == "" {
		baseURL = "http://localhost:8080"
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(shortenResponse{
		ShortCode: code,
		ShortURL:  baseURL + "/" + code,
	})
}

// redirectHandler 用短碼查回原始網址並跳轉
func redirectHandler(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")

	var originalURL string
	err := db.QueryRowContext(r.Context(),
		`SELECT original_url FROM links WHERE short_code = $1`,
		code,
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
	_, _ = db.ExecContext(r.Context(),
		`UPDATE links SET hit_count = hit_count + 1 WHERE short_code = $1`, code,
	)

	http.Redirect(w, r, originalURL, http.StatusFound)
}

func initDB() (*sql.DB, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres:postgres@localhost:5432/linkpulse?sslmode=disable"
	}
	return sql.Open("pgx", dsn)
}

func main() {
	var err error
	db, err = initDB()
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		log.Fatalf("database ping failed: %v", err)
	}
	log.Println("connected to database")

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Get("/", indexHandler)
	r.Get("/healthz", healthzHandler)
	r.Post("/shorten", shortenHandler)
	r.Get("/{code}", redirectHandler)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("server starting on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, r))
}