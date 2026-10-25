package app_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Nexora48/nexora-api/internal/limits"
	"github.com/Nexora48/nexora-api/internal/superadmin"
	"github.com/Nexora48/nexora-api/internal/usage"
)

func newPlan(t *testing.T, tok, key string, feats []string, lim map[string]any) string {
	t.Helper()
	r := sa(t, "POST", "/plans", tok, map[string]any{"key": key, "name": "Plan " + key, "rate_per_unit_paise": 1200, "flat_fee_paise": 50000, "features": feats, "limits": lim})
	if r.Status != 201 {
		t.Fatalf("create plan: %d %v", r.Status, r.Body)
	}
	return r.str("id")
}

func uniq(p string) string { return fmt.Sprintf("%s_%s", p, uuid.NewString()[:6]) }

func assign(t *testing.T, tok string, society uuid.UUID, plan string, keep bool) resp {
	t.Helper()
	return sa(t, "PUT", "/societies/"+society.String()+"/plan", tok, map[string]any{"plan_id": plan, "keep_custom_price": keep})
}

func TestPlansCRUDAndAssign(t *testing.T) {
	s := newSoc(t)
	tok := superToken(t)
	sup := supportToken(t)
	key := uniq("tiny")

	if r := sa(t, "POST", "/plans", sup, map[string]any{"key": key, "name": "X"}); r.Status != 403 {
		t.Fatalf("support creates plan: %d", r.Status)
	}
	for _, bad := range []map[string]any{
		{"key": "Bad Key", "name": "x"},
		{"key": uniq("a"), "name": ""},
		{"key": uniq("a"), "name": "x", "features": []string{"nope"}},
		{"key": uniq("a"), "name": "x", "limits": map[string]any{"max_dogs": 2}},
		{"key": uniq("a"), "name": "x", "limits": map[string]any{"max_units": -1}},
		{"key": uniq("a"), "name": "x", "rate_per_unit_paise": -5},
	} {
		if r := sa(t, "POST", "/plans", tok, bad); r.Status != 400 {
			t.Fatalf("bad plan %v: %d", bad, r.Status)
		}
	}
	pid := newPlan(t, tok, key, []string{"visitors", "visitors", "sos"}, map[string]any{"max_units": 3})
	if r := sa(t, "POST", "/plans", tok, map[string]any{"key": key, "name": "dup"}); r.Status != 409 || r.errCode() != "PLAN_KEY_TAKEN" {
		t.Fatalf("duplicate key: %d %v", r.Status, r.Body)
	}
	if r := sa(t, "GET", "/plans/"+pid, sup, nil); r.Status != 200 || len(r.data()["features"].([]any)) != 2 {
		t.Fatalf("read as support: %d %v", r.Status, r.Body)
	}

	// assigning copies the price and sets the services
	r := assign(t, tok, s.ID, pid, false)
	if r.Status != 200 {
		t.Fatalf("assign: %d %v", r.Status, r.Body)
	}
	sub := r.obj("subscription")
	if num(sub, "rate_per_unit_paise") != 1200 || num(sub, "flat_fee_paise") != 50000 {
		t.Fatalf("price not copied: %v", sub)
	}
	f := call(t, "GET", "/api/v1/me/features", s.Admin, nil)
	if f.Status != 200 {
		t.Fatalf("features: %d %v", f.Status, f.Body)
	}
	on := map[string]bool{}
	for k, v := range f.data() {
		if b, ok := v.(bool); ok {
			on[k] = b
		}
	}
	if fm, ok := f.data()["features"].(map[string]any); ok {
		for k, v := range fm {
			on[k], _ = v.(bool)
		}
	}
	if !on["visitors"] || !on["sos"] || on["payments"] || on["amenities"] {
		t.Fatalf("services not set from plan: %v", on)
	}
	var n int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_logs WHERE society_id=$1 AND action='plan.assign'`, s.ID).Scan(&n)
	if n != 1 {
		t.Fatalf("assign not audited: %d", n)
	}

	// editing the plan changes existing bills only when asked
	up := sa(t, "PUT", "/plans/"+pid, tok, map[string]any{"name": "Tiny", "rate_per_unit_paise": 2000, "flat_fee_paise": 0, "features": []string{"visitors"}, "limits": map[string]any{"max_units": 3}})
	if up.Status != 200 {
		t.Fatalf("update: %d %v", up.Status, up.Body)
	}
	if sub := sa(t, "GET", "/societies/"+s.ID.String()+"/subscription", tok, nil); num(sub.data(), "rate_per_unit_paise") != 1200 {
		t.Fatalf("plan edit changed an existing price: %v", sub.Body)
	}
	up = sa(t, "PUT", "/plans/"+pid, tok, map[string]any{"name": "Tiny", "rate_per_unit_paise": 2000, "flat_fee_paise": 0, "features": []string{"visitors"}, "limits": map[string]any{"max_units": 3}, "apply_to_societies": true})
	if sub := sa(t, "GET", "/societies/"+s.ID.String()+"/subscription", tok, nil); num(sub.data(), "rate_per_unit_paise") != 2000 {
		t.Fatalf("apply_to_societies did not re-price: %v", sub.Body)
	}

	// keep_custom_price
	other := newPlan(t, tok, uniq("other"), []string{"visitors"}, nil)
	if r := assign(t, tok, s.ID, other, true); r.Status != 200 || num(r.obj("subscription"), "rate_per_unit_paise") != 2000 {
		t.Fatalf("keep custom price: %d %v", r.Status, r.Body)
	}

	// a plan in use cannot be archived; an archived plan cannot be assigned
	if r := sa(t, "POST", "/plans/"+other+"/archive", tok, nil); r.Status != 409 || r.errCode() != "PLAN_IN_USE" {
		t.Fatalf("archive in use: %d %v", r.Status, r.Body)
	}
	if r := sa(t, "POST", "/plans/"+pid+"/archive", tok, nil); r.Status != 200 {
		t.Fatalf("archive: %d %v", r.Status, r.Body)
	}
	if r := assign(t, tok, s.ID, pid, false); r.Status != 409 || r.errCode() != "PLAN_ARCHIVED" {
		t.Fatalf("assign archived: %d %v", r.Status, r.Body)
	}
	if r := assign(t, tok, uuid.New(), other, false); r.Status != 404 {
		t.Fatalf("assign to unknown society: %d", r.Status)
	}
	if r := sa(t, "GET", "/plans/"+other+"/societies", tok, nil); r.Status != 200 || len(r.Body["data"].([]any)) != 1 {
		t.Fatalf("plan societies: %d %v", r.Status, r.Body)
	}
}

func TestPlanLimitsEnforced(t *testing.T) {
	s := newSoc(t)
	tok := superToken(t)
	ctx := context.Background()
	pid := newPlan(t, tok, uniq("lim"), []string{"visitors"}, map[string]any{"max_units": 4, "max_residents": 2, "max_guards": 1, "max_admins": 1, "sms_per_month": 0, "visitors_per_month": 1})

	// no plan = unlimited
	bidFree, _ := setupBlock(t, s, "F", 1, 5)
	_ = bidFree

	assign(t, tok, s.ID, pid, false) // 5 units already: over the limit, but nothing is removed
	snap := call(t, "GET", "/api/v1/society/plan", s.Admin, nil)
	if snap.Status != 200 || snap.str("plan_key") == "" {
		t.Fatalf("my plan: %d %v", snap.Status, snap.Body)
	}
	var units map[string]any
	for _, u := range snap.data()["usage"].([]any) {
		if m := u.(map[string]any); m["key"] == "max_units" {
			units = m
		}
	}
	if num(units, "used") != 5 || num(units, "max") != 4 || units["exceeded"] != true {
		t.Fatalf("usage: %v", units)
	}

	// units: single and bulk are refused / trimmed
	b := call(t, "POST", "/api/v1/society/blocks", s.Admin, map[string]any{"name": "Block G", "code": "G"})
	bid := b.str("id")
	if r := call(t, "POST", "/api/v1/society/units", s.Admin, map[string]any{"block_id": bid, "floor": 1, "number": "G101", "unit_type": "2BHK"}); r.Status != 422 || r.errCode() != "PLAN_LIMIT" {
		t.Fatalf("single unit over limit: %d %v", r.Status, r.Body)
	}
	if r := call(t, "POST", "/api/v1/society/blocks/"+bid+"/units/bulk", s.Admin, map[string]any{"floors": 1, "units_per_floor": 3, "unit_type": "2BHK"}); r.Status != 201 || num(r.data(), "created") != 0 || len(r.data()["skipped_plan_limit"].([]any)) != 3 {
		t.Fatalf("bulk over limit: %d %v", r.Status, r.Body)
	}

	// raising the limit unblocks (and a bulk creates only what fits)
	sa(t, "PUT", "/plans/"+pid, tok, map[string]any{"name": "Lim", "features": []string{"visitors"}, "limits": map[string]any{"max_units": 7, "max_residents": 2, "max_guards": 1, "max_admins": 1, "sms_per_month": 0, "visitors_per_month": 1}})
	if r := call(t, "POST", "/api/v1/society/blocks/"+bid+"/units/bulk", s.Admin, map[string]any{"floors": 1, "units_per_floor": 3, "unit_type": "2BHK"}); r.Status != 201 || num(r.data(), "created") != 2 || len(r.data()["skipped_plan_limit"].([]any)) != 1 {
		t.Fatalf("bulk partly fits: %d %v", r.Status, r.Body)
	}
	if r := call(t, "POST", "/api/v1/society/blocks/"+bid+"/units/bulk?dry_run=true", s.Admin, map[string]any{"floors": 1, "units_per_floor": 3, "unit_type": "2BHK"}); r.Status >= 500 {
		t.Fatalf("dry run: %d", r.Status)
	}

	// residents and family
	units2, _ := callList(t, "/api/v1/society/units?page_size=100", s.Admin)
	u := func(i int) string { return units2[i]["id"].(string) }
	if r := addResident(t, s, u(0), "owner", "One", nextPhone()); r.Status != 201 {
		t.Fatalf("resident 1: %d %v", r.Status, r.Body)
	}
	if r := addResident(t, s, u(1), "owner", "Two", nextPhone()); r.Status != 201 {
		t.Fatalf("resident 2: %d %v", r.Status, r.Body)
	}
	if r := addResident(t, s, u(2), "owner", "Three", nextPhone()); r.Status != 422 || r.errCode() != "PLAN_LIMIT" {
		t.Fatalf("resident 3: %d %v", r.Status, r.Body)
	}

	// guards
	if r := call(t, "POST", "/api/v1/society/guards", s.Admin, map[string]any{"full_name": "G1", "phone": nextPhone()}); r.Status != 201 {
		t.Fatalf("guard 1: %d %v", r.Status, r.Body)
	}
	if r := call(t, "POST", "/api/v1/society/guards", s.Admin, map[string]any{"full_name": "G2", "phone": nextPhone()}); r.Status != 422 || r.errCode() != "PLAN_LIMIT" {
		t.Fatalf("guard 2: %d %v", r.Status, r.Body)
	}

	// admins (the society already has its one admin)
	if r := sa(t, "POST", "/societies/"+s.ID.String()+"/admins", tok, map[string]any{"full_name": "Second Admin", "email": fmt.Sprintf("a2-%s@x.test", uuid.NewString()[:6]), "phone": nextPhone()}); r.Status != 422 || r.errCode() != "PLAN_LIMIT" {
		t.Fatalf("second admin: %d %v", r.Status, r.Body)
	}

	// residents and guards cannot read the plan card
	if r := call(t, "GET", "/api/v1/society/plan", newResidentTok(t, s, u(3)), nil); r.Status != 403 {
		t.Fatalf("resident reads plan: %d", r.Status)
	}

	// SMS: OTP/SOS are never blocked, other messages stop at the limit (0 here)
	l := limits.New(pool)
	if ok, _ := l.AllowSMS(ctx, s.ID, true); !ok {
		t.Fatal("critical SMS blocked")
	}
	if ok, _ := l.AllowSMS(ctx, s.ID, false); ok {
		t.Fatal("non-critical SMS allowed at limit 0")
	}
	// soft limits never block
	if err := l.Check(ctx, s.ID, limits.VisitorsPerMonth, 100); err != nil {
		t.Fatalf("soft limit blocked: %v", err)
	}
	// a society with no plan is unlimited
	free := newSoc(t)
	if err := l.Check(ctx, free.ID, limits.MaxUnits, 100000); err != nil {
		t.Fatalf("no plan blocked: %v", err)
	}
	// a nil checker allows everything
	var none *limits.Checker
	if err := none.Check(ctx, free.ID, limits.MaxUnits, 1); err != nil {
		t.Fatal(err)
	}
}

func newResidentTok(t *testing.T, s soc, unit string) string {
	t.Helper()
	phone := nextPhone()
	mustExec(t, context.Background(), `INSERT INTO users (id, full_name, phone) VALUES ($1,'Direct Resident',$2)`, uuid.New(), phone)
	mustExec(t, context.Background(), `INSERT INTO memberships (user_id, society_id, unit_id, role_key) SELECT id,$2,$3,'resident_owner' FROM users WHERE phone=$1`, phone, s.ID, unit)
	mr.FlushAll()
	l := otpLogin(t, phone)
	if l.Status != 200 {
		t.Fatalf("resident login: %d %v", l.Status, l.Body)
	}
	return l.str("access_token")
}

// ── usage rollup & analytics ─────────────────────────────────────────────────

func usageVal(t *testing.T, soc uuid.UUID, day, metric string) int64 {
	t.Helper()
	var v int64
	err := pool.QueryRow(context.Background(), `SELECT COALESCE(sum(value),0) FROM usage_daily WHERE society_id=$1 AND day=$2 AND metric=$3`, soc, day, metric).Scan(&v)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func insertVisit(t *testing.T, c community, purpose, at string) {
	t.Helper()
	mustExec(t, context.Background(), `INSERT INTO visits (society_id, unit_id, visitor_name, purpose, status, source, expected_from, expected_until, created_at) VALUES ($1,$2,'Rollup Visitor',$3,'completed','walk_in',$4::timestamptz,$4::timestamptz,$4::timestamptz)`,
		c.s.ID, c.units["A101"], purpose, at)
}

func TestUsageRollupIdempotentAndTimezoneAware(t *testing.T) {
	c := newCommunity(t)
	ctx := context.Background()
	// the society lives in Auckland (UTC+13 in January), so 2026-01-10 11:30 UTC is already 11 Jan locally
	mustExec(t, ctx, `UPDATE societies SET timezone='Pacific/Auckland' WHERE id=$1`, c.s.ID)
	insertVisit(t, c, "guest", "2026-01-10 08:00+00")    // 10 Jan 21:00 local
	insertVisit(t, c, "guest", "2026-01-10 11:30+00")    // 11 Jan 00:30 local
	insertVisit(t, c, "delivery", "2026-01-10 12:00+00") // 11 Jan 01:00 local

	svc := usage.New(pool)
	clock := time.Date(2026, 1, 12, 5, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return clock })
	for i := 0; i < 2; i++ { // twice: same numbers
		if err := svc.Rollup(ctx, time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)); err != nil {
			t.Fatal(err)
		}
		if err := svc.Rollup(ctx, time.Date(2026, 1, 11, 0, 0, 0, 0, time.UTC)); err != nil {
			t.Fatal(err)
		}
	}
	if v := usageVal(t, c.s.ID, "2026-01-10", usage.Visitors); v != 1 {
		t.Fatalf("10 Jan visitors: %d", v)
	}
	if v := usageVal(t, c.s.ID, "2026-01-11", usage.Visitors); v != 1 {
		t.Fatalf("11 Jan visitors: %d", v)
	}
	if v := usageVal(t, c.s.ID, "2026-01-11", usage.Deliveries); v != 1 {
		t.Fatalf("11 Jan deliveries: %d", v)
	}
	var rows int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM usage_daily WHERE society_id=$1 AND day='2026-01-10' AND metric='visitors'`, c.s.ID).Scan(&rows)
	if rows != 1 {
		t.Fatalf("rollup duplicated rows: %d", rows)
	}
	// stock metrics exist only near "today" (12 Jan local): 11 Jan is yesterday, 10 Jan is too old
	if usageVal(t, c.s.ID, "2026-01-11", usage.Units) != 4 {
		t.Fatalf("units snapshot missing for yesterday")
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM usage_daily WHERE society_id=$1 AND day='2026-01-10' AND metric='units'`, c.s.ID).Scan(&n)
	if n != 0 {
		t.Fatal("stock metric written for an old day")
	}
}

func TestUsageActiveUsersLoginsAndPrune(t *testing.T) {
	c := newCommunity(t) // logs in an owner of A101 and B101 (and others) just now
	ctx := context.Background()
	svc := usage.New(pool)
	if err := svc.Rollup(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	today := time.Now().In(ist).Format("2006-01-02")
	var live int64
	_ = pool.QueryRow(ctx, `SELECT count(DISTINCT t.user_id) FROM refresh_tokens t JOIN memberships m ON m.id=t.membership_id WHERE m.society_id=$1`, c.s.ID).Scan(&live)
	if got := usageVal(t, c.s.ID, today, usage.ActiveUsers); got != live || live < 2 {
		t.Fatalf("active users %d, direct count %d", got, live)
	}
	if got := usageVal(t, c.s.ID, today, usage.Logins); got < 2 {
		t.Fatalf("logins %d", got)
	}
	// a refresh is activity but not a new sign-in
	before := usageVal(t, c.s.ID, today, usage.Logins)
	_ = before
	var users int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM usage_active_users WHERE society_id=$1`, c.s.ID).Scan(&users)
	if int64(users) != live {
		t.Fatalf("active user rows %d vs %d", users, live)
	}
	// prune drops rows older than 30 days only
	mustExec(t, ctx, `INSERT INTO usage_active_users (society_id, day, user_id) SELECT $1, current_date - 45, user_id FROM memberships WHERE society_id=$1 LIMIT 1`, c.s.ID)
	if err := svc.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	var old int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM usage_active_users WHERE society_id=$1 AND day < current_date - 30`, c.s.ID).Scan(&old)
	if old != 0 {
		t.Fatal("old activity rows survived prune")
	}
}

func TestAnalyticsEndpoints(t *testing.T) {
	c := newCommunity(t)
	ctx := context.Background()
	tok, sup := superToken(t), supportToken(t)
	insertVisit(t, c, "guest", time.Now().Format(time.RFC3339))
	insertVisit(t, c, "guest", time.Now().Format(time.RFC3339))
	if err := usage.New(pool).Backfill(ctx, 3); err != nil {
		t.Fatal(err)
	}
	sid := c.s.ID.String()

	// tenants cannot reach platform analytics
	if r := sa(t, "GET", "/analytics/overview", c.s.Admin, nil); r.Status != 401 {
		t.Fatalf("admin on platform route: %d", r.Status)
	}
	// support can read
	if r := sa(t, "GET", "/analytics/overview", sup, nil); r.Status != 200 {
		t.Fatalf("support overview: %d %v", r.Status, r.Body)
	}
	if r := sa(t, "GET", "/analytics/overview?range=11", tok, nil); r.Status != 400 {
		t.Fatalf("bad range: %d", r.Status)
	}
	if r := sa(t, "GET", "/analytics/timeseries?metric=nope", tok, nil); r.Status != 400 {
		t.Fatalf("bad metric: %d", r.Status)
	}
	ts := sa(t, "GET", "/analytics/timeseries?metric=visitors&range=7&society_id="+sid, tok, nil)
	pts, _ := ts.Body["data"].([]any)
	if ts.Status != 200 || len(pts) != 7 {
		t.Fatalf("timeseries: %d len %d", ts.Status, len(pts))
	}
	var sum int64
	for _, p := range pts {
		sum += num(p.(map[string]any), "value")
	}
	var direct int64
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM visits WHERE society_id=$1 AND purpose <> 'delivery' AND created_at > now() - interval '7 days'`, c.s.ID).Scan(&direct)
	if sum != direct || sum < 2 {
		t.Fatalf("series sum %d vs direct %d", sum, direct)
	}
	top := sa(t, "GET", "/analytics/top?metric=visitors&range=7", tok, nil)
	found := false
	for _, r := range top.Body["data"].([]any) {
		if r.(map[string]any)["society_id"] == sid {
			found = true
		}
	}
	if top.Status != 200 || !found {
		t.Fatalf("top: %d %v", top.Status, top.Body)
	}
	// services: visitors is on for the society and used; complaints is on but unused
	ad := sa(t, "GET", "/analytics/services?range=30", tok, nil)
	var visitorsUsed, complaintsUnused bool
	for _, a := range ad.Body["data"].([]any) {
		m := a.(map[string]any)
		for _, u := range m["unused"].([]any) {
			if m["key"] == "complaints" && u.(map[string]any)["id"] == sid {
				complaintsUnused = true
			}
			if m["key"] == "visitors" && u.(map[string]any)["id"] == sid {
				visitorsUsed = false
			}
		}
		if m["key"] == "visitors" && num(m, "enabled") >= 1 {
			visitorsUsed = true
		}
	}
	if ad.Status != 200 || !visitorsUsed || !complaintsUnused {
		t.Fatalf("adoption: %d visitorsUsed=%v complaintsUnused=%v", ad.Status, visitorsUsed, complaintsUnused)
	}
	// per-society usage with plan, differs-from-plan and month totals
	pid := newPlan(t, tok, uniq("usg"), []string{"visitors"}, map[string]any{"max_units": 4})
	assign(t, tok, c.s.ID, pid, false)
	call(t, "PUT", "/api/v1/superadmin/societies/"+sid+"/features/polls", tok, map[string]any{"enabled": true, "reason": "test"})
	us := sa(t, "GET", "/societies/"+sid+"/usage", tok, nil)
	if us.Status != 200 || us.obj("plan")["plan_key"] == "" {
		t.Fatalf("society usage: %d %v", us.Status, us.Body)
	}
	if num(us.obj("month_totals"), "visitors") < 2 {
		t.Fatalf("month totals: %v", us.obj("month_totals"))
	}
	if r := sa(t, "GET", "/societies/"+uuid.NewString()+"/usage", tok, nil); r.Status != 404 {
		t.Fatalf("unknown society usage: %d", r.Status)
	}
	ov := sa(t, "GET", "/analytics/overview?range=7", tok, nil)
	if ov.Status != 200 || num(ov.data(), "societies") < 1 || ov.obj("series")["visitors"] == nil {
		t.Fatalf("overview: %d %v", ov.Status, ov.Body)
	}
	// the society is at its flat limit (4 of 4 = at the plan limit) → shows as an alert
	alerts, _ := ov.data()["alerts"].([]any)
	got := false
	for _, a := range alerts {
		if a.(map[string]any)["society_id"] == sid {
			got = true
		}
	}
	if !got {
		t.Fatalf("no limit alert: %v", alerts)
	}
}

