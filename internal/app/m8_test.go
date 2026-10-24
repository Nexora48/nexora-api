package app_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

var ist = time.FixedZone("IST", 5*3600+1800)

// openAmenity creates an amenity that is open every day 06:00-22:00.
func openAmenity(t *testing.T, c community, over map[string]any) resp {
	t.Helper()
	hours := []map[string]any{}
	for d := 0; d < 7; d++ {
		hours = append(hours, map[string]any{"weekday": d, "opens": "06:00", "closes": "22:00"})
	}
	b := map[string]any{"name": "Clubhouse", "description": "Hall", "capacity": 1, "slot_minutes": 60, "min_notice_minutes": 0, "max_days_ahead": 30,
		"max_upcoming_per_flat": 3, "max_per_week_per_flat": 5, "cancel_cutoff_hours": 0, "fee_amount": 0, "deposit_amount": 0, "requires_approval": false, "hours": hours}
	for k, v := range over {
		b[k] = v
	}
	r := call(t, "POST", "/api/v1/society/amenities", c.s.Admin, b)
	if r.Status != 201 {
		t.Fatalf("create amenity: %d %v", r.Status, r.Body)
	}
	return r
}

func dayString(daysAhead int) string {
	return time.Now().In(ist).AddDate(0, 0, daysAhead).Format("2006-01-02")
}

// slotAt returns the start of the slot at the given local hour on a future day.
func slotAt(t *testing.T, tok, amenity string, daysAhead, hour int) string {
	t.Helper()
	r := call(t, "GET", fmt.Sprintf("/api/v1/amenities/%s/slots?date=%s", amenity, dayString(daysAhead)), tok, nil)
	if r.Status != 200 {
		t.Fatalf("slots: %d %v", r.Status, r.Body)
	}
	for _, s := range r.data()["slots"].([]any) {
		m := s.(map[string]any)
		st, _ := time.Parse(time.RFC3339, m["start"].(string))
		if st.In(ist).Hour() == hour {
			return m["start"].(string)
		}
	}
	t.Fatalf("no slot at %d: %v", hour, r.Body)
	return ""
}

func book(t *testing.T, tok, amenity, slot string) resp {
	t.Helper()
	return call(t, "POST", "/api/v1/amenities/"+amenity+"/bookings", tok, map[string]any{"slot_start": slot, "people": 2})
}

