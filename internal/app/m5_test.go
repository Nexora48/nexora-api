package app_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/Nexora48/nexora-api/internal/notify"
)

// ── helpers ──────────────────────────────────────────────────────────────────

type pushRecorder struct {
	mu   sync.Mutex
	sent []pushed
}
type pushed struct {
	Token string
	Msg   notify.Message
}

func (p *pushRecorder) Send(_ context.Context, token string, m notify.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, pushed{token, m})
	if strings.HasPrefix(token, "dead-") {
		return notify.ErrUnregistered
	}
	return nil
}
func (p *pushRecorder) to(token string) []pushed {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []pushed
	for _, s := range p.sent {
		if s.Token == token {
			out = append(out, s)
		}
	}
	return out
}

// wsClient is a connected app: it collects events until closed.
type wsClient struct {
	conn *websocket.Conn
	ch   chan map[string]any
	done chan struct{}
}

func dialWS(t *testing.T, token string) *wsClient {
	t.Helper()
	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/ws?token=" + token
	c, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	w := &wsClient{conn: c, ch: make(chan map[string]any, 64), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		for {
			_, b, err := c.ReadMessage()
			if err != nil {
				return
			}
			var m map[string]any
			if json.Unmarshal(b, &m) == nil && m["type"] != "hello" {
				w.ch <- m
			}
		}
	}()
	t.Cleanup(func() { c.Close() })
	// registration happens right after the upgrade; wait for the hello to be safe
	time.Sleep(50 * time.Millisecond)
	return w
}

// next returns the next event or nil after d.
func (w *wsClient) next(d time.Duration) map[string]any {
	select {
	case m := <-w.ch:
		return m
	case <-time.After(d):
		return nil
	}
}

func (w *wsClient) expect(t *testing.T, typ string) map[string]any {
	t.Helper()
	m := w.next(2 * time.Second)
	if m == nil || m["type"] != typ {
		t.Fatalf("expected event %q, got %v", typ, m)
	}
	d, _ := m["data"].(map[string]any)
	return d
}

func (w *wsClient) expectNothing(t *testing.T) {
	t.Helper()
	if m := w.next(300 * time.Millisecond); m != nil {
		t.Fatalf("unexpected event %v", m)
	}
}

func uploadPhoto(t *testing.T, path, token string, content []byte) resp {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "p.jpg")
	_, _ = fw.Write(content)
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

func rawGet(t *testing.T, path, token string) (int, string, []byte) {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(res.Body)
	return res.StatusCode, res.Header.Get("Content-Type"), b.Bytes()
}

var jpeg = append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, bytes.Repeat([]byte{1}, 200)...)

func walkIn(t *testing.T, g guardUser, unit, name string) resp {
	return call(t, "POST", "/api/v1/gate/visits", g.Token, map[string]any{"unit_id": unit, "visitor_name": name, "purpose": "guest"})
}

// ── guards & gates ───────────────────────────────────────────────────────────

