package app_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

const parkingSlots = "/api/v1/society/parking/slots"

func enableParking(t *testing.T, id uuid.UUID) {
	t.Helper()
	mustExec(t, context.Background(), `INSERT INTO society_features (society_id, feature_key, enabled) VALUES ($1,'parking',true) ON CONFLICT (society_id, feature_key) DO UPDATE SET enabled=true`, id)
}

func mySlots(t *testing.T, tok string) []any {
	t.Helper()
	r := call(t, "GET", "/api/v1/me/parking", tok, nil)
	if r.Status != 200 {
		t.Fatalf("my parking: %d %v", r.Status, r.Body)
	}
	l, _ := r.Body["data"].([]any)
	return l
}

func TestParkingIsOffUntilTurnedOnAndEveryRouteIsBehindIt(t *testing.T) {
	c := newCommunity(t)
	for _, tc := range []struct{ method, path, tok string }{
		{"GET", parkingSlots, c.s.Admin},
		{"POST", parkingSlots, c.s.Admin},
		{"GET", "/api/v1/society/parking/summary", c.s.Admin},
		{"GET", "/api/v1/me/parking", c.rA},
	} {
		if r := call(t, tc.method, tc.path, tc.tok, map[string]any{"area": "B1", "number": "1"}); r.Status != 403 || r.errCode() != "FEATURE_DISABLED" {
			t.Fatalf("%s %s with the service off: %d %v", tc.method, tc.path, r.Status, r.Body)
		}
	}
	// the feature shows in /me as off, and the other services are untouched
	m := call(t, "GET", "/api/v1/me/features", c.rA, nil)
	if m.Status != 200 {
		t.Fatalf("features: %d", m.Status)
	}
	f := m.data()
	if f["parking"] != false || f["visitors"] != true {
		t.Fatalf("features map: %v", m.Body)
	}
}