// ── audit v2 ─────────────────────────────────────────────────────────────────

func TestAuditFiltersDetailAndExport(t *testing.T) {
	s := newSoc(t)
	tok, sup := superToken(t), supportToken(t)
	ctx := context.Background()
	sid := s.ID.String()
	// platform actions on this society
	if r := sa(t, "PUT", "/societies/"+sid+"/features/polls", tok, map[string]any{"enabled": false, "reason": "test"}); r.Status != 200 {
		t.Fatalf("toggle: %d %v", r.Status, r.Body)
	}
	mustExec(t, ctx, `INSERT INTO audit_logs (actor_type, society_id, action, entity, entity_id, meta) VALUES ('system',$1,'job.run','job','=SUM(1)','{}')`, s.ID)

	list := func(q string) []map[string]any {
		r := sa(t, "GET", "/audit?"+q, sup, nil) // support can read
		if r.Status != 200 {
			t.Fatalf("audit %s: %d %v", q, r.Status, r.Body)
		}
		var out []map[string]any
		for _, x := range r.Body["data"].([]any) {
			out = append(out, x.(map[string]any))
		}
		return out
	}
	if rows := list("society_id=" + sid + "&actor_type=platform"); len(rows) != 1 || rows[0]["action"] != "feature.toggle" || rows[0]["actor_role"] != "super_admin" || rows[0]["actor_email"] == nil {
		t.Fatalf("platform filter: %v", rows)
	}
	if rows := list("society_id=" + sid + "&actor_type=system"); len(rows) != 1 {
		t.Fatalf("system filter: %v", rows)
	}
	if rows := list("society_id=" + sid + "&q=job.run"); len(rows) != 1 || rows[0]["action"] != "job.run" {
		t.Fatalf("text filter: %v", rows)
	}
	if rows := list("society_id=" + sid + "&action=feature.*"); len(rows) != 1 {
		t.Fatalf("prefix filter: %v", rows)
	}
	if rows := list("society_id=" + sid + "&entity=job"); len(rows) != 1 {
		t.Fatalf("entity filter: %v", rows)
	}
	if rows := list("society_id=" + sid + "&from=" + time.Now().Format("2006-01-02") + "&to=" + time.Now().Format("2006-01-02")); len(rows) < 2 {
		t.Fatalf("a bare 'to' date should include that day: %d", len(rows))
	}
	if r := sa(t, "GET", "/audit?actor_type=robot", tok, nil); r.Status != 400 {
		t.Fatalf("bad actor_type: %d", r.Status)
	}
	id := list("society_id=" + sid + "&action=feature.toggle")[0]["id"].(float64)
	d := sa(t, "GET", fmt.Sprintf("/audit/%d", int64(id)), sup, nil)
	if d.Status != 200 || d.obj("meta")["changes"] == nil {
		t.Fatalf("detail: %d %v", d.Status, d.Body)
	}
	if r := sa(t, "GET", "/audit/99999999", tok, nil); r.Status != 404 {
		t.Fatalf("unknown entry: %d", r.Status)
	}

	// CSV export: formulas are neutralised and the export is itself audited
	st, ct, body := rawGet(t, saBase+"/audit/export.csv?society_id="+sid, tok)
	if st != 200 || !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("export: %d %s", st, ct)
	}
	txt := string(body)
	if !strings.Contains(txt, "feature.toggle") || strings.Contains(txt, ",=SUM(1)") || !strings.Contains(txt, ",'=SUM(1),") {
		t.Fatalf("csv content: %s", txt)
	}
	if rows := list("society_id=&action=audit.export"); len(rows) < 1 {
		t.Fatalf("export not audited")
	}
	if st, _, _ := rawGet(t, saBase+"/audit/export.csv", s.Admin); st != 401 {
		t.Fatalf("tenant token on export: %d", st)
	}
}