func TestGuardProvisioning(t *testing.T) {
	s := newSoc(t)
	other := newSoc(t)
	_, units := setupBlock(t, s, "A", 1, 1)
	res, _ := newResident(t, s, units["A101"], "owner")

	phone := nextPhone()
	c := call(t, "POST", "/api/v1/society/guards", s.Admin, map[string]any{"full_name": "Ravi Guard", "phone": phone})
	if c.Status != 201 || c.str("status") != "active" || c.data()["on_duty"] != false {
		t.Fatalf("create guard: %d %v", c.Status, c.Body)
	}
	gid := c.str("id")
	if d := call(t, "POST", "/api/v1/society/guards", s.Admin, map[string]any{"full_name": "Ravi Again", "phone": phone}); d.Status != 409 || d.errCode() != "GUARD_EXISTS" {
		t.Fatalf("duplicate: %d %v", d.Status, d.Body)
	}
	if d := call(t, "POST", "/api/v1/society/guards", s.Admin, map[string]any{"full_name": "Bad", "phone": "123"}); d.Status != 400 {
		t.Fatalf("bad phone: %d", d.Status)
	}

	// role matrix and tenancy
	for name, tok := range map[string]string{"resident": res} {
		if r := call(t, "GET", "/api/v1/society/guards", tok, nil); r.Status != 403 {
			t.Fatalf("%s lists guards: %d", name, r.Status)
		}
	}
	mr.FlushAll()
	l := otpLogin(t, phone)
	if l.Status != 200 {
		t.Fatalf("new guard cannot sign in: %d %v", l.Status, l.Body)
	}
	gtok, grefresh := l.str("access_token"), l.str("refresh_token")
	if r := call(t, "GET", "/api/v1/society/guards", gtok, nil); r.Status != 403 {
		t.Fatalf("guard lists guards: %d", r.Status)
	}
	if r := call(t, "PATCH", "/api/v1/society/guards/"+gid, other.Admin, map[string]any{"status": "suspended"}); r.Status != 404 {
		t.Fatalf("other society's admin: %d", r.Status)
	}
	if r := call(t, "DELETE", "/api/v1/society/guards/"+gid, other.Admin, nil); r.Status != 404 {
		t.Fatalf("other society's admin delete: %d", r.Status)
	}

	// suspend cuts the session; reactivate restores access
	if r := call(t, "PATCH", "/api/v1/society/guards/"+gid, s.Admin, map[string]any{"status": "suspended"}); r.Status != 200 || r.str("status") != "suspended" {
		t.Fatalf("suspend: %d %v", r.Status, r.Body)
	}
	if r := call(t, "POST", "/api/v1/auth/refresh", "", map[string]any{"refresh_token": grefresh}); r.Status != 401 {
		t.Fatalf("refresh after suspend: %d %v", r.Status, r.Body)
	}
	if r := call(t, "PATCH", "/api/v1/society/guards/"+gid, s.Admin, map[string]any{"status": "active", "full_name": "Ravi Kumar"}); r.Status != 200 || r.str("full_name") != "Ravi Kumar" {
		t.Fatalf("reactivate: %d %v", r.Status, r.Body)
	}
	if r := call(t, "PATCH", "/api/v1/society/guards/"+gid, s.Admin, map[string]any{"status": "banned"}); r.Status != 400 {
		t.Fatalf("bad status: %d", r.Status)
	}
	rows, _ := callList(t, "/api/v1/society/guards", s.Admin)
	if len(rows) != 1 {
		t.Fatalf("list: %v", rows)
	}
	if rows, _ := callList(t, "/api/v1/society/guards", other.Admin); len(rows) != 0 {
		t.Fatalf("other society sees guards: %v", rows)
	}
	if r := call(t, "DELETE", "/api/v1/society/guards/"+gid, s.Admin, nil); r.Status != 204 {
		t.Fatalf("remove: %d", r.Status)
	}
	if rows, _ := callList(t, "/api/v1/society/guards", s.Admin); len(rows) != 0 {
		t.Fatalf("removed guard still listed: %v", rows)
	}
	// the same person can be hired again
	if r := call(t, "POST", "/api/v1/society/guards", s.Admin, map[string]any{"full_name": "Ravi Kumar", "phone": phone}); r.Status != 201 {
		t.Fatalf("re-hire: %d %v", r.Status, r.Body)
	}
}