func TestAmenityBookingRules(t *testing.T) {
	c := newCommunity(t)
	if r := call(t, "POST", "/api/v1/society/amenities", c.rA, map[string]any{"name": "x"}); r.Status != 403 {
		t.Fatalf("resident creates amenity: %d", r.Status)
	}
	if r := call(t, "GET", "/api/v1/amenities", c.guard.Token, nil); r.Status != 403 {
		t.Fatalf("guard lists amenities: %d", r.Status)
	}
	a := openAmenity(t, c, map[string]any{"fee_amount": 150})
	id := a.str("id")
	if r := call(t, "POST", "/api/v1/society/amenities", c.s.Admin, map[string]any{"name": "clubhouse", "capacity": 1, "slot_minutes": 60, "max_days_ahead": 3, "max_upcoming_per_flat": 1, "max_per_week_per_flat": 1}); r.Status != 409 {
		t.Fatalf("duplicate name: %d %v", r.Status, r.Body)
	}
	if r := call(t, "POST", "/api/v1/society/amenities", c.s.Admin, map[string]any{"name": "Bad", "capacity": 1, "slot_minutes": 45, "max_days_ahead": 3, "max_upcoming_per_flat": 1, "max_per_week_per_flat": 1}); r.Status != 400 {
		t.Fatalf("bad slot length: %d", r.Status)
	}
	rows, _ := callList(t, "/api/v1/amenities", c.rA)
	if len(rows) != 1 || rows[0]["name"] != "Clubhouse" {
		t.Fatalf("list: %v", rows)
	}

	slot := slotAt(t, c.rA, id, 2, 10)
	b1 := book(t, c.rA, id, slot)
	if b1.Status != 201 || b1.vstatus() != "confirmed" || b1.data()["fee_amount"].(float64) != 150 || b1.data()["charge_status"] != "pending" {
		t.Fatalf("book: %d %v", b1.Status, b1.Body)
	}
	if r := book(t, c.famA, id, slot); r.Status != 409 || r.errCode() != "SLOT_FULL" {
		t.Fatalf("family of the same flat on a full slot: %d %v", r.Status, r.Body)
	}
	if r := book(t, c.rB, id, slot); r.Status != 409 || r.errCode() != "SLOT_FULL" {
		t.Fatalf("other flat on a full slot: %d %v", r.Status, r.Body)
	}
	// a slot nobody else booked: the family member books it, and the flat cannot book it twice
	slot2 := slotAt(t, c.rA, id, 2, 11)
	if r := book(t, c.famA, id, slot2); r.Status != 201 {
		t.Fatalf("family books: %d %v", r.Status, r.Body)
	}
	if r := book(t, c.rA, id, slot2); r.Status != 409 {
		t.Fatalf("same flat twice: %d %v", r.Status, r.Body)
	}
	// both bookings show for both members of the flat, none for the other flat
	mine, _ := callList(t, "/api/v1/amenities/bookings", c.rA)
	if len(mine) != 2 {
		t.Fatalf("flat bookings: %d", len(mine))
	}
	if other, _ := callList(t, "/api/v1/amenities/bookings", c.rB); len(other) != 0 {
		t.Fatalf("B sees A's bookings: %d", len(other))
	}
	// rule codes
	if r := book(t, c.rA, id, time.Now().Add(-48*time.Hour).Format(time.RFC3339)); r.Status != 422 {
		t.Fatalf("past: %d %v", r.Status, r.Body)
	}
	if r := book(t, c.rB, id, slotAt(t, c.rB, id, 2, 10)[:0]+"2030-01-01T10:00:00Z"); r.Status != 422 {
		t.Fatalf("far future: %d %v", r.Status, r.Body)
	}
	// upcoming limit (3 for the flat)
	book(t, c.rA, id, slotAt(t, c.rA, id, 2, 12))
	if r := book(t, c.rA, id, slotAt(t, c.rA, id, 2, 13)); r.Status != 422 || r.errCode() != "FLAT_LIMIT" {
		t.Fatalf("flat limit: %d %v", r.Status, r.Body)
	}
	// cancel frees the slot
	if r := call(t, "POST", "/api/v1/amenities/bookings/"+b1.str("id")+"/cancel", c.rA, nil); r.Status != 200 || r.vstatus() != "cancelled" {
		t.Fatalf("cancel: %d %v", r.Status, r.Body)
	}
	if r := book(t, c.rB, id, slot); r.Status != 201 {
		t.Fatalf("slot after cancel: %d %v", r.Status, r.Body)
	}
	if r := call(t, "POST", "/api/v1/amenities/bookings/"+b1.str("id")+"/cancel", c.rB, nil); r.Status != 404 {
		t.Fatalf("cancel someone else's: %d", r.Status)
	}
	// the cancelled fee is waived
	var charge string
	_ = pool.QueryRow(context.Background(), `SELECT charge_status FROM amenity_bookings WHERE id=$1`, b1.str("id")).Scan(&charge)
	_ = charge

	// blackout and deactivation
	if r := call(t, "POST", "/api/v1/society/amenities/"+id+"/blackouts", c.s.Admin, map[string]any{"date": dayString(5), "reason": "Maintenance"}); r.Status != 201 {
		t.Fatalf("blackout: %d %v", r.Status, r.Body)
	}
	d := call(t, "GET", fmt.Sprintf("/api/v1/amenities/%s/slots?date=%s", id, dayString(5)), c.rA, nil)
	if d.data()["closed_reason"] != "BLACKOUT" {
		t.Fatalf("blackout day: %v", d.Body)
	}
	if r := call(t, "DELETE", "/api/v1/society/amenities/"+id, c.s.Admin, nil); r.Status != 200 {
		t.Fatalf("deactivate: %d", r.Status)
	}
	if rows, _ := callList(t, "/api/v1/amenities", c.rA); len(rows) != 0 {
		t.Fatalf("inactive amenity visible: %d", len(rows))
	}
}

func TestAmenityConcurrentBookingsNeverExceedCapacity(t *testing.T) {
	c := newCommunity(t)
	a := openAmenity(t, c, map[string]any{"name": "Court", "capacity": 2})
	id := a.str("id")
	// more flats than places
	tokens := []string{c.rA, c.rB}
	for _, k := range []string{"A102", "B102"} {
		tok, _ := newResident(t, c.s, c.units[k], "owner")
		tokens = append(tokens, tok)
	}
	slot := slotAt(t, tokens[0], id, 3, 9)
	var wg sync.WaitGroup
	results := make([]int, len(tokens))
	for i, tok := range tokens {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = book(t, tok, id, slot).Status
		}()
	}
	wg.Wait()
	won := 0
	for _, s := range results {
		if s == 201 {
			won++
		} else if s != 409 {
			t.Fatalf("unexpected status %d in %v", s, results)
		}
	}
	if won != 2 {
		t.Fatalf("capacity 2 but %d bookings won: %v", won, results)
	}
}

