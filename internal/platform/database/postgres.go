package database

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nexora48/nexora-api/internal/platform/authctx"
)

// Options configure the pool.
type Options struct {
	// RLS makes connections used for a signed-in user run as the role nexora_app with that user's society set;
	// all other work stays on the login role (the table owner). See migration 000051.
	RLS bool
	// MaxConns is the pool size (0 = 25). Keep it below the server's max_connections divided by the number of API instances.
	MaxConns int32
	// StatementTimeout aborts any single statement that runs longer (0 = 30s, negative = off).
	StatementTimeout time.Duration
	// SlowQuery logs (and aggregates) statements slower than this; 0 = off.
	SlowQuery time.Duration
	Log       *slog.Logger
}

type ctxKey struct{}

// Unscoped marks a context whose queries may cross society boundaries even though a user is signed in
// (for example listing every society one person belongs to). Use it sparingly: every use is a deliberate hole.
func Unscoped(ctx context.Context) context.Context { return context.WithValue(ctx, ctxKey{}, true) }

// scope is the session setting a context needs: a society id, or "bypass".
func scope(ctx context.Context) (society uuid.UUID, bypass bool) {
	if v, _ := ctx.Value(ctxKey{}).(bool); v {
		return uuid.Nil, true
	}
	if p, ok := authctx.From(ctx); ok && p.SocietyID != uuid.Nil {
		return p.SocietyID, false
	}
	return uuid.Nil, true // sign-in, sweepers, webhooks, the SuperAdmin console
}

func NewPostgres(ctx context.Context, url string, opts ...Options) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	cfg.MaxConns = 25
	if o.MaxConns > 0 {
		cfg.MaxConns = o.MaxConns
	}
	cfg.MinConns = 2
	cfg.MaxConnLifetime, cfg.MaxConnIdleTime, cfg.HealthCheckPeriod = time.Hour, 15*time.Minute, time.Minute
	st := o.StatementTimeout
	if st == 0 {
		st = 30 * time.Second
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	if st > 0 {
		cfg.ConnConfig.RuntimeParams["statement_timeout"] = strconv.FormatInt(st.Milliseconds(), 10)
	}
	cfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = "60000"
	if o.SlowQuery > 0 {
		tr := &slowTracer{limit: o.SlowQuery, log: o.Log, stats: map[string]*SlowStat{}}
		cfg.ConnConfig.Tracer = tr
		tracers.Store(tr, tr)
	}
	if o.RLS {
		var last sync.Map // *pgx.Conn -> what the session is currently set to
		// Every acquire rewrites the session when it differs from what the caller needs, so a connection can never carry
		// a previous caller's society: signed-in callers run as nexora_app with their society set; everything else
		// runs as the login role (the table owner), which the policies do not apply to.
		cfg.BeforeAcquire = func(ctx context.Context, c *pgx.Conn) bool {
			soc, bypass := scope(ctx)
			role, sid, want := "none", "", "none"
			if !bypass {
				role, sid, want = "nexora_app", soc.String(), soc.String()
			}
			if v, ok := last.Load(c); ok && v == want {
				return true
			}
			if _, err := c.Exec(ctx, `SELECT set_config('role', $1, false), set_config('app.society_id', $2, false)`, role, sid); err != nil {
				last.Delete(c)
				return false
			}
			last.Store(c, want)
			return true
		}
		cfg.BeforeClose = func(c *pgx.Conn) { last.Delete(c) }
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}