func TestGatesAndShifts(t *testing.T) {
	s := newSoc(t)
	_, units := setupBlock(t, s, "A", 1, 1)
	r1, _ := newResident(t, s, units["A101"], "owner")
	g := newGuard(t, s)

	gates, _ := callList(t, "/api/v1/society/gates", s.Admin)
	if len(gates) != 1 || gates[0]["name"] != "Main Gate" {
		t.Fatalf("default gate: %v", gates)
	}
	mainID := gates[0]["id"].(string)
	back := call(t, "POST", "/api/v1/society/gates", s.Admin, map[string]any{"name": "Back Gate"})
	if back.Status != 201 {
		t.Fatalf("add gate: %d %v", back.Status, back.Body)
	}
	if d := call(t, "POST", "/api/v1/society/gates", s.Admin, map[string]any{"name": "back gate"}); d.Status != 409 || d.errCode() != "GATE_EXISTS" {
		t.Fatalf("duplicate gate: %d %v", d.Status, d.Body)
	}
	if r := call(t, "GET", "/api/v1/society/gates", r1, nil); r.Status != 403 {
		t.Fatalf("resident gates: %d", r.Status)
	}

	if r := call(t, "POST", "/api/v1/gate/shift/end", g.Token, nil); r.Status != 409 || r.errCode() != "NO_SHIFT" {
		t.Fatalf("end without shift: %d %v", r.Status, r.Body)
	}
	if r := call(t, "POST", "/api/v1/gate/shift/start", g.Token, map[string]any{"gate_id": uuid.NewString()}); r.Status != 404 {
		t.Fatalf("unknown gate: %d", r.Status)
	}
	if sh := call(t, "POST", "/api/v1/gate/shift/start", g.Token, map[string]any{"gate_id": mainID}); sh.Status != 200 || sh.str("gate_name") != "Main Gate" {
		t.Fatalf("start: %d %v", sh.Status, sh.Body)
	}
	// the walk-in records the gate
	w := walkIn(t, g, units["A101"], "Gate Visitor")
	if w.Status != 201 || w.str("gate_name") != "Main Gate" {
		t.Fatalf("walk-in gate: %d %v", w.Status, w.Body)
	}
	// switching gates closes the old shift
	if sh := call(t, "POST", "/api/v1/gate/shift/start", g.Token, map[string]any{"gate_id": back.str("id")}); sh.Status != 200 {
		t.Fatalf("switch: %d", sh.Status)
	}
	if cur := call(t, "GET", "/api/v1/gate/shift", g.Token, nil); cur.Status != 200 || cur.data()["shift"].(map[string]any)["gate_name"] != "Back Gate" {
		t.Fatalf("current shift: %v", cur.Body)
	}
	guards, _ := callList(t, "/api/v1/society/guards", s.Admin)
	if len(guards) != 1 || guards[0]["on_duty"] != true || guards[0]["gate_name"] != "Back Gate" {
		t.Fatalf("on duty: %v", guards)
	}
	adminRows, _ := callList(t, "/api/v1/society/visitors", s.Admin)
	if len(adminRows) != 1 || adminRows[0]["gate_name"] != "Main Gate" {
		t.Fatalf("admin log gate: %v", adminRows)
	}

	// inactive gate cannot start shifts; shifts keep working with Visitors switched off
	call(t, "PATCH", "/api/v1/society/gates/"+back.str("id"), s.Admin, map[string]any{"active": false})
	if r := call(t, "POST", "/api/v1/gate/shift/start", g.Token, map[string]any{"gate_id": back.str("id")}); r.Status != 409 || r.errCode() != "GATE_INACTIVE" {
		t.Fatalf("inactive gate: %d %v", r.Status, r.Body)
	}
	if rows, _ := callList(t, "/api/v1/gate/shift/gates", g.Token); len(rows) != 1 {
		t.Fatalf("guard sees only active gates: %v", rows)
	}
	mustExec(t, context.Background(), `INSERT INTO society_features (society_id, feature_key, enabled) VALUES ($1,'visitors',false)`, s.ID)
	if r := call(t, "GET", "/api/v1/gate/dashboard", g.Token, nil); r.Status != 403 || r.errCode() != "FEATURE_DISABLED" {
		t.Fatalf("dashboard with service off: %d %v", r.Status, r.Body)
	}
	if r := call(t, "POST", "/api/v1/gate/shift/end", g.Token, nil); r.Status != 204 {
		t.Fatalf("end shift with service off: %d", r.Status)
	}
}

func TestGuardCannotUseResidentRoutes(t *testing.T) {
	s := newSoc(t)
	setupBlock(t, s, "A", 1, 1)
	g := newGuard(t, s)
	for _, p := range []string{"/api/v1/visitors", "/api/v1/visitors/summary"} {
		if r := call(t, "GET", p, g.Token, nil); r.Status == 200 {
			t.Fatalf("guard reached resident route %s: %v", p, r.Body)
		}
	}
	if r := call(t, "POST", "/api/v1/visitors", g.Token, map[string]any{"visitor_name": "X Y", "purpose": "guest"}); r.Status == 201 {
		t.Fatal("guard created a pass")
	}
	if r := call(t, "GET", "/api/v1/society/visitors", g.Token, nil); r.Status != 403 {
		t.Fatalf("guard reads admin log: %d", r.Status)
	}
}

// ── realtime & push ──────────────────────────────────────────────────────────