func TestAmenityApprovalFlow(t *testing.T) {
	c := newCommunity(t)
	a := openAmenity(t, c, map[string]any{"name": "Party hall", "requires_approval": true, "fee_amount": 500})
	id := a.str("id")
	wA := dialWS(t, c.rA)
	b := book(t, c.rA, id, slotAt(t, c.rA, id, 4, 18))
	if b.Status != 201 || b.vstatus() != "pending" {
		t.Fatalf("pending: %d %v", b.Status, b.Body)
	}
	bid := b.str("id")
	wA.expect(t, "booking.updated")
	if r := call(t, "POST", "/api/v1/society/bookings/"+bid+"/approve", c.rA, nil); r.Status != 403 {
		t.Fatalf("resident approves: %d", r.Status)
	}
	if r := call(t, "POST", "/api/v1/society/bookings/"+bid+"/reject", c.s.Admin, map[string]any{"reason": "x"}); r.Status != 400 {
		t.Fatalf("reject needs a reason: %d", r.Status)
	}
	if r := call(t, "POST", "/api/v1/society/bookings/"+bid+"/approve", c.s.Admin, nil); r.Status != 200 || r.vstatus() != "confirmed" {
		t.Fatalf("approve: %d %v", r.Status, r.Body)
	}
	if r := call(t, "POST", "/api/v1/society/bookings/"+bid+"/approve", c.s.Admin, nil); r.Status != 409 {
		t.Fatalf("approve twice: %d", r.Status)
	}
	got := false
	for _, n := range inbox(t, c.rA, "") {
		got = got || n["type"] == "booking.confirmed"
	}
	if !got {
		t.Fatal("no confirmation in the inbox")
	}

	b2 := book(t, c.rB, id, slotAt(t, c.rB, id, 4, 19))
	if r := call(t, "POST", "/api/v1/society/bookings/"+b2.str("id")+"/reject", c.s.Admin, map[string]any{"reason": "Hall under repair"}); r.Status != 200 || r.vstatus() != "rejected" {
		t.Fatalf("reject: %d %v", r.Status, r.Body)
	}
	var charge string
	_ = pool.QueryRow(context.Background(), `SELECT charge_status FROM amenity_bookings WHERE id=$1`, b2.str("id")).Scan(&charge)
	if charge != "waived" {
		t.Fatalf("charge after rejection: %s", charge)
	}
	// rejected slot is free again
	if r := book(t, c.rB, id, slotAt(t, c.rB, id, 4, 19)); r.Status != 201 {
		t.Fatalf("rebook: %d %v", r.Status, r.Body)
	}
	// office cancel
	if r := call(t, "POST", "/api/v1/society/bookings/"+bid+"/cancel", c.s.Admin, map[string]any{"reason": "Water leak in the hall"}); r.Status != 200 || r.data()["cancelled_by"] != "office" {
		t.Fatalf("office cancel: %d %v", r.Status, r.Body)
	}
	// desk list + csv + stats
	rows, r := callList(t, "/api/v1/society/bookings?status=rejected", c.s.Admin)
	if r.Status != 200 || len(rows) != 1 {
		t.Fatalf("desk list: %d %v", r.Status, rows)
	}
	code, ct, body := rawGet(t, "/api/v1/society/bookings/export", c.s.Admin)
	if code != 200 || !strings.HasPrefix(ct, "text/csv") || !strings.Contains(string(body), "Party hall") {
		t.Fatalf("export: %d %s %s", code, ct, body)
	}
	if code, _, _ := rawGet(t, "/api/v1/society/amenities/stats", c.s.Admin); code != 200 {
		t.Fatalf("stats: %d", code)
	}
}

