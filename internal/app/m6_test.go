package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ── helpers ──────────────────────────────────────────────────────────────────

func inbox(t *testing.T, tok string, extra string) []map[string]any {
	t.Helper()
	rows, r := callList(t, "/api/v1/me/notifications"+extra, tok)
	if r.Status != 200 {
		t.Fatalf("inbox: %d %v", r.Status, r.Body)
	}
	return rows
}

func unread(t *testing.T, tok string) float64 {
	t.Helper()
	r := call(t, "GET", "/api/v1/me/notifications/unread-count", tok, nil)
	n, _ := r.data()["unread"].(float64)
	return n
}

func raise(t *testing.T, tok string, body map[string]any) resp {
	t.Helper()
	if body["kind"] == nil {
		body["kind"] = "medical"
	}
	return call(t, "POST", "/api/v1/sos", tok, body)
}

func setStatus(t *testing.T, id, status string) {
	t.Helper()
	mustExec(t, context.Background(), `UPDATE sos_events SET status=$2 WHERE id=$1`, id, status)
}

// ── notifications ────────────────────────────────────────────────────────────

func TestNotificationInboxAndPrefs(t *testing.T) {
	s := newSoc(t)
	_, units := setupBlock(t, s, "A", 1, 2)
	r1, phone1 := newResident(t, s, units["A101"], "owner")
	r2, _ := newResident(t, s, units["A102"], "owner")
	g := newGuard(t, s)
	call(t, "PUT", "/api/v1/me/devices", r1, map[string]any{"device_id": "d1", "platform": "android", "fcm_token": "tok-" + phone1})
	w1 := dialWS(t, r1)

	w := walkIn(t, g, units["A101"], "Inbox Visitor")
	id := w.str("id")
	if n := w1.note(2 * time.Second); n == nil || n["type"] != "visit.request" {
		t.Fatalf("live notification: %v", n)
	}
	items := inbox(t, r1, "")
	if len(items) != 1 || items[0]["type"] != "visit.request" || items[0]["read_at"] != nil || items[0]["title"] != "Visitor at the gate" {
		t.Fatalf("inbox: %v", items)
	}
	if strings.Contains(fmt.Sprint(items[0]["body"]), "Inbox Visitor") {
		t.Fatal("notification text must not carry visitor details")
	}
	if unread(t, r1) != 1 {
		t.Fatalf("unread: %v", unread(t, r1))
	}
	// the neighbour hears nothing and cannot read it
	if len(inbox(t, r2, "")) != 0 || unread(t, r2) != 0 {
		t.Fatal("neighbour got the notification")
	}
	nid := items[0]["id"].(string)
	if r := call(t, "POST", "/api/v1/me/notifications/"+nid+"/read", r2, nil); r.Status != 404 {
		t.Fatalf("neighbour marks it read: %d", r.Status)
	}
	if r := call(t, "POST", "/api/v1/me/notifications/"+nid+"/read", newSoc(t).Admin, nil); r.Status != 404 {
		t.Fatalf("other society marks it read: %d", r.Status)
	}
	if r := call(t, "POST", "/api/v1/me/notifications/"+nid+"/read", r1, nil); r.Status != 204 || unread(t, r1) != 0 {
		t.Fatalf("mark read: %d", r.Status)
	}
	if len(inbox(t, r1, "?unread=1")) != 0 || len(inbox(t, r1, "")) != 1 {
		t.Fatal("unread filter")
	}

	// the decision lands silently in the flat's inbox (no push)
	before := len(pushRec.to("tok-" + phone1))
	call(t, "POST", "/api/v1/visitors/"+id+"/decision", r1, map[string]any{"decision": "approve"})
	if len(pushRec.to("tok-"+phone1)) != before {
		t.Fatal("a silent notification must not push")
	}
	if items := inbox(t, r1, ""); len(items) != 2 || items[0]["priority"] != "silent" {
		t.Fatalf("decided in inbox: %v", items)
	}
	if r := call(t, "POST", "/api/v1/me/notifications/read-all", r1, nil); r.Status != 200 || unread(t, r1) != 0 {
		t.Fatalf("read-all: %d", r.Status)
	}

	// preferences: visits push can be muted, the notification is still stored
	prefs := call(t, "GET", "/api/v1/me/notification-prefs", r1, nil)
	list, _ := prefs.Body["data"].([]any)
	if prefs.Status != 200 || len(list) != 7 || list[6].(map[string]any)["group"] != "sos" || list[6].(map[string]any)["locked"] != true {
		t.Fatalf("prefs: %d %v", prefs.Status, prefs.Body)
	}
	if r := call(t, "PUT", "/api/v1/me/notification-prefs", r1, map[string]any{"group": "visits", "push": false, "email": false}); r.Status != 200 {
		t.Fatalf("mute: %d %v", r.Status, r.Body)
	}
	before = len(pushRec.to("tok-" + phone1))
	walkIn(t, g, units["A101"], "While Muted")
	if len(pushRec.to("tok-"+phone1)) != before {
		t.Fatal("muted group still pushed")
	}
	if len(inbox(t, r1, "?unread=1")) == 0 {
		t.Fatal("muted notifications must still be stored")
	}
	if r := call(t, "PUT", "/api/v1/me/notification-prefs", r1, map[string]any{"group": "sos", "push": false}); r.Status != 400 {
		t.Fatalf("muting SOS: %d", r.Status)
	}
	if r := call(t, "PUT", "/api/v1/me/notification-prefs", r1, map[string]any{"group": "weather", "push": false}); r.Status != 400 {
		t.Fatalf("unknown group: %d", r.Status)
	}
}

