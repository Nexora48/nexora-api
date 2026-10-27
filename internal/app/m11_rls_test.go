package app_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/database"
)

// tables that deliberately have no row-level security: global catalogues and per-user data (see migration 000051)
var rlsExempt = map[string]bool{
	"users": true, "devices": true, "refresh_tokens": true, "notification_prefs": true, "notification_outbox": true,
	"emergency_contacts": true, "roles": true, "permissions": true, "role_permissions": true, "features": true,
	"plans": true, "app_versions": true, "audit_settings": true, "schema_migrations": true,
	"platform_users": true, "platform_recovery_codes": true, "platform_refresh_tokens": true, "platform_broadcasts": true,
}

// A table added by a later migration must either be protected or be listed above on purpose.
func TestEveryTableIsProtectedOrExemptOnPurpose(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	rows, err := pool.Query(context.Background(), `SELECT c.relname, c.relrowsecurity, c.relforcerowsecurity FROM pg_class c
		JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind='r' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var on, forced bool
		if err := rows.Scan(&name, &on, &forced); err != nil {
			t.Fatal(err)
		}
		if rlsExempt[name] {
			if on {
				t.Errorf("%s is listed as exempt but has RLS on: update the list", name)
			}
			continue
		}
		_ = forced
		if !on {
			t.Errorf("table %s has no row-level security: add a policy in a migration, or list it as exempt with a reason", name)
		}
	}
}

// rlsPool is a second pool that runs as the restricted role, like the API with DB_RLS=true.
func rlsPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p, err := database.NewPostgres(context.Background(), testDBURL, database.Options{RLS: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func asSociety(id uuid.UUID) context.Context {
	return authctx.With(context.Background(), authctx.Principal{UserID: uuid.New(), SocietyID: id, Role: "society_admin"})
}

// populate gives a community rows in as many tenant tables as the public API can create.
func populate(t *testing.T, c community) {
	t.Helper()
	fixedHead(t, c, "Maintenance", 100000)
	issuePeriod(t, c, periodOf(0))
	recordPay(t, c, c.units["A101"], 50000, "cash", "RLS-1")
	publishedNotice(t, c, nil)
	raiseComplaint(t, c.rA, nil)
	createPoll(t, c, nil)
	openAmenity(t, c, nil)
	registerHelper(t, c.rA, nextPhone(), nil)
	walkIn(t, c.guard, c.units["A101"], "Walker")
	call(t, "POST", "/api/v1/society/contacts", c.s.Admin, map[string]any{"name": "Clinic", "phone": "9876543210", "kind": "medical"})
}

func TestRLSHidesOtherSocietiesAtTheSQLLevel(t *testing.T) {
	a := newCommunity(t)
	b := newCommunity(t)
	ctx := context.Background()
	populate(t, b)
	rp := rlsPool(t)

	// tenant tables = every table with a society_id column
	rows, err := pool.Query(ctx, `SELECT table_name FROM information_schema.columns WHERE table_schema='public' AND column_name='society_id'
		AND table_name NOT IN ('platform_invoices','platform_broadcast_targets','usage_daily','usage_active_users') ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		tables = append(tables, n)
	}
	rows.Close()
	if len(tables) < 30 {
		t.Fatalf("expected many tenant tables, got %d", len(tables))
	}

	seen := 0
	for _, tbl := range tables {
		var inB int
		if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %q WHERE society_id=$1`, tbl), b.s.ID).Scan(&inB); err != nil {
			t.Fatal(err)
		}
		// as society A: B's rows are invisible, cannot be changed, cannot be written
		var visible int
		if err := rp.QueryRow(asSociety(a.s.ID), fmt.Sprintf(`SELECT count(*) FROM %q WHERE society_id=$1`, tbl), b.s.ID).Scan(&visible); err != nil {
			t.Fatalf("%s select: %v", tbl, err)
		}
		if visible != 0 {
			t.Errorf("%s: society A can see %d of B's rows", tbl, visible)
		}
		tag, err := rp.Exec(asSociety(a.s.ID), fmt.Sprintf(`DELETE FROM %q WHERE society_id=$1`, tbl), b.s.ID)
		if err != nil || tag.RowsAffected() != 0 {
			t.Errorf("%s: delete as A removed %d rows (err %v)", tbl, tag.RowsAffected(), err)
		}
		var still int
		_ = pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %q WHERE society_id=$1`, tbl), b.s.ID).Scan(&still)
		if still != inB {
			t.Errorf("%s: B's rows changed from %d to %d", tbl, inB, still)
		}
		if inB > 0 {
			seen++
			// the owner sees its own rows through the restricted role
			var own int
			if err := rp.QueryRow(asSociety(b.s.ID), fmt.Sprintf(`SELECT count(*) FROM %q WHERE society_id=$1`, tbl), b.s.ID).Scan(&own); err != nil || own != inB {
				t.Errorf("%s: B sees %d of its own %d rows (err %v)", tbl, own, inB, err)
			}
		}
	}
	if seen < 10 {
		t.Fatalf("the check only exercised %d populated tables", seen)
	}

	// a write for another society is refused by the policy
	if _, err := rp.Exec(asSociety(a.s.ID), `INSERT INTO blocks (society_id, name, code) VALUES ($1,'Sneaky','Z')`, b.s.ID); err == nil || !strings.Contains(err.Error(), "row-level security") {
		t.Errorf("insert into B as A: %v", err)
	}
	// code with no signed-in user (sign-in, sweepers, the console) still sees everything
	var all int
	if err := rp.QueryRow(ctx, `SELECT count(DISTINCT society_id) FROM memberships WHERE society_id IN ($1,$2)`, a.s.ID, b.s.ID).Scan(&all); err != nil || all != 2 {
		t.Errorf("platform context sees %d societies (err %v)", all, err)
	}
	// Unscoped is the explicit opt-out
	if err := rp.QueryRow(database.Unscoped(asSociety(a.s.ID)), `SELECT count(DISTINCT society_id) FROM memberships WHERE society_id IN ($1,$2)`, a.s.ID, b.s.ID).Scan(&all); err != nil || all != 2 {
		t.Errorf("unscoped context sees %d societies (err %v)", all, err)
	}
	// child tables follow their parent
	var lines int
	_ = rp.QueryRow(asSociety(a.s.ID), `SELECT count(*) FROM invoice_lines`).Scan(&lines)
	if lines != 0 {
		t.Errorf("society A sees %d invoice lines that belong to B", lines)
	}
	var blines int
	_ = rp.QueryRow(asSociety(b.s.ID), `SELECT count(*) FROM invoice_lines`).Scan(&blines)
	if blines == 0 {
		t.Error("society B sees none of its own invoice lines")
	}
	// platform-only tables are closed to tenants
	var inv int
	_ = rp.QueryRow(asSociety(b.s.ID), `SELECT count(*) FROM platform_invoices`).Scan(&inv)
	if inv != 0 {
		t.Errorf("a tenant reads %d platform invoices", inv)
	}
}