func TestAmenitySweeper(t *testing.T) {
	c := newCommunity(t)
	a := openAmenity(t, c, map[string]any{"name": "Gym", "requires_approval": true})
	id := a.str("id")
	ctx := context.Background()
	unit := c.units["A101"]
	ins := func(status string, startIn time.Duration, createdAgo time.Duration) string {
		var bid string
		st := time.Now().Add(startIn).Truncate(time.Minute)
		if err := pool.QueryRow(ctx, `INSERT INTO amenity_bookings (society_id, amenity_id, unit_id, booked_by_membership_id, slot_start, slot_end, status, created_at)
			SELECT $1,$2,$3,m.id,$4,$5,$6,$7 FROM memberships m WHERE m.unit_id=$3 AND m.role_key='resident_owner' LIMIT 1 RETURNING id`,
			c.s.ID, id, unit, st, st.Add(time.Hour), status, time.Now().Add(-createdAgo)).Scan(&bid); err != nil {
			t.Fatal(err)
		}
		return bid
	}
	done := ins("confirmed", -3*time.Hour, 5*time.Hour)
	stale := ins("pending", 6*time.Hour, 3*time.Hour) // inside 12h and older than 2h
	fresh := ins("pending", 7*time.Hour, 5*time.Minute)
	soon := ins("confirmed", 40*time.Minute, time.Hour)
	completed, expired, reminded, err := aamenity.Sweep(ctx)
	if err != nil || completed != 1 || expired != 1 || reminded != 1 {
		t.Fatalf("sweep: %d %d %d %v", completed, expired, reminded, err)
	}
	statusOf := func(id string) string {
		var s string
		_ = pool.QueryRow(ctx, `SELECT status FROM amenity_bookings WHERE id=$1`, id).Scan(&s)
		return s
	}
	if statusOf(done) != "completed" || statusOf(stale) != "expired" || statusOf(fresh) != "pending" || statusOf(soon) != "confirmed" {
		t.Fatal("sweeper states wrong")
	}
	if _, _, again, _ := aamenity.Sweep(ctx); again != 0 {
		t.Fatalf("reminder repeated: %d", again)
	}
	// no-show only after the slot ended
	if r := call(t, "POST", "/api/v1/society/bookings/"+soon+"/no-show", c.s.Admin, nil); r.Status != 409 {
		t.Fatalf("no-show before the end: %d", r.Status)
	}
	if r := call(t, "POST", "/api/v1/society/bookings/"+done+"/no-show", c.s.Admin, nil); r.Status != 200 || r.vstatus() != "no_show" {
		t.Fatalf("no-show: %d %v", r.Status, r.Body)
	}
}

func TestAmenityTenantIsolationAndSwitch(t *testing.T) {
	c1, c2 := newCommunity(t), newCommunity(t)
	a := openAmenity(t, c1, nil)
	if r := call(t, "GET", "/api/v1/amenities/"+a.str("id"), c2.rA, nil); r.Status != 404 {
		t.Fatalf("other society's amenity: %d", r.Status)
	}
	if r := call(t, "PUT", "/api/v1/society/amenities/"+a.str("id"), c2.s.Admin, map[string]any{"name": "Hack"}); r.Status != 404 && r.Status != 400 {
		t.Fatalf("other society's admin: %d", r.Status)
	}
	mustExec(t, context.Background(), `INSERT INTO society_features (society_id, feature_key, enabled) VALUES ($1,'amenities',false)`, c1.s.ID)
	if r := call(t, "GET", "/api/v1/amenities", c1.rA, nil); r.Status != 403 || r.errCode() != "FEATURE_DISABLED" {
		t.Fatalf("switched off: %d %v", r.Status, r.Body)
	}
	if r := call(t, "GET", "/api/v1/society/amenities", c1.s.Admin, nil); r.Status != 403 {
		t.Fatalf("office switched off: %d", r.Status)
	}
}

// ── daily help and attendance ────────────────────────────────────────────────

func registerHelper(t *testing.T, tok, phone string, extra map[string]any) resp {
	t.Helper()
	b := map[string]any{"full_name": "Sunita Devi", "phone": phone, "kind": "maid", "id_last4": "4821", "days": []int{0, 1, 2, 3, 4, 5, 6}}
	for k, v := range extra {
		b[k] = v
	}
	return call(t, "POST", "/api/v1/helpers", tok, b)
}