func TestOutboxRetriesAndRetention(t *testing.T) {
	s := newSoc(t)
	_, units := setupBlock(t, s, "A", 1, 1)
	r1, phone := newResident(t, s, units["A101"], "owner")
	g := newGuard(t, s)
	tok := "flaky-" + phone
	call(t, "PUT", "/api/v1/me/devices", r1, map[string]any{"device_id": "d1", "platform": "android", "fcm_token": tok})

	pushRec.mu.Lock()
	pushRec.failing = true
	pushRec.mu.Unlock()
	walkIn(t, g, units["A101"], "Provider Down")
	var attempts int
	var sent *time.Time
	q := `SELECT attempts, sent_at FROM notification_outbox WHERE target=$1`
	if err := pool.QueryRow(context.Background(), q, tok).Scan(&attempts, &sent); err != nil || attempts != 1 || sent != nil {
		t.Fatalf("failed send must stay queued: %d %v %v", attempts, sent, err)
	}
	// the provider recovers; the worker's next pass delivers it exactly once
	pushRec.mu.Lock()
	pushRec.failing = false
	pushRec.mu.Unlock()
	mustExec(t, context.Background(), `UPDATE notification_outbox SET next_at=now() WHERE target=$1`, tok)
	before := len(pushRec.to(tok))
	anote.Retry(context.Background())
	if err := pool.QueryRow(context.Background(), q, tok).Scan(&attempts, &sent); err != nil || sent == nil || attempts != 2 {
		t.Fatalf("retry: %d %v %v", attempts, sent, err)
	}
	if n := len(pushRec.to(tok)); n != before+1 {
		t.Fatalf("pushes after retry: %d", n)
	}
	anote.Retry(context.Background())
	if n := len(pushRec.to(tok)); n != before+1 {
		t.Fatal("a delivered row must not be sent again")
	}

	// old notifications are removed by the cleanup
	mustExec(t, context.Background(), `UPDATE notifications SET created_at = now() - interval '100 days' WHERE user_id=(SELECT id FROM users WHERE phone=$1)`, phone)
	anote.Cleanup(context.Background())
	if len(inbox(t, r1, "")) != 0 {
		t.Fatal("notifications older than the retention must be dropped")
	}
}

// ── SOS ──────────────────────────────────────────────────────────────────────

type sosWorld struct {
	s        soc
	units    map[string]string
	r1, r2   string // owner of A101, owner of A102
	fam      string // family member of A101
	phone1   string
	guard    guardUser
	guardTok string
	other    guardUser // guard of another society
	wg, wa   *wsClient
	wr1, wf  *wsClient
	wr2, wo  *wsClient
}

func newSOSWorld(t *testing.T) sosWorld {
	t.Helper()
	w := sosWorld{s: newSoc(t)}
	_, w.units = setupBlock(t, w.s, "A", 1, 2)
	w.r1, w.phone1 = newResident(t, w.s, w.units["A101"], "owner")
	w.r2, _ = newResident(t, w.s, w.units["A102"], "owner")
	w.fam, _ = newResident(t, w.s, w.units["A101"], "family")
	w.guard = newGuard(t, w.s)
	w.other = newGuard(t, newSoc(t))
	w.wg, w.wa = dialWS(t, w.guard.Token), dialWS(t, w.s.Admin)
	w.wr1, w.wf = dialWS(t, w.r1), dialWS(t, w.fam)
	w.wr2, w.wo = dialWS(t, w.r2), dialWS(t, w.other.Token)
	return w
}

