package app_test

import (
	"context"
	"testing"
)

func dash(t *testing.T, tok string) resp {
	t.Helper()
	return call(t, "GET", "/api/v1/society/dashboard", tok, nil)
}

func list(r resp, section, key string) []any {
	s, _ := r.data()[section].(map[string]any)
	l, _ := s[key].([]any)
	return l
}

// A new society has nothing to show: zeros and empty lists, never invented numbers.
func TestDashboardOfAnEmptySocietyIsEmptyNotInvented(t *testing.T) {
	s := newSoc(t)
	r := dash(t, s.Admin)
	if r.Status != 200 {
		t.Fatalf("dashboard: %d %v", r.Status, r.Body)
	}
	d := r.data()
	res := d["residents"].(map[string]any)
	if num(res, "total") != 0 || num(res, "flats") != 0 {
		t.Fatalf("residents: %v", res)
	}
	v := d["visitors"].(map[string]any)
	if num(v, "today") != 0 || num(v, "inside_now") != 0 || v["avg_gate_minutes"] != nil {
		t.Fatalf("visitors: %v", v)
	}
	if series := v["series"].([]any); len(series) != 7 {
		t.Fatalf("the chart needs 7 points even when empty, got %d", len(series))
	} else {
		for _, p := range series {
			m := p.(map[string]any)
			if num(m, "pre_approved") != 0 || num(m, "walk_in") != 0 || m["label"] == "" {
				t.Fatalf("point: %v", m)
			}
		}
	}
	if len(list(r, "visitors", "recent")) != 0 || len(list(r, "complaints", "recent")) != 0 {
		t.Fatal("recent lists must be empty")
	}
	if c := d["complaints"].(map[string]any); num(c, "pending") != 0 {
		t.Fatalf("complaints: %v", c)
	}
	if p := d["payments"].(map[string]any); p["has_invoices"] != false || num(p, "billed_paise") != 0 {
		t.Fatalf("payments: %v", p)
	}
}

func TestDashboardShowsRealNumbersOnlyForTheSignedInSociety(t *testing.T) {
	a := newCommunity(t)
	b := newCommunity(t)
	populate(t, b) // lots of activity in another society

	// society A: one walk-in waiting for approval, one complaint, one issued invoice, one payment today
	if w := walkIn(t, a.guard, a.units["A101"], "Rahul Walker"); w.Status != 201 {
		t.Fatalf("walk-in: %d %v", w.Status, w.Body)
	}
	if c := raiseComplaint(t, a.rA, map[string]any{"title": "Leaking kitchen tap"}); c.Status != 201 {
		t.Fatalf("complaint: %d %v", c.Status, c.Body)
	}
	fixedHead(t, a, "Maintenance", 100000)
	issuePeriod(t, a, periodOf(0))
	recordPay(t, a, a.units["A101"], 40000, "cash", "DASH-1")

	r := dash(t, a.s.Admin)
	if r.Status != 200 {
		t.Fatalf("dashboard: %d %v", r.Status, r.Body)
	}
	d := r.data()
	v := d["visitors"].(map[string]any)
	if num(v, "awaiting_approval") != 1 {
		t.Fatalf("awaiting: %v", v)
	}
	recent := list(r, "visitors", "recent")
	if len(recent) != 1 || recent[0].(map[string]any)["name"] != "Rahul Walker" || recent[0].(map[string]any)["flat"] == "" {
		t.Fatalf("recent visitors: %v", recent)
	}
	c := d["complaints"].(map[string]any)
	if num(c, "open") != 1 || num(c, "pending") != 1 {
		t.Fatalf("complaints: %v", c)
	}
	rc := list(r, "complaints", "recent")
	if len(rc) != 1 || rc[0].(map[string]any)["title"] != "Leaking kitchen tap" || rc[0].(map[string]any)["reported_by"] == "" || rc[0].(map[string]any)["assignee"] != nil {
		t.Fatalf("recent complaints: %v", rc)
	}
	pm := d["payments"].(map[string]any)
	if pm["has_invoices"] != true || num(pm, "billed_paise") != 200000 || num(pm, "collected_today_paise") != 40000 || num(pm, "outstanding_paise") != 160000 {
		t.Fatalf("payments: %v", pm)
	}
	if res := d["residents"].(map[string]any); num(res, "flats") != 4 || num(res, "total") < 2 {
		t.Fatalf("residents: %v", res)
	}
}

func TestDashboardFollowsServiceSwitchesAndPermissions(t *testing.T) {
	c := newCommunity(t)
	ctx := context.Background()
	mustExec(t, ctx, `INSERT INTO society_features (society_id, feature_key, enabled) VALUES ($1,'visitors',false),($1,'complaints',false) ON CONFLICT (society_id, feature_key) DO UPDATE SET enabled=false`, c.s.ID)
	r := dash(t, c.s.Admin)
	d := r.data()
	if d["visitors"] != nil || d["complaints"] != nil {
		t.Fatalf("a switched-off service must not appear: %v", d)
	}
	if d["payments"] == nil || d["residents"] == nil {
		t.Fatalf("the other sections stay: %v", d)
	}
	if r := dash(t, c.rA); r.Status != 403 {
		t.Fatalf("resident reads the office dashboard: %d", r.Status)
	}
	if r := dash(t, c.guard.Token); r.Status != 403 {
		t.Fatalf("guard reads the office dashboard: %d", r.Status)
	}
	if r := call(t, "GET", "/api/v1/society/dashboard", "", nil); r.Status != 401 {
		t.Fatalf("anonymous: %d", r.Status)
	}
}