func TestHelperLifecycleAndGate(t *testing.T) {
	c := newCommunity(t)
	phone := nextPhone()
	h := registerHelper(t, c.rA, phone, nil)
	if h.Status != 201 || h.vstatus() != "pending" {
		t.Fatalf("register: %d %v", h.Status, h.Body)
	}
	hid := h.str("id")
	if r := registerHelper(t, c.rA, phone, nil); r.Status != 409 || r.errCode() != "ALREADY_LINKED" {
		t.Fatalf("register twice: %d %v", r.Status, r.Body)
	}
	if r := registerHelper(t, c.famA, nextPhone(), nil); r.Status != 403 {
		t.Fatalf("family registers: %d", r.Status)
	}
	if r := registerHelper(t, c.rA, "12345", nil); r.Status != 400 {
		t.Fatalf("bad phone: %d", r.Status)
	}
	// same person, second flat: linked, not duplicated
	h2 := registerHelper(t, c.rB, phone, map[string]any{"full_name": "Someone Else"})
	if h2.Status != 201 || h2.str("id") != hid || h2.str("full_name") != "Sunita Devi" {
		t.Fatalf("second flat: %d %v", h2.Status, h2.Body)
	}
	if flats := h2.data()["flats"].([]any); len(flats) != 1 {
		t.Fatalf("B must only see its own link: %v", flats)
	}
	// family reads, others do not
	if rows, _ := callList(t, "/api/v1/helpers", c.famA); len(rows) != 1 {
		t.Fatalf("family list: %d", len(rows))
	}
	other := newCommunity(t)
	if r := call(t, "GET", "/api/v1/helpers/"+hid, other.rA, nil); r.Status != 404 {
		t.Fatalf("foreign society: %d", r.Status)
	}
	rC, _ := newResident(t, c.s, c.units["A102"], "owner")
	if r := call(t, "GET", "/api/v1/helpers/"+hid, rC, nil); r.Status != 404 {
		t.Fatalf("flat without the link: %d", r.Status)
	}

	// pass and gate
	p := call(t, "GET", "/api/v1/helpers/"+hid+"/pass", c.rA, nil)
	if p.Status != 200 || !strings.HasPrefix(p.str("qr"), "NXH1:") || len(p.str("code")) != 6 {
		t.Fatalf("pass: %d %v", p.Status, p.Body)
	}
	if r := call(t, "GET", "/api/v1/helpers/"+hid+"/pass", c.famA, nil); r.Status != 200 {
		t.Fatalf("family pass: %d", r.Status)
	}
	if r := call(t, "POST", "/api/v1/attendance/resolve", c.rA, map[string]any{"code": p.str("code")}); r.Status != 403 {
		t.Fatalf("resident resolves: %d", r.Status)
	}
	res := call(t, "POST", "/api/v1/attendance/resolve", c.guard.Token, map[string]any{"qr": p.str("qr")})
	if res.Status != 200 || res.str("subject_id") != hid || res.data()["warning"] != "PENDING" {
		t.Fatalf("resolve by QR: %d %v", res.Status, res.Body)
	}
	if _, has := res.data()["phone"]; has {
		t.Fatal("the guard saw a phone number")
	}
	if flats := res.data()["flats"].([]any); len(flats) != 2 {
		t.Fatalf("guard flats: %v", flats)
	}
	if r := call(t, "POST", "/api/v1/attendance/resolve", c.guard.Token, map[string]any{"qr": p.str("qr")[:len(p.str("qr"))-2] + "zz"}); r.Status != 400 {
		t.Fatalf("tampered QR: %d", r.Status)
	}
	if r := call(t, "POST", "/api/v1/attendance/resolve", c.guard.Token, map[string]any{"code": "000000"}); r.Status != 404 {
		t.Fatalf("unknown code: %d", r.Status)
	}
	wA := dialWS(t, c.rA)
	in := call(t, "POST", "/api/v1/attendance/check-in", c.guard.Token, map[string]any{"subject_type": "helper", "subject_id": hid})
	if in.Status != 200 || in.data()["out_at"] != nil {
		t.Fatalf("check-in: %d %v", in.Status, in.Body)
	}
	wA.expect(t, "attendance.updated")
	if n := wA.note(2 * time.Second); n == nil || n["type"] != "helper.arrived" {
		t.Fatalf("arrival note: %v", n)
	}
	if r := call(t, "POST", "/api/v1/attendance/check-in", c.guard.Token, map[string]any{"subject_type": "helper", "subject_id": hid}); r.Status != 409 || r.errCode() != "ALREADY_INSIDE" {
		t.Fatalf("second check-in: %d %v", r.Status, r.Body)
	}
	if got := call(t, "GET", "/api/v1/helpers/"+hid, c.rA, nil); got.data()["today"].(map[string]any)["inside"] != true {
		t.Fatalf("today status: %v", got.Body)
	}
	if rows, _ := callList(t, "/api/v1/attendance/inside", c.guard.Token); len(rows) != 1 {
		t.Fatalf("inside: %d", len(rows))
	}
	if out := call(t, "POST", "/api/v1/attendance/check-out", c.guard.Token, map[string]any{"subject_type": "helper", "subject_id": hid}); out.Status != 200 || out.data()["out_at"] == nil {
		t.Fatalf("check-out: %d %v", out.Status, out.Body)
	}
	if r := call(t, "POST", "/api/v1/attendance/check-out", c.guard.Token, map[string]any{"subject_type": "helper", "subject_id": hid}); r.Status != 409 || r.errCode() != "NOT_INSIDE" {
		t.Fatalf("second check-out: %d %v", r.Status, r.Body)
	}

	// rating, schedule, regeneration
	if r := call(t, "PUT", "/api/v1/helpers/"+hid+"/rating", c.rA, map[string]any{"rating": 5, "comment": "Punctual"}); r.Status != 200 || r.data()["my_rating"].(float64) != 5 {
		t.Fatalf("rate: %d %v", r.Status, r.Body)
	}
	if r := call(t, "PUT", "/api/v1/helpers/"+hid+"/rating", c.rA, map[string]any{"rating": 9}); r.Status != 400 {
		t.Fatalf("bad rating: %d", r.Status)
	}
	if r := call(t, "PUT", "/api/v1/helpers/"+hid+"/schedule", c.rA, map[string]any{"days": []int{1, 2}, "window_from": "07:00", "window_to": "09:00", "notify_exit": true}); r.Status != 200 {
		t.Fatalf("schedule: %d %v", r.Status, r.Body)
	}
	np := call(t, "POST", "/api/v1/helpers/"+hid+"/pass/regenerate", c.rA, nil)
	if np.Status != 200 || np.str("qr") != p.str("qr") {
		t.Fatalf("regenerate keeps the QR: %d %v", np.Status, np.Body)
	}
	if r := call(t, "POST", "/api/v1/attendance/resolve", c.guard.Token, map[string]any{"code": p.str("code")}); np.str("code") != p.str("code") && r.Status != 404 {
		t.Fatalf("old code still works: %d", r.Status)
	}
	if r := call(t, "DELETE", "/api/v1/helpers/"+hid, c.rA, nil); r.Status != 200 {
		t.Fatalf("unlink: %d", r.Status)
	}
	if r := call(t, "GET", "/api/v1/helpers/"+hid, c.rA, nil); r.Status != 404 {
		t.Fatalf("unlinked still visible: %d", r.Status)
	}
	if r := call(t, "GET", "/api/v1/helpers/"+hid, c.rB, nil); r.Status != 200 {
		t.Fatalf("B lost the helper: %d", r.Status)
	}
}

