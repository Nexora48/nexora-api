package middleware

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
	"github.com/Nexora48/nexora-api/internal/platform/metrics"
)

// Fixed-window counters in Redis. Three flavours:
//   - RateLimit / RateLimitStrict: per IP for one route (strict refuses when Redis is down: sign-in, OTP, MFA)
//   - GlobalIPLimit: per IP for the whole API (fails open and logs when Redis is down)
//   - UserRateLimit: per signed-in user (falls back to the IP), for the app as a whole or for heavy routes
// Every answer carries X-RateLimit-* headers; a refusal is 429 RATE_LIMITED with Retry-After.

// hit is atomic: the key is created with its expiry in one step, so a crash can never leave a counter without one.
var hit = redis.NewScript(`
local n = redis.call('INCR', KEYS[1])
if n == 1 then redis.call('EXPIRE', KEYS[1], ARGV[1]) end
return {n, redis.call('TTL', KEYS[1])}`)

type verdict struct {
	n, ttl int64
	err    error
}

func count(ctx context.Context, rdb *redis.Client, key string, window time.Duration) verdict {
	secs := int64(window / time.Second)
	if secs < 1 {
		secs = 1
	}
	res, err := hit.Run(ctx, rdb, []string{key}, secs).Int64Slice()
	if err != nil || len(res) != 2 {
		return verdict{err: fmt.Errorf("rate limit counter: %v", err)}
	}
	return verdict{n: res[0], ttl: res[1]}
}

// ClientIP is the address the limiter keys on (RealIP has already run when a proxy is trusted).
func ClientIP(r *http.Request) string {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}

func answer(w http.ResponseWriter, name string, limit int, v verdict) bool {
	left := int64(limit) - v.n
	if left < 0 {
		left = 0
	}
	h := w.Header()
	h.Set("X-RateLimit-Limit", strconv.Itoa(limit))
	h.Set("X-RateLimit-Remaining", strconv.FormatInt(left, 10))
	if v.ttl > 0 {
		h.Set("X-RateLimit-Reset", strconv.FormatInt(v.ttl, 10))
	}
	if v.n > int64(limit) {
		retry := v.ttl
		if retry < 1 {
			retry = 1
		}
		h.Set("Retry-After", strconv.FormatInt(retry, 10))
		metrics.RateLimited.WithLabelValues(name).Inc()
		httpx.Error(w, http.StatusTooManyRequests, "RATE_LIMITED", "too many requests, try again in a moment")
		return false
	}
	return true
}

func limiter(rdb *redis.Client, name string, limit int, window time.Duration, closed bool, key func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			v := count(r.Context(), rdb, "rl:"+name+":"+key(r), window)
			if v.err != nil {
				if closed {
					httpx.Error(w, http.StatusServiceUnavailable, "UNAVAILABLE", "temporarily unavailable, please try again")
					return
				}
				slog.Warn("rate limiter unavailable, letting the request through", "limiter", name, "err", v.err)
				next.ServeHTTP(w, r)
				return
			}
			if answer(w, name, limit, v) {
				next.ServeHTTP(w, r)
			}
		})
	}
}

// RateLimit is a fixed-window per-IP limiter for one route; it fails open when Redis is unavailable.
func RateLimit(rdb *redis.Client, name string, limit int, window time.Duration) func(http.Handler) http.Handler {
	return limiter(rdb, name, limit, window, false, ClientIP)
}

// RateLimitStrict is RateLimit that refuses (503) when Redis is unavailable. Use it for sign-in, OTP and MFA.
func RateLimitStrict(rdb *redis.Client, name string, limit int, window time.Duration) func(http.Handler) http.Handler {
	return limiter(rdb, name, limit, window, true, ClientIP)
}

// GlobalIPLimit limits every request of one address per minute, except the probes.
func GlobalIPLimit(rdb *redis.Client, perMin int) func(http.Handler) http.Handler {
	lim := limiter(rdb, "ip", perMin, time.Minute, false, ClientIP)
	return func(next http.Handler) http.Handler {
		limited := lim(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/health", "/ready", "/metrics", "/api/v1/ping":
				next.ServeHTTP(w, r)
				return
			}
			limited.ServeHTTP(w, r)
		})
	}
}

// UserRateLimit limits a signed-in user (the IP when there is no principal yet). Place it after Auth.
func UserRateLimit(rdb *redis.Client, name string, limit int, window time.Duration) func(http.Handler) http.Handler {
	return limiter(rdb, name, limit, window, false, func(r *http.Request) string {
		if p, ok := authctx.From(r.Context()); ok {
			return "u:" + p.UserID.String()
		}
		return "ip:" + ClientIP(r)
	})
}
