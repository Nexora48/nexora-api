package app_test

import (
	"context"
	"testing"
)

// applicant signs in with the resident app for a number nobody registered, and gets an applicant token.
func applicantLogin(t *testing.T, phone string) resp {
	t.Helper()
	mr.FlushAll()
	if r := call(t, "POST", "/api/v1/auth/otp/request", "", map[string]any{"phone": phone, "app": "living"}); r.Status != 204 {
		t.Fatalf("otp request: %d %v", r.Status, r.Body)
	}
	return call(t, "POST", "/api/v1/auth/otp/verify", "", map[string]any{"phone": phone, "otp": "123456", "device": map[string]any{"device_id": "d-" + phone, "app": "living"}})
}

func joinCode(t *testing.T, s soc) string {
	t.Helper()
	r := call(t, "GET", "/api/v1/society/join-settings", s.Admin, nil)
	if r.Status != 200 || r.str("join_code") == "" {
		t.Fatalf("join settings: %d %v", r.Status, r.Body)
	}
	return r.str("join_code")
}

func TestUnknownNumberOnlyGetsAnApplicantSessionInTheResidentApp(t *testing.T) {
	phone := nextPhone()
	mr.FlushAll()
	// other apps keep the old behaviour
	if r := call(t, "POST", "/api/v1/auth/otp/request", "", map[string]any{"phone": phone}); r.Status != 404 || r.errCode() != "PHONE_NOT_REGISTERED" {
		t.Fatalf("no app: %d %v", r.Status, r.Body)
	}
	if r := call(t, "POST", "/api/v1/auth/otp/request", "", map[string]any{"phone": phone, "app": "get"}); r.Status != 404 {
		t.Fatalf("guard app: %d", r.Status)
	}
	l := applicantLogin(t, phone)
	if l.Status != 200 || l.data()["applicant_required"] != true || l.str("applicant_token") == "" || l.str("access_token") != "" {
		t.Fatalf("applicant: %d %v", l.Status, l.Body)
	}
	// the guard app cannot use the same number to become an applicant
	mr.FlushAll()
	call(t, "POST", "/api/v1/auth/otp/request", "", map[string]any{"phone": nextPhone(), "app": "living"})
	// an applicant token opens nothing but /join
	if r := call(t, "GET", "/api/v1/me", l.str("applicant_token"), nil); r.Status != 401 {
		t.Fatalf("applicant token on /me: %d", r.Status)
	}
	if r := call(t, "GET", "/api/v1/society/stats", l.str("applicant_token"), nil); r.Status != 401 {
		t.Fatalf("applicant token on the office API: %d", r.Status)
	}
	// and a normal access token is not an applicant token
	s := newSoc(t)
	if r := call(t, "GET", "/api/v1/join/status", s.Admin, nil); r.Status != 401 {
		t.Fatalf("access token on /join: %d", r.Status)
	}
	if r := call(t, "GET", "/api/v1/join/status", "", nil); r.Status != 401 {
		t.Fatalf("no token on /join: %d", r.Status)
	}
}

