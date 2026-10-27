package middleware

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Nexora48/nexora-api/internal/platform/errtrack"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
	"github.com/go-chi/chi/v5/middleware"
)

// Logger logs one line per request. Slow requests (over slow) and 5xx responses are logged as warnings/errors.
func Logger(log *slog.Logger, slow ...time.Duration) func(http.Handler) http.Handler {
	threshold := time.Second
	if len(slow) > 0 && slow[0] > 0 {
		threshold = slow[0]
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)
			took := time.Since(start)
			attrs := []any{"method", r.Method, "path", r.URL.Path, "status", ww.Status(),
				"ms", took.Milliseconds(), "request_id", middleware.GetReqID(r.Context())}
			switch {
			case ww.Status() >= 500:
				log.Error("request", attrs...)
				errtrack.ServerError(r, ww.Status())
			case took > threshold:
				log.Warn("slow request", attrs...)
			default:
				log.Info("request", attrs...)
			}
		})
	}
}

// BodyCeiling caps every request body (handlers apply tighter caps of their own).
func BodyCeiling(max int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil && r.Body != http.NoBody {
				r.Body = http.MaxBytesReader(w, r.Body, max)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// SecurityHeaders adds the headers every response should carry. API responses are never cached.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// Recoverer converts panics into the standard error envelope.
func Recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					log.Error("panic", "err", rec, "request_id", middleware.GetReqID(r.Context()))
					errtrack.Panic(r, rec)
					httpx.Error(w, http.StatusInternalServerError, "INTERNAL", "internal server error")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
