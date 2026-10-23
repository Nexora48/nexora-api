package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/Nexora48/nexora-api/internal/auth"
)

// ── helpers ──────────────────────────────────────────────────────────────────

var (
	phoneSeq  atomic.Int64
	adminHash string
	hashOnce  sync.Once
)

func nextPhone() string { return fmt.Sprintf("%d", 9100000000+phoneSeq.Add(1)) }

type soc struct {
	ID    uuid.UUID
	Admin string // access token
}

// newSoc creates an isolated society with its own admin and returns an admin session.
func newSoc(t *testing.T) soc {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test (needs embedded Postgres)")
	}
	mr.FlushAll()
	hashOnce.Do(func() { adminHash, _ = auth.HashPassword("Admin@123") })
	ctx := context.Background()
	id, uid := uuid.New(), uuid.New()
	email := fmt.Sprintf("admin-%s@test.local", id.String()[:8])
	mustExec(t, ctx, `INSERT INTO societies (id, name, slug) VALUES ($1,$2,$3)`, id, "Society "+id.String()[:6], "s-"+id.String()[:8])
	mustExec(t, ctx, `INSERT INTO users (id, full_name, phone, email, password_hash) VALUES ($1,'Admin',$2,$3,$4)`, uid, nextPhone(), email, adminHash)
	mustExec(t, ctx, `INSERT INTO memberships (user_id, society_id, role_key) VALUES ($1,$2,'society_admin')`, uid, id)
	r := call(t, "POST", "/api/v1/auth/login", "", map[string]any{"email": email, "password": "Admin@123"})
	if r.Status != 200 {
		t.Fatalf("admin login: %d %v", r.Status, r.Body)
	}
	return soc{ID: id, Admin: r.str("access_token")}
}

func mustExec(t *testing.T, ctx context.Context, q string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, q, args...); err != nil {
		t.Fatalf("sql: %v\n%s", err, q)
	}
}

func (r resp) list() []any { l, _ := r.data()["__list"].([]any); return l }

// callList is like call but returns the data array.
func callList(t *testing.T, path, token string) ([]map[string]any, resp) {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(res.Body).Decode(&body)
	out := []map[string]any{}
	if arr, ok := body["data"].([]any); ok {
		for _, x := range arr {
			out = append(out, x.(map[string]any))
		}
	}
	return out, resp{res.StatusCode, body}
}

func (r resp) total() int {
	m, _ := r.Body["meta"].(map[string]any)
	f, _ := m["total"].(float64)
	return int(f)
}

func upload(t *testing.T, path, token, csv string) resp {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "r.csv")
	io.WriteString(fw, csv)
	mw.Close()
	req, _ := http.NewRequest("POST", srv.URL+path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(res.Body).Decode(&m)
	return resp{res.StatusCode, m}
}

// setupBlock creates block A with floors×perFloor flats and returns the block id and unit ids by number.
func setupBlock(t *testing.T, s soc, code string, floors, perFloor int) (string, map[string]string) {
	t.Helper()
	b := call(t, "POST", "/api/v1/society/blocks", s.Admin, map[string]any{"name": "Block " + code, "code": code})
	if b.Status != 201 {
		t.Fatalf("create block: %d %v", b.Status, b.Body)
	}
	bid := b.str("id")
	r := call(t, "POST", "/api/v1/society/blocks/"+bid+"/units/bulk", s.Admin, map[string]any{"floors": floors, "units_per_floor": perFloor, "unit_type": "2BHK"})
	if r.Status != 201 {
		t.Fatalf("bulk: %d %v", r.Status, r.Body)
	}
	units, _ := callList(t, "/api/v1/society/units?block_id="+bid+"&page_size=100", s.Admin)
	m := map[string]string{}
	for _, u := range units {
		m[u["number"].(string)] = u["id"].(string)
	}
	return bid, m
}

func addResident(t *testing.T, s soc, unitID, typ, name, phone string) resp {
	t.Helper()
	return call(t, "POST", "/api/v1/society/residents", s.Admin, map[string]any{"full_name": name, "phone": phone, "unit_id": unitID, "type": typ})
}