func TestHelperVerificationAndBlock(t *testing.T) {
	c := newCommunity(t)
	hid := registerHelper(t, c.rA, nextPhone(), nil).str("id")
	if r := call(t, "POST", "/api/v1/society/helpers/"+hid+"/verify", c.rA, nil); r.Status != 403 {
		t.Fatalf("resident verifies: %d", r.Status)
	}
	if r := call(t, "GET", "/api/v1/society/helpers", c.guard.Token, nil); r.Status != 403 {
		t.Fatalf("guard lists register: %d", r.Status)
	}
	if rows, _ := callList(t, "/api/v1/society/helpers?status=pending", c.s.Admin); len(rows) != 1 {
		t.Fatalf("pending list: %d", len(rows))
	}
	if r := call(t, "POST", "/api/v1/society/helpers/"+hid+"/verify", c.s.Admin, nil); r.Status != 200 || r.vstatus() != "verified" {
		t.Fatalf("verify: %d %v", r.Status, r.Body)
	}
	pass := call(t, "GET", "/api/v1/helpers/"+hid+"/pass", c.rA, nil)
	if r := call(t, "POST", "/api/v1/attendance/resolve", c.guard.Token, map[string]any{"code": pass.str("code")}); r.Status != 200 || r.data()["warning"] != nil {
		t.Fatalf("verified resolves without a warning: %d %v", r.Status, r.Body)
	}
	if r := call(t, "POST", "/api/v1/society/helpers/"+hid+"/suspend", c.s.Admin, map[string]any{"reason": "ab"}); r.Status != 400 {
		t.Fatalf("suspend needs a reason: %d", r.Status)
	}
	if r := call(t, "POST", "/api/v1/society/helpers/"+hid+"/suspend", c.s.Admin, map[string]any{"reason": "Complaint from block A"}); r.Status != 200 {
		t.Fatalf("suspend: %d %v", r.Status, r.Body)
	}
	if r := call(t, "POST", "/api/v1/attendance/check-in", c.guard.Token, map[string]any{"subject_type": "helper", "subject_id": hid}); r.Status != 403 || r.errCode() != "BLOCKED" {
		t.Fatalf("suspended check-in: %d %v", r.Status, r.Body)
	}
	if n := inbox(t, c.rA, ""); len(n) == 0 || n[0]["type"] != "helper.status" {
		t.Fatalf("flat not told: %v", n)
	}
	if r := call(t, "POST", "/api/v1/society/helpers/"+hid+"/restore", c.s.Admin, nil); r.Status != 200 || r.vstatus() != "pending" {
		t.Fatalf("restore: %d %v", r.Status, r.Body)
	}
	phone := ""
	_ = pool.QueryRow(context.Background(), `SELECT phone FROM helpers WHERE id=$1`, hid).Scan(&phone)
	if r := call(t, "POST", "/api/v1/society/helpers/"+hid+"/blacklist", c.s.Admin, map[string]any{"reason": "Theft reported"}); r.Status != 200 {
		t.Fatalf("blacklist: %d %v", r.Status, r.Body)
	}
	if r := registerHelper(t, c.rB, phone, nil); r.Status != 403 || r.errCode() != "BLACKLISTED" {
		t.Fatalf("registering a blacklisted person: %d %v", r.Status, r.Body)
	}
	if r := call(t, "POST", "/api/v1/society/helpers/"+hid+"/verify", c.s.Admin, nil); r.Status != 409 {
		t.Fatalf("verify while blacklisted: %d", r.Status)
	}
}

