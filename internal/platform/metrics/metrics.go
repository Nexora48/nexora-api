// Package metrics exposes Prometheus metrics on /metrics: HTTP traffic by route, database pool,
// live connections, the message outbox, background-job freshness and a few business counters.
package metrics

import (
	"context"
	"crypto/subtle"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Nexora48/nexora-api/internal/platform/heartbeat"
)

// Registry is private so tests can build several instances.
type Metrics struct {
	reg      *prometheus.Registry
	requests *prometheus.CounterVec
	latency  *prometheus.HistogramVec
	inflight prometheus.Gauge
}

// Counters incremented from other packages (registered on every Metrics instance).
var (
	RateLimited = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "nexora_rate_limited_total", Help: "Requests refused by a rate limiter."}, []string{"limiter"})
	PlanLimit   = prometheus.NewCounter(prometheus.CounterOpts{Name: "nexora_plan_limit_refusals_total", Help: "Creations refused because of a plan limit."})
	Logins      = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "nexora_logins_total", Help: "Sign-in attempts."}, []string{"result"})
	Webhooks    = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "nexora_payment_webhooks_total", Help: "Payment webhook outcomes."}, []string{"outcome"})
)

func New(pool *pgxpool.Pool, conns func() int) *Metrics {
	m := &Metrics{reg: prometheus.NewRegistry()}
	m.requests = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "nexora_http_requests_total", Help: "HTTP requests by route pattern and status."}, []string{"method", "route", "status"})
	m.latency = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "nexora_http_request_duration_seconds", Help: "HTTP latency by route pattern.",
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}}, []string{"method", "route"})
	m.inflight = prometheus.NewGauge(prometheus.GaugeOpts{Name: "nexora_http_in_flight_requests", Help: "Requests being served right now."})
	m.reg.MustRegister(m.requests, m.latency, m.inflight, RateLimited, PlanLimit, Logins, Webhooks,
		prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	if pool != nil {
		m.reg.MustRegister(&dbCollector{pool: pool, conns: conns})
	}
	return m
}

// Middleware records every request. The route label is the chi pattern, never the raw path (no ids, no cardinality blow-up).
func (m *Metrics) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		m.inflight.Inc()
		ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		m.inflight.Dec()
		route := "unmatched"
		if rc := chi.RouteContext(r.Context()); rc != nil && rc.RoutePattern() != "" {
			route = rc.RoutePattern()
		}
		status := ww.Status()
		if status == 0 {
			status = 200
		}
		m.requests.WithLabelValues(r.Method, route, strconv.Itoa(status)).Inc()
		m.latency.WithLabelValues(r.Method, route).Observe(time.Since(start).Seconds())
	})
}

// Handler serves /metrics: a bearer token when one is configured, otherwise loopback only.
func (m *Metrics) Handler(token string) http.Handler {
	inner := promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		} else if host, _, err := net.SplitHostPort(r.RemoteAddr); err != nil || !net.ParseIP(host).IsLoopback() {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		inner.ServeHTTP(w, r)
	})
}

// dbCollector reports pool stats on every scrape and the (cheap, cached) backlog counts.
type dbCollector struct {
	pool  *pgxpool.Pool
	conns func() int

	mu     sync.Mutex
	at     time.Time
	counts struct{ outbox, failed, stuckHooks, oldestOutboxSec float64 }
}

var (
	descPool    = prometheus.NewDesc("nexora_db_pool_connections", "Database pool connections by state.", []string{"state"}, nil)
	descWS      = prometheus.NewDesc("nexora_ws_connections", "Live app connections.", nil, nil)
	descOutbox  = prometheus.NewDesc("nexora_outbox_pending", "Messages waiting to be sent.", nil, nil)
	descOutOld  = prometheus.NewDesc("nexora_outbox_oldest_age_seconds", "Age of the oldest waiting message.", nil, nil)
	descFailed  = prometheus.NewDesc("nexora_outbox_failed", "Messages that failed for good.", nil, nil)
	descHooks   = prometheus.NewDesc("nexora_payment_events_stuck", "Payment events unprocessed for over 5 minutes.", nil, nil)
	descJobAge  = prometheus.NewDesc("nexora_job_last_run_age_seconds", "Seconds since a background job last ran.", []string{"job"}, nil)
	descVersion = prometheus.NewDesc("nexora_up", "1 while the API is serving.", nil, nil)
)

func (c *dbCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{descPool, descWS, descOutbox, descOutOld, descFailed, descHooks, descJobAge, descVersion} {
		ch <- d
	}
}

func (c *dbCollector) refresh() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.at) < 15*time.Second {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = c.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE attempts < 5), count(*) FILTER (WHERE attempts >= 5),
		COALESCE(extract(epoch FROM now() - min(created_at) FILTER (WHERE attempts < 5)), 0)
		FROM notification_outbox WHERE sent_at IS NULL`).Scan(&c.counts.outbox, &c.counts.failed, &c.counts.oldestOutboxSec)
	_ = c.pool.QueryRow(ctx, `SELECT count(*) FROM payment_events WHERE NOT processed AND received_at < now() - interval '5 minutes' AND received_at > now() - interval '24 hours'`).Scan(&c.counts.stuckHooks)
	c.at = time.Now()
}

func (c *dbCollector) Collect(ch chan<- prometheus.Metric) {
	st := c.pool.Stat()
	ch <- prometheus.MustNewConstMetric(descPool, prometheus.GaugeValue, float64(st.AcquiredConns()), "busy")
	ch <- prometheus.MustNewConstMetric(descPool, prometheus.GaugeValue, float64(st.IdleConns()), "idle")
	ch <- prometheus.MustNewConstMetric(descPool, prometheus.GaugeValue, float64(st.MaxConns()), "max")
	ch <- prometheus.MustNewConstMetric(descVersion, prometheus.GaugeValue, 1)
	if c.conns != nil {
		ch <- prometheus.MustNewConstMetric(descWS, prometheus.GaugeValue, float64(c.conns()))
	}
	c.refresh()
	c.mu.Lock()
	ch <- prometheus.MustNewConstMetric(descOutbox, prometheus.GaugeValue, c.counts.outbox)
	ch <- prometheus.MustNewConstMetric(descOutOld, prometheus.GaugeValue, c.counts.oldestOutboxSec)
	ch <- prometheus.MustNewConstMetric(descFailed, prometheus.GaugeValue, c.counts.failed)
	ch <- prometheus.MustNewConstMetric(descHooks, prometheus.GaugeValue, c.counts.stuckHooks)
	c.mu.Unlock()
	for _, j := range heartbeat.All() {
		ch <- prometheus.MustNewConstMetric(descJobAge, prometheus.GaugeValue, time.Since(j.At).Seconds(), j.Name)
	}
}
