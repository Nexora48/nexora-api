package app_test

import (
	"context"
	"testing"
)

// A resident can remove a finished visit from their history; the office log keeps it.
func TestResidentDeletesFinishedVisit(t *testing.T) {
	s := newSoc(t)
	_, units := setupBlock(t, s, "A", 1, 3)
	r1, _ := newResident(t, s, units["A101"], "owner")
	r2, _ := newResident(t, s, units["A102"], "owner")
	ctx := context.Background()

	a := invite(t, r1, map[string]any{"visitor_name": "Old Friend"})
	id := a.str("id")

	// an open invitation cannot be deleted
	if d := call(t, "DELETE", "/api/v1/visitors/"+id, r1, nil); d.Status != 409 || d.errCode() != "INVALID_STATE" {
		t.Fatalf("delete open: %d %v", d.Status, d.Body)
	}
	// another flat cannot touch it
	if d := call(t, "DELETE", "/api/v1/visitors/"+id, r2, nil); d.Status != 404 {
		t.Fatalf("other flat: %d %v", d.Status, d.Body)
	}

	// cancelled: now it is history and can go
	if c := call(t, "POST", "/api/v1/visitors/"+id+"/cancel", r1, nil); c.Status != 200 {
		t.Fatalf("cancel: %d %v", c.Status, c.Body)
	}
	hist, _ := callList(t, "/api/v1/visitors?tab=history", r1)
	if len(hist) != 1 {
		t.Fatalf("history before: %d", len(hist))
	}
	if d := call(t, "DELETE", "/api/v1/visitors/"+id, r1, nil); d.Status != 204 {
		t.Fatalf("delete: %d %v", d.Status, d.Body)
	}
	if hist, _ := callList(t, "/api/v1/visitors?tab=history", r1); len(hist) != 0 {
		t.Fatalf("history after delete: %d", len(hist))
	}
	if g := call(t, "GET", "/api/v1/visitors/"+id, r1, nil); g.Status != 404 {
		t.Fatalf("get after delete: %d %v", g.Status, g.Body)
	}
	// deleting again is a plain 404
	if d := call(t, "DELETE", "/api/v1/visitors/"+id, r1, nil); d.Status != 404 {
		t.Fatalf("delete twice: %d %v", d.Status, d.Body)
	}
	// the office still has the visit
	if rows, rr := callList(t, "/api/v1/society/visitors", s.Admin); rr.Status != 200 || len(rows) != 1 {
		t.Fatalf("admin log keeps it: %d %v", len(rows), rr.Body)
	}

	// an invitation whose window has passed counts as expired history and can be deleted too
	b := invite(t, r1, map[string]any{"visitor_name": "Late Guest"})
	mustExec(t, ctx, `UPDATE visits SET expected_from=now()-interval '3 hours', expected_until=now()-interval '1 hour' WHERE id=$1`, b.str("id"))
	if d := call(t, "DELETE", "/api/v1/visitors/"+b.str("id"), r1, nil); d.Status != 204 {
		t.Fatalf("delete expired: %d %v", d.Status, d.Body)
	}
}
