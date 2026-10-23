package middleware

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/Nexora48/nexora-api/internal/platform/httpx"
	"github.com/go-chi/chi/v5/middleware"
)

// Logger logs one line per request.
func Logger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)
			log.Info("request",
				"method", r.Method, "path", r.URL.Path, "status", ww.Status(),
				"ms", time.Since(start).Milliseconds(), "request_id", middleware.GetReqID(r.Context()))
		})
	}
}

// Recoverer converts panics into the standard error envelope.
func Recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					log.Error("panic", "err", rec)
					httpx.Error(w, http.StatusInternalServerError, "INTERNAL", "internal server error")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