// A pooled connection must never carry the previous caller's society, and an unset session must see nothing.
func TestRLSConnectionsDoNotLeakBetweenCallersAndFailClosed(t *testing.T) {
	a := newSoc(t)
	b := newSoc(t)
	rp := rlsPool(t)
	rp.Config() // keep the linter honest about use
	for i := 0; i < 40; i++ {
		soc := a.ID
		other := b.ID
		if i%2 == 1 {
			soc, other = b.ID, a.ID
		}
		var mine, theirs int
		_ = rp.QueryRow(asSociety(soc), `SELECT count(*) FROM memberships WHERE society_id=$1`, soc).Scan(&mine)
		_ = rp.QueryRow(asSociety(soc), `SELECT count(*) FROM memberships WHERE society_id=$1`, other).Scan(&theirs)
		if mine == 0 || theirs != 0 {
			t.Fatalf("round %d: mine=%d theirs=%d", i, mine, theirs)
		}
	}
	// a raw connection with nothing set (role only) sees no tenant row at all
	conn, err := rp.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(context.Background(), `SELECT set_config('role','nexora_app',false), set_config('app.society_id','',false)`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM memberships`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("an unset session sees %d memberships (err %v)", n, err)
	}
	// and the role cannot switch the lock off by itself being a superuser
	var super bool
	_ = conn.QueryRow(context.Background(), `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&super)
	if super {
		t.Fatal("the restricted role is a superuser or bypasses RLS")
	}
}