func otpLogin(t *testing.T, phone string) resp {
	t.Helper()
	if r := call(t, "POST", "/api/v1/auth/otp/request", "", map[string]any{"phone": phone}); r.Status != 204 {
		t.Fatalf("otp request %s: %d %v", phone, r.Status, r.Body)
	}
	return call(t, "POST", "/api/v1/auth/otp/verify", "", map[string]any{"phone": phone, "otp": "123456"})
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestBlocksAndBulkUnits(t *testing.T) {
	s := newSoc(t)
	b := call(t, "POST", "/api/v1/society/blocks", s.Admin, map[string]any{"name": "Block A", "code": "a"})
	if b.Status != 201 || b.str("code") != "A" {
		t.Fatalf("create block: %d %v", b.Status, b.Body)
	}
	bid := b.str("id")
	if d := call(t, "POST", "/api/v1/society/blocks", s.Admin, map[string]any{"name": "Dup", "code": "A"}); d.Status != 409 || d.errCode() != "BLOCK_EXISTS" {
		t.Fatalf("dup block: %d %v", d.Status, d.Body)
	}

	spec := map[string]any{"floors": 4, "units_per_floor": 6, "unit_type": "3BHK"}
	dry := call(t, "POST", "/api/v1/society/blocks/"+bid+"/units/bulk?dry_run=true", s.Admin, spec)
	if dry.Status != 200 || len(dry.data()["numbers"].([]any)) != 24 || dry.data()["numbers"].([]any)[0] != "A101" {
		t.Fatalf("dry run: %d %v", dry.Status, dry.Body)
	}
	if got, _ := callList(t, "/api/v1/society/units?block_id="+bid, s.Admin); len(got) != 0 {
		t.Fatal("dry run must not write")
	}
	real := call(t, "POST", "/api/v1/society/blocks/"+bid+"/units/bulk", s.Admin, spec)
	if real.Status != 201 || real.data()["created"] != float64(24) {
		t.Fatalf("bulk: %d %v", real.Status, real.Body)
	}
	again := call(t, "POST", "/api/v1/society/blocks/"+bid+"/units/bulk", s.Admin, spec)
	if again.data()["created"] != float64(0) || len(again.data()["skipped_existing"].([]any)) != 24 {
		t.Fatalf("re-run must skip existing: %v", again.Body)
	}
	if bad := call(t, "POST", "/api/v1/society/blocks/"+bid+"/units/bulk", s.Admin, map[string]any{"floors": 2, "units_per_floor": 2, "unit_type": "2BHK", "number_format": "{block}"}); bad.Status != 400 {
		t.Fatalf("duplicate-producing format: %d %v", bad.Status, bad.Body)
	}

	ov := call(t, "GET", "/api/v1/society/apartments", s.Admin, nil)
	blocks := ov.data()["blocks"].([]any)
	blk := blocks[0].(map[string]any)
	floors := blk["floors"].([]any)
	if len(blocks) != 1 || blk["total_flats"] != float64(24) || blk["occupied_pct"] != float64(0) || len(floors) != 4 ||
		len(floors[0].(map[string]any)["flats"].([]any)) != 6 {
		t.Fatalf("overview: %v", ov.Body)
	}
	// ground floor + custom format
	g := call(t, "POST", "/api/v1/society/blocks", s.Admin, map[string]any{"name": "Block G", "code": "G"})
	call(t, "POST", "/api/v1/society/blocks/"+g.str("id")+"/units/bulk", s.Admin, map[string]any{"floors": 2, "units_per_floor": 1, "start_floor": 0, "unit_type": "Villa", "number_format": "{block}-{floor}{seq:03}"})
	units, _ := callList(t, "/api/v1/society/units?block_id="+g.str("id"), s.Admin)
	if len(units) != 2 || units[0]["number"] != "G-0001" || units[1]["number"] != "G-1001" {
		t.Fatalf("custom format: %v", units)
	}
}

func TestUnitCRUDAndDeleteGuards(t *testing.T) {
	s := newSoc(t)
	bid, units := setupBlock(t, s, "B", 1, 2)
	u := call(t, "POST", "/api/v1/society/units", s.Admin, map[string]any{"block_id": bid, "floor": 2, "number": "b201", "unit_type": "4BHK", "area_sqft": 1450})
	if u.Status != 201 || u.str("number") != "B201" {
		t.Fatalf("create unit: %d %v", u.Status, u.Body)
	}
	if d := call(t, "POST", "/api/v1/society/units", s.Admin, map[string]any{"block_id": bid, "floor": 2, "number": "B201", "unit_type": "2BHK"}); d.Status != 409 || d.errCode() != "UNIT_EXISTS" {
		t.Fatalf("dup unit: %d %v", d.Status, d.Body)
	}
	if up := call(t, "PUT", "/api/v1/society/units/"+u.str("id"), s.Admin, map[string]any{"block_id": bid, "floor": 2, "number": "B201", "unit_type": "3BHK"}); up.Status != 204 {
		t.Fatalf("update: %d %v", up.Status, up.Body)
	}
	if del := call(t, "DELETE", "/api/v1/society/units/"+u.str("id"), s.Admin, nil); del.Status != 204 {
		t.Fatalf("delete empty unit: %d %v", del.Status, del.Body)
	}
	// occupied flat / block cannot be deleted
	if r := addResident(t, s, units["B101"], "owner", "Occupant One", nextPhone()); r.Status != 201 {
		t.Fatalf("add resident: %d %v", r.Status, r.Body)
	}
	if del := call(t, "DELETE", "/api/v1/society/units/"+units["B101"], s.Admin, nil); del.Status != 409 || del.errCode() != "UNIT_OCCUPIED" {
		t.Fatalf("occupied unit delete: %d %v", del.Status, del.Body)
	}
	if del := call(t, "DELETE", "/api/v1/society/blocks/"+bid, s.Admin, nil); del.Status != 409 || del.errCode() != "BLOCK_NOT_EMPTY" {
		t.Fatalf("occupied block delete: %d %v", del.Status, del.Body)
	}
	// empty block deletes (cascade units)
	e := call(t, "POST", "/api/v1/society/blocks", s.Admin, map[string]any{"name": "Empty", "code": "E"})
	if del := call(t, "DELETE", "/api/v1/society/blocks/"+e.str("id"), s.Admin, nil); del.Status != 204 {
		t.Fatalf("empty block delete: %d %v", del.Status, del.Body)
	}
	// validation
	if bad := call(t, "POST", "/api/v1/society/units", s.Admin, map[string]any{"block_id": bid, "floor": 1, "number": "X1", "unit_type": "Castle"}); bad.Status != 400 {
		t.Fatalf("bad unit type: %d", bad.Status)
	}
}

func TestResidentLifecycle(t *testing.T) {
	s := newSoc(t)
	_, units := setupBlock(t, s, "A", 2, 2)
	ownerPhone, tenantPhone := nextPhone(), nextPhone()

	own := addResident(t, s, units["A101"], "owner", "Olive Owner", ownerPhone)
	if own.Status != 201 || own.str("status") != "pending" || own.str("type") != "owner" || own.str("unit_number") != "A101" {
		t.Fatalf("create owner: %d %v", own.Status, own.Body)
	}
	if d := addResident(t, s, units["A101"], "owner", "Second Owner", nextPhone()); d.Status != 409 || d.errCode() != "UNIT_ROLE_TAKEN" {
		t.Fatalf("second owner: %d %v", d.Status, d.Body)
	}
	if d := addResident(t, s, units["A102"], "owner", "Olive Again", ownerPhone); d.Status != 409 || d.errCode() != "ALREADY_RESIDENT" {
		t.Fatalf("same person twice: %d %v", d.Status, d.Body)
	}
	if d := addResident(t, s, units["A102"], "family", "Lonely Family", nextPhone()); d.Status != 409 || d.errCode() != "NO_HEAD" {
		t.Fatalf("family without head: %d %v", d.Status, d.Body)
	}
	if d := call(t, "POST", "/api/v1/society/residents", s.Admin, map[string]any{"full_name": "Bad", "phone": "12345", "unit_id": units["A102"], "type": "owner"}); d.Status != 400 || d.errCode() != "INVALID_PHONE" {
		t.Fatalf("bad phone: %d %v", d.Status, d.Body)
	}
	ten := addResident(t, s, units["A101"], "tenant", "Tina Tenant", "+91 "+tenantPhone[:5]+" "+tenantPhone[5:])
	if ten.Status != 201 || ten.str("phone") != tenantPhone {
		t.Fatalf("tenant (formatted phone): %d %v", ten.Status, ten.Body)
	}
	fam := addResident(t, s, units["A101"], "family", "Fred Family", nextPhone())
	if fam.Status != 201 || fam.str("type") != "family" {
		t.Fatalf("family: %d %v", fam.Status, fam.Body)
	}

	// occupancy derives from residents
	un := call(t, "GET", "/api/v1/society/units/"+units["A101"], s.Admin, nil)
	if un.str("occupancy") != "rented" {
		t.Fatalf("A101 occupancy: %v", un.Body)
	}
	if v, _ := callList(t, "/api/v1/society/units?occupancy=vacant&page_size=100", s.Admin); len(v) != 3 {
		t.Fatalf("vacant units want 3 got %d", len(v))
	}

	// list filters
	for q, want := range map[string]int{"": 3, "?type=owner": 1, "?type=tenant": 1, "?type=family": 1, "?status=pending": 3, "?status=active": 0, "?q=olive": 1, "?q=A101": 3} {
		path := "/api/v1/society/residents" + q
		rows, r := callList(t, path, s.Admin)
		if r.Status != 200 || len(rows) != want || r.total() != want {
			t.Fatalf("list %q: want %d got %d (%v)", q, want, len(rows), r.Body)
		}
	}
	if rows, r := callList(t, "/api/v1/society/residents?page=2&page_size=2", s.Admin); len(rows) != 1 || r.total() != 3 {
		t.Fatalf("pagination: %d rows, total %d", len(rows), r.total())
	}

	// owner logs in -> no longer pending; /me shows the flat
	login := otpLogin(t, ownerPhone)
	if login.Status != 200 {
		t.Fatalf("owner login: %d %v", login.Status, login.Body)
	}
	me := call(t, "GET", "/api/v1/me", login.str("access_token"), nil).data()
	unit, _ := me["unit"].(map[string]any)
	if unit == nil || unit["number"] != "A101" || unit["block"] != "A" || me["member_since"] == nil {
		t.Fatalf("/me unit: %v", me)
	}
	if det := call(t, "GET", "/api/v1/society/residents/"+own.str("membership_id"), s.Admin, nil); det.str("status") != "active" {
		t.Fatalf("status after login: %v", det.Body)
	}

	// suspend: sessions die, login refused; restore re-enables
	mid := own.str("membership_id")
	if r := call(t, "POST", "/api/v1/society/residents/"+mid+"/suspend", s.Admin, nil); r.Status != 204 {
		t.Fatalf("suspend: %d %v", r.Status, r.Body)
	}
	if r := call(t, "POST", "/api/v1/society/residents/"+mid+"/suspend", s.Admin, nil); r.Status != 409 || r.errCode() != "INVALID_STATE" {
		t.Fatalf("double suspend: %d %v", r.Status, r.Body)
	}
	if rf := call(t, "POST", "/api/v1/auth/refresh", "", map[string]any{"refresh_token": login.str("refresh_token")}); rf.Status != 401 {
		t.Fatalf("refresh after suspend: %d %v", rf.Status, rf.Body)
	}
	if m := call(t, "GET", "/api/v1/me", login.str("access_token"), nil); m.Status != 403 {
		t.Fatalf("/me with old token after suspend: %d", m.Status)
	}
	if r := call(t, "POST", "/api/v1/auth/otp/request", "", map[string]any{"phone": ownerPhone}); r.Status != 404 {
		t.Fatalf("otp for suspended: %d %v", r.Status, r.Body)
	}
	if rows, _ := callList(t, "/api/v1/society/residents?status=suspended", s.Admin); len(rows) != 1 {
		t.Fatal("suspended filter")
	}
	if r := call(t, "POST", "/api/v1/society/residents/"+mid+"/restore", s.Admin, nil); r.Status != 204 {
		t.Fatalf("restore: %d %v", r.Status, r.Body)
	}
	mr.FlushAll()
	if l := otpLogin(t, ownerPhone); l.Status != 200 {
		t.Fatalf("login after restore: %d %v", l.Status, l.Body)
	}

	// move-out frees the flat (and the family login of that head); re-adding re-activates
	tmid := ten.str("membership_id")
	if r := call(t, "POST", "/api/v1/society/residents/"+tmid+"/move-out", s.Admin, nil); r.Status != 204 {
		t.Fatalf("move-out: %d %v", r.Status, r.Body)
	}
	if un := call(t, "GET", "/api/v1/society/units/"+units["A101"], s.Admin, nil); un.str("occupancy") != "self_occupied" {
		t.Fatalf("after tenant left: %v", un.Body)
	}
	back := addResident(t, s, units["A101"], "tenant", "Tina Tenant", tenantPhone)
	if back.Status != 201 {
		t.Fatalf("re-add after move-out: %d %v", back.Status, back.Body)
	}

	// edit + stats
	up := call(t, "PUT", "/api/v1/society/residents/"+mid, s.Admin, map[string]any{"full_name": "Olive Q. Owner", "email": "olive@example.com",
		"agreement_end_date": "2027-01-31", "can_pay_maintenance": true, "emergency_contact": map[string]any{"name": "Pat", "phone": "9876543210", "relation": "Sister"}})
	if up.Status != 200 || up.str("full_name") != "Olive Q. Owner" || up.data()["can_pay_maintenance"] != true ||
		up.data()["emergency_contact"].(map[string]any)["name"] != "Pat" {
		t.Fatalf("update: %d %v", up.Status, up.Body)
	}
	st := call(t, "GET", "/api/v1/society/stats", s.Admin, nil).data()
	if st["total_flats"] != float64(4) || st["occupied_flats"] != float64(1) || st["blocks"] != float64(1) {
		t.Fatalf("stats: %v", st)
	}
}

func TestAppAccessToggleBlocksLogin(t *testing.T) {
	s := newSoc(t)
	_, units := setupBlock(t, s, "A", 1, 1)
	phone := nextPhone()
	r := addResident(t, s, units["A101"], "owner", "Access Test", phone)
	l := otpLogin(t, phone)
	if l.Status != 200 {
		t.Fatalf("login: %d %v", l.Status, l.Body)
	}
	call(t, "PUT", "/api/v1/society/residents/"+r.str("membership_id"), s.Admin, map[string]any{"app_access": false})
	if rf := call(t, "POST", "/api/v1/auth/refresh", "", map[string]any{"refresh_token": l.str("refresh_token")}); rf.Status != 401 {
		t.Fatalf("refresh after access off: %d", rf.Status)
	}
	if o := call(t, "POST", "/api/v1/auth/otp/request", "", map[string]any{"phone": phone}); o.Status != 404 {
		t.Fatalf("otp after access off: %d %v", o.Status, o.Body)
	}
}

func TestTenantIsolationM2(t *testing.T) {
	a, b := newSoc(t), newSoc(t)
	_, unitsA := setupBlock(t, a, "A", 1, 2)
	bidB, unitsB := setupBlock(t, b, "A", 1, 2) // same block code & numbers in both societies is fine
	resA := addResident(t, a, unitsA["A101"], "owner", "Alice A", nextPhone())
	_ = bidB

	// B cannot see or touch A's data
	if r := call(t, "GET", "/api/v1/society/residents/"+resA.str("membership_id"), b.Admin, nil); r.Status != 404 {
		t.Fatalf("B reads A resident: %d", r.Status)
	}
	for _, act := range []string{"suspend", "restore", "move-out"} {
		if r := call(t, "POST", "/api/v1/society/residents/"+resA.str("membership_id")+"/"+act, b.Admin, nil); r.Status != 404 {
			t.Fatalf("B %s A resident: %d", act, r.Status)
		}
	}
	if r := call(t, "PUT", "/api/v1/society/residents/"+resA.str("membership_id"), b.Admin, map[string]any{"full_name": "Hacked"}); r.Status != 404 {
		t.Fatalf("B edits A resident: %d", r.Status)
	}
	if r := call(t, "GET", "/api/v1/society/units/"+unitsA["A101"], b.Admin, nil); r.Status != 404 {
		t.Fatalf("B reads A unit: %d", r.Status)
	}
	if r := call(t, "DELETE", "/api/v1/society/units/"+unitsA["A101"], b.Admin, nil); r.Status != 404 {
		t.Fatalf("B deletes A unit: %d", r.Status)
	}
	if r := addResident(t, b, unitsA["A102"], "owner", "Sneaky", nextPhone()); r.Status != 404 || r.errCode() != "UNIT_NOT_FOUND" {
		t.Fatalf("B adds resident into A's flat: %d %v", r.Status, r.Body)
	}
	if rows, _ := callList(t, "/api/v1/society/residents", b.Admin); len(rows) != 0 {
		t.Fatalf("B sees %d residents", len(rows))
	}
	if rows, _ := callList(t, "/api/v1/society/units?page_size=100", b.Admin); len(rows) != 2 {
		t.Fatalf("B units: %d", len(rows))
	}
	if r := call(t, "GET", "/api/v1/society/stats", b.Admin, nil); r.data()["residents"] != float64(0) {
		t.Fatalf("B stats leak: %v", r.Body)
	}
	_ = unitsB

	// residents cannot use admin endpoints
	phone := nextPhone()
	addResident(t, a, unitsA["A102"], "owner", "Plain Resident", phone)
	tok := otpLogin(t, phone).str("access_token")
	for _, path := range []string{"/api/v1/society/residents", "/api/v1/society/blocks", "/api/v1/society/units", "/api/v1/society/stats", "/api/v1/society/apartments"} {
		if r := call(t, "GET", path, tok, nil); r.Status != 403 {
			t.Fatalf("resident GET %s: %d", path, r.Status)
		}
	}
	if r := call(t, "POST", "/api/v1/society/blocks", tok, map[string]any{"name": "X", "code": "X"}); r.Status != 403 {
		t.Fatalf("resident creates block: %d", r.Status)
	}
}

func TestImportCSV(t *testing.T) {
	s := newSoc(t)
	_, units := setupBlock(t, s, "A", 1, 4)
	_ = units
	csv := "full_name,phone,email,unit,role,move_in_date\n" +
		"Asha Verma,9876500001,asha@example.com,A101,owner,2024-04-01\n" + // ok
		"Ravi Iyer,98765 00002,,A102,tenant,\n" + // ok (formatted phone)
		"Bad Phone,12345,,A103,owner,\n" + // invalid phone
		"Ghost Unit,9876500003,,Z999,owner,\n" + // unknown unit
		"Dup Phone,9876500001,,A103,owner,\n" + // phone repeated
		"Bad Role,9876500004,,A103,landlord,\n" + // bad role
		"Second Owner,9876500005,,A101,owner,\n" + // flat already has owner (earlier row)
		",9876500006,,A103,owner,\n" + // missing name
		"Bad Date,9876500007,,A104,owner,01/04/2024\n" // bad date

	dry := upload(t, "/api/v1/society/residents/import?dry_run=true", s.Admin, csv)
	if dry.Status != 200 || dry.data()["total"] != float64(9) || dry.data()["valid"] != float64(2) || dry.data()["created"] != float64(0) ||
		len(dry.data()["errors"].([]any)) != 7 {
		t.Fatalf("dry run: %d %v", dry.Status, dry.Body)
	}
	if rows, _ := callList(t, "/api/v1/society/residents", s.Admin); len(rows) != 0 {
		t.Fatal("dry run wrote data")
	}
	errs := dry.data()["errors"].([]any)
	if first := errs[0].(map[string]any); first["row"] != float64(4) {
		t.Fatalf("row numbering (header=1): %v", first)
	}
	real := upload(t, "/api/v1/society/residents/import", s.Admin, csv)
	if real.Status != 200 || real.data()["created"] != float64(2) {
		t.Fatalf("commit: %d %v", real.Status, real.Body)
	}
	if rows, _ := callList(t, "/api/v1/society/residents", s.Admin); len(rows) != 2 {
		t.Fatalf("imported rows: %d", len(rows))
	}
	if bad := upload(t, "/api/v1/society/residents/import", s.Admin, "name,phone\nx,y\n"); bad.Status != 400 || bad.errCode() != "IMPORT_INVALID" {
		t.Fatalf("missing columns: %d %v", bad.Status, bad.Body)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/api/v1/society/residents/import/template", nil)
	req.Header.Set("Authorization", "Bearer "+s.Admin)
	res, _ := http.DefaultClient.Do(req)
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.HasPrefix(string(body), "full_name,phone,email,unit,role,move_in_date") {
		t.Fatalf("template: %d %s", res.StatusCode, body)
	}
}

func TestResidentSelfService(t *testing.T) {
	s := newSoc(t)
	_, units := setupBlock(t, s, "A", 1, 2)
	phone := nextPhone()
	addResident(t, s, units["A101"], "owner", "Self Service", phone)
	tok := otpLogin(t, phone).str("access_token")

	// household view
	un := call(t, "GET", "/api/v1/me/unit", tok, nil)
	if un.Status != 200 || un.str("number") != "A101" || un.data()["members_count"] != float64(1) {
		t.Fatalf("/me/unit: %d %v", un.Status, un.Body)
	}

	// family: record only, then with login
	f1 := call(t, "POST", "/api/v1/me/family", tok, map[string]any{"name": "Kid One", "relation": "Son", "date_of_birth": "2015-05-05"})
	if f1.Status != 201 || f1.data()["has_login"] != false {
		t.Fatalf("family no phone: %d %v", f1.Status, f1.Body)
	}
	famPhone := nextPhone()
	f2 := call(t, "POST", "/api/v1/me/family", tok, map[string]any{"name": "Spouse Two", "relation": "Spouse", "phone": famPhone})
	if f2.Status != 201 || f2.data()["has_login"] != true {
		t.Fatalf("family with phone: %d %v", f2.Status, f2.Body)
	}
	if d := call(t, "POST", "/api/v1/me/family", tok, map[string]any{"name": "Dup", "relation": "Friend", "phone": famPhone}); d.Status != 409 || d.errCode() != "ALREADY_RESIDENT" {
		t.Fatalf("family dup phone: %d %v", d.Status, d.Body)
	}
	if up := call(t, "PUT", "/api/v1/me/family/"+f1.str("id"), tok, map[string]any{"name": "Kid Uno", "relation": "Son"}); up.Status != 204 {
		t.Fatalf("family update: %d %v", up.Status, up.Body)
	}
	rows, _ := callList(t, "/api/v1/me/family", tok)
	if len(rows) != 2 || rows[0]["name"] != "Kid Uno" {
		t.Fatalf("family list: %v", rows)
	}
	// family member logs in with limited permissions
	famLogin := otpLogin(t, famPhone)
	if famLogin.Status != 200 {
		t.Fatalf("family login: %d %v", famLogin.Status, famLogin.Body)
	}
	famTok := famLogin.str("access_token")
	if me := call(t, "GET", "/api/v1/me", famTok, nil); me.data()["membership"].(map[string]any)["role"] != "family_member" {
		t.Fatalf("family role: %v", me.Body)
	}
	if r := call(t, "GET", "/api/v1/me/family", famTok, nil); r.Status != 403 {
		t.Fatalf("family member manages family: %d", r.Status)
	}
	if r := call(t, "POST", "/api/v1/me/vehicles", famTok, map[string]any{"vehicle_type": "car", "number": "MH01AA0001"}); r.Status != 403 {
		t.Fatalf("family member adds vehicle: %d", r.Status)
	}
	if r := call(t, "GET", "/api/v1/me/unit", famTok, nil); r.Status != 200 || r.data()["members_count"] != float64(3) { // owner + spouse (login) + kid (record only)
		t.Fatalf("family sees household: %d %v", r.Status, r.Body)
	}

	// vehicles
	v := call(t, "POST", "/api/v1/me/vehicles", tok, map[string]any{"vehicle_type": "car", "number": "mh 02-ab 1234", "make_model": "Creta", "color": "White"})
	if v.Status != 201 || v.str("number") != "MH02AB1234" {
		t.Fatalf("vehicle add: %d %v", v.Status, v.Body)
	}
	if d := call(t, "POST", "/api/v1/me/vehicles", tok, map[string]any{"vehicle_type": "bike", "number": "MH02AB1234"}); d.Status != 409 || d.errCode() != "VEHICLE_EXISTS" {
		t.Fatalf("vehicle dup: %d %v", d.Status, d.Body)
	}
	if b := call(t, "POST", "/api/v1/me/vehicles", tok, map[string]any{"vehicle_type": "car", "number": "!!"}); b.Status != 400 {
		t.Fatalf("vehicle invalid: %d %v", b.Status, b.Body)
	}
	if up := call(t, "PUT", "/api/v1/me/vehicles/"+v.str("id"), tok, map[string]any{"vehicle_type": "car", "number": "MH02AB1234", "color": "Black"}); up.Status != 204 {
		t.Fatalf("vehicle update: %d %v", up.Status, up.Body)
	}
	if vs, _ := callList(t, "/api/v1/me/vehicles", tok); len(vs) != 1 || vs[0]["color"] != "Black" {
		t.Fatalf("vehicle list: %v", vs)
	}
	if un := call(t, "GET", "/api/v1/me/unit", tok, nil); un.data()["vehicle_count"] != float64(1) {
		t.Fatalf("vehicle count: %v", un.Body)
	}
	// vehicle numbers are unique per society, not globally
	s2 := newSoc(t)
	_, u2 := setupBlock(t, s2, "A", 1, 1)
	p2 := nextPhone()
	addResident(t, s2, u2["A101"], "owner", "Other Society", p2)
	t2 := otpLogin(t, p2).str("access_token")
	if r := call(t, "POST", "/api/v1/me/vehicles", t2, map[string]any{"vehicle_type": "car", "number": "MH02AB1234"}); r.Status != 201 {
		t.Fatalf("same number other society: %d %v", r.Status, r.Body)
	}
	// ...and one society cannot touch another's rows
	if r := call(t, "DELETE", "/api/v1/me/vehicles/"+v.str("id"), t2, nil); r.Status != 404 {
		t.Fatalf("cross-society vehicle delete: %d", r.Status)
	}

	// profile + emergency contact
	if r := call(t, "PUT", "/api/v1/me/profile", tok, map[string]any{"full_name": "Self Service Renamed", "email": "self@example.com"}); r.Status != 204 {
		t.Fatalf("profile: %d %v", r.Status, r.Body)
	}
	if me := call(t, "GET", "/api/v1/me", tok, nil); me.data()["user"].(map[string]any)["full_name"] != "Self Service Renamed" {
		t.Fatalf("profile not saved: %v", me.Body)
	}
	if r := call(t, "PUT", "/api/v1/me/emergency-contact", tok, map[string]any{"name": "Mum", "phone": "98765 43210", "relation": "Mother"}); r.Status != 204 {
		t.Fatalf("emergency set: %d %v", r.Status, r.Body)
	}
	if r := call(t, "GET", "/api/v1/me/emergency-contact", tok, nil); r.str("name") != "Mum" || r.str("phone") != "9876543210" {
		t.Fatalf("emergency get: %v", r.Body)
	}

	// removing a family member with login kills their session
	if d := call(t, "DELETE", "/api/v1/me/family/"+f2.str("id"), tok, nil); d.Status != 204 {
		t.Fatalf("family delete: %d %v", d.Status, d.Body)
	}
	if rf := call(t, "POST", "/api/v1/auth/refresh", "", map[string]any{"refresh_token": famLogin.str("refresh_token")}); rf.Status != 401 {
		t.Fatalf("family refresh after removal: %d", rf.Status)
	}
	if d := call(t, "DELETE", "/api/v1/me/family/"+uuid.NewString(), tok, nil); d.Status != 404 {
		t.Fatalf("delete unknown family: %d", d.Status)
	}
}

func TestUserSharedRule(t *testing.T) {
	s1, s2 := newSoc(t), newSoc(t)
	_, u1 := setupBlock(t, s1, "A", 1, 1)
	_, u2 := setupBlock(t, s2, "A", 1, 1)
	phone := nextPhone()
	r1 := addResident(t, s1, u1["A101"], "owner", "Shared Person", phone)
	r2 := addResident(t, s2, u2["A101"], "tenant", "Ignored Name", phone) // same person, second society
	if r2.Status != 201 || r2.str("full_name") != "Shared Person" {
		t.Fatalf("second society reuses person: %d %v", r2.Status, r2.Body)
	}
	if d := call(t, "GET", "/api/v1/society/residents/"+r1.str("membership_id"), s1.Admin, nil); d.data()["user_shared"] != true {
		t.Fatalf("user_shared flag: %v", d.Body)
	}
	if r := call(t, "PUT", "/api/v1/society/residents/"+r1.str("membership_id"), s1.Admin, map[string]any{"full_name": "Renamed"}); r.Status != 403 || r.errCode() != "USER_SHARED" {
		t.Fatalf("admin rename shared user: %d %v", r.Status, r.Body)
	}
	// non-identity fields stay editable
	if r := call(t, "PUT", "/api/v1/society/residents/"+r1.str("membership_id"), s1.Admin, map[string]any{"can_pay_maintenance": true}); r.Status != 200 {
		t.Fatalf("flag edit: %d %v", r.Status, r.Body)
	}
	// the person logs in and must choose a society; self-service rename is also blocked
	l := otpLogin(t, phone)
	if l.data()["selection_required"] != true {
		t.Fatalf("expected selection: %v", l.Body)
	}
	mems := l.data()["memberships"].([]any)
	sel := call(t, "POST", "/api/v1/auth/select-society", "", map[string]any{"selection_token": l.str("selection_token"), "membership_id": mems[0].(map[string]any)["id"]})
	if r := call(t, "PUT", "/api/v1/me/profile", sel.str("access_token"), map[string]any{"full_name": "Self Rename"}); r.Status != 403 || r.errCode() != "USER_SHARED" {
		t.Fatalf("self rename shared: %d %v", r.Status, r.Body)
	}
}