func TestSOSFlow(t *testing.T) {
	w := newSOSWorld(t)
	call(t, "PUT", "/api/v1/me/devices", w.guard.Token, map[string]any{"device_id": "gd", "platform": "android", "fcm_token": "guard-tok-" + w.phone1})

	if r := raise(t, w.r1, map[string]any{"kind": "earthquake"}); r.Status != 400 {
		t.Fatalf("bad kind: %d", r.Status)
	}
	if r := raise(t, w.s.Admin, map[string]any{}); r.Status != 409 || r.errCode() != "NO_UNIT" {
		t.Fatalf("admin without a flat raises: %d %v", r.Status, r.Body)
	}
	r := raise(t, w.r1, map[string]any{"kind": "medical", "note": "Grandfather fell"})
	if r.Status != 201 || r.vstatus() != "raised" || r.str("flat") != "A-A101" || r.data()["can_cancel_until"] == nil {
		t.Fatalf("raise: %d %v", r.Status, r.Body)
	}
	id := r.str("id")

	// everyone who must know, and nobody else, hears it at once
	for name, c := range map[string]*wsClient{"guard": w.wg, "admin": w.wa, "resident": w.wr1, "family": w.wf} {
		d := c.expect(t, "sos.raised")
		if d["sos_id"] != id || d["kind"] != "medical" || d["flat"] != "A-A101" {
			t.Fatalf("%s payload: %v", name, d)
		}
	}
	w.wr2.expectNothing(t)
	w.wo.expectNothing(t)

	// critical notifications: responders get the alarm, the flat gets the confirmation; both are stored
	gi := inbox(t, w.guard.Token, "")
	if len(gi) != 1 || gi[0]["type"] != "sos.raised" || gi[0]["priority"] != "critical" || !strings.Contains(gi[0]["body"].(string), "A-A101") {
		t.Fatalf("guard inbox: %v", gi)
	}
	if ai := inbox(t, w.s.Admin, ""); len(ai) != 1 || ai[0]["priority"] != "critical" {
		t.Fatalf("admin inbox: %v", ai)
	}
	if fi := inbox(t, w.fam, ""); len(fi) != 1 || fi[0]["title"] != "SOS sent" {
		t.Fatalf("family inbox: %v", fi)
	}
	if len(inbox(t, w.r2, "")) != 0 {
		t.Fatal("neighbour was notified")
	}
	p := pushRec.to("guard-tok-" + w.phone1)
	if len(p) != 1 || p[0].Msg.Channel != "sos_alerts" || p[0].Msg.Data["sos_id"] != id {
		t.Fatalf("guard push: %+v", p)
	}

	// a second alert from the same flat joins the open one
	m := raise(t, w.fam, map[string]any{"kind": "fire"})
	if m.Status != 200 || m.str("id") != id || m.data()["merged"] != true {
		t.Fatalf("merge: %d %v", m.Status, m.Body)
	}
	w.wg.expectNothing(t)

	// responders see the callback number; residents cannot list alerts; other societies see nothing
	act, ar := callList(t, "/api/v1/sos/active", w.guard.Token)
	if ar.Status != 200 || len(act) != 1 || act[0]["callback_phone"] != w.phone1 || act[0]["raised_by"] == "" {
		t.Fatalf("active: %d %v", ar.Status, act)
	}
	if r := call(t, "GET", "/api/v1/sos/active", w.r2, nil); r.Status != 403 {
		t.Fatalf("resident lists alerts: %d", r.Status)
	}
	if rows, _ := callList(t, "/api/v1/sos/active", w.other.Token); len(rows) != 0 {
		t.Fatalf("other society sees alerts: %v", rows)
	}
	// the phone never travels over the socket or in the inbox
	raw, _ := json.Marshal(inbox(t, w.guard.Token, ""))
	if strings.Contains(string(raw), w.phone1) {
		t.Fatal("phone number leaked into the inbox")
	}

	// first acknowledgement wins; others learn who is responding
	if r := call(t, "POST", "/api/v1/sos/"+id+"/acknowledge", w.r2, nil); r.Status != 403 {
		t.Fatalf("resident acknowledges: %d", r.Status)
	}
	if r := call(t, "POST", "/api/v1/sos/"+id+"/acknowledge", w.other.Token, nil); r.Status != 404 {
		t.Fatalf("other society acknowledges: %d", r.Status)
	}
	ack := call(t, "POST", "/api/v1/sos/"+id+"/acknowledge", w.guard.Token, nil)
	if ack.Status != 200 || ack.vstatus() != "acknowledged" || ack.data()["acknowledged_by"] == nil {
		t.Fatalf("acknowledge: %d %v", ack.Status, ack.Body)
	}
	if d := w.wr1.expect(t, "sos.acknowledged"); d["status"] != "acknowledged" {
		t.Fatalf("resident told: %v", d)
	}
	w.wa.expect(t, "sos.acknowledged")
	if r := call(t, "POST", "/api/v1/sos/"+id+"/acknowledge", w.s.Admin, nil); r.Status != 409 || r.errCode() != "ALREADY_ACKNOWLEDGED" {
		t.Fatalf("second acknowledge: %d %v", r.Status, r.Body)
	}
	if fi := inbox(t, w.fam, ""); fi[0]["title"] != "Help is on the way" {
		t.Fatalf("family told: %v", fi)
	}

	// resolve closes it for everyone and removes the phone
	res := call(t, "POST", "/api/v1/sos/"+id+"/resolve", w.s.Admin, map[string]any{"outcome": "Ambulance called, patient stable"})
	if res.Status != 200 || res.vstatus() != "resolved" || res.str("resolution_note") != "Ambulance called, patient stable" || res.data()["callback_phone"] != nil {
		t.Fatalf("resolve: %d %v", res.Status, res.Body)
	}
	w.wg.expect(t, "sos.acknowledged")
	w.wg.expect(t, "sos.resolved")
	if rows, _ := callList(t, "/api/v1/sos/active", w.guard.Token); len(rows) != 0 {
		t.Fatalf("active after resolve: %v", rows)
	}
	if r := call(t, "POST", "/api/v1/sos/"+id+"/resolve", w.guard.Token, nil); r.Status != 409 {
		t.Fatalf("resolve twice: %d", r.Status)
	}
	if mine, r := callList(t, "/api/v1/me/sos", w.r1); r.Status != 200 || len(mine) != 1 || mine[0]["status"] != "resolved" {
		t.Fatalf("my history: %d %v", r.Status, mine)
	}
	if mine, _ := callList(t, "/api/v1/me/sos", w.r2); len(mine) != 0 {
		t.Fatalf("neighbour's history: %v", mine)
	}
}