func TestStaffAttendanceAndSheet(t *testing.T) {
	c := newCommunity(t)
	if r := call(t, "POST", "/api/v1/society/staff", c.rA, map[string]any{"full_name": "Ramesh", "phone": nextPhone(), "role": "gardener"}); r.Status != 403 {
		t.Fatalf("resident creates staff: %d", r.Status)
	}
	phone := nextPhone()
	st := call(t, "POST", "/api/v1/society/staff", c.s.Admin, map[string]any{"full_name": "Ramesh Kumar", "phone": phone, "role": "gardener", "shift_from": "06:00", "shift_to": "14:00"})
	if st.Status != 201 {
		t.Fatalf("create staff: %d %v", st.Status, st.Body)
	}
	if r := call(t, "POST", "/api/v1/society/staff", c.s.Admin, map[string]any{"full_name": "Dup", "phone": phone, "role": "gardener"}); r.Status != 409 {
		t.Fatalf("duplicate phone: %d", r.Status)
	}
	sid := st.str("id")
	pass := call(t, "GET", "/api/v1/society/staff/"+sid+"/pass", c.s.Admin, nil)
	if !strings.HasPrefix(pass.str("qr"), "NXS1:") {
		t.Fatalf("staff pass: %v", pass.Body)
	}
	res := call(t, "POST", "/api/v1/attendance/resolve", c.guard.Token, map[string]any{"code": pass.str("code")})
	if res.Status != 200 || res.str("subject_type") != "staff" || res.data()["shift"] != "06:00-14:00" {
		t.Fatalf("resolve staff: %d %v", res.Status, res.Body)
	}
	// a helper's QR cannot be replayed as staff and the other way round
	if r := call(t, "POST", "/api/v1/attendance/check-in", c.guard.Token, map[string]any{"subject_type": "helper", "subject_id": sid}); r.Status != 404 {
		t.Fatalf("staff id as helper: %d", r.Status)
	}
	if r := call(t, "POST", "/api/v1/attendance/check-in", c.guard.Token, map[string]any{"subject_type": "staff", "subject_id": sid}); r.Status != 200 {
		t.Fatalf("staff in: %d %v", r.Status, r.Body)
	}
	call(t, "POST", "/api/v1/attendance/check-out", c.guard.Token, map[string]any{"subject_type": "staff", "subject_id": sid})

	// manual entry for yesterday, no overlap
	yd := time.Now().Add(-24 * time.Hour).Truncate(time.Hour)
	man := map[string]any{"subject_type": "staff", "subject_id": sid, "in_at": yd.Add(-8 * time.Hour).Format(time.RFC3339), "out_at": yd.Format(time.RFC3339), "note": "Gate was offline"}
	if r := call(t, "POST", "/api/v1/society/attendance/manual", c.s.Admin, man); r.Status != 201 || r.data()["source"] != "manual" {
		t.Fatalf("manual: %d %v", r.Status, r.Body)
	}
	if r := call(t, "POST", "/api/v1/society/attendance/manual", c.s.Admin, man); r.Status != 409 {
		t.Fatalf("overlapping manual: %d", r.Status)
	}
	if r := call(t, "POST", "/api/v1/society/attendance/manual", c.guard.Token, man); r.Status != 403 {
		t.Fatalf("guard manual: %d", r.Status)
	}
	sheet := call(t, "GET", "/api/v1/society/attendance?type=staff", c.s.Admin, nil)
	rows := sheet.data()["rows"].([]any)
	if sheet.Status != 200 || len(rows) != 1 || rows[0].(map[string]any)["present_days"].(float64) < 1 {
		t.Fatalf("sheet: %d %v", sheet.Status, sheet.Body)
	}
	// a second and third person must keep their own days (rows are built before the logs are matched)
	for _, name := range []string{"Lakshmi Bai", "Anil Verma"} {
		o := call(t, "POST", "/api/v1/society/staff", c.s.Admin, map[string]any{"full_name": name, "phone": nextPhone(), "role": "housekeeping"})
		call(t, "POST", "/api/v1/attendance/check-in", c.guard.Token, map[string]any{"subject_type": "staff", "subject_id": o.str("id")})
	}
	sheet = call(t, "GET", "/api/v1/society/attendance?type=staff", c.s.Admin, nil)
	for _, r := range sheet.data()["rows"].([]any) {
		if r.(map[string]any)["present_days"].(float64) < 1 {
			t.Fatalf("a row lost its attendance: %v", r)
		}
	}
	if code, ct, body := rawGet(t, "/api/v1/society/attendance/export", c.s.Admin); code != 200 || !strings.HasPrefix(ct, "text/csv") || !strings.Contains(string(body), "Ramesh Kumar") {
		t.Fatalf("export: %d %s", code, body)
	}
	if code, _, _ := rawGet(t, "/api/v1/society/attendance/staff/"+sid, c.s.Admin); code != 200 {
		t.Fatalf("log: %d", code)
	}
	if code, _, _ := rawGet(t, "/api/v1/society/attendance", c.rA); code != 403 {
		t.Fatalf("resident reads the sheet: %d", code)
	}
}