func TestRealtimeWalkInFlow(t *testing.T) {
	s := newSoc(t)
	_, units := setupBlock(t, s, "A", 1, 2)
	r1, phone1 := newResident(t, s, units["A101"], "owner")
	r2, _ := newResident(t, s, units["A102"], "owner")
	g := newGuard(t, s)
	call(t, "PUT", "/api/v1/me/devices", r1, map[string]any{"device_id": "d1", "platform": "android", "fcm_token": "tok-" + phone1})

	if r := call(t, "GET", "/api/v1/ws", "", nil); r.Status != 401 {
		t.Fatalf("ws without token: %d", r.Status)
	}
	if r := call(t, "GET", "/api/v1/ws?token=garbage", "", nil); r.Status != 401 {
		t.Fatalf("ws bad token: %d", r.Status)
	}

	w1, w2, wg := dialWS(t, r1), dialWS(t, r2), dialWS(t, g.Token)
	w := walkIn(t, g, units["A101"], "Zomato Delivery")
	if w.Status != 201 {
		t.Fatalf("walk-in: %d %v", w.Status, w.Body)
	}
	id := w.str("id")

	d := w1.expect(t, "visit.requested")
	if d["visit_id"] != id || d["visitor_name"] != "Zomato Delivery" || d["flat"] != "A-A101" || d["expires_at"] == nil {
		t.Fatalf("payload: %v", d)
	}
	w2.expectNothing(t) // another flat hears nothing
	wg.expectNothing(t) // the guard is not told about his own request

	// push goes to the flat's devices and carries no visitor details
	got := pushRec.to("tok-" + phone1)
	if len(got) != 1 || got[0].Msg.Data["visit_id"] != id || strings.Contains(got[0].Msg.Body+got[0].Msg.Title, "Zomato") {
		t.Fatalf("push: %+v", got)
	}

	if r := call(t, "POST", "/api/v1/visitors/"+id+"/decision", r1, map[string]any{"decision": "approve"}); r.Status != 200 {
		t.Fatalf("approve: %d %v", r.Status, r.Body)
	}
	if d := wg.expect(t, "visit.decided"); d["status"] != "approved" {
		t.Fatalf("guard decided: %v", d)
	}
	w1.expect(t, "visit.decided")
	w2.expectNothing(t)

	call(t, "POST", "/api/v1/gate/visits/"+id+"/check-in", g.Token, nil)
	wg.expect(t, "visit.checked_in")
	w1.expect(t, "visit.checked_in")
	call(t, "POST", "/api/v1/gate/visits/"+id+"/check-out", g.Token, nil)
	if d := wg.expect(t, "visit.checked_out"); d["status"] != "completed" {
		t.Fatalf("checkout: %v", d)
	}
	w1.expect(t, "visit.checked_out")

	// a rejected request tells the guard with the reason
	w3 := walkIn(t, g, units["A101"], "Salesman")
	w1.expect(t, "visit.requested")
	call(t, "POST", "/api/v1/visitors/"+w3.str("id")+"/decision", r1, map[string]any{"decision": "reject", "reason": "Not expecting anyone"})
	if d := wg.expect(t, "visit.decided"); d["status"] != "rejected" || d["decision_reason"] != "Not expecting anyone" {
		t.Fatalf("reject: %v", d)
	}
}

func TestRealtimeSessionRevokedAndFeatures(t *testing.T) {
	s := newSoc(t)
	_, units := setupBlock(t, s, "A", 1, 1)
	r1, _ := newResident(t, s, units["A101"], "owner")
	phone := nextPhone()
	c := call(t, "POST", "/api/v1/society/guards", s.Admin, map[string]any{"full_name": "Live Guard", "phone": phone})
	mr.FlushAll()
	gtok := otpLogin(t, phone).str("access_token")
	wg, wr := dialWS(t, gtok), dialWS(t, r1)

	// SuperAdmin switch → every connected app of the society is told
	tok := superToken(t)
	if r := sa(t, "PUT", "/societies/"+s.ID.String()+"/features/visitors", tok, map[string]any{"enabled": false, "reason": "test"}); r.Status != 200 {
		t.Fatalf("toggle: %d %v", r.Status, r.Body)
	}
	wg.expect(t, "features.updated")
	wr.expect(t, "features.updated")

	// removing the guard drops his socket
	call(t, "DELETE", "/api/v1/society/guards/"+c.str("id"), s.Admin, nil)
	wg.expect(t, "session.revoked")
	select {
	case <-wg.done:
	case <-time.After(2 * time.Second):
		t.Fatal("socket stayed open after removal")
	}
	// and he cannot reconnect
	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/ws?token=" + gtok
	if _, res, err := websocket.DefaultDialer.Dial(u, nil); err == nil || res == nil || res.StatusCode != 401 {
		t.Fatalf("removed guard reconnects: %v", err)
	}
}