func TestAuditRetentionPurge(t *testing.T) {
	tok := superToken(t)
	ctx := context.Background()
	mustExec(t, ctx, `DELETE FROM audit_logs`)
	for _, typ := range []string{"user", "platform"} {
		mustExec(t, ctx, `INSERT INTO audit_logs (actor_type, action, entity, created_at) VALUES ($1,'old.thing','x', now() - interval '500 days')`, typ)
		mustExec(t, ctx, `INSERT INTO audit_logs (actor_type, action, entity, created_at) VALUES ($1,'older.thing','x', now() - interval '900 days')`, typ)
		mustExec(t, ctx, `INSERT INTO audit_logs (actor_type, action, entity, created_at) VALUES ($1,'fresh.thing','x', now())`, typ)
	}
	if r := sa(t, "PUT", "/audit/settings", tok, map[string]any{"retention_days": 10}); r.Status != 400 {
		t.Fatalf("too short retention: %d", r.Status)
	}
	if r := sa(t, "PUT", "/audit/settings", tok, map[string]any{"retention_days": 400}); r.Status != 200 {
		t.Fatalf("set retention: %d %v", r.Status, r.Body)
	}
	n, err := aplat.PurgeAudit(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// user rows older than 400 days (2) go; platform rows are kept 800 days, so only the 900-day one goes (1)
	if n != 3 {
		t.Fatalf("purged %d", n)
	}
	var left, summary int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action IN ('old.thing','older.thing','fresh.thing')`).Scan(&left)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action='audit.purge'`).Scan(&summary)
	if left != 3 || summary != 1 {
		t.Fatalf("left %d summary %d", left, summary)
	}
	// running again removes nothing and adds no second summary
	if n, _ := aplat.PurgeAudit(ctx, time.Now()); n != 0 {
		t.Fatalf("second purge removed %d", n)
	}
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action='audit.purge'`).Scan(&summary)
	if summary != 1 {
		t.Fatalf("summaries %d", summary)
	}
}

// ── staff users ──────────────────────────────────────────────────────────────

func TestStaffInviteAcceptAndMFAReset(t *testing.T) {
	boss := newStaff(t, superadmin.RoleSuper, true)
	bossTok := saLogin(t, boss, 0).str("access_token")
	sup := supportToken(t)
	email := fmt.Sprintf("new-%s@nexora.test", uuid.NewString()[:6])

	if r := sa(t, "POST", "/staff", sup, map[string]any{"email": email, "full_name": "Nia", "role": "platform_support"}); r.Status != 403 {
		t.Fatalf("support invites: %d", r.Status)
	}
	if r := sa(t, "GET", "/staff", sup, nil); r.Status != 403 {
		t.Fatalf("support lists staff: %d", r.Status)
	}
	for _, bad := range []map[string]any{
		{"email": "nope", "full_name": "x", "role": "platform_support"},
		{"email": email, "full_name": "", "role": "platform_support"},
		{"email": email, "full_name": "x", "role": "owner"},
	} {
		if r := sa(t, "POST", "/staff", bossTok, bad); r.Status != 400 {
			t.Fatalf("bad invite %v: %d", bad, r.Status)
		}
	}
	inv := sa(t, "POST", "/staff", bossTok, map[string]any{"email": email, "full_name": "Nia", "role": "platform_support"})
	if inv.Status != 201 || inv.str("invite_token") == "" || inv.obj("user")["invite_pending"] != true {
		t.Fatalf("invite: %d %v", inv.Status, inv.Body)
	}
	if r := sa(t, "POST", "/staff", bossTok, map[string]any{"email": strings.ToUpper(email), "full_name": "Dup", "role": "platform_support"}); r.Status != 409 {
		t.Fatalf("duplicate email: %d", r.Status)
	}
	uid := inv.obj("user")["id"].(string)
	tokenValue := inv.str("invite_token")

	// before accepting, no password works
	if r := sa(t, "POST", "/auth/login", "", map[string]any{"email": email, "password": "!invited"}); r.Status != 401 {
		t.Fatalf("login before accept: %d", r.Status)
	}
	if r := sa(t, "POST", "/auth/accept-invite", "", map[string]any{"token": tokenValue, "password": "weak"}); r.Status != 400 {
		t.Fatalf("weak password: %d", r.Status)
	}
	if r := sa(t, "POST", "/auth/accept-invite", "", map[string]any{"token": "deadbeef", "password": "Strong@12345"}); r.Status != 400 || r.errCode() != "INVITE_INVALID" {
		t.Fatalf("bad token: %d %v", r.Status, r.Body)
	}
	if r := sa(t, "POST", "/auth/accept-invite", "", map[string]any{"token": tokenValue, "password": "Strong@12345"}); r.Status != 204 {
		t.Fatalf("accept: %d %v", r.Status, r.Body)
	}
	if r := sa(t, "POST", "/auth/accept-invite", "", map[string]any{"token": tokenValue, "password": "Other@12345"}); r.Status != 400 {
		t.Fatalf("token reused: %d", r.Status)
	}

	// first sign-in enrols an authenticator
	l := sa(t, "POST", "/auth/login", "", map[string]any{"email": email, "password": "Strong@12345"})
	if l.Status != 200 || l.data()["mfa_setup_required"] != true {
		t.Fatalf("first login: %d %v", l.Status, l.Body)
	}
	secret := l.str("secret")
	v := sa(t, "POST", "/auth/mfa/verify", "", map[string]any{"mfa_token": l.str("mfa_token"), "code": code(t, secret, 0)})
	if v.Status != 200 {
		t.Fatalf("enrol: %d %v", v.Status, v.Body)
	}
	newTok := v.str("access_token")
	if r := sa(t, "GET", "/plans", newTok, nil); r.Status != 200 {
		t.Fatalf("support reads: %d", r.Status)
	}

	// an expired invitation is refused and can be re-issued
	inv2 := sa(t, "POST", "/staff", bossTok, map[string]any{"email": "late-" + email, "full_name": "Late", "role": "platform_support"})
	mustExec(t, context.Background(), `UPDATE platform_users SET invite_expires_at = now() - interval '1 hour' WHERE id=$1`, inv2.obj("user")["id"])
	if r := sa(t, "POST", "/auth/accept-invite", "", map[string]any{"token": inv2.str("invite_token"), "password": "Strong@12345"}); r.Status != 400 {
		t.Fatalf("expired invite: %d", r.Status)
	}
	re := sa(t, "POST", "/staff/"+inv2.obj("user")["id"].(string)+"/reinvite", bossTok, nil)
	if re.Status != 200 || re.str("invite_token") == inv2.str("invite_token") {
		t.Fatalf("reinvite: %d %v", re.Status, re.Body)
	}
	if r := sa(t, "POST", "/auth/accept-invite", "", map[string]any{"token": re.str("invite_token"), "password": "Strong@12345"}); r.Status != 204 {
		t.Fatalf("accept reissued: %d", r.Status)
	}
	if r := sa(t, "POST", "/staff/"+uid+"/reinvite", bossTok, nil); r.Status != 409 {
		t.Fatalf("reinvite accepted user: %d", r.Status)
	}

	// reset MFA: the old session dies, the next sign-in enrols again; you cannot reset your own
	if r := sa(t, "POST", "/staff/"+boss.ID.String()+"/reset-mfa", bossTok, nil); r.Status != 409 || r.errCode() != "CANNOT_MODIFY_SELF" {
		t.Fatalf("self reset: %d %v", r.Status, r.Body)
	}
	if r := sa(t, "POST", "/staff/"+uid+"/reset-mfa", bossTok, nil); r.Status != 200 || r.data()["mfa_enabled"] != false {
		t.Fatalf("reset mfa: %d %v", r.Status, r.Body)
	}
	l = sa(t, "POST", "/auth/login", "", map[string]any{"email": email, "password": "Strong@12345"})
	if l.data()["mfa_setup_required"] != true || l.str("secret") == secret {
		t.Fatalf("re-enrolment not forced: %v", l.Body)
	}
	if r := sa(t, "POST", "/staff/"+uuid.NewString()+"/reset-mfa", bossTok, nil); r.Status != 404 {
		t.Fatalf("unknown user: %d", r.Status)
	}
}

func TestStaffGuards(t *testing.T) {
	// a clean slate: exactly two active super admins (A acts on B) plus one support user
	ctx := context.Background()
	mustExec(t, ctx, `UPDATE platform_users SET status='disabled'`)
	a := newStaff(t, superadmin.RoleSuper, true)
	b := newStaff(t, superadmin.RoleSuper, true)
	c := newStaff(t, superadmin.RolePlatformSupport(), true)
	tokA := saLogin(t, a, 0).str("access_token")
	tokB := saLogin(t, b, 0).str("access_token")
	tokC := saLogin(t, c, 0).str("access_token")

	put := func(tok string, id uuid.UUID, body map[string]any) resp {
		return sa(t, "PUT", "/staff/"+id.String(), tok, body)
	}
	if r := put(tokA, a.ID, map[string]any{"role": "platform_support"}); r.Status != 409 || r.errCode() != "CANNOT_MODIFY_SELF" {
		t.Fatalf("self demote: %d %v", r.Status, r.Body)
	}
	if r := put(tokA, a.ID, map[string]any{"status": "disabled"}); r.Status != 409 {
		t.Fatalf("self disable: %d", r.Status)
	}
	if r := put(tokA, b.ID, map[string]any{}); r.Status != 400 {
		t.Fatalf("empty change: %d", r.Status)
	}
	if r := put(tokA, b.ID, map[string]any{"role": "owner"}); r.Status != 400 {
		t.Fatalf("bad role: %d", r.Status)
	}
	if r := put(tokC, b.ID, map[string]any{"status": "disabled"}); r.Status != 403 {
		t.Fatalf("support disables: %d", r.Status)
	}
	// A demotes B (A stays the only super admin), then B can no longer write
	if r := put(tokA, b.ID, map[string]any{"role": "platform_support"}); r.Status != 200 {
		t.Fatalf("demote B: %d %v", r.Status, r.Body)
	}
	if r := sa(t, "POST", "/plans", tokB, map[string]any{"key": uniq("zz"), "name": "x"}); r.Status != 401 && r.Status != 403 {
		t.Fatalf("demoted B still writes: %d", r.Status)
	}
	// the last super admin cannot be removed by anyone else (simulate with a second actor id via support→super promotion race)
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM platform_users WHERE role='super_admin' AND status='active'`).Scan(&n)
	if n != 1 {
		t.Fatalf("super admins: %d", n)
	}
	if _, err := aplat.UpdateStaff(ctx, b.ID, a.ID, superadmin.StaffUpdate{Status: ptr("disabled")}); err == nil || !strings.Contains(err.Error(), "LAST_SUPERADMIN") {
		t.Fatalf("last super admin removed: %v", err)
	}
	// disabling a user ends their session
	if r := put(tokA, c.ID, map[string]any{"status": "disabled"}); r.Status != 200 || r.data()["status"] != "disabled" {
		t.Fatalf("disable C: %d %v", r.Status, r.Body)
	}
	if r := sa(t, "GET", "/plans", tokC, nil); r.Status != 401 {
		t.Fatalf("disabled user's token: %d", r.Status)
	}
	if r := sa(t, "POST", "/auth/refresh", "", map[string]any{"refresh_token": "x"}); r.Status == 200 {
		t.Fatal("refresh with junk worked")
	}
	l := sa(t, "POST", "/auth/login", "", map[string]any{"email": c.Email, "password": c.Password})
	if l.Status != 401 {
		t.Fatalf("disabled user signs in: %d", l.Status)
	}
	if r := put(tokA, c.ID, map[string]any{"status": "active"}); r.Status != 200 {
		t.Fatalf("re-enable: %d", r.Status)
	}
	// every change is audited with before/after
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action='staff.update' AND meta ? 'before'`).Scan(&n)
	if n < 3 {
		t.Fatalf("staff.update audit rows: %d", n)
	}
}

func ptr[T any](v T) *T { return &v }

// ── broadcast, health, app versions ──────────────────────────────────────────

func TestBroadcastAudienceBatchesCancelAndLimit(t *testing.T) {
	ctx := context.Background()
	mustExec(t, ctx, `DELETE FROM platform_broadcasts`)
	tok, sup := superToken(t), supportToken(t)
	c := newCommunity(t) // admin + owners A101, B101, family
	other := newSoc(t)
	sid := c.s.ID.String()
	bc := func(body map[string]any) resp { return sa(t, "POST", "/broadcasts", tok, body) }

	if r := sa(t, "POST", "/broadcasts", sup, map[string]any{"title": "x", "body": "y", "audience": map[string]any{"all": true}}); r.Status != 403 {
		t.Fatalf("support broadcasts: %d", r.Status)
	}
	for _, bad := range []map[string]any{
		{"title": "", "body": "b", "audience": map[string]any{"society_ids": []string{sid}}},
		{"title": "t", "body": "b", "audience": map[string]any{}},
		{"title": "t", "body": "b", "audience": map[string]any{"all": true, "society_ids": []string{sid}}},
		{"title": "t", "body": "b", "link": "http://insecure", "audience": map[string]any{"society_ids": []string{sid}}},
	} {
		if r := bc(bad); r.Status != 400 {
			t.Fatalf("bad broadcast %v: %d", bad, r.Status)
		}
	}
	if r := bc(map[string]any{"title": "t", "body": "b", "audience": map[string]any{"society_ids": []string{uuid.NewString()}}}); r.Status != 422 || r.errCode() != "NO_RECIPIENTS" {
		t.Fatalf("nobody: %d %v", r.Status, r.Body)
	}
	// preview counts without sending: admins only by default, residents on request
	if r := bc(map[string]any{"title": "t", "body": "b", "preview": true, "audience": map[string]any{"society_ids": []string{sid}}}); num(r.data(), "recipients") != 1 {
		t.Fatalf("preview admins: %v", r.Body)
	}
	if r := bc(map[string]any{"title": "t", "body": "b", "preview": true, "include_residents": true, "audience": map[string]any{"society_ids": []string{sid}}}); num(r.data(), "recipients") < 4 {
		t.Fatalf("preview residents: %v", r.Body)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM platform_broadcasts`).Scan(&n)
	if n != 0 {
		t.Fatal("a preview created a broadcast")
	}

	// send to one society's office
	r := bc(map[string]any{"title": "Maintenance window", "body": "Sunday 2am", "link": "https://status.nexora.test", "audience": map[string]any{"society_ids": []string{sid}}})
	if r.Status != 201 || r.str("status") != "queued" || num(r.data(), "recipients") != 1 {
		t.Fatalf("create: %d %v", r.Status, r.Body)
	}
	if got := inbox(t, c.s.Admin, ""); len(got) != 0 {
		t.Fatal("delivered before the worker ran")
	}
	if sent, err := aplat.DeliverBroadcasts(ctx); err != nil || sent != 1 {
		t.Fatalf("deliver: %d %v", sent, err)
	}
	got := inbox(t, c.s.Admin, "")
	if len(got) != 1 || got[0]["type"] != "system.broadcast" || got[0]["title"] != "Maintenance window" {
		t.Fatalf("admin inbox: %v", got)
	}
	if got := inbox(t, other.Admin, ""); len(got) != 0 {
		t.Fatal("another society received it")
	}
	if sent, _ := aplat.DeliverBroadcasts(ctx); sent != 0 {
		t.Fatalf("delivered twice: %d", sent)
	}
	list := sa(t, "GET", "/broadcasts", sup, nil)
	first := list.Body["data"].([]any)[0].(map[string]any)
	if first["status"] != "sent" || num(first, "sent_count") != 1 {
		t.Fatalf("history: %v", first)
	}
	if r := sa(t, "POST", "/broadcasts/"+first["id"].(string)+"/cancel", tok, nil); r.Status != 409 || r.errCode() != "NOT_CANCELLABLE" {
		t.Fatalf("cancel after send: %d %v", r.Status, r.Body)
	}

	// batches: residents included, 2 per pass; cancelling before the first pass delivers nothing
	aud := map[string]any{"society_ids": []string{sid}}
	r2 := bc(map[string]any{"title": "Cancel me", "body": "b", "audience": aud})
	if r := sa(t, "POST", "/broadcasts/"+r2.str("id")+"/cancel", tok, nil); r.Status != 200 || r.data()["status"] != "cancelled" {
		t.Fatalf("cancel: %d %v", r.Status, r.Body)
	}
	if sent, _ := aplat.DeliverBroadcasts(ctx); sent != 0 {
		t.Fatalf("cancelled broadcast delivered %d", sent)
	}
	if r := sa(t, "POST", "/broadcasts/"+uuid.NewString()+"/cancel", tok, nil); r.Status != 404 {
		t.Fatalf("cancel unknown: %d", r.Status)
	}
	r3 := bc(map[string]any{"title": "Everyone", "body": "b", "include_residents": true, "audience": aud})
	total := int(num(r3.data(), "recipients"))
	aplat.SetBroadcastBatch(2)
	defer aplat.SetBroadcastBatch(0)
	s1, _ := aplat.DeliverBroadcasts(ctx)
	if s1 != 2 || total < 4 {
		t.Fatalf("first batch %d of %d", s1, total)
	}
	if r := sa(t, "POST", "/broadcasts/"+r3.str("id")+"/cancel", tok, nil); r.Status != 409 {
		t.Fatalf("cancel mid-delivery: %d", r.Status)
	}
	done := s1
	for i := 0; i < 5 && done < total; i++ {
		x, _ := aplat.DeliverBroadcasts(ctx)
		done += x
	}
	if done != total {
		t.Fatalf("delivered %d of %d", done, total)
	}
	if got := inbox(t, c.rA, ""); len(got) != 1 || got[0]["title"] != "Everyone" {
		t.Fatalf("resident inbox: %v", got)
	}

	// 5 per day
	for i := 0; i < 5; i++ {
		_ = bc(map[string]any{"title": fmt.Sprint("n", i), "body": "b", "audience": aud})
	}
	if r := bc(map[string]any{"title": "one too many", "body": "b", "audience": aud}); r.Status != 429 || r.errCode() != "BROADCAST_LIMIT" {
		t.Fatalf("daily limit: %d %v", r.Status, r.Body)
	}
	mustExec(t, ctx, `DELETE FROM platform_broadcasts`)
}