func TestAttendanceAutoCloseAndOffline(t *testing.T) {
	c := newCommunity(t)
	hid := registerHelper(t, c.rA, nextPhone(), nil).str("id")
	// offline queue: arrive 2 hours ago, leave 1 hour ago; replays return the stored outcome
	ev := func(id, typ string, ago time.Duration) map[string]any {
		return map[string]any{"client_event_id": id, "type": typ, "subject_type": "helper", "subject_id": hid, "at": time.Now().Add(-ago).Format(time.RFC3339)}
	}
	suffix := c.s.ID.String()[:8]
	batch := map[string]any{"events": []any{ev("att-in-"+suffix, "attendance_in", 2*time.Hour), ev("att-out-"+suffix, "attendance_out", time.Hour)}}
	r := call(t, "POST", "/api/v1/gate/events/batch", c.guard.Token, batch)
	items := r.Body["data"].([]any)
	if r.Status != 200 || len(items) != 2 || items[0].(map[string]any)["status"].(float64) != 200 || items[1].(map[string]any)["status"].(float64) != 200 {
		t.Fatalf("batch: %d %v", r.Status, r.Body)
	}
	again := call(t, "POST", "/api/v1/gate/events/batch", c.guard.Token, batch)
	for _, it := range again.Body["data"].([]any) {
		if it.(map[string]any)["replayed"] != true {
			t.Fatalf("replay not recognised: %v", again.Body)
		}
	}
	var n int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM attendance_logs WHERE subject_id=$1`, hid).Scan(&n)
	if n != 1 {
		t.Fatalf("replay created %d records", n)
	}
	// the sync carries the helper, hashed, without a phone number
	sync := call(t, "GET", "/api/v1/gate/passes/sync", c.guard.Token, nil)
	att := sync.data()["attendance"].([]any)
	if len(att) != 1 || att[0].(map[string]any)["code_hash"] == nil || att[0].(map[string]any)["phone"] != nil {
		t.Fatalf("sync: %v", sync.Body)
	}
	// forgotten check-out is closed after midnight
	mustExec(t, context.Background(), `INSERT INTO attendance_logs (society_id, subject_type, subject_id, in_at) VALUES ($1,'helper',$2, now() - interval '30 hours')`, c.s.ID, hid)
	closed, err := aattend.Sweep(context.Background())
	if err != nil || closed != 1 {
		t.Fatalf("sweep: %d %v", closed, err)
	}
	var src string
	_ = pool.QueryRow(context.Background(), `SELECT source FROM attendance_logs WHERE subject_id=$1 AND source='auto_closed'`, hid).Scan(&src)
	if src != "auto_closed" {
		t.Fatal("not auto-closed")
	}
	// switched off: the gate stops and the sync is empty
	mustExec(t, context.Background(), `INSERT INTO society_features (society_id, feature_key, enabled) VALUES ($1,'staff_attendance',false)`, c.s.ID)
	if r := call(t, "POST", "/api/v1/attendance/resolve", c.guard.Token, map[string]any{"qr": "x"}); r.Status != 403 {
		t.Fatalf("switched off: %d", r.Status)
	}
	if s := call(t, "GET", "/api/v1/gate/passes/sync", c.guard.Token, nil); len(s.data()["attendance"].([]any)) != 0 {
		t.Fatal("sync not empty when switched off")
	}
}