func TestSOSCancelAndRateLimit(t *testing.T) {
	w := newSOSWorld(t)
	id := raise(t, w.r1, map[string]any{}).str("id")
	if r := call(t, "POST", "/api/v1/sos/"+id+"/cancel", w.r2, nil); r.Status != 404 {
		t.Fatalf("neighbour cancels: %d", r.Status)
	}
	if r := call(t, "POST", "/api/v1/sos/"+id+"/cancel", w.guard.Token, nil); r.Status != 403 {
		t.Fatalf("guard cancels (no sos.trigger): %d", r.Status)
	}
	w.wg.expect(t, "sos.raised")
	if r := call(t, "POST", "/api/v1/sos/"+id+"/cancel", w.r1, nil); r.Status != 200 || r.vstatus() != "cancelled" {
		t.Fatalf("cancel: %d %v", r.Status, r.Body)
	}
	w.wg.expect(t, "sos.cancelled")
	if r := call(t, "POST", "/api/v1/sos/"+id+"/cancel", w.r1, nil); r.Status != 409 {
		t.Fatalf("cancel twice: %d", r.Status)
	}
	// after the window it is a real emergency that only responders can close
	id2 := raise(t, w.r1, map[string]any{}).str("id")
	mustExec(t, context.Background(), `UPDATE sos_events SET raised_at = now() - interval '3 minutes' WHERE id=$1`, id2)
	if r := call(t, "POST", "/api/v1/sos/"+id2+"/cancel", w.r1, nil); r.Status != 409 || r.errCode() != "CANCEL_WINDOW_OVER" {
		t.Fatalf("late cancel: %d %v", r.Status, r.Body)
	}

	// three alerts an hour: the fourth is refused (joined alerts do not count)
	r3, _ := newResident(t, w.s, w.units["A102"], "tenant")
	for i := 0; i < 3; i++ {
		x := raise(t, r3, map[string]any{})
		if x.Status != 201 {
			t.Fatalf("alert %d: %d %v", i, x.Status, x.Body)
		}
		setStatus(t, x.str("id"), "resolved")
	}
	if x := raise(t, r3, map[string]any{}); x.Status != 429 || x.errCode() != "SOS_RATE_LIMIT" {
		t.Fatalf("rate limit: %d %v", x.Status, x.Body)
	}
}

func TestSOSTestAlertOnlyReachesRaiserAndOffice(t *testing.T) {
	w := newSOSWorld(t)
	r := raise(t, w.r1, map[string]any{"test": true})
	if r.Status != 201 || r.data()["is_test"] != true {
		t.Fatalf("test alert: %d %v", r.Status, r.Body)
	}
	id := r.str("id")
	w.wr1.expect(t, "sos.raised")
	w.wa.expect(t, "sos.raised")
	w.wg.expectNothing(t) // no guard is alarmed
	w.wf.expectNothing(t)
	w.wr2.expectNothing(t)
	if len(inbox(t, w.guard.Token, "")) != 0 {
		t.Fatal("guard notified about a test")
	}
	if rows, _ := callList(t, "/api/v1/sos/active", w.guard.Token); len(rows) != 0 {
		t.Fatalf("guard lists a test alert: %v", rows)
	}
	if rows, _ := callList(t, "/api/v1/sos/active", w.s.Admin); len(rows) != 1 {
		t.Fatalf("admin sees the test alert: %v", rows)
	}
	if r := call(t, "POST", "/api/v1/sos/"+id+"/acknowledge", w.guard.Token, nil); r.Status != 404 {
		t.Fatalf("guard acknowledges a test: %d", r.Status)
	}
	// a test does not join or block real alerts, and never escalates
	real := raise(t, w.r1, map[string]any{})
	if real.Status != 201 || real.str("id") == id {
		t.Fatalf("real alert after a test: %d", real.Status)
	}
	setStatus(t, real.str("id"), "resolved")
	mustExec(t, context.Background(), `UPDATE sos_events SET last_alert_at = now() - interval '5 minutes' WHERE id=$1`, id)
	if n, _ := asos.Sweep(context.Background()); n != 0 {
		t.Fatalf("a test alert escalated: %d", n)
	}
}

