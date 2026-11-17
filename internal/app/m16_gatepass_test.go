package app_test

import (
	"context"
	"strings"
	"testing"
)

func countRows(t *testing.T, ctx context.Context, q string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("sql: %v\n%s", err, q)
	}
	return n
}

// The resident shows a short-lived QR; the guard scans it and the entry/exit is logged.
func TestResidentGatePass(t *testing.T) {
	s := newSoc(t)
	_, units := setupBlock(t, s, "A", 1, 3)
	res, _ := newResident(t, s, units["A101"], "owner")
	g := newGuard(t, s)
	ctx := context.Background()

	// the resident gets a pass with their details
	p := call(t, "GET", "/api/v1/me/gate-pass", res, nil)
	if p.Status != 200 || !strings.HasPrefix(p.str("qr"), "NXR1:") || p.str("unit_number") != "A101" || p.str("role") != "owner" || p.str("expires_at") == "" {
		t.Fatalf("pass: %d %v", p.Status, p.Body)
	}
	qr := p.str("qr")

	// the office admin has no flat: no pass
	if a := call(t, "GET", "/api/v1/me/gate-pass", s.Admin, nil); a.Status != 409 || a.errCode() != "NO_FLAT" {
		t.Fatalf("admin pass: %d %v", a.Status, a.Body)
	}
	// a resident cannot use the guard endpoints
	if r := call(t, "POST", "/api/v1/gate/residents/verify", res, map[string]any{"qr": qr}); r.Status != 403 {
		t.Fatalf("resident verify: %d %v", r.Status, r.Body)
	}

	// verify writes nothing and says an entry is next
	v := call(t, "POST", "/api/v1/gate/residents/verify", g.Token, map[string]any{"qr": qr})
	d := v.data()
	if v.Status != 200 || d["valid"] != true || d["next"] != "in" || d["inside"] != false || d["recorded"] == true {
		t.Fatalf("verify: %d %v", v.Status, v.Body)
	}
	if n := countRows(t, ctx, `SELECT count(*) FROM resident_gate_logs WHERE society_id=$1`, s.ID); n != 0 {
		t.Fatalf("verify wrote %d rows", n)
	}

	// entry
	e := call(t, "POST", "/api/v1/gate/residents/entry", g.Token, map[string]any{"qr": qr})
	if e.Status != 200 || e.data()["recorded"] != true || e.data()["inside"] != true || e.data()["next"] != "out" {
		t.Fatalf("entry: %d %v", e.Status, e.Body)
	}
	// a second scan straight away is the guard scanning twice: nothing new is logged
	dup := call(t, "POST", "/api/v1/gate/residents/entry", g.Token, map[string]any{"qr": qr})
	if dup.Status != 200 || dup.data()["duplicate"] != true {
		t.Fatalf("duplicate: %d %v", dup.Status, dup.Body)
	}
	if n := countRows(t, ctx, `SELECT count(*) FROM resident_gate_logs WHERE society_id=$1`, s.ID); n != 1 {
		t.Fatalf("rows after duplicate: %d", n)
	}
	// later the same pass logs the exit
	mustExec(t, ctx, `UPDATE resident_gate_logs SET created_at = now() - interval '2 hours' WHERE society_id=$1`, s.ID)
	x := call(t, "POST", "/api/v1/gate/residents/entry", g.Token, map[string]any{"qr": qr})
	if x.Status != 200 || x.data()["inside"] != false || x.data()["next"] != "in" {
		t.Fatalf("exit: %d %v", x.Status, x.Body)
	}

	// a tampered code is invalid, a made-up one too
	bad := qr[:len(qr)-3] + "AAA"
	if r := call(t, "POST", "/api/v1/gate/residents/verify", g.Token, map[string]any{"qr": bad}); r.Status != 200 || r.data()["valid"] != false || r.data()["reason"] != "INVALID" {
		t.Fatalf("tampered: %d %v", r.Status, r.Body)
	}
	if r := call(t, "POST", "/api/v1/gate/residents/entry", g.Token, map[string]any{"qr": "NXV1:not-ours"}); r.Status != 409 || r.errCode() != "PASS_NOT_VALID" {
		t.Fatalf("foreign qr entry: %d %v", r.Status, r.Body)
	}
	if r := call(t, "POST", "/api/v1/gate/residents/verify", g.Token, map[string]any{}); r.Status != 400 {
		t.Fatalf("empty: %d %v", r.Status, r.Body)
	}

	// another society's guard cannot use it
	s2 := newSoc(t)
	g2 := newGuard(t, s2)
	if r := call(t, "POST", "/api/v1/gate/residents/verify", g2.Token, map[string]any{"qr": qr}); r.Status != 200 || r.data()["reason"] != "NOT_FOUND" {
		t.Fatalf("other society: %d %v", r.Status, r.Body)
	}

	// a suspended resident is refused and the guard is told why
	mustExec(t, ctx, `UPDATE memberships SET status='suspended' WHERE society_id=$1 AND unit_id=$2`, s.ID, units["A101"])
	if r := call(t, "POST", "/api/v1/gate/residents/verify", g.Token, map[string]any{"qr": qr}); r.Status != 200 || r.data()["valid"] != false || r.data()["reason"] != "SUSPENDED" {
		t.Fatalf("suspended: %d %v", r.Status, r.Body)
	}
}