func TestUnregisteredPushTokenIsDropped(t *testing.T) {
	s := newSoc(t)
	_, units := setupBlock(t, s, "A", 1, 1)
	r1, phone := newResident(t, s, units["A101"], "owner")
	g := newGuard(t, s)
	tok := "dead-" + phone
	call(t, "PUT", "/api/v1/me/devices", r1, map[string]any{"device_id": "d1", "platform": "android", "fcm_token": tok})
	walkIn(t, g, units["A101"], "One")
	if n := len(pushRec.to(tok)); n != 1 {
		t.Fatalf("first push: %d", n)
	}
	var left *string
	_ = pool.QueryRow(context.Background(), `SELECT fcm_token FROM devices WHERE device_id='d1' AND user_id=(SELECT id FROM users WHERE phone=$1)`, phone).Scan(&left)
	if left != nil {
		t.Fatalf("dead token kept: %v", *left)
	}
	walkIn(t, g, units["A101"], "Two")
	if n := len(pushRec.to(tok)); n != 1 {
		t.Fatalf("pushed to a dropped token: %d", n)
	}
}

// ── photo, ring, cancel ──────────────────────────────────────────────────────

func TestPhotoRequiredFlow(t *testing.T) {
	s := newSoc(t)
	mustExec(t, context.Background(), `UPDATE societies SET walkin_photo_required=true WHERE id=$1`, s.ID)
	_, units := setupBlock(t, s, "A", 1, 2)
	r1, _ := newResident(t, s, units["A101"], "owner")
	r2, _ := newResident(t, s, units["A102"], "owner")
	g := newGuard(t, s)
	w1 := dialWS(t, r1)

	w := walkIn(t, g, units["A101"], "Photo Needed")
	if w.Status != 201 || w.data()["awaiting_photo"] != true || w.data()["has_photo"] != false {
		t.Fatalf("walk-in: %d %v", w.Status, w.Body)
	}
	id := w.str("id")
	w1.expectNothing(t) // the resident is not asked before the photo is in
	if d := call(t, "POST", "/api/v1/visitors/"+id+"/decision", r1, map[string]any{"decision": "approve"}); d.Status != 409 || d.errCode() != "PHOTO_PENDING" {
		t.Fatalf("decide before photo: %d %v", d.Status, d.Body)
	}
	if r := call(t, "POST", "/api/v1/gate/visits/"+id+"/ring", g.Token, nil); r.Status != 409 {
		t.Fatalf("ring before photo: %d", r.Status)
	}

	// content decides, not the declared type; limits apply
	if r := uploadPhoto(t, "/api/v1/gate/visits/"+id+"/photo", g.Token, []byte("<html>not an image</html>")); r.Status != 400 || r.errCode() != "BAD_PHOTO" {
		t.Fatalf("non-image: %d %v", r.Status, r.Body)
	}
	if r := uploadPhoto(t, "/api/v1/gate/visits/"+id+"/photo", g.Token, append([]byte{0xFF, 0xD8, 0xFF}, bytes.Repeat([]byte{1}, 3<<20)...)); r.Status != 400 {
		t.Fatalf("oversize: %d", r.Status)
	}
	if r := uploadPhoto(t, "/api/v1/gate/visits/"+id+"/photo", r1, jpeg); r.Status != 403 {
		t.Fatalf("resident uploads: %d", r.Status)
	}
	if r := uploadPhoto(t, "/api/v1/gate/visits/"+id+"/photo", g.Token, jpeg); r.Status != 200 || r.data()["has_photo"] != true {
		t.Fatalf("upload: %d %v", r.Status, r.Body)
	}
	if d := w1.expect(t, "visit.requested"); d["has_photo"] != true {
		t.Fatalf("request after photo: %v", d)
	}

	// who can read the photo
	st, ct, body := rawGet(t, "/api/v1/visitors/"+id+"/photo", r1)
	if st != 200 || ct != "image/jpeg" || !bytes.Equal(body, jpeg) {
		t.Fatalf("flat reads photo: %d %s", st, ct)
	}
	if st, _, _ := rawGet(t, "/api/v1/visitors/"+id+"/photo", r2); st != 404 {
		t.Fatalf("neighbour reads photo: %d", st)
	}
	if st, _, _ := rawGet(t, "/api/v1/visitors/"+id+"/photo", s.Admin); st != 200 {
		t.Fatalf("admin reads photo: %d", st)
	}
	if st, _, _ := rawGet(t, "/api/v1/visitors/"+id+"/photo", newSoc(t).Admin); st != 404 {
		t.Fatalf("other society reads photo: %d", st)
	}
	if d := call(t, "POST", "/api/v1/visitors/"+id+"/decision", r1, map[string]any{"decision": "approve"}); d.Status != 200 {
		t.Fatalf("decide after photo: %d %v", d.Status, d.Body)
	}
	// a visit without a photo answers 404, not an empty image
	inv := invite(t, r1, nil)
	if st, _, _ := rawGet(t, "/api/v1/visitors/"+inv.str("id")+"/photo", r1); st != 404 {
		t.Fatalf("no photo: %d", st)
	}
}