func TestSOSEscalation(t *testing.T) {
	w := newSOSWorld(t)
	id := raise(t, w.r1, map[string]any{}).str("id")
	w.wg.expect(t, "sos.raised")
	ctx := context.Background()
	if n, _ := asos.Sweep(ctx); n != 0 {
		t.Fatalf("escalated before the wait: %d", n)
	}
	for i := 1; i <= 3; i++ {
		mustExec(t, ctx, `UPDATE sos_events SET last_alert_at = now() - interval '2 minutes' WHERE id=$1`, id)
		if n, err := asos.Sweep(ctx); err != nil || n != 1 {
			t.Fatalf("escalation %d: %d %v", i, n, err)
		}
		d := w.wg.expect(t, "sos.escalated")
		if d["escalations"] != float64(i) {
			t.Fatalf("escalation count: %v", d)
		}
	}
	mustExec(t, ctx, `UPDATE sos_events SET last_alert_at = now() - interval '2 minutes' WHERE id=$1`, id)
	if n, _ := asos.Sweep(ctx); n != 0 {
		t.Fatalf("a fourth escalation: %d", n)
	}
	// every re-alert is a critical notification for the responders
	crit := 0
	for _, it := range inbox(t, w.guard.Token, "") {
		if it["type"] == "sos.escalated" && it["priority"] == "critical" {
			crit++
		}
	}
	if crit != 3 {
		t.Fatalf("escalation notifications: %d", crit)
	}
	// an acknowledged alert is not escalated
	id2 := raise(t, w.r2, map[string]any{}).str("id")
	call(t, "POST", "/api/v1/sos/"+id2+"/acknowledge", w.guard.Token, nil)
	mustExec(t, ctx, `UPDATE sos_events SET last_alert_at = now() - interval '2 minutes' WHERE id=$1`, id2)
	if n, _ := asos.Sweep(ctx); n != 0 {
		t.Fatalf("acknowledged alert escalated: %d", n)
	}
}

func TestSOSAdminLogAndSwitch(t *testing.T) {
	w := newSOSWorld(t)
	a := raise(t, w.r1, map[string]any{"kind": "fire", "note": "=HYPERLINK(\"http://evil\")"}).str("id")
	call(t, "POST", "/api/v1/sos/"+a+"/acknowledge", w.guard.Token, nil)
	call(t, "POST", "/api/v1/sos/"+a+"/resolve", w.guard.Token, map[string]any{"outcome": "False alarm, cooking smoke"})
	b := raise(t, w.r2, map[string]any{"kind": "security"}).str("id")

	rows, r := callList(t, "/api/v1/society/sos", w.s.Admin)
	if r.Status != 200 || len(rows) != 2 {
		t.Fatalf("log: %d %v", r.Status, rows)
	}
	if rows, _ := callList(t, "/api/v1/society/sos?status=resolved", w.s.Admin); len(rows) != 1 || rows[0]["id"] != a {
		t.Fatalf("status filter: %v", rows)
	}
	if rows, _ := callList(t, "/api/v1/society/sos?kind=security", w.s.Admin); len(rows) != 1 || rows[0]["id"] != b {
		t.Fatalf("kind filter: %v", rows)
	}
	if r := call(t, "GET", "/api/v1/society/sos?status=weird", w.s.Admin, nil); r.Status != 400 {
		t.Fatalf("bad status: %d", r.Status)
	}
	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	if rows, _ := callList(t, "/api/v1/society/sos?from="+strings.ReplaceAll(future, "+", "%2B"), w.s.Admin); len(rows) != 0 {
		t.Fatalf("date filter: %v", rows)
	}
	d := call(t, "GET", "/api/v1/society/sos/"+a, w.s.Admin, nil)
	if d.Status != 200 || d.str("resolved_by") == "" || d.str("acknowledged_by") == "" || d.data()["callback_phone"] != nil {
		t.Fatalf("detail: %d %v", d.Status, d.Body)
	}
	// the log is for the office only
	for name, tok := range map[string]string{"guard": w.guard.Token, "resident": w.r1} {
		if r := call(t, "GET", "/api/v1/society/sos", tok, nil); r.Status != 403 {
			t.Fatalf("%s reads the log: %d", name, r.Status)
		}
	}
	other := newSoc(t)
	if r := call(t, "GET", "/api/v1/society/sos/"+a, other.Admin, nil); r.Status != 404 {
		t.Fatalf("other society's detail: %d", r.Status)
	}
	if rows, _ := callList(t, "/api/v1/society/sos", other.Admin); len(rows) != 0 {
		t.Fatalf("other society's log: %v", rows)
	}
	// CSV: formula-safe
	st, _, body := rawGet(t, "/api/v1/society/sos/export", w.s.Admin)
	if st != 200 || !strings.HasPrefix(string(body), "Raised,Kind,Flat") || !strings.Contains(string(body), "'=HYPERLINK") || strings.Contains(string(body), ",=HYPERLINK") {
		t.Fatalf("export: %d %s", st, body)
	}

	// SOS switched off: every route says so
	mustExec(t, context.Background(), `INSERT INTO society_features (society_id, feature_key, enabled) VALUES ($1,'sos',false)`, w.s.ID)
	for name, r := range map[string]resp{
		"raise":  raise(t, w.r1, map[string]any{}),
		"active": call(t, "GET", "/api/v1/sos/active", w.guard.Token, nil),
		"log":    call(t, "GET", "/api/v1/society/sos", w.s.Admin, nil),
		"mine":   call(t, "GET", "/api/v1/me/sos", w.r1, nil),
	} {
		if r.Status != 403 || r.errCode() != "FEATURE_DISABLED" {
			t.Fatalf("%s with SOS off: %d %v", name, r.Status, r.Body)
		}
	}
}

