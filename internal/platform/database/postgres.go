package database

import (
	"context"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nexora48/nexora-api/internal/platform/authctx"
)

// Options configure the pool.
type Options struct {
	// RLS runs every connection as the role nexora_app and, before each use, sets which society it may see
	// (from the signed-in user in the request context) or marks it as platform code. See migration 000051.
	RLS bool
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
	if len(opts) > 0 && opts[0].RLS {
		var last sync.Map // *pgx.Conn -> what the session is currently set to
		cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
			_, err := c.Exec(ctx, `SET ROLE nexora_app`)
			return err
		}
		// Every acquire rewrites the setting when it differs, so a connection can never carry a previous caller's society.
		cfg.BeforeAcquire = func(ctx context.Context, c *pgx.Conn) bool {
			soc, bypass := scope(ctx)
			want := "bypass"
			if !bypass {
				want = soc.String()
			}
			if v, ok := last.Load(c); ok && v == want {
				return true
			}
			sid, byp := "", "off"
			if bypass {
				byp = "on"
			} else {
				sid = soc.String()
			}
			if _, err := c.Exec(ctx, `SELECT set_config('app.society_id', $1, false), set_config('app.bypass', $2, false)`, sid, byp); err != nil {
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