func TestRingAndGateCancel(t *testing.T) {
	s := newSoc(t)
	_, units := setupBlock(t, s, "A", 1, 1)
	r1, _ := newResident(t, s, units["A101"], "owner")
	g := newGuard(t, s)
	w1 := dialWS(t, r1)

	w := walkIn(t, g, units["A101"], "Impatient")
	id := w.str("id")
	w1.expect(t, "visit.requested")
	if r := call(t, "POST", "/api/v1/gate/visits/"+id+"/ring", g.Token, nil); r.Status != 200 || r.data()["rerings"] != float64(1) {
		t.Fatalf("first ring: %d %v", r.Status, r.Body)
	}
	w1.expect(t, "visit.requested")
	if r := call(t, "POST", "/api/v1/gate/visits/"+id+"/ring", g.Token, nil); r.Status != 429 || r.errCode() != "RING_LIMIT" {
		t.Fatalf("ring again at once: %d %v", r.Status, r.Body)
	}
	// the interval and the cap of 3
	for i := 2; i <= 3; i++ {
		mustExec(t, context.Background(), `UPDATE visits SET last_ring_at = now() - interval '2 minutes' WHERE id=$1`, id)
		if r := call(t, "POST", "/api/v1/gate/visits/"+id+"/ring", g.Token, nil); r.Status != 200 {
			t.Fatalf("ring %d: %d", i, r.Status)
		}
	}
	mustExec(t, context.Background(), `UPDATE visits SET last_ring_at = now() - interval '2 minutes' WHERE id=$1`, id)
	if r := call(t, "POST", "/api/v1/gate/visits/"+id+"/ring", g.Token, nil); r.Status != 429 {
		t.Fatalf("fourth ring: %d", r.Status)
	}
	for i := 0; i < 4; i++ {
		w1.next(100 * time.Millisecond)
	}

	// the guard can withdraw the request
	if r := call(t, "POST", "/api/v1/gate/visits/"+id+"/cancel", g.Token, nil); r.Status != 200 || r.vstatus() != "cancelled" {
		t.Fatalf("cancel: %d %v", r.Status, r.Body)
	}
	w1.expect(t, "visit.cancelled")
	if d := call(t, "POST", "/api/v1/visitors/"+id+"/decision", r1, map[string]any{"decision": "approve"}); d.Status != 409 {
		t.Fatalf("decide after cancel: %d", d.Status)
	}
	if r := call(t, "POST", "/api/v1/gate/visits/"+id+"/cancel", g.Token, nil); r.Status != 409 {
		t.Fatalf("cancel twice: %d", r.Status)
	}
	// a pre-approved pass is not a request: it cannot be gate-cancelled
	inv := invite(t, r1, nil)
	if r := call(t, "POST", "/api/v1/gate/visits/"+inv.str("id")+"/cancel", g.Token, nil); r.Status != 409 {
		t.Fatalf("gate cancels an invite: %d", r.Status)
	}
	// ring and cancel are per society
	other := newSoc(t)
	og := newGuard(t, other)
	if r := call(t, "POST", "/api/v1/gate/visits/"+id+"/ring", og.Token, nil); r.Status != 404 {
		t.Fatalf("other society's guard: %d", r.Status)
	}
}

func TestExpiredWalkInIsAnnounced(t *testing.T) {
	s := newSoc(t)
	_, units := setupBlock(t, s, "A", 1, 1)
	r1, _ := newResident(t, s, units["A101"], "owner")
	g := newGuard(t, s)
	w := walkIn(t, g, units["A101"], "Slow Answer")
	wg, w1 := dialWS(t, g.Token), dialWS(t, r1)
	mustExec(t, context.Background(), `UPDATE visits SET created_at = now() - interval '11 minutes' WHERE id=$1`, w.str("id"))
	if n, err := vsvc.Sweep(context.Background()); err != nil || n != 1 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	if d := wg.expect(t, "visit.expired"); d["visit_id"] != w.str("id") {
		t.Fatalf("expired payload: %v", d)
	}
	w1.expect(t, "visit.expired")
}

// ── gate feeds, offline sync and batch ───────────────────────────────────────