// ── deliveries ───────────────────────────────────────────────────────────────

func walkInAs(t *testing.T, g guardUser, unit, name, purpose string, company *string) resp {
	body := map[string]any{"unit_id": unit, "visitor_name": name, "purpose": purpose}
	if company != nil {
		body["company"] = *company
	}
	return call(t, "POST", "/api/v1/gate/visits", g.Token, body)
}

func co(s string) *string { return &s }

func TestDeliveryRules(t *testing.T) {
	s := newSoc(t)
	_, units := setupBlock(t, s, "A", 1, 2)
	r1, _ := newResident(t, s, units["A101"], "owner")
	r2, _ := newResident(t, s, units["A102"], "owner")
	fam, _ := newResident(t, s, units["A101"], "family")
	g := newGuard(t, s)
	w1 := dialWS(t, r1)

	if rr := call(t, "GET", "/api/v1/me/delivery-rules", r1, nil); rr.Status != 200 {
		t.Fatalf("rules: %d", rr.Status)
	} else if rules, _ := rr.data()["rules"].([]any); len(rules) != 2 || rules[0].(map[string]any)["mode"] != "ask" {
		t.Fatalf("defaults: %v", rr.Body)
	}
	set := func(tok string, body map[string]any) resp {
		return call(t, "PUT", "/api/v1/me/delivery-rules", tok, body)
	}
	if r := set(fam, map[string]any{"scope": "delivery", "mode": "auto_approve"}); r.Status != 403 {
		t.Fatalf("family sets a rule: %d", r.Status)
	}
	for _, bad := range []map[string]any{
		{"scope": "guest", "mode": "auto_approve"},
		{"scope": "delivery", "mode": "always"},
		{"scope": "cab", "mode": "leave_at_gate"},
		{"scope": "delivery", "mode": "auto_approve", "companies": []string{"x"}},
	} {
		if r := set(r1, bad); r.Status != 400 {
			t.Fatalf("bad rule %v: %d", bad, r.Status)
		}
	}
	if r := set(r1, map[string]any{"scope": "delivery", "mode": "auto_approve", "companies": []string{"Swiggy", "swiggy", " Zomato "}}); r.Status != 200 || len(r.data()["companies"].([]any)) != 2 {
		t.Fatalf("set rule: %d %v", r.Status, r.Body)
	}

	// matching company: let in without asking, the flat is informed
	a := walkInAs(t, g, units["A101"], "Swiggy Rider", "delivery", co("SWIGGY"))
	if a.Status != 201 || a.vstatus() != "approved" || a.data()["auto_approved"] != true {
		t.Fatalf("auto-approve: %d %v", a.Status, a.Body)
	}
	w1.expect(t, "visit.arrived")
	w1.expectNothing(t) // no approval prompt
	if n := w1.note(time.Second); n == nil || n["type"] != "visit.arrived" {
		t.Fatalf("arrival notification: %v", n)
	}
	if c := call(t, "POST", "/api/v1/gate/visits/"+a.str("id")+"/check-in", g.Token, nil); c.Status != 200 {
		t.Fatalf("check-in of an auto-approved visit: %d", c.Status)
	}
	// other company, guests and services still ask; the neighbour is unaffected
	for name, x := range map[string]resp{
		"other company": walkInAs(t, g, units["A101"], "Amazon Guy", "delivery", co("Amazon")),
		"no company":    walkInAs(t, g, units["A101"], "Unknown", "delivery", nil),
		"guest":         walkInAs(t, g, units["A101"], "Friend", "guest", co("Swiggy")),
		"service":       walkInAs(t, g, units["A101"], "Plumber", "service", co("Swiggy")),
		"neighbour":     walkInAs(t, g, units["A102"], "Swiggy Rider", "delivery", co("Swiggy")),
	} {
		if x.Status != 201 || x.vstatus() != "waiting" {
			t.Fatalf("%s must ask: %d %v", name, x.Status, x.Body)
		}
	}
	// an empty company list means any company; cabs follow their own rule
	set(r1, map[string]any{"scope": "cab", "mode": "auto_approve"})
	if c := walkInAs(t, g, units["A101"], "Cab Driver", "cab", co("Uber")); c.vstatus() != "approved" {
		t.Fatalf("cab rule: %v", c.Body)
	}
	// leave at the gate
	set(r1, map[string]any{"scope": "delivery", "mode": "leave_at_gate", "companies": []string{}})
	l := walkInAs(t, g, units["A101"], "Parcel", "delivery", co("Flipkart"))
	if l.vstatus() != "approved" || l.str("handover") != "leave_at_gate" {
		t.Fatalf("leave at gate: %v", l.Body)
	}
	if n := w1.note(time.Second); n == nil {
		t.Fatal("leave-at-gate notification")
	}
	// back to ask
	set(r1, map[string]any{"scope": "delivery", "mode": "ask"})
	if x := walkInAs(t, g, units["A101"], "Swiggy Again", "delivery", co("Swiggy")); x.vstatus() != "waiting" {
		t.Fatalf("ask again: %v", x.Body)
	}
	// the log shows which visits were let in by a rule
	st, _, body := rawGet(t, "/api/v1/society/visitors/export", s.Admin)
	if st != 200 || !strings.Contains(string(body), "Auto-approved") || !strings.Contains(string(body), "yes (leave_at_gate)") {
		t.Fatalf("export: %d %s", st, body)
	}
	_ = r2

	// Deliveries switched off: rules are ignored (always ask) and the routes say so
	set(r1, map[string]any{"scope": "delivery", "mode": "auto_approve"})
	mustExec(t, context.Background(), `INSERT INTO society_features (society_id, feature_key, enabled) VALUES ($1,'deliveries',false)`, s.ID)
	if x := walkInAs(t, g, units["A101"], "Swiggy Off", "delivery", co("Swiggy")); x.vstatus() != "waiting" {
		t.Fatalf("rule applied with the service off: %v", x.Body)
	}
	if r := call(t, "GET", "/api/v1/me/delivery-rules", r1, nil); r.Status != 403 || r.errCode() != "FEATURE_DISABLED" {
		t.Fatalf("rules with the service off: %d", r.Status)
	}
}