func TestBroadcastByPlan(t *testing.T) {
	ctx := context.Background()
	mustExec(t, ctx, `DELETE FROM platform_broadcasts`)
	tok := superToken(t)
	a, b := newSoc(t), newSoc(t)
	p1 := newPlan(t, tok, uniq("bp"), []string{"visitors"}, nil)
	assign(t, tok, a.ID, p1, false)
	r := sa(t, "POST", "/broadcasts", tok, map[string]any{"title": "Plan news", "body": "b", "audience": map[string]any{"plan_ids": []string{p1}}})
	if r.Status != 201 || num(r.data(), "recipients") != 1 {
		t.Fatalf("by plan: %d %v", r.Status, r.Body)
	}
	_, _ = aplat.DeliverBroadcasts(ctx)
	if len(inbox(t, a.Admin, "")) != 1 || len(inbox(t, b.Admin, "")) != 0 {
		t.Fatal("plan audience delivered to the wrong societies")
	}
	mustExec(t, ctx, `DELETE FROM platform_broadcasts`)
}

func TestHealthAndAppVersions(t *testing.T) {
	tok, sup := superToken(t), supportToken(t)
	h := sa(t, "GET", "/health", sup, nil)
	if h.Status != 200 || h.data()["version"] == "" || num(h.data(), "migration_version") < 50 {
		t.Fatalf("health: %d %v", h.Status, h.Body)
	}
	names := map[string]string{}
	for _, c := range h.data()["checks"].([]any) {
		m := c.(map[string]any)
		names[m["name"].(string)] = m["status"].(string)
	}
	for _, n := range []string{"PostgreSQL", "Redis", "Migrations", "Message outbox", "Payment webhooks"} {
		if names[n] == "" {
			t.Fatalf("missing check %s: %v", n, names)
		}
	}
	if names["PostgreSQL"] != "ok" || names["Redis"] != "ok" {
		t.Fatalf("checks: %v", names)
	}
	blob := fmt.Sprint(h.Body)
	for _, secret := range []string{"postgres://", "password", strings.Repeat("e", 40), strings.Repeat("p", 40)} {
		if strings.Contains(strings.ToLower(blob), strings.ToLower(secret)) {
			t.Fatalf("health leaks %q", secret)
		}
	}
	// a stuck message and a stuck webhook show up as warnings
	mustExec(t, context.Background(), `INSERT INTO notification_outbox (user_id, channel, target, payload, attempts) SELECT id,'push','tok','{}',5 FROM users LIMIT 1`)
	h = sa(t, "GET", "/health", sup, nil)
	for _, c := range h.data()["checks"].([]any) {
		if m := c.(map[string]any); m["name"] == "Message outbox" && m["status"] != "warn" {
			t.Fatalf("failed outbox not flagged: %v", m)
		}
	}
	mustExec(t, context.Background(), `DELETE FROM notification_outbox WHERE attempts >= 5`)

	// app versions: validation, role, and the public answer
	put := func(tk, app string, body map[string]any) resp { return sa(t, "PUT", "/app-versions/"+app, tk, body) }
	if r := put(sup, "living", map[string]any{"min_version": "1.0.0", "latest_version": "1.0.0"}); r.Status != 403 {
		t.Fatalf("support sets versions: %d", r.Status)
	}
	if r := put(tok, "toaster", map[string]any{"min_version": "1.0.0", "latest_version": "1.0.0"}); r.Status != 400 {
		t.Fatalf("unknown app: %d", r.Status)
	}
	if r := put(tok, "living", map[string]any{"min_version": "2.0.0", "latest_version": "1.0.0"}); r.Status != 400 {
		t.Fatalf("min above latest: %d", r.Status)
	}
	if r := put(tok, "living", map[string]any{"min_version": "abc", "latest_version": "1.0.0"}); r.Status != 400 {
		t.Fatalf("bad version: %d", r.Status)
	}
	if r := put(tok, "living", map[string]any{"min_version": "1.2.0", "latest_version": "1.4.0", "message": "Please update", "store_url": "http://x"}); r.Status != 400 {
		t.Fatalf("insecure store url: %d", r.Status)
	}
	if r := put(tok, "living", map[string]any{"min_version": "1.2.0", "latest_version": "1.4.0", "message": "Please update", "store_url": "https://stores.example/nexora"}); r.Status != 200 {
		t.Fatalf("set: %d %v", r.Status, r.Body)
	}
	cfg := func(app, ver string) map[string]any {
		r := call(t, "GET", "/api/v1/app-config?app="+app+"&version="+ver, "", nil) // no token
		if r.Status != 200 {
			t.Fatalf("app-config: %d %v", r.Status, r.Body)
		}
		return r.data()
	}
	if c := cfg("living", "1.1.9%2B3"); c["update"] != "required" || c["message"] != "Please update" || c["store_url"] == "" {
		t.Fatalf("old build: %v", c)
	}
	if c := cfg("living", "1.3.0"); c["update"] != "available" {
		t.Fatalf("behind latest: %v", c)
	}
	if c := cfg("living", "1.10.0"); c["update"] != "none" { // 1.10 is above 1.4, not below
		t.Fatalf("numeric compare: %v", c)
	}
	if c := cfg("living", "1.4.0-rc1"); c["update"] != "none" {
		t.Fatalf("suffix ignored: %v", c)
	}
	// unknown app or garbage never blocks
	for _, q := range []string{"app=nope&version=1.0.0", "app=living&version=banana", "app=&version="} {
		r := call(t, "GET", "/api/v1/app-config?"+q, "", nil)
		if r.Status != 200 || r.data()["update"] != "none" {
			t.Fatalf("fail-open %s: %d %v", q, r.Status, r.Body)
		}
	}
	// reset so other tests are unaffected
	put(tok, "living", map[string]any{"min_version": "0.0.0", "latest_version": "0.0.0"})
}