func TestGateFeedsAndPassSync(t *testing.T) {
	s := newSoc(t)
	_, units := setupBlock(t, s, "A", 1, 2)
	r1, _ := newResident(t, s, units["A101"], "owner")
	g := newGuard(t, s)

	inv := invite(t, r1, map[string]any{"visitor_name": "Pass Holder", "vehicle_number": "MH12AB1234"})
	code := inv.str("pass_code")
	w := walkIn(t, g, units["A101"], "Inside Person")
	call(t, "POST", "/api/v1/visitors/"+w.str("id")+"/decision", r1, map[string]any{"decision": "approve"})
	call(t, "POST", "/api/v1/gate/visits/"+w.str("id")+"/check-in", g.Token, nil)
	waiting := walkIn(t, g, units["A101"], "Still Waiting")

	d := call(t, "GET", "/api/v1/gate/dashboard", g.Token, nil)
	c, _ := d.data()["counts"].(map[string]any)
	if d.Status != 200 || c["inside"] != float64(1) || c["waiting"] != float64(1) || c["expected"] != float64(1) || c["today"] != float64(3) {
		t.Fatalf("dashboard: %d %v", d.Status, d.Body)
	}
	inside, _ := callList(t, "/api/v1/gate/inside", g.Token)
	if len(inside) != 1 || inside[0]["visitor_name"] != "Inside Person" || inside[0]["entered_at"] == nil {
		t.Fatalf("inside: %v", inside)
	}
	if _, leaked := inside[0]["visitor_phone"]; leaked {
		t.Fatal("phone leaked to the guard")
	}
	veh, _ := callList(t, "/api/v1/gate/vehicles?q=mh%2012-ab", g.Token)
	if len(veh) != 1 || veh[0]["visitor_name"] != "Pass Holder" {
		t.Fatalf("vehicle search: %v", veh)
	}
	if veh, _ := callList(t, "/api/v1/gate/vehicles?q=", g.Token); len(veh) != 0 {
		t.Fatalf("empty query: %v", veh)
	}
	if gv := call(t, "GET", "/api/v1/gate/visits/"+waiting.str("id"), g.Token, nil); gv.Status != 200 || gv.vstatus() != "waiting" {
		t.Fatalf("gate get: %d %v", gv.Status, gv.Body)
	}

	// offline cache: hashed codes only, scoped to the society
	sync := call(t, "GET", "/api/v1/gate/passes/sync", g.Token, nil)
	raw, _ := json.Marshal(sync.Body)
	if sync.Status != 200 || strings.Contains(string(raw), code) {
		t.Fatalf("sync leaks the plaintext code: %s", raw)
	}
	passes, _ := sync.data()["passes"].([]any)
	if len(passes) != 1 {
		t.Fatalf("passes: %v", sync.Body)
	}
	sum := sha256.Sum256([]byte(s.ID.String() + ":" + code))
	if passes[0].(map[string]any)["code_hash"] != hex.EncodeToString(sum[:]) || sync.data()["society_id"] != s.ID.String() {
		t.Fatalf("hash mismatch: %v", passes[0])
	}
	other := newSoc(t)
	if p, _ := call(t, "GET", "/api/v1/gate/passes/sync", newGuard(t, other).Token, nil).data()["passes"].([]any); len(p) != 0 {
		t.Fatalf("other society's cache: %v", p)
	}
	if r := call(t, "GET", "/api/v1/gate/passes/sync", r1, nil); r.Status != 403 {
		t.Fatalf("resident syncs: %d", r.Status)
	}
}