func TestExpectDelivery(t *testing.T) {
	s := newSoc(t)
	_, units := setupBlock(t, s, "A", 1, 2)
	r1, _ := newResident(t, s, units["A101"], "owner")
	fam, _ := newResident(t, s, units["A101"], "family")
	newResident(t, s, units["A102"], "owner")
	g := newGuard(t, s)

	if r := call(t, "POST", "/api/v1/visitors/expect-delivery", fam, map[string]any{"company": "Amazon"}); r.Status != 403 {
		t.Fatalf("family expects: %d", r.Status)
	}
	if r := call(t, "POST", "/api/v1/visitors/expect-delivery", r1, map[string]any{"company": "A"}); r.Status != 400 {
		t.Fatalf("short company: %d", r.Status)
	}
	if r := call(t, "POST", "/api/v1/visitors/expect-delivery", r1, map[string]any{"company": "Amazon", "hours": 48}); r.Status != 400 {
		t.Fatalf("long window: %d", r.Status)
	}
	e := call(t, "POST", "/api/v1/visitors/expect-delivery", r1, map[string]any{"company": "Amazon", "hours": 4})
	if e.Status != 201 || e.str("company") != "Amazon" {
		t.Fatalf("expect: %d %v", e.Status, e.Body)
	}
	// it applies once, to that flat and company only
	if x := walkInAs(t, g, units["A102"], "Amazon Guy", "delivery", co("Amazon")); x.vstatus() != "waiting" {
		t.Fatalf("another flat: %v", x.Body)
	}
	if x := walkInAs(t, g, units["A101"], "Zomato Guy", "delivery", co("Zomato")); x.vstatus() != "waiting" {
		t.Fatalf("another company: %v", x.Body)
	}
	first := walkInAs(t, g, units["A101"], "Amazon Guy", "delivery", co("amazon"))
	if first.vstatus() != "approved" || first.data()["auto_approved"] != true {
		t.Fatalf("expected delivery: %v", first.Body)
	}
	if x := walkInAs(t, g, units["A101"], "Amazon Second", "delivery", co("Amazon")); x.vstatus() != "waiting" {
		t.Fatalf("expectation used twice: %v", x.Body)
	}
	// expired and withdrawn expectations do nothing
	call(t, "POST", "/api/v1/visitors/expect-delivery", r1, map[string]any{"company": "Myntra"})
	mustExec(t, context.Background(), `UPDATE delivery_expectations SET expires_at = now() - interval '1 minute' WHERE company='Myntra' AND society_id=$1`, s.ID)
	if x := walkInAs(t, g, units["A101"], "Myntra Guy", "delivery", co("Myntra")); x.vstatus() != "waiting" {
		t.Fatalf("expired expectation: %v", x.Body)
	}
	e2 := call(t, "POST", "/api/v1/visitors/expect-delivery", r1, map[string]any{"company": "Nykaa"})
	if r := call(t, "DELETE", "/api/v1/visitors/expect-delivery/"+e2.str("id"), fam, nil); r.Status != 403 {
		t.Fatalf("family withdraws: %d", r.Status)
	}
	if r := call(t, "DELETE", "/api/v1/visitors/expect-delivery/"+e2.str("id"), r1, nil); r.Status != 204 {
		t.Fatalf("withdraw: %d", r.Status)
	}
	if x := walkInAs(t, g, units["A101"], "Nykaa Guy", "delivery", co("Nykaa")); x.vstatus() != "waiting" {
		t.Fatalf("withdrawn expectation: %v", x.Body)
	}
	if rr := call(t, "GET", "/api/v1/me/delivery-rules", r1, nil); rr.Status != 200 {
		t.Fatal("rules")
	} else if ex, _ := rr.data()["expectations"].([]any); len(ex) != 0 {
		t.Fatalf("open expectations: %v", ex)
	}
	_ = uuid.Nil
}

