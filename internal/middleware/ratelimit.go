package middleware

import (
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Nexora48/nexora-api/internal/platform/httpx"
)

// RateLimit is a fixed-window per-IP limiter backed by Redis.
func RateLimit(rdb *redis.Client, name string, limit int, window time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				ip = r.RemoteAddr
			}
			k := fmt.Sprintf("rl:%s:%s", name, ip)
			n, err := rdb.Incr(r.Context(), k).Result()
			if err == nil && n == 1 {
				rdb.Expire(r.Context(), k, window)
			}
			if err == nil && n > int64(limit) {
				httpx.Error(w, http.StatusTooManyRequests, "RATE_LIMITED", "too many requests")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