func TestBatchIsIdempotent(t *testing.T) {
	s := newSoc(t)
	_, units := setupBlock(t, s, "A", 1, 1)
	r1, _ := newResident(t, s, units["A101"], "owner")
	g := newGuard(t, s)
	inv := invite(t, r1, nil)
	id := inv.str("id")

	post := func(events ...map[string]any) []any {
		r := call(t, "POST", "/api/v1/gate/events/batch", g.Token, map[string]any{"events": events})
		if r.Status != 200 {
			t.Fatalf("batch: %d %v", r.Status, r.Body)
		}
		l, _ := r.Body["data"].([]any)
		return l
	}
	ev := map[string]any{"client_event_id": "evt-000001-in", "type": "check_in", "visit_id": id}
	first := post(ev)[0].(map[string]any)
	if first["status"] != float64(200) || first["replayed"] != nil {
		t.Fatalf("first: %v", first)
	}
	// the response was lost; the app retries: same outcome, no second entry, no 409
	again := post(ev)[0].(map[string]any)
	if again["status"] != float64(200) || again["replayed"] != true {
		t.Fatalf("replay: %v", again)
	}
	// a different event for the same visit is a real conflict, and it is remembered too
	conflict := post(map[string]any{"client_event_id": "evt-000002-in", "type": "check_in", "visit_id": id})[0].(map[string]any)
	if conflict["status"] != float64(409) || conflict["error"].(map[string]any)["code"] != "INVALID_STATE" {
		t.Fatalf("conflict: %v", conflict)
	}
	res := post(
		map[string]any{"client_event_id": "evt-000003-out", "type": "check_out", "visit_id": id},
		map[string]any{"client_event_id": "evt-000004-wi", "type": "walk_in", "walk_in": map[string]any{"unit_id": units["A101"], "visitor_name": "Offline Walker", "purpose": "guest"}},
		map[string]any{"client_event_id": "evt-000005-bad", "type": "check_out", "visit_id": uuid.NewString()},
	)
	if res[0].(map[string]any)["status"] != float64(200) || res[1].(map[string]any)["status"] != float64(200) || res[2].(map[string]any)["status"] != float64(404) {
		t.Fatalf("mixed batch: %v", res)
	}
	// replaying the walk-in must not create a second visit
	post(map[string]any{"client_event_id": "evt-000004-wi", "type": "walk_in", "walk_in": map[string]any{"unit_id": units["A101"], "visitor_name": "Offline Walker", "purpose": "guest"}})
	rows, _ := callList(t, "/api/v1/society/visitors", s.Admin)
	if len(rows) != 2 {
		t.Fatalf("visits after replays: %d", len(rows))
	}
	// validation and scoping
	for _, bad := range []map[string]any{
		{"client_event_id": "short", "type": "check_in", "visit_id": id},
		{"client_event_id": "evt-000006-xx", "type": "teleport"},
	} {
		if r := call(t, "POST", "/api/v1/gate/events/batch", g.Token, map[string]any{"events": []any{bad}}); r.Status != 400 {
			t.Fatalf("bad event %v: %d", bad, r.Status)
		}
	}
	if r := call(t, "POST", "/api/v1/gate/events/batch", g.Token, map[string]any{"events": []any{}}); r.Status != 400 {
		t.Fatalf("empty batch: %d", r.Status)
	}
	og := newGuard(t, newSoc(t))
	r := call(t, "POST", "/api/v1/gate/events/batch", og.Token, map[string]any{"events": []any{ev}})
	l, _ := r.Body["data"].([]any)
	if l[0].(map[string]any)["replayed"] == true || l[0].(map[string]any)["status"] != float64(404) {
		t.Fatalf("another society must not see the first society's event ids: %v", l)
	}
}

func TestPushTokenFollowsThePersonSignedIn(t *testing.T) {
	s := newSoc(t)
	_, units := setupBlock(t, s, "A", 1, 2)
	r1, p1 := newResident(t, s, units["A101"], "owner")
	r2, p2 := newResident(t, s, units["A102"], "owner")
	g := newGuard(t, s)
	shared := "shared-phone-token"
	call(t, "PUT", "/api/v1/me/devices", r1, map[string]any{"device_id": "phone", "platform": "android", "fcm_token": shared, "app": "living"})
	// a second person signs in on the same phone: the first one must stop receiving pushes
	call(t, "PUT", "/api/v1/me/devices", r2, map[string]any{"device_id": "phone", "platform": "android", "fcm_token": shared, "app": "living"})
	walkIn(t, g, units["A101"], "For Flat One")
	if n := len(pushRec.to(shared)); n != 0 {
		t.Fatalf("previous owner of the phone still gets pushes: %d", n)
	}
	walkIn(t, g, units["A102"], "For Flat Two")
	if n := len(pushRec.to(shared)); n != 1 {
		t.Fatalf("current owner: %d", n)
	}
	// logout clears the token
	if r := call(t, "DELETE", "/api/v1/me/devices/phone", r2, nil); r.Status != 204 {
		t.Fatalf("clear: %d", r.Status)
	}
	walkIn(t, g, units["A102"], "After Logout")
	if n := len(pushRec.to(shared)); n != 1 {
		t.Fatalf("push after logout: %d", n)
	}
	if r := call(t, "PUT", "/api/v1/me/devices", r1, map[string]any{"device_id": "x", "app": "toaster"}); r.Status != 400 {
		t.Fatalf("bad app: %d", r.Status)
	}
	_, _ = p1, p2
}