// ── emergency contacts ───────────────────────────────────────────────────────

func TestSocietyContacts(t *testing.T) {
	s := newSoc(t)
	_, units := setupBlock(t, s, "A", 1, 1)
	r1, _ := newResident(t, s, units["A101"], "owner")
	g := newGuard(t, s)

	c := call(t, "POST", "/api/v1/society/contacts", s.Admin, map[string]any{"label": "Security Control Room", "phone": "+91 98765-43210", "hours": "24/7"})
	if c.Status != 201 || c.str("phone") != "919876543210" {
		t.Fatalf("create: %d %v", c.Status, c.Body)
	}
	for _, bad := range []map[string]any{{"label": "X", "phone": "12345"}, {"label": "Manager", "phone": "12"}, {"label": "Manager", "phone": "abc"}} {
		if r := call(t, "POST", "/api/v1/society/contacts", s.Admin, bad); r.Status != 400 {
			t.Fatalf("bad contact %v: %d", bad, r.Status)
		}
	}
	for name, tok := range map[string]string{"resident": r1, "guard": g.Token} {
		if r := call(t, "POST", "/api/v1/society/contacts", tok, map[string]any{"label": "Spam", "phone": "1234567"}); r.Status != 403 {
			t.Fatalf("%s writes contacts: %d", name, r.Status)
		}
	}
	if rows, r := callList(t, "/api/v1/me/contacts", r1); r.Status != 200 || len(rows) != 1 || rows[0]["label"] != "Security Control Room" {
		t.Fatalf("resident reads: %d %v", r.Status, rows)
	}
	if rows, _ := callList(t, "/api/v1/me/contacts", newSoc(t).Admin); len(rows) != 0 {
		t.Fatalf("other society: %v", rows)
	}
	id := c.str("id")
	if r := call(t, "PUT", "/api/v1/society/contacts/"+id, s.Admin, map[string]any{"label": "Control Room", "phone": "1800123456", "hours": "24x7"}); r.Status != 200 || r.str("label") != "Control Room" {
		t.Fatalf("update: %d %v", r.Status, r.Body)
	}
	other := newSoc(t)
	if r := call(t, "PUT", "/api/v1/society/contacts/"+id, other.Admin, map[string]any{"label": "Hijack", "phone": "1234567"}); r.Status != 404 {
		t.Fatalf("other society updates: %d", r.Status)
	}
	if r := call(t, "DELETE", "/api/v1/society/contacts/"+id, other.Admin, nil); r.Status != 404 {
		t.Fatalf("other society deletes: %d", r.Status)
	}
	if r := call(t, "DELETE", "/api/v1/society/contacts/"+id, s.Admin, nil); r.Status != 204 {
		t.Fatalf("delete: %d", r.Status)
	}
	if rows, _ := callList(t, "/api/v1/me/contacts", r1); len(rows) != 0 {
		t.Fatalf("after delete: %v", rows)
	}
}