func TestOnlyTheSocietyAdminCreatesAndAllocatesParkingSlots(t *testing.T) {
	c := newCommunity(t)
	enableParking(t, c.s.ID)

	// residents and family can read their own slots but cannot manage anything
	for _, tok := range []string{c.rA, c.rB, c.famA} {
		if r := call(t, "POST", parkingSlots, tok, map[string]any{"area": "Basement 1", "number": "1"}); r.Status != 403 {
			t.Fatalf("resident creating a slot: %d %v", r.Status, r.Body)
		}
		if r := call(t, "GET", parkingSlots, tok, nil); r.Status != 403 {
			t.Fatalf("resident listing the office view: %d", r.Status)
		}
	}
	guard := c.guard
	if r := call(t, "POST", parkingSlots, guard.Token, map[string]any{"area": "Basement 1", "number": "1"}); r.Status != 403 {
		t.Fatalf("guard creating a slot: %d %v", r.Status, r.Body)
	}

	// validation and duplicates
	if r := call(t, "POST", parkingSlots, c.s.Admin, map[string]any{"area": "", "number": "1"}); r.Status != 400 {
		t.Fatalf("empty area: %d", r.Status)
	}
	if r := call(t, "POST", parkingSlots, c.s.Admin, map[string]any{"area": "Basement 1", "number": "1", "kind": "truck"}); r.Status != 400 {
		t.Fatalf("bad kind: %d", r.Status)
	}
	one := call(t, "POST", parkingSlots, c.s.Admin, map[string]any{"area": "Basement 1", "number": "P-01", "note": "near the lift"})
	if one.Status != 201 || one.data()["allocated"] != false || one.str("kind") != "car" {
		t.Fatalf("create: %d %v", one.Status, one.Body)
	}
	if r := call(t, "POST", parkingSlots, c.s.Admin, map[string]any{"area": "basement 1", "number": "p-01"}); r.Status != 409 || r.errCode() != "SLOT_EXISTS" {
		t.Fatalf("duplicate (case-insensitive): %d %v", r.Status, r.Body)
	}

	// a run of slots; existing numbers are skipped, not duplicated
	b := call(t, "POST", parkingSlots+"/bulk", c.s.Admin, map[string]any{"area": "Basement 1", "prefix": "P-", "from": 1, "to": 10})
	if b.Status != 201 || b.data()["created"] != float64(9) || b.data()["skipped"] != float64(1) { // P-01 .. P-10 padded to two digits
		t.Fatalf("bulk: %d %v", b.Status, b.Body)
	}
	if r := call(t, "POST", parkingSlots+"/bulk", c.s.Admin, map[string]any{"area": "X", "from": 1, "to": 500}); r.Status != 400 {
		t.Fatalf("bulk too large: %d", r.Status)
	}
	bike := call(t, "POST", parkingSlots, c.s.Admin, map[string]any{"area": "Gate", "number": "V1", "kind": "visitor"})
	if bike.Status != 201 {
		t.Fatalf("visitor slot: %d %v", bike.Status, bike.Body)
	}

	// allocate: the owner and the family member of A101 see it, B101 does not
	id := one.str("id")
	al := call(t, "POST", parkingSlots+"/"+id+"/allocate", c.s.Admin, map[string]any{"unit_id": c.units["A101"]})
	if al.Status != 200 || al.data()["allocated"] != true || al.str("flat") != "A-A101" {
		t.Fatalf("allocate: %d %v", al.Status, al.Body)
	}
	for _, tok := range []string{c.rA, c.famA} {
		l := mySlots(t, tok)
		if len(l) != 1 || l[0].(map[string]any)["number"] != "P-01" || l[0].(map[string]any)["area"] != "Basement 1" {
			t.Fatalf("A101 sees: %v", l)
		}
	}
	if l := mySlots(t, c.rB); len(l) != 0 {
		t.Fatalf("B101 must see nothing: %v", l)
	}
	// the flat is told in its inbox
	n, _ := callList(t, "/api/v1/me/notifications", c.rA)
	found := false
	for _, x := range n {
		if x["type"] == "parking.allocated" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no parking.allocated notification: %v", n)
	}

	// rules: visitor slots are shared, the flat must exist in this society
	if r := call(t, "POST", parkingSlots+"/"+bike.str("id")+"/allocate", c.s.Admin, map[string]any{"unit_id": c.units["A101"]}); r.Status != 409 || r.errCode() != "SLOT_VISITOR" {
		t.Fatalf("allocating a visitor slot: %d %v", r.Status, r.Body)
	}
	if r := call(t, "POST", parkingSlots+"/"+id+"/allocate", c.s.Admin, map[string]any{"unit_id": uuid.NewString()}); r.Status != 404 || r.errCode() != "UNIT_NOT_FOUND" {
		t.Fatalf("unknown flat: %d %v", r.Status, r.Body)
	}
	other := newSoc(t)
	_, ou := setupBlock(t, other, "Z", 1, 1)
	if r := call(t, "POST", parkingSlots+"/"+id+"/allocate", c.s.Admin, map[string]any{"unit_id": ou["Z101"]}); r.Status != 404 {
		t.Fatalf("flat of another society: %d %v", r.Status, r.Body)
	}
	// and another society's admin cannot touch this slot at all
	enableParking(t, other.ID)
	if r := call(t, "POST", parkingSlots+"/"+id+"/release", other.Admin, nil); r.Status != 404 {
		t.Fatalf("other society releasing: %d %v", r.Status, r.Body)
	}
	if r := call(t, "DELETE", parkingSlots+"/"+id, other.Admin, nil); r.Status != 404 {
		t.Fatalf("other society deleting: %d", r.Status)
	}
	if _, r := callList(t, parkingSlots, other.Admin); r.total() != 0 {
		t.Fatalf("other society sees %d slots", r.total())
	}

	// moving it to B101 takes it off A101
	if r := call(t, "POST", parkingSlots+"/"+id+"/allocate", c.s.Admin, map[string]any{"unit_id": c.units["B101"]}); r.Status != 200 {
		t.Fatalf("move: %d %v", r.Status, r.Body)
	}
	if len(mySlots(t, c.rA)) != 0 || len(mySlots(t, c.rB)) != 1 {
		t.Fatal("slot did not move from A101 to B101")
	}

	// an allocated slot cannot be deleted; release first
	if r := call(t, "DELETE", parkingSlots+"/"+id, c.s.Admin, nil); r.Status != 409 || r.errCode() != "SLOT_ALLOCATED" {
		t.Fatalf("delete allocated: %d %v", r.Status, r.Body)
	}
	if r := call(t, "POST", parkingSlots+"/"+id+"/release", c.s.Admin, nil); r.Status != 200 || r.data()["allocated"] != false {
		t.Fatalf("release: %d %v", r.Status, r.Body)
	}
	if r := call(t, "POST", parkingSlots+"/"+id+"/release", c.s.Admin, nil); r.Status != 409 || r.errCode() != "SLOT_FREE" {
		t.Fatalf("release twice: %d %v", r.Status, r.Body)
	}
	if len(mySlots(t, c.rB)) != 0 {
		t.Fatal("B101 still sees a released slot")
	}

	// filters and summary: P-01..P-10 (one of them now a bike) + 1 visitor, none allocated
	up := call(t, "PUT", parkingSlots+"/"+id, c.s.Admin, map[string]any{"area": "Basement 2", "number": "P-01", "kind": "bike", "note": ""})
	if up.Status != 200 || up.str("area") != "Basement 2" || up.str("kind") != "bike" {
		t.Fatalf("update: %d %v", up.Status, up.Body)
	}
	sum := call(t, "GET", "/api/v1/society/parking/summary", c.s.Admin, nil)
	if sum.Status != 200 || sum.data()["total"] != float64(11) || sum.data()["free"] != float64(11) || sum.data()["bikes"] != float64(1) || sum.data()["cars"] != float64(9) || sum.data()["visitor"] != float64(1) {
		t.Fatalf("summary: %v", sum.Body)
	}
	if _, r := callList(t, parkingSlots+"?kind=bike", c.s.Admin); r.total() != 1 {
		t.Fatalf("kind filter: %d", r.total())
	}
	if _, r := callList(t, parkingSlots+"?q=basement%202", c.s.Admin); r.total() != 1 {
		t.Fatalf("search: %d", r.total())
	}
	if r := call(t, "GET", parkingSlots+"?status=nope", c.s.Admin, nil); r.Status != 400 {
		t.Fatalf("bad status: %d", r.Status)
	}
	if r := call(t, "DELETE", parkingSlots+"/"+id, c.s.Admin, nil); r.Status != 204 {
		t.Fatalf("delete free: %d", r.Status)
	}
}