// Walk every route with society B's real ids while signed in as society A: nothing may answer 2xx.
func TestNoRouteLetsOneSocietyTouchAnothers(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	a := newCommunity(t)
	b := newCommunity(t)
	populate(t, b)
	ctx := context.Background()

	// B's ids, from every tenant table
	var ids []string
	rows, err := pool.Query(ctx, `SELECT table_name FROM information_schema.columns WHERE table_schema='public' AND column_name='society_id' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		tables = append(tables, n)
	}
	rows.Close()
	for _, tbl := range tables {
		r, err := pool.Query(ctx, fmt.Sprintf(`SELECT id::text FROM %q WHERE society_id=$1 AND id IS NOT NULL LIMIT 2`, tbl), b.s.ID)
		if err != nil {
			continue // table without an id column
		}
		for r.Next() {
			var id string
			if r.Scan(&id) == nil && len(id) == 36 {
				ids = append(ids, id)
			}
		}
		r.Close()
	}
	ids = append(ids, b.units["A101"], b.units["B101"], b.blockA, b.blockB)
	if len(ids) < 20 {
		t.Fatalf("only %d ids harvested", len(ids))
	}

	type route struct{ method, path string }
	var routes []route
	_ = chi.Walk(arouter, func(method, path string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(path, "/api/v1/") || strings.HasPrefix(path, "/api/v1/superadmin") || strings.Contains(path, "/auth/") || path == "/api/v1/ws" {
			return nil
		}
		if strings.Contains(path, "{") { // routes that take an id: those are the ones that can reach someone else's row
			routes = append(routes, route{method, path})
		}
		return nil
	})
	if len(routes) < 40 {
		t.Fatalf("only %d id routes found", len(routes))
	}

	// routes that answer 2xx for any id by design (idempotent and per-user): reviewed one by one
	allowed := map[string]bool{
		// deletes the caller's own device by its client-side id (`WHERE user_id = me`); an unknown id is a no-op
		"DELETE /api/v1/me/devices/{id}": true,
	}
	calls := 0
	for _, who := range []struct{ name, tok string }{{"admin of A", a.s.Admin}, {"resident of A", a.rA}, {"guard of A", a.guard.Token}} {
		for _, rt := range routes {
			for _, id := range ids {
				path := rt.path
				for strings.Contains(path, "{") {
					i, j := strings.Index(path, "{"), strings.Index(path, "}")
					path = path[:i] + id + path[j+1:]
				}
				req := httptest.NewRequest(rt.method, path, strings.NewReader(`{}`))
				req.Header.Set("Authorization", "Bearer "+who.tok)
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				done := make(chan struct{})
				go func() { arouter.ServeHTTP(w, req); close(done) }()
				select {
				case <-done:
				case <-time.After(20 * time.Second):
					t.Fatalf("%s: %s %s hangs (a code path holds a database connection while asking for another?)", who.name, rt.method, path)
				}
				calls++
				if w.Code >= 200 && w.Code < 300 && !allowed[rt.method+" "+rt.path] {
					t.Errorf("%s: %s %s answered %d for another society's id %s: %.200s", who.name, rt.method, rt.path, w.Code, id, w.Body.String())
				}
			}
		}
	}
	t.Logf("%d routes x %d ids x 3 users = %d calls", len(routes), len(ids), calls)
}
