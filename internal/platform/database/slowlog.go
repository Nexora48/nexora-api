package database

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// slowTracer records statements slower than a threshold: the SQL text only (never the arguments, which can
// hold personal data) and how long they took. They are logged as warnings and aggregated per statement.
type slowTracer struct {
	limit time.Duration
	log   *slog.Logger
	mu    sync.Mutex
	stats map[string]*SlowStat
}

type startKey struct{}

type startVal struct {
	at  time.Time
	sql string
}

// SlowStat aggregates one statement for reports (the load test prints the worst).
type SlowStat struct {
	SQL   string
	Count int
	Total time.Duration
	Max   time.Duration
}

func (t *slowTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, startKey{}, startVal{time.Now(), d.SQL})
}

func (t *slowTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	v, ok := ctx.Value(startKey{}).(startVal)
	if !ok {
		return
	}
	took := time.Since(v.at)
	if took < t.limit {
		return
	}
	sql := strings.Join(strings.Fields(v.sql), " ")
	if len(sql) > 400 {
		sql = sql[:400] + "…"
	}
	if t.log != nil {
		t.log.Warn("slow query", "ms", took.Milliseconds(), "sql", sql)
	}
	t.mu.Lock()
	st := t.stats[sql]
	if st == nil {
		st = &SlowStat{SQL: sql}
		t.stats[sql] = st
	}
	st.Count++
	st.Total += took
	if took > st.Max {
		st.Max = took
	}
	t.mu.Unlock()
}

var tracers sync.Map // *pgxpool.Pool-independent registry: pool config pointer -> tracer (see SlowStats)

// SlowStats returns the slowest statements seen by every pool created with Options.SlowQuery, worst total first.
func SlowStats() []SlowStat {
	var out []SlowStat
	tracers.Range(func(_, v any) bool {
		t := v.(*slowTracer)
		t.mu.Lock()
		for _, s := range t.stats {
			out = append(out, *s)
		}
		t.mu.Unlock()
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Total > out[j].Total })
	return out
}