func TestSelfRegistrationNeedsApprovalThenLogsIn(t *testing.T) {
	s := newSoc(t)
	_, units := setupBlock(t, s, "A", 1, 2)
	code := joinCode(t, s)
	phone := nextPhone()
	tok := applicantLogin(t, phone).str("applicant_token")

	// a wrong code, then the right one (lower case works)
	if r := call(t, "GET", "/api/v1/join/society?code=NOPE0000", tok, nil); r.Status != 404 || r.errCode() != "JOIN_CODE_UNKNOWN" {
		t.Fatalf("wrong code: %d %v", r.Status, r.Body)
	}
	soc := call(t, "GET", "/api/v1/join/society?code="+lower(code), tok, nil)
	if soc.Status != 200 || len(soc.data()["blocks"].([]any)) != 1 {
		t.Fatalf("society: %d %v", soc.Status, soc.Body)
	}
	if st := call(t, "GET", "/api/v1/join/status", tok, nil); st.str("state") != "none" || st.data()["member"] != false {
		t.Fatalf("before asking: %v", st.Body)
	}

	req := map[string]any{"code": code, "unit_id": units["A101"], "type": "owner", "full_name": "Neha Kapoor"}
	for _, bad := range []map[string]any{
		{"code": code, "unit_id": units["A101"], "type": "family", "full_name": "Neha"},
		{"code": code, "unit_id": units["A101"], "type": "owner", "full_name": "N"},
		{"code": code, "unit_id": "not-a-uuid", "type": "owner", "full_name": "Neha"},
		{"code": code, "unit_id": newSoc(t).ID.String(), "type": "owner", "full_name": "Neha"}, // a flat of nowhere
	} {
		if r := call(t, "POST", "/api/v1/join/requests", tok, bad); r.Status != 400 && r.Status != 404 {
			t.Fatalf("bad request %v: %d", bad, r.Status)
		}
	}
	r := call(t, "POST", "/api/v1/join/requests", tok, req)
	if r.Status != 201 || r.str("state") != "pending" || r.str("flat") != "A-A101" {
		t.Fatalf("request: %d %v", r.Status, r.Body)
	}
	if r := call(t, "POST", "/api/v1/join/requests", tok, req); r.Status != 409 || r.errCode() != "REQUEST_PENDING" {
		t.Fatalf("duplicate: %d %v", r.Status, r.Body)
	}
	// until the office approves there is no session and no resident
	if c := call(t, "POST", "/api/v1/join/claim", tok, map[string]any{}); c.Status != 409 || c.errCode() != "NOT_APPROVED" {
		t.Fatalf("claim before approval: %d %v", c.Status, c.Body)
	}
	var members int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM memberships m JOIN users u ON u.id=m.user_id WHERE u.phone=$1`, phone).Scan(&members)
	if members != 0 {
		t.Fatalf("a resident exists before approval: %d", members)
	}

	// the office sees it (dashboard + list) and the admin got a notification
	d := call(t, "GET", "/api/v1/society/dashboard", s.Admin, nil)
	jr := d.obj("join_requests")
	if num(jr, "pending") != 1 || len(jr["recent"].([]any)) != 1 {
		t.Fatalf("dashboard: %v", jr)
	}
	rows, lr := callList(t, "/api/v1/society/join-requests?status=pending", s.Admin)
	if lr.Status != 200 || len(rows) != 1 || rows[0]["full_name"] != "Neha Kapoor" || rows[0]["phone"] != phone {
		t.Fatalf("list: %d %v", lr.Status, rows)
	}
	if n := inbox(t, s.Admin, ""); len(n) == 0 || n[0]["type"] != "join.request" {
		t.Fatalf("admin notification: %v", n)
	}
	id := rows[0]["id"].(string)

	// residents and guards cannot decide
	if r := call(t, "POST", "/api/v1/society/join-requests/"+id+"/approve", newResidentTok(t, s, units["A102"]), nil); r.Status != 403 {
		t.Fatalf("resident approves: %d", r.Status)
	}
	ap := call(t, "POST", "/api/v1/society/join-requests/"+id+"/approve", s.Admin, nil)
	if ap.Status != 200 || ap.str("status") != "approved" || ap.str("decided_by") == "" {
		t.Fatalf("approve: %d %v", ap.Status, ap.Body)
	}
	if r := call(t, "POST", "/api/v1/society/join-requests/"+id+"/approve", s.Admin, nil); r.Status != 409 || r.errCode() != "NOT_PENDING" {
		t.Fatalf("approve twice: %d %v", r.Status, r.Body)
	}
	if call(t, "GET", "/api/v1/society/dashboard", s.Admin, nil).obj("join_requests")["pending"].(float64) != 0 {
		t.Fatal("the dashboard still counts an approved request")
	}

	// the waiting screen sees it, claims a session, and lands in the society with the flat
	st := call(t, "GET", "/api/v1/join/status", tok, nil)
	if st.str("state") != "approved" || st.data()["member"] != true {
		t.Fatalf("status after approval: %v", st.Body)
	}
	cl := call(t, "POST", "/api/v1/join/claim", tok, map[string]any{"device": map[string]any{"device_id": "dev-1", "app": "living"}})
	if cl.Status != 200 || cl.str("access_token") == "" {
		t.Fatalf("claim: %d %v", cl.Status, cl.Body)
	}
	me := call(t, "GET", "/api/v1/me", cl.str("access_token"), nil)
	if me.Status != 200 || me.obj("membership")["role"] != "resident_owner" || me.obj("user")["full_name"] != "Neha Kapoor" {
		t.Fatalf("me: %d %v", me.Status, me.Body)
	}
	// from now on a normal sign-in goes straight to the society, no applicant step
	if l := otpLogin(t, phone); l.Status != 200 || l.str("access_token") == "" {
		t.Fatalf("later sign-in: %d %v", l.Status, l.Body)
	}
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

func TestRejectionShowsTheReasonAndAllowsANewRequest(t *testing.T) {
	s := newSoc(t)
	_, units := setupBlock(t, s, "A", 1, 2)
	code := joinCode(t, s)
	phone := nextPhone()
	tok := applicantLogin(t, phone).str("applicant_token")
	call(t, "POST", "/api/v1/join/requests", tok, map[string]any{"code": code, "unit_id": units["A101"], "type": "tenant", "full_name": "Wrong Flat"})
	rows, _ := callList(t, "/api/v1/society/join-requests?status=pending", s.Admin)
	id := rows[0]["id"].(string)
	rj := call(t, "POST", "/api/v1/society/join-requests/"+id+"/reject", s.Admin, map[string]any{"reason": "Flat A101 is not rented out"})
	if rj.Status != 200 || rj.str("status") != "rejected" {
		t.Fatalf("reject: %d %v", rj.Status, rj.Body)
	}
	st := call(t, "GET", "/api/v1/join/status", tok, nil)
	if st.str("state") != "rejected" || st.str("reason") != "Flat A101 is not rented out" || st.data()["member"] != false {
		t.Fatalf("status: %v", st.Body)
	}
	if c := call(t, "POST", "/api/v1/join/claim", tok, map[string]any{}); c.Status != 409 {
		t.Fatalf("claim after rejection: %d", c.Status)
	}
	// a rejected person can ask again for the right flat
	if r := call(t, "POST", "/api/v1/join/requests", tok, map[string]any{"code": code, "unit_id": units["A102"], "type": "tenant", "full_name": "Right Flat"}); r.Status != 201 {
		t.Fatalf("second request: %d %v", r.Status, r.Body)
	}
	if st := call(t, "GET", "/api/v1/join/status", tok, nil); st.str("state") != "pending" || st.str("flat") != "A-A102" {
		t.Fatalf("latest request wins: %v", st.Body)
	}
	// and can withdraw it
	if r := call(t, "POST", "/api/v1/join/cancel", tok, nil); r.Status != 204 {
		t.Fatalf("cancel: %d", r.Status)
	}
	if st := call(t, "GET", "/api/v1/join/status", tok, nil); st.str("state") != "rejected" {
		t.Fatalf("after cancel the earlier answer shows: %v", st.Body)
	}
}

func TestApprovalKeepsTheFlatRulesAndTheOfficeSwitches(t *testing.T) {
	s := newSoc(t)
	_, units := setupBlock(t, s, "A", 1, 1)
	code := joinCode(t, s)
	tok := applicantLogin(t, nextPhone()).str("applicant_token")
	// the flat is free: the request is filed, then the office gives the flat an owner before approving
	call(t, "POST", "/api/v1/join/requests", tok, map[string]any{"code": code, "unit_id": units["A101"], "type": "owner", "full_name": "Second Owner"})
	addResident(t, s, units["A101"], "owner", "Existing Owner", nextPhone())
	rows, _ := callList(t, "/api/v1/society/join-requests?status=pending", s.Admin)
	id := rows[0]["id"].(string)
	if r := call(t, "POST", "/api/v1/society/join-requests/"+id+"/approve", s.Admin, nil); r.Status != 409 || r.errCode() != "UNIT_ROLE_TAKEN" {
		t.Fatalf("second owner: %d %v", r.Status, r.Body)
	}
	if st := call(t, "GET", "/api/v1/join/status", tok, nil); st.str("state") != "pending" {
		t.Fatalf("the request must stay open: %v", st.Body)
	}

	// the office can switch self-registration off and rotate the code
	if r := call(t, "PUT", "/api/v1/society/join-settings", s.Admin, map[string]any{"enabled": false}); r.Status != 200 || r.data()["enabled"] != false {
		t.Fatalf("switch off: %d %v", r.Status, r.Body)
	}
	other := applicantLogin(t, nextPhone()).str("applicant_token")
	if r := call(t, "GET", "/api/v1/join/society?code="+code, other, nil); r.Status != 403 || r.errCode() != "JOIN_DISABLED" {
		t.Fatalf("disabled lookup: %d %v", r.Status, r.Body)
	}
	if r := call(t, "POST", "/api/v1/join/requests", other, map[string]any{"code": code, "unit_id": units["A101"], "type": "tenant", "full_name": "Late Comer"}); r.Status != 403 {
		t.Fatalf("disabled request: %d", r.Status)
	}
	call(t, "PUT", "/api/v1/society/join-settings", s.Admin, map[string]any{"enabled": true})
	rg := call(t, "PUT", "/api/v1/society/join-settings", s.Admin, map[string]any{"regenerate": true})
	if rg.Status != 200 || rg.str("join_code") == code {
		t.Fatalf("regenerate: %d %v", rg.Status, rg.Body)
	}
	if r := call(t, "GET", "/api/v1/join/society?code="+code, other, nil); r.Status != 404 {
		t.Fatalf("the old code must stop working: %d", r.Status)
	}
	// a person who is already a resident cannot ask again
	ph := nextPhone()
	addResident(t, s, units["A101"], "tenant", "Already Here", ph)
	mem := applicantLogin(t, ph) // signs straight in (they are a member), no applicant token
	if mem.str("access_token") == "" || mem.data()["applicant_required"] == true {
		t.Fatalf("a resident must not become an applicant: %v", mem.Body)
	}
}

func TestSelfRegisteredRequestsAreIsolatedBetweenSocieties(t *testing.T) {
	a, b := newSoc(t), newSoc(t)
	_, ua := setupBlock(t, a, "A", 1, 1)
	_, _ = setupBlock(t, b, "B", 1, 1)
	tok := applicantLogin(t, nextPhone()).str("applicant_token")
	call(t, "POST", "/api/v1/join/requests", tok, map[string]any{"code": joinCode(t, a), "unit_id": ua["A101"], "type": "owner", "full_name": "Only For A"})
	rowsB, _ := callList(t, "/api/v1/society/join-requests?status=all", b.Admin)
	if len(rowsB) != 0 {
		t.Fatalf("society B sees A's request: %v", rowsB)
	}
	rowsA, _ := callList(t, "/api/v1/society/join-requests?status=all", a.Admin)
	id := rowsA[0]["id"].(string)
	if r := call(t, "POST", "/api/v1/society/join-requests/"+id+"/approve", b.Admin, nil); r.Status != 404 {
		t.Fatalf("B approves A's request: %d", r.Status)
	}
	if r := call(t, "POST", "/api/v1/society/join-requests/"+id+"/reject", b.Admin, map[string]any{}); r.Status != 404 {
		t.Fatalf("B rejects A's request: %d", r.Status)
	}
	// a flat of society A cannot be requested with society B's code
	if r := call(t, "POST", "/api/v1/join/requests", tok, map[string]any{"code": joinCode(t, b), "unit_id": ua["A101"], "type": "owner", "full_name": "Crossed"}); r.Status != 404 {
		t.Fatalf("crossed flat: %d", r.Status)
	}
}

func TestApplicantPicksASocietyFromTheListWithoutACode(t *testing.T) {
	s := newSoc(t)
	_, units := setupBlock(t, s, "A", 1, 2)
	tok := applicantLogin(t, nextPhone()).str("applicant_token")
	id := s.ID.String()
	listed := func() bool {
		items, r := callList(t, "/api/v1/join/societies", tok)
		if r.Status != 200 {
			t.Fatalf("list: %d %v", r.Status, r.Body)
		}
		for _, x := range items {
			if x["id"] == id {
				return true
			}
		}
		return false
	}
	if !listed() {
		t.Fatal("a society that accepts sign-ups is in the picker")
	}
	if r := call(t, "GET", "/api/v1/join/society?id="+id, tok, nil); r.Status != 200 || len(r.data()["blocks"].([]any)) != 1 {
		t.Fatalf("lookup by id: %d %v", r.Status, r.Body)
	}
	if r := call(t, "POST", "/api/v1/join/requests", tok, map[string]any{"society_id": id, "unit_id": units["A101"], "type": "tenant", "full_name": "Pick Er"}); r.Status != 201 {
		t.Fatalf("request by id: %d %v", r.Status, r.Body)
	}
	if r := call(t, "POST", "/api/v1/join/requests", tok, map[string]any{"unit_id": units["A102"], "type": "tenant", "full_name": "Pick Er"}); r.Status == 201 {
		t.Fatal("a request with neither society nor code must fail")
	}
	// switched off: gone from the picker and cannot be looked up
	call(t, "PUT", "/api/v1/society/join-settings", s.Admin, map[string]any{"enabled": false})
	if listed() {
		t.Fatal("a society with sign-ups off must not be listed")
	}
	if r := call(t, "GET", "/api/v1/join/society?id="+id, tok, nil); r.Status == 200 {
		t.Fatalf("lookup with sign-ups off: %d", r.Status)
	}
}

func TestTakenFlatsAreMarkedAndCannotBeRequested(t *testing.T) {
	s := newSoc(t)
	_, units := setupBlock(t, s, "A", 1, 2)
	code := joinCode(t, s)
	addResident(t, s, units["A101"], "owner", "Existing Owner", nextPhone())
	tok := applicantLogin(t, nextPhone()).str("applicant_token")

	soc := call(t, "GET", "/api/v1/join/society?code="+code, tok, nil)
	flags := map[string][2]bool{}
	for _, u := range soc.data()["blocks"].([]any)[0].(map[string]any)["units"].([]any) {
		m := u.(map[string]any)
		flags[m["number"].(string)] = [2]bool{m["owner_taken"] == true, m["tenant_taken"] == true}
	}
	if flags["A101"] != [2]bool{true, false} || flags["A102"] != [2]bool{false, false} {
		t.Fatalf("flags: %v", flags)
	}
	// an owned flat still takes a tenant, but not a second owner
	if r := call(t, "POST", "/api/v1/join/requests", tok, map[string]any{"code": code, "unit_id": units["A101"], "type": "owner", "full_name": "Late Owner"}); r.Status != 409 || r.errCode() != "FLAT_TAKEN" {
		t.Fatalf("second owner: %d %v", r.Status, r.Body)
	}
	if r := call(t, "POST", "/api/v1/join/requests", tok, map[string]any{"code": code, "unit_id": units["A101"], "type": "tenant", "full_name": "A Tenant"}); r.Status != 201 {
		t.Fatalf("tenant of an owned flat: %d %v", r.Status, r.Body)
	}
}
