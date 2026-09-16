package main

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// RED metrics: Rate（httpRequestsTotal）、Errors（httpRequestsTotal 的 status label）、Duration（httpRequestDuration）
var (
	httpRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "linkpulse_http_requests_total",
			Help: "Total number of HTTP requests processed, labeled by route, method and status code.",
		},
		[]string{"route", "method", "status"},
	)

	httpRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "linkpulse_http_request_duration_seconds",
			Help:    "HTTP request latency in seconds, labeled by route and method.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"route", "method"},
	)
)

// metricsMiddleware 記錄每個請求的次數跟延遲。route label 用 chi 的
// RoutePattern（例如 "/{code}"）而不是原始 URL path，避免短碼這種高基數
// (high cardinality) 的值把 metrics 灌爆。
func metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := chimiddleware.NewWrapResponseWriter(w, r.ProtoMajor)

		next.ServeHTTP(ww, r)

		route := chi.RouteContext(r.Context()).RoutePattern()
		if route == "" {
			route = "unmatched"
		}

		duration := time.Since(start).Seconds()
		status := strconv.Itoa(ww.Status())

		httpRequestsTotal.WithLabelValues(route, r.Method, status).Inc()
		httpRequestDuration.WithLabelValues(route, r.Method).Observe(duration)
	})
}
