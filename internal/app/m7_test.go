package app_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

var (
	pdfBytes = append([]byte("%PDF-1.4\n"), bytes.Repeat([]byte("x"), 300)...)
	pngBytes = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{2}, 300)...)
)

// community is a society with two blocks and a resident, family member and guard.
type community struct {
	s              soc
	blockA, blockB string
	units          map[string]string
	rA, rB, famA   string // owner of A101, owner of B101, family of A101
	guard          guardUser
}

func newCommunity(t *testing.T) community {
	t.Helper()
	c := community{s: newSoc(t)}
	var ua, ub map[string]string
	c.blockA, ua = setupBlock(t, c.s, "A", 1, 2)
	c.blockB, ub = setupBlock(t, c.s, "B", 1, 2)
	c.units = map[string]string{}
	for k, v := range ua {
		c.units[k] = v
	}
	for k, v := range ub {
		c.units[k] = v
	}
	c.rA, _ = newResident(t, c.s, c.units["A101"], "owner")
	c.rB, _ = newResident(t, c.s, c.units["B101"], "owner")
	c.famA, _ = newResident(t, c.s, c.units["A101"], "family")
	c.guard = newGuard(t, c.s)
	return c
}

func publishedNotice(t *testing.T, c community, body map[string]any) resp {
	t.Helper()
	b := map[string]any{"title": "Water supply cut", "body": "No water 10-12 on Sunday.", "category": "maintenance", "status": "published"}
	for k, v := range body {
		b[k] = v
	}
	return call(t, "POST", "/api/v1/society/notices", c.s.Admin, b)
}

func titles(t *testing.T, tok, path string) []string {
	t.Helper()
	rows, r := callList(t, path, tok)
	if r.Status != 200 {
		t.Fatalf("%s: %d %v", path, r.Status, r.Body)
	}
	var out []string
	for _, row := range rows {
		out = append(out, row["title"].(string))
	}
	return out
}

// ── notices ──────────────────────────────────────────────────────────────────

func TestNoticeTargetingAndLifecycle(t *testing.T) {
	c := newCommunity(t)
	wA := dialWS(t, c.rA)

	// drafts stay with the office
	d := call(t, "POST", "/api/v1/society/notices", c.s.Admin, map[string]any{"title": "Draft only", "body": "later", "category": "general"})
	if d.Status != 201 || d.vstatus() != "draft" {
		t.Fatalf("draft: %d %v", d.Status, d.Body)
	}
	if len(titles(t, c.rA, "/api/v1/notices")) != 0 || len(titles(t, c.guard.Token, "/api/v1/notices")) != 0 {
		t.Fatal("a draft leaked")
	}
	if p := call(t, "POST", "/api/v1/society/notices/"+d.str("id")+"/publish", c.s.Admin, nil); p.Status != 200 || p.vstatus() != "published" {
		t.Fatalf("publish: %d %v", p.Status, p.Body)
	}
	if p := call(t, "POST", "/api/v1/society/notices/"+d.str("id")+"/publish", c.s.Admin, nil); p.Status != 409 {
		t.Fatalf("publish twice: %d", p.Status)
	}
	wA.expect(t, "notice.published")
	if n := wA.note(2 * time.Second); n == nil || n["type"] != "notice.new" || n["body"] != "Draft only" {
		t.Fatalf("notification: %v", n)
	}

	// block targeting
	onlyA := publishedNotice(t, c, map[string]any{"title": "Block A lift service", "all_blocks": false, "block_ids": []string{c.blockA}})
	if onlyA.Status != 201 || len(onlyA.data()["block_codes"].([]any)) != 1 {
		t.Fatalf("targeted: %d %v", onlyA.Status, onlyA.Body)
	}
	if got := titles(t, c.rA, "/api/v1/notices"); len(got) != 2 {
		t.Fatalf("block A resident: %v", got)
	}
	if got := titles(t, c.famA, "/api/v1/notices"); len(got) != 2 {
		t.Fatalf("family of A: %v", got)
	}
	if got := titles(t, c.rB, "/api/v1/notices"); len(got) != 1 || got[0] != "Draft only" {
		t.Fatalf("block B resident: %v", got)
	}
	if r := call(t, "GET", "/api/v1/notices/"+onlyA.str("id"), c.rB, nil); r.Status != 404 {
		t.Fatalf("B reads A's notice: %d", r.Status)
	}
	if got := titles(t, c.guard.Token, "/api/v1/notices"); len(got) != 2 {
		t.Fatalf("guards read every published notice: %v", got)
	}
	// B's resident got no notification for A's notice
	for _, it := range inbox(t, c.rB, "") {
		if it["body"] == "Block A lift service" {
			t.Fatal("block B was notified")
		}
	}
	other := newSoc(t)
	if got, _ := callList(t, "/api/v1/notices", other.Admin); len(got) != 0 {
		t.Fatalf("other society: %v", got)
	}

	// validation
	for name, body := range map[string]map[string]any{
		"category":       {"title": "x", "body": "y", "category": "gossip"},
		"empty title":    {"title": " ", "body": "y", "category": "general"},
		"past schedule":  {"title": "x", "body": "y", "category": "general", "status": "scheduled", "publish_at": time.Now().Add(-time.Hour).Format(time.RFC3339)},
		"no blocks":      {"title": "x", "body": "y", "category": "general", "all_blocks": false},
		"unknown block":  {"title": "x", "body": "y", "category": "general", "all_blocks": false, "block_ids": []string{uuid.NewString()}},
		"foreign block":  {"title": "x", "body": "y", "category": "general", "all_blocks": false, "block_ids": []string{newCommunity(t).blockA}},
		"bad priority":   {"title": "x", "body": "y", "category": "general", "priority": "panic"},
		"bad status":     {"title": "x", "body": "y", "category": "general", "status": "archived"},
		"expiry earlier": {"title": "x", "body": "y", "category": "general", "status": "published", "expires_at": time.Now().Add(-time.Hour).Format(time.RFC3339)},
	} {
		if r := call(t, "POST", "/api/v1/society/notices", c.s.Admin, body); r.Status != 400 {
			t.Fatalf("%s: %d %v", name, r.Status, r.Body)
		}
	}

	// roles
	for name, tok := range map[string]string{"resident": c.rA, "guard": c.guard.Token} {
		if r := call(t, "GET", "/api/v1/society/notices", tok, nil); r.Status != 403 {
			t.Fatalf("%s lists the office view: %d", name, r.Status)
		}
		if r := call(t, "POST", "/api/v1/society/notices", tok, map[string]any{"title": "x", "body": "y", "category": "general"}); r.Status != 403 {
			t.Fatalf("%s posts: %d", name, r.Status)
		}
	}
	if r := call(t, "GET", "/api/v1/society/notices/"+onlyA.str("id"), other.Admin, nil); r.Status != 404 {
		t.Fatalf("other society's office: %d", r.Status)
	}

	// editing keeps it published and marks it edited; archiving hides it
	id := onlyA.str("id")
	e := call(t, "PUT", "/api/v1/society/notices/"+id, c.s.Admin, map[string]any{"title": "Block A lift service (moved)", "body": "Now Monday.", "category": "maintenance", "status": "published", "all_blocks": false, "block_ids": []string{c.blockA}})
	if e.Status != 200 || e.data()["edited_at"] == nil {
		t.Fatalf("edit: %d %v", e.Status, e.Body)
	}
	if r := call(t, "PUT", "/api/v1/society/notices/"+id, c.s.Admin, map[string]any{"title": "x", "body": "y", "category": "general", "status": "draft"}); r.Status != 409 {
		t.Fatalf("unpublish: %d", r.Status)
	}
	if r := call(t, "DELETE", "/api/v1/society/notices/"+id, c.s.Admin, nil); r.Status != 204 {
		t.Fatalf("archive: %d", r.Status)
	}
	if got := titles(t, c.rA, "/api/v1/notices"); len(got) != 1 {
		t.Fatalf("after archive: %v", got)
	}
	if r := call(t, "DELETE", "/api/v1/society/notices/"+id, c.s.Admin, nil); r.Status != 404 {
		t.Fatalf("archive twice: %d", r.Status)
	}

	// the switch
	mustExec(t, context.Background(), `INSERT INTO society_features (society_id, feature_key, enabled) VALUES ($1,'community',false)`, c.s.ID)
	for name, r := range map[string]resp{"list": call(t, "GET", "/api/v1/notices", c.rA, nil), "office": call(t, "GET", "/api/v1/society/notices", c.s.Admin, nil)} {
		if r.Status != 403 || r.errCode() != "FEATURE_DISABLED" {
			t.Fatalf("%s with the service off: %d %v", name, r.Status, r.Body)
		}
	}
}

func TestNoticePinningScheduleExpiryAndReads(t *testing.T) {
	c := newCommunity(t)
	ctx := context.Background()

	pins := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		r := publishedNotice(t, c, map[string]any{"title": fmt.Sprintf("Pinned %d", i), "pinned": true})
		if r.Status != 201 {
			t.Fatalf("pin %d: %d %v", i, r.Status, r.Body)
		}
		pins = append(pins, r.str("id"))
	}
	if r := publishedNotice(t, c, map[string]any{"title": "Fourth pin", "pinned": true}); r.Status != 409 || r.errCode() != "PIN_LIMIT" {
		t.Fatalf("pin cap: %d %v", r.Status, r.Body)
	}
	plain := publishedNotice(t, c, map[string]any{"title": "Plain"})
	got := titles(t, c.rA, "/api/v1/notices")
	if len(got) != 4 || got[3] != "Plain" {
		t.Fatalf("pinned notices come first: %v", got)
	}
	// unpinning one frees a slot
	call(t, "PUT", "/api/v1/society/notices/"+pins[0], c.s.Admin, map[string]any{"title": "Pinned 0", "body": "b", "category": "general", "status": "published", "pinned": false})
	if r := publishedNotice(t, c, map[string]any{"title": "Now pinned", "pinned": true}); r.Status != 201 {
		t.Fatalf("pin after unpin: %d", r.Status)
	}

	// scheduled notices are invisible until their time, then published once and announced once
	at := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	sch := call(t, "POST", "/api/v1/society/notices", c.s.Admin, map[string]any{"title": "Scheduled", "body": "soon", "category": "event", "status": "scheduled", "publish_at": at})
	if sch.Status != 201 || sch.vstatus() != "scheduled" {
		t.Fatalf("schedule: %d %v", sch.Status, sch.Body)
	}
	for _, x := range titles(t, c.rA, "/api/v1/notices") {
		if x == "Scheduled" {
			t.Fatal("scheduled notice is visible early")
		}
	}
	if n, _ := c.noticesSweep(ctx); n != 0 {
		t.Fatalf("swept early: %d", n)
	}
	mustExec(t, ctx, `UPDATE notices SET publish_at = now() - interval '1 minute' WHERE id=$1`, sch.str("id"))
	before := len(inbox(t, c.rA, ""))
	if n, err := c.noticesSweep(ctx); err != nil || n != 1 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	if n, _ := c.noticesSweep(ctx); n != 0 {
		t.Fatalf("published twice: %d", n)
	}
	if len(inbox(t, c.rA, "")) != before+1 {
		t.Fatal("a scheduled notice must notify exactly once")
	}
	seen := false
	for _, x := range titles(t, c.rA, "/api/v1/notices") {
		seen = seen || x == "Scheduled"
	}
	if !seen {
		t.Fatal("scheduled notice never appeared")
	}

	// expiry hides it
	mustExec(t, ctx, `UPDATE notices SET expires_at = now() - interval '1 minute' WHERE id=$1`, plain.str("id"))
	for _, x := range titles(t, c.rA, "/api/v1/notices") {
		if x == "Plain" {
			t.Fatal("expired notice is visible")
		}
	}

	// read tracking: per person for the dot, per flat for the office
	id := pins[1]
	if cnt := call(t, "GET", "/api/v1/notices/unread-count", c.rA, nil); cnt.data()["unread"] == float64(0) {
		t.Fatalf("unread: %v", cnt.Body)
	}
	rows, _ := callList(t, "/api/v1/notices", c.rA)
	if rows[0]["read"] != false {
		t.Fatalf("read flag: %v", rows[0])
	}
	if r := call(t, "GET", "/api/v1/notices/"+id, c.rA, nil); r.Status != 200 || r.data()["read"] != true {
		t.Fatalf("open: %d %v", r.Status, r.Body)
	}
	call(t, "GET", "/api/v1/notices/"+id, c.famA, nil) // same flat: still one flat
	call(t, "GET", "/api/v1/notices/"+id, c.rB, nil)
	st := call(t, "GET", "/api/v1/society/notices/"+id+"/stats", c.s.Admin, nil)
	if st.Status != 200 || st.data()["read_flats"] != float64(2) || st.data()["total_flats"] != float64(2) {
		t.Fatalf("stats count flats, not people: %d %v", st.Status, st.Body)
	}
	if strings.Contains(fmt.Sprint(st.Body), "full_name") {
		t.Fatal("stats must not expose names")
	}
	if r := call(t, "GET", "/api/v1/society/notices/"+id+"/stats", c.rA, nil); r.Status != 403 {
		t.Fatalf("resident stats: %d", r.Status)
	}
	unreadList := titles(t, c.rA, "/api/v1/notices?unread=1")
	for _, x := range unreadList {
		if x == "Pinned 1" {
			t.Fatal("a read notice is still listed as unread")
		}
	}
}

func (c community) noticesSweep(ctx context.Context) (int, error) { return anotices.Sweep(ctx) }

func TestNoticeAttachments(t *testing.T) {
	c := newCommunity(t)
	n := call(t, "POST", "/api/v1/society/notices", c.s.Admin, map[string]any{"title": "With files", "body": "see attached", "category": "event", "all_blocks": false, "block_ids": []string{c.blockA}})
	id := n.str("id")
	up := func(content []byte) resp {
		return uploadPhoto(t, "/api/v1/society/notices/"+id+"/attachments", c.s.Admin, content)
	}

	if r := uploadPhoto(t, "/api/v1/society/notices/"+id+"/attachments", c.rA, pdfBytes); r.Status != 403 {
		t.Fatalf("resident uploads: %d", r.Status)
	}
	if r := up([]byte("<script>alert(1)</script>")); r.Status != 400 || r.errCode() != "BAD_FILE" {
		t.Fatalf("not a file: %d %v", r.Status, r.Body)
	}
	if r := up(append([]byte("%PDF-1.4\n"), bytes.Repeat([]byte("z"), 6<<20)...)); r.Status != 400 {
		t.Fatalf("oversize: %d", r.Status)
	}
	pdf := up(pdfBytes)
	if pdf.Status != 201 || pdf.str("content_type") != "application/pdf" {
		t.Fatalf("pdf: %d %v", pdf.Status, pdf.Body)
	}
	if r := up(pngBytes); r.Status != 201 {
		t.Fatalf("png: %d", r.Status)
	}
	for i := 0; i < 3; i++ {
		if r := up(jpeg); r.Status != 201 {
			t.Fatalf("jpeg %d: %d", i, r.Status)
		}
	}
	if r := up(jpeg); r.Status != 409 || r.errCode() != "TOO_MANY_FILES" {
		t.Fatalf("sixth file: %d %v", r.Status, r.Body)
	}

	// a draft is not downloadable by residents; once published only its audience can open it
	aid := pdf.str("id")
	path := "/api/v1/notices/" + id + "/attachments/" + aid
	if st, _, _ := rawGet(t, path, c.rA); st != 404 {
		t.Fatalf("draft attachment: %d", st)
	}
	if st, _, body := rawGet(t, "/api/v1/society/notices/"+id+"/attachments/"+aid, c.s.Admin); st != 200 || !bytes.Equal(body, pdfBytes) {
		t.Fatalf("office reads its draft file: %d", st)
	}
	call(t, "POST", "/api/v1/society/notices/"+id+"/publish", c.s.Admin, nil)
	st, ct, body := rawGet(t, path, c.rA)
	if st != 200 || ct != "application/pdf" || !bytes.Equal(body, pdfBytes) {
		t.Fatalf("audience downloads: %d %s", st, ct)
	}
	if st, _, _ := rawGet(t, path, c.rB); st != 404 {
		t.Fatalf("other block downloads: %d", st)
	}
	if st, _, _ := rawGet(t, path, newSoc(t).Admin); st != 404 {
		t.Fatalf("other society downloads: %d", st)
	}
	if st, _, _ := rawGet(t, "/api/v1/notices/"+id+"/attachments/"+uuid.NewString(), c.rA); st != 404 {
		t.Fatalf("unknown file: %d", st)
	}
	if r := call(t, "GET", "/api/v1/notices/"+id, c.rA, nil); r.Status != 200 || len(r.data()["attachments"].([]any)) != 5 {
		t.Fatalf("attachments listed: %v", r.Body)
	}
	// removing one frees a slot
	if r := call(t, "DELETE", "/api/v1/society/notices/"+id+"/attachments/"+aid, c.s.Admin, nil); r.Status != 204 {
		t.Fatalf("remove file: %d", r.Status)
	}
	if r := up(pngBytes); r.Status != 201 {
		t.Fatalf("upload after removal: %d", r.Status)
	}
}

// ── complaints ───────────────────────────────────────────────────────────────

func raiseComplaint(t *testing.T, tok string, extra map[string]any) resp {
	t.Helper()
	b := map[string]any{"title": "Water leakage in bathroom", "description": "Water drips from the ceiling pipe.", "category": "plumbing"}
	for k, v := range extra {
		b[k] = v
	}
	return call(t, "POST", "/api/v1/complaints", tok, b)
}

func officeMemberID(t *testing.T, tok string) string {
	t.Helper()
	rows, r := callList(t, "/api/v1/society/office-members", tok)
	if r.Status != 200 || len(rows) == 0 {
		t.Fatalf("office members: %d %v", r.Status, r.Body)
	}
	return rows[0]["id"].(string)
}

func eventTypes(r resp) []string {
	var out []string
	evs, _ := r.data()["events"].([]any)
	for _, e := range evs {
		out = append(out, e.(map[string]any)["type"].(string))
	}
	return out
}

func TestComplaintLifecycle(t *testing.T) {
	c := newCommunity(t)
	ctx := context.Background()
	wA, wAdmin := dialWS(t, c.rA), dialWS(t, c.s.Admin)

	// validation, and who may raise
	for name, body := range map[string]map[string]any{
		"short title": {"title": "ab"},
		"description": {"description": "no"},
		"category":    {"category": "gossip"},
		"priority":    {"priority": "asap"},
	} {
		if r := raiseComplaint(t, c.rA, body); r.Status != 400 {
			t.Fatalf("%s: %d %v", name, r.Status, r.Body)
		}
	}
	if r := raiseComplaint(t, c.s.Admin, nil); r.Status != 409 || r.errCode() != "NO_UNIT" {
		t.Fatalf("admin without a flat raises: %d %v", r.Status, r.Body)
	}
	if r := call(t, "POST", "/api/v1/complaints", c.guard.Token, map[string]any{"title": "abc", "description": "hello there", "category": "other"}); r.Status != 403 {
		t.Fatalf("guard raises: %d", r.Status)
	}
	one := raiseComplaint(t, c.rA, map[string]any{"priority": "high"})
	if one.Status != 201 || one.str("code") != "CMP-0001" || one.vstatus() != "open" || one.str("priority") != "high" {
		t.Fatalf("raise: %d %v", one.Status, one.Body)
	}
	id := one.str("id")
	if n := wAdmin.note(2 * time.Second); n == nil || n["type"] != "complaint.new" {
		t.Fatalf("office notified: %v", n)
	}
	wAdmin.expect(t, "complaint.updated")
	fam := raiseComplaint(t, c.famA, map[string]any{"title": "Corridor light out", "category": "electrical"})
	nb := raiseComplaint(t, c.rB, map[string]any{"title": "Loud music", "category": "noise", "priority": "low"})
	if fam.str("code") != "CMP-0002" || nb.str("code") != "CMP-0003" {
		t.Fatalf("numbers run per society: %s %s", fam.str("code"), nb.str("code"))
	}
	if other := raiseComplaint(t, newCommunity(t).rA, nil); other.str("code") != "CMP-0001" {
		t.Fatalf("another society starts at 1: %v", other.Body)
	}

	// flat scoping
	if rows, _ := callList(t, "/api/v1/complaints", c.rA); len(rows) != 2 {
		t.Fatalf("my flat: %v", rows)
	}
	if rows, _ := callList(t, "/api/v1/complaints?group=resolved", c.rA); len(rows) != 0 {
		t.Fatalf("resolved group: %v", rows)
	}
	if r := call(t, "GET", "/api/v1/complaints/"+id, c.rB, nil); r.Status != 404 {
		t.Fatalf("neighbour reads: %d", r.Status)
	}
	if r := call(t, "GET", "/api/v1/complaints/"+id, c.famA, nil); r.Status != 200 {
		t.Fatalf("family of the flat reads: %d", r.Status)
	}
	// residents must never reach the office desk (they hold complaint.read)
	for _, p := range []string{"/api/v1/society/complaints", "/api/v1/society/complaints/stats", "/api/v1/society/complaints/" + id, "/api/v1/society/office-members", "/api/v1/society/complaints/export"} {
		if r := call(t, "GET", p, c.rA, nil); r.Status != 403 {
			t.Fatalf("resident reaches %s: %d", p, r.Status)
		}
		if r := call(t, "GET", p, c.guard.Token, nil); r.Status != 403 {
			t.Fatalf("guard reaches %s: %d", p, r.Status)
		}
	}
	rows, ar := callList(t, "/api/v1/society/complaints", c.s.Admin)
	if ar.Status != 200 || len(rows) != 3 {
		t.Fatalf("office list: %d %v", ar.Status, rows)
	}
	if rows, _ := callList(t, "/api/v1/society/complaints?category=noise", c.s.Admin); len(rows) != 1 {
		t.Fatalf("category filter: %v", rows)
	}
	if rows, _ := callList(t, "/api/v1/society/complaints?block=B&status=active", c.s.Admin); len(rows) != 1 {
		t.Fatalf("block filter: %v", rows)
	}
	if rows, _ := callList(t, "/api/v1/society/complaints?q=cmp-0002", c.s.Admin); len(rows) != 1 {
		t.Fatalf("search by id: %v", rows)
	}
	if r := call(t, "GET", "/api/v1/society/complaints?status=weird", c.s.Admin, nil); r.Status != 400 {
		t.Fatalf("bad filter: %d", r.Status)
	}

	// assignment: member of the office or a vendor, never a stranger
	if r := call(t, "POST", "/api/v1/society/complaints/"+id+"/assign", c.s.Admin, map[string]any{}); r.Status != 400 {
		t.Fatalf("empty assign: %d", r.Status)
	}
	foreign := officeMemberID(t, newSoc(t).Admin)
	if r := call(t, "POST", "/api/v1/society/complaints/"+id+"/assign", c.s.Admin, map[string]any{"membership_id": foreign}); r.Status != 400 {
		t.Fatalf("assign another society's member: %d", r.Status)
	}
	var resMem string
	_ = pool.QueryRow(ctx, `SELECT id::text FROM memberships WHERE society_id=$1 AND role_key='resident_owner' LIMIT 1`, c.s.ID).Scan(&resMem)
	if r := call(t, "POST", "/api/v1/society/complaints/"+id+"/assign", c.s.Admin, map[string]any{"membership_id": resMem}); r.Status != 400 {
		t.Fatalf("assign a resident: %d", r.Status)
	}
	mine := officeMemberID(t, c.s.Admin)
	tgt := time.Now().Add(48 * time.Hour).Format("2006-01-02")
	a := call(t, "POST", "/api/v1/society/complaints/"+id+"/assign", c.s.Admin, map[string]any{"membership_id": mine, "vendor": "Aqua Plumbers 98xxxxxx", "target_date": tgt})
	if a.Status != 200 || a.vstatus() != "assigned" || a.str("assigned_vendor") != "Aqua Plumbers 98xxxxxx" || a.str("target_date") != tgt || a.data()["assigned_to"] == nil {
		t.Fatalf("assign: %d %v", a.Status, a.Body)
	}
	told := false
	for i := 0; i < 5 && !told; i++ { // earlier events of this flat (raised) come first
		if m := wA.next(2 * time.Second); m != nil && m["type"] == "complaint.updated" {
			told = m["data"].(map[string]any)["status"] == "assigned"
		}
	}
	if !told {
		t.Fatal("flat was not told live about the assignment")
	}
	if n := wA.note(2 * time.Second); n == nil || n["type"] != "complaint.assigned" {
		t.Fatalf("flat notified: %v", n)
	}

	// the state machine
	post := func(path string, body map[string]any) resp {
		return call(t, "POST", "/api/v1/society/complaints/"+id+path, c.s.Admin, body)
	}
	for _, bad := range []string{"open", "assigned", "closed", "teleported"} {
		if r := post("/status", map[string]any{"status": bad}); r.Status != 409 {
			t.Fatalf("assigned → %s must be refused: %d", bad, r.Status)
		}
	}
	if r := post("/status", map[string]any{"status": "in_progress", "note": "Plumber is on the way"}); r.Status != 200 || r.vstatus() != "in_progress" {
		t.Fatalf("in progress: %d %v", r.Status, r.Body)
	}
	if r := post("/status", map[string]any{"status": "closed"}); r.Status != 409 {
		t.Fatalf("in progress → closed: %d", r.Status)
	}
	if r := post("/priority", map[string]any{"priority": "urgent"}); r.Status != 200 || r.str("priority") != "urgent" {
		t.Fatalf("priority: %d", r.Status)
	}
	if r := post("/priority", map[string]any{"priority": "whenever"}); r.Status != 400 {
		t.Fatalf("bad priority: %d", r.Status)
	}

	// comments: internal notes stay internal, family only watches
	if r := post("/comments", map[string]any{"text": "Vendor quote is 4,500 — within budget", "internal": true}); r.Status != 200 {
		t.Fatalf("internal note: %d", r.Status)
	}
	before := len(inbox(t, c.rA, ""))
	if r := post("/comments", map[string]any{"text": "We will fix it tomorrow morning."}); r.Status != 200 {
		t.Fatalf("office comment: %d", r.Status)
	}
	if len(inbox(t, c.rA, "")) != before+1 {
		t.Fatal("a public comment notifies the flat, an internal note does not")
	}
	res := call(t, "GET", "/api/v1/complaints/"+id, c.rA, nil)
	raw := fmt.Sprint(res.Body)
	if strings.Contains(raw, "Vendor quote") || strings.Contains(raw, "Society office") == false {
		t.Fatalf("residents must not see internal notes and see the office as one actor: %s", raw)
	}
	if admin := call(t, "GET", "/api/v1/society/complaints/"+id, c.s.Admin, nil); !strings.Contains(fmt.Sprint(admin.Body), "Vendor quote") {
		t.Fatal("the office sees its internal notes")
	}
	if r := call(t, "POST", "/api/v1/complaints/"+id+"/comments", c.famA, map[string]any{"text": "Me too"}); r.Status != 403 {
		t.Fatalf("family comments: %d", r.Status)
	}
	if r := call(t, "POST", "/api/v1/complaints/"+id+"/comments", c.rB, map[string]any{"text": "Me too"}); r.Status != 404 {
		t.Fatalf("neighbour comments: %d", r.Status)
	}
	if r := call(t, "POST", "/api/v1/complaints/"+id+"/comments", c.rA, map[string]any{"text": "Thank you, I will be home.", "internal": true}); r.Status != 200 {
		t.Fatalf("resident comments: %d", r.Status)
	}
	if res := call(t, "GET", "/api/v1/complaints/"+id, c.s.Admin, nil); res.Status != 409 {
		t.Fatalf("the resident routes need a flat: %d", res.Status)
	}
	if strings.Contains(fmt.Sprint(call(t, "GET", "/api/v1/complaints/"+id, c.rA, nil).Body), `"internal":true`) {
		t.Fatal("a resident cannot make a comment internal")
	}

	// resolve → rate, reopen
	if r := post("/status", map[string]any{"status": "resolved", "note": "Pipe replaced"}); r.Status != 200 || r.vstatus() != "resolved" || r.data()["resolved_at"] == nil {
		t.Fatalf("resolve: %d %v", r.Status, r.Body)
	}
	if r := post("/status", map[string]any{"status": "in_progress"}); r.Status != 409 {
		t.Fatalf("resolved → in progress: %d", r.Status)
	}
	if r := post("/assign", map[string]any{"vendor": "X"}); r.Status != 409 {
		t.Fatalf("assign a resolved complaint: %d", r.Status)
	}
	res = call(t, "GET", "/api/v1/complaints/"+id, c.rA, nil)
	if res.data()["can_reopen"] != true || res.data()["can_rate"] != true {
		t.Fatalf("flags: %v", res.Body)
	}
	if r := call(t, "POST", "/api/v1/complaints/"+id+"/reopen", c.famA, map[string]any{"reason": "still wet"}); r.Status != 403 {
		t.Fatalf("family reopens: %d", r.Status)
	}
	if r := call(t, "POST", "/api/v1/complaints/"+id+"/reopen", c.rA, map[string]any{"reason": "x"}); r.Status != 400 {
		t.Fatalf("reopen without a reason: %d", r.Status)
	}
	if r := call(t, "POST", "/api/v1/complaints/"+id+"/rate", c.rA, map[string]any{"rating": 0}); r.Status != 400 {
		t.Fatalf("rating 0: %d", r.Status)
	}
	if r := call(t, "POST", "/api/v1/complaints/"+id+"/rate", c.rA, map[string]any{"rating": 4, "comment": "Quick work"}); r.Status != 200 || r.data()["rating"] != float64(4) {
		t.Fatalf("rate: %d %v", r.Status, r.Body)
	}
	if r := call(t, "POST", "/api/v1/complaints/"+id+"/rate", c.rA, map[string]any{"rating": 5}); r.Status != 409 || r.errCode() != "ALREADY_RATED" {
		t.Fatalf("rate twice: %d %v", r.Status, r.Body)
	}
	mustExec(t, ctx, `UPDATE complaints SET resolved_at = now() - interval '8 days' WHERE id=$1`, id)
	if r := call(t, "POST", "/api/v1/complaints/"+id+"/reopen", c.rA, map[string]any{"reason": "It is leaking again"}); r.Status != 409 || r.errCode() != "REOPEN_WINDOW_OVER" {
		t.Fatalf("late reopen: %d %v", r.Status, r.Body)
	}
	mustExec(t, ctx, `UPDATE complaints SET resolved_at = now() - interval '1 day' WHERE id=$1`, id)
	re := call(t, "POST", "/api/v1/complaints/"+id+"/reopen", c.rA, map[string]any{"reason": "It is leaking again"})
	if re.Status != 200 || re.vstatus() != "open" || re.data()["resolved_at"] != nil {
		t.Fatalf("reopen: %d %v", re.Status, re.Body)
	}
	if r := call(t, "POST", "/api/v1/complaints/"+id+"/reopen", c.rA, map[string]any{"reason": "again"}); r.Status != 409 {
		t.Fatalf("reopen an open complaint: %d", r.Status)
	}
	types := eventTypes(call(t, "GET", "/api/v1/society/complaints/"+id, c.s.Admin, nil))
	for _, want := range []string{"raised", "assigned", "status", "priority", "comment", "rated", "reopened"} {
		found := false
		for _, x := range types {
			found = found || x == want
		}
		if !found {
			t.Fatalf("timeline lacks %q: %v", want, types)
		}
	}

	// stats
	st := call(t, "GET", "/api/v1/society/complaints/stats", c.s.Admin, nil)
	if st.Status != 200 || st.data()["open"] != float64(3) || st.data()["avg_rating"] != float64(4) || st.data()["rated"] != float64(1) {
		t.Fatalf("stats: %d %v", st.Status, st.Body)
	}
	if st.data()["by_category"].(map[string]any)["plumbing"] != float64(1) {
		t.Fatalf("by category: %v", st.Body)
	}
	// switch
	mustExec(t, ctx, `INSERT INTO society_features (society_id, feature_key, enabled) VALUES ($1,'complaints',false)`, c.s.ID)
	for name, r := range map[string]resp{"raise": raiseComplaint(t, c.rA, nil), "list": call(t, "GET", "/api/v1/complaints", c.rA, nil), "office": call(t, "GET", "/api/v1/society/complaints", c.s.Admin, nil)} {
		if r.Status != 403 || r.errCode() != "FEATURE_DISABLED" {
			t.Fatalf("%s with the service off: %d %v", name, r.Status, r.Body)
		}
	}
}

func TestComplaintPhotosCommonAreaAndSweepers(t *testing.T) {
	c := newCommunity(t)
	ctx := context.Background()
	mine := raiseComplaint(t, c.rA, nil)
	id := mine.str("id")
	up := func(tok string, content []byte) resp {
		return uploadPhoto(t, "/api/v1/complaints/"+id+"/photos", tok, content)
	}

	if r := up(c.rB, jpeg); r.Status != 404 {
		t.Fatalf("neighbour uploads: %d", r.Status)
	}
	if r := up(c.famA, jpeg); r.Status != 403 {
		t.Fatalf("family uploads: %d", r.Status)
	}
	if r := up(c.rA, []byte("not an image")); r.Status != 400 {
		t.Fatalf("bad photo: %d", r.Status)
	}
	first := up(c.rA, jpeg)
	if first.Status != 201 {
		t.Fatalf("photo: %d %v", first.Status, first.Body)
	}
	up(c.rA, pngBytes)
	up(c.rA, jpeg)
	if r := up(c.rA, jpeg); r.Status != 409 {
		t.Fatalf("fourth photo: %d", r.Status)
	}
	path := "/api/v1/complaints/" + id + "/attachments/" + first.str("id")
	if st, ct, body := rawGet(t, path, c.rA); st != 200 || ct != "image/jpeg" || !bytes.Equal(body, jpeg) {
		t.Fatalf("flat downloads: %d %s", st, ct)
	}
	if st, _, _ := rawGet(t, path, c.famA); st != 200 {
		t.Fatalf("family downloads: %d", st)
	}
	if st, _, _ := rawGet(t, path, c.rB); st != 404 {
		t.Fatalf("neighbour downloads: %d", st)
	}
	if st, _, _ := rawGet(t, "/api/v1/society/complaints/"+id+"/attachments/"+first.str("id"), c.s.Admin); st != 200 {
		t.Fatalf("office downloads: %d", st)
	}
	if st, _, _ := rawGet(t, "/api/v1/society/complaints/"+id+"/attachments/"+first.str("id"), newSoc(t).Admin); st != 404 {
		t.Fatalf("other society downloads: %d", st)
	}

	// common-area complaints form a public board without names, flats, descriptions or photos
	common := raiseComplaint(t, c.rA, map[string]any{"title": "Lift lobby is dirty", "category": "cleaning", "common_area": true})
	board, br := callList(t, "/api/v1/complaints/common", c.rB)
	if br.Status != 200 || len(board) != 1 || board[0]["title"] != "Lift lobby is dirty" {
		t.Fatalf("board: %d %v", br.Status, board)
	}
	for _, k := range []string{"flat", "raised_by", "description", "unit_id", "photos"} {
		if v, ok := board[0][k]; ok && v != nil && v != "" {
			if arr, isArr := v.([]any); !isArr || len(arr) > 0 {
				t.Fatalf("board leaks %s: %v", k, v)
			}
		}
	}
	if r := call(t, "GET", "/api/v1/complaints/"+common.str("id"), c.rB, nil); r.Status != 404 {
		t.Fatalf("other flat opens a common complaint: %d", r.Status)
	}
	if rows, _ := callList(t, "/api/v1/complaints/common", newSoc(t).Admin); len(rows) != 0 {
		t.Fatalf("other society's board: %v", rows)
	}

	// auto-close after the reopen window, with a timeline entry
	call(t, "POST", "/api/v1/society/complaints/"+id+"/status", c.s.Admin, map[string]any{"status": "resolved"})
	mustExec(t, ctx, `UPDATE complaints SET resolved_at = now() - interval '8 days' WHERE id=$1`, id)
	if n, err := acomplaints.AutoClose(ctx); err != nil || n != 1 {
		t.Fatalf("auto-close: %d %v", n, err)
	}
	r := call(t, "GET", "/api/v1/complaints/"+id, c.rA, nil)
	if r.vstatus() != "closed" || r.data()["can_reopen"] == true {
		t.Fatalf("closed: %v", r.Body)
	}
	if got := eventTypes(r); got[len(got)-1] != "auto_closed" {
		t.Fatalf("timeline: %v", got)
	}
	if x := call(t, "POST", "/api/v1/complaints/"+id+"/comments", c.rA, map[string]any{"text": "hello"}); x.Status != 409 {
		t.Fatalf("comment on a closed complaint: %d", x.Status)
	}
	if x := call(t, "POST", "/api/v1/society/complaints/"+id+"/priority", c.s.Admin, map[string]any{"priority": "low"}); x.Status != 409 {
		t.Fatalf("priority of a closed complaint: %d", x.Status)
	}

	// overdue: flagged, and the office gets one digest a day
	late := raiseComplaint(t, c.rB, map[string]any{"title": "Overdue complaint", "priority": "urgent"})
	mustExec(t, ctx, `UPDATE complaints SET created_at = now() - interval '3 days' WHERE id=$1`, late.str("id"))
	if rows, _ := callList(t, "/api/v1/society/complaints?overdue=1", c.s.Admin); len(rows) != 1 || rows[0]["overdue"] != true {
		t.Fatalf("overdue filter: %v", rows)
	}
	if call(t, "GET", "/api/v1/society/complaints/stats", c.s.Admin, nil).data()["overdue"] != float64(1) {
		t.Fatal("overdue stat")
	}
	n0 := len(inbox(t, c.s.Admin, ""))
	if n, _ := acomplaints.Digest(ctx); n != 1 {
		t.Fatalf("digest: %d", n)
	}
	if n, _ := acomplaints.Digest(ctx); n != 0 {
		t.Fatalf("digest twice a day: %d", n)
	}
	items := inbox(t, c.s.Admin, "")
	if len(items) != n0+1 || items[0]["type"] != "complaint.overdue" {
		t.Fatalf("digest notification: %v", items[:1])
	}

	// export is formula-safe and office-only
	raiseComplaint(t, c.rA, map[string]any{"title": "=HYPERLINK(\"http://evil\")", "category": "other"})
	st, _, body := rawGet(t, "/api/v1/society/complaints/export", c.s.Admin)
	if st != 200 || !strings.HasPrefix(string(body), "ID,Title,Category") || !strings.Contains(string(body), "'=HYPERLINK") || strings.Contains(string(body), ",=HYPERLINK") {
		t.Fatalf("export: %d %s", st, body)
	}
	if st, _, _ := rawGet(t, "/api/v1/society/complaints/export", c.rA); st != 403 {
		t.Fatalf("resident exports: %d", st)
	}
}

// ── polls ────────────────────────────────────────────────────────────────────

func createPoll(t *testing.T, c community, extra map[string]any) resp {
	t.Helper()
	b := map[string]any{"question": "Paint the lobby?", "options": []string{"Yes", "No", "Later"}}
	for k, v := range extra {
		b[k] = v
	}
	return call(t, "POST", "/api/v1/society/polls", c.s.Admin, b)
}

func optionIDs(r resp) []string {
	var out []string
	for _, o := range r.data()["options"].([]any) {
		out = append(out, o.(map[string]any)["id"].(string))
	}
	return out
}

func votes(r resp) []float64 {
	var out []float64
	for _, o := range r.data()["options"].([]any) {
		v, _ := o.(map[string]any)["votes"].(float64)
		out = append(out, v)
	}
	return out
}

func TestPollVotingAndResults(t *testing.T) {
	c := newCommunity(t)
	tenantA, _ := newResident(t, c.s, c.units["A101"], "tenant")

	for name, body := range map[string]map[string]any{
		"one option": {"options": []string{"Only"}},
		"nine":       {"options": []string{"1", "2", "3", "4", "5", "6", "7", "8", "9"}},
		"duplicates": {"options": []string{"Yes", "yes"}},
		"question":   {"question": "x"},
		"closing":    {"closes_at": time.Now().Add(-time.Hour).Format(time.RFC3339)},
		"blocks":     {"all_blocks": false},
	} {
		if r := createPoll(t, c, body); r.Status != 400 {
			t.Fatalf("%s: %d %v", name, r.Status, r.Body)
		}
	}
	p := createPoll(t, c, nil)
	if p.Status != 201 || p.vstatus() != "open" || p.data()["anonymous"] != true {
		t.Fatalf("create: %d %v", p.Status, p.Body)
	}
	id := p.str("id")
	opts := optionIDs(p)
	if n := inbox(t, c.rA, ""); len(n) == 0 || n[0]["type"] != "poll.new" {
		t.Fatalf("announcement: %v", n)
	}

	// roles
	if r := call(t, "GET", "/api/v1/polls", c.guard.Token, nil); r.Status != 403 {
		t.Fatalf("guard reads polls: %d", r.Status)
	}
	if r := call(t, "POST", "/api/v1/society/polls", c.rA, map[string]any{"question": "Hack?", "options": []string{"a", "b"}}); r.Status != 403 {
		t.Fatalf("resident creates: %d", r.Status)
	}
	if r := call(t, "POST", "/api/v1/polls/"+id+"/vote", c.famA, map[string]any{"option_ids": []string{opts[0]}}); r.Status != 403 {
		t.Fatalf("family votes: %d", r.Status)
	}
	if r := call(t, "POST", "/api/v1/polls/"+id+"/vote", c.s.Admin, map[string]any{"option_ids": []string{opts[0]}}); r.Status != 409 {
		t.Fatalf("admin without a flat votes: %d", r.Status)
	}
	// family can read; results stay hidden until the flat has voted
	fam := call(t, "GET", "/api/v1/polls/"+id, c.famA, nil)
	if fam.Status != 200 || fam.data()["can_vote"] != false || fam.data()["results_visible"] != false {
		t.Fatalf("family view: %d %v", fam.Status, fam.Body)
	}
	if strings.Contains(fmt.Sprint(fam.data()["options"]), "votes") {
		t.Fatal("counts leaked before voting")
	}

	// validation of the vote itself
	for name, ids := range map[string][]string{"none": {}, "two for a single choice": {opts[0], opts[1]}, "unknown": {uuid.NewString()}} {
		if r := call(t, "POST", "/api/v1/polls/"+id+"/vote", c.rA, map[string]any{"option_ids": ids}); r.Status != 400 {
			t.Fatalf("%s: %d", name, r.Status)
		}
	}
	v := call(t, "POST", "/api/v1/polls/"+id+"/vote", c.rA, map[string]any{"option_ids": []string{opts[0]}})
	if v.Status != 200 || v.data()["voted"] != true || v.data()["results_visible"] != true || v.data()["voted_flats"] != float64(1) || v.data()["eligible_flats"] != float64(2) {
		t.Fatalf("vote: %d %v", v.Status, v.Body)
	}
	if got := votes(v); got[0] != 1 || got[1] != 0 {
		t.Fatalf("tally: %v", got)
	}
	// one vote per flat: the tenant of the same flat replaces it, the flat still counts once
	v = call(t, "POST", "/api/v1/polls/"+id+"/vote", tenantA, map[string]any{"option_ids": []string{opts[1]}})
	if got := votes(v); got[0] != 0 || got[1] != 1 || v.data()["voted_flats"] != float64(1) {
		t.Fatalf("one vote per flat: %v %v", got, v.Body)
	}
	// the other flat votes; turnout is by flat
	call(t, "POST", "/api/v1/polls/"+id+"/vote", c.rB, map[string]any{"option_ids": []string{opts[1]}})
	d := call(t, "GET", "/api/v1/society/polls/"+id, c.s.Admin, nil)
	if got := votes(d); got[1] != 2 || d.data()["voted_flats"] != float64(2) {
		t.Fatalf("office results: %v", d.Body)
	}
	if o := d.data()["options"].([]any)[1].(map[string]any); o["percent"] != float64(100) {
		t.Fatalf("percent: %v", o)
	}
	// anonymous: nobody can tell who voted
	if d.data()["voters"] != nil || strings.Contains(fmt.Sprint(call(t, "GET", "/api/v1/polls/"+id, c.rB, nil).Body), "A-A101") {
		t.Fatal("an anonymous poll exposes voters")
	}

	// closing: no more votes, results for everybody, other poll untouched
	if r := call(t, "POST", "/api/v1/society/polls/"+id+"/close", c.rA, nil); r.Status != 403 {
		t.Fatalf("resident closes: %d", r.Status)
	}
	if r := call(t, "POST", "/api/v1/society/polls/"+id+"/close", c.s.Admin, nil); r.Status != 200 || r.vstatus() != "closed" {
		t.Fatalf("close: %d %v", r.Status, r.Body)
	}
	if r := call(t, "POST", "/api/v1/society/polls/"+id+"/close", c.s.Admin, nil); r.Status != 409 {
		t.Fatalf("close twice: %d", r.Status)
	}
	if r := call(t, "POST", "/api/v1/polls/"+id+"/vote", c.rB, map[string]any{"option_ids": []string{opts[0]}}); r.Status != 409 || r.errCode() != "POLL_CLOSED" {
		t.Fatalf("vote after close: %d %v", r.Status, r.Body)
	}
	closed := call(t, "GET", "/api/v1/polls/"+id, c.famA, nil)
	if closed.data()["results_visible"] != true || votes(closed)[1] != 2 {
		t.Fatalf("results after close: %v", closed.Body)
	}
	st, _, body := rawGet(t, "/api/v1/society/polls/"+id+"/export", c.s.Admin)
	if st != 200 || !strings.Contains(string(body), "Turnout,2 of 2 flats") || strings.Contains(string(body), "Flat,Voted for") {
		t.Fatalf("export: %d %s", st, body)
	}
	if rows, _ := callList(t, "/api/v1/society/polls", c.s.Admin); len(rows) != 1 {
		t.Fatalf("office list: %v", rows)
	}
	if r := call(t, "DELETE", "/api/v1/society/polls/"+id, c.s.Admin, nil); r.Status != 204 {
		t.Fatalf("archive: %d", r.Status)
	}
	if rows, _ := callList(t, "/api/v1/polls", c.rA); len(rows) != 0 {
		t.Fatalf("archived poll still listed: %v", rows)
	}
}

func TestPollTargetingMultiChoiceEditingAndSweeper(t *testing.T) {
	c := newCommunity(t)
	ctx := context.Background()

	// block targeting + a poll that names its voters to the office only
	blockPoll := createPoll(t, c, map[string]any{"question": "Block A parking rules?", "all_blocks": false, "block_ids": []string{c.blockA}, "anonymous": false, "multi": true})
	if blockPoll.Status != 201 || blockPoll.data()["anonymous"] != false {
		t.Fatalf("create: %d %v", blockPoll.Status, blockPoll.Body)
	}
	id := blockPoll.str("id")
	opts := optionIDs(blockPoll)
	if rows, _ := callList(t, "/api/v1/polls", c.rB); len(rows) != 0 {
		t.Fatalf("block B sees block A's poll: %v", rows)
	}
	if r := call(t, "GET", "/api/v1/polls/"+id, c.rB, nil); r.Status != 404 {
		t.Fatalf("block B opens it: %d", r.Status)
	}
	if r := call(t, "POST", "/api/v1/polls/"+id+"/vote", c.rB, map[string]any{"option_ids": []string{opts[0]}}); r.Status != 404 {
		t.Fatalf("block B votes: %d", r.Status)
	}
	for _, it := range inbox(t, c.rB, "") {
		if it["type"] == "poll.new" {
			t.Fatal("block B was told about block A's poll")
		}
	}
	if d := call(t, "GET", "/api/v1/society/polls/"+id, c.s.Admin, nil); d.data()["eligible_flats"] != float64(1) {
		t.Fatalf("eligible flats follow the audience: %v", d.Body)
	}
	// multi-choice accepts several answers
	v := call(t, "POST", "/api/v1/polls/"+id+"/vote", c.rA, map[string]any{"option_ids": []string{opts[0], opts[2]}})
	if got := votes(v); v.Status != 200 || got[0] != 1 || got[1] != 0 || got[2] != 1 {
		t.Fatalf("multi vote: %d %v", v.Status, v.Body)
	}
	d := call(t, "GET", "/api/v1/society/polls/"+id, c.s.Admin, nil)
	voters, _ := d.data()["voters"].([]any)
	if len(voters) != 1 || voters[0].(map[string]any)["flat"] != "A-A101" {
		t.Fatalf("the office sees who voted in a named poll: %v", d.Body)
	}
	if strings.Contains(fmt.Sprint(call(t, "GET", "/api/v1/polls/"+id, c.rA, nil).Body), "voters") {
		t.Fatal("residents must not see voters")
	}
	st, _, body := rawGet(t, "/api/v1/society/polls/"+id+"/export", c.s.Admin)
	if st != 200 || !strings.Contains(string(body), "Flat,Voted for") || !strings.Contains(string(body), "A-A101") {
		t.Fatalf("named export: %d %s", st, body)
	}
	// editing is only possible before the first vote
	if r := call(t, "PUT", "/api/v1/society/polls/"+id, c.s.Admin, map[string]any{"question": "Changed?", "options": []string{"a", "b"}}); r.Status != 409 || r.errCode() != "HAS_VOTES" {
		t.Fatalf("edit after a vote: %d %v", r.Status, r.Body)
	}
	fresh := createPoll(t, c, map[string]any{"question": "Before votes"})
	if r := call(t, "PUT", "/api/v1/society/polls/"+fresh.str("id"), c.s.Admin, map[string]any{"question": "Edited question", "options": []string{"x", "y", "z", "w"}}); r.Status != 200 || len(r.data()["options"].([]any)) != 4 {
		t.Fatalf("edit: %d %v", r.Status, r.Body)
	}

	// time: closes by itself, and flats that have not voted are reminded a day before
	soon := createPoll(t, c, map[string]any{"question": "Closing soon?", "closes_at": time.Now().Add(20 * time.Hour).Format(time.RFC3339)})
	sid := soon.str("id")
	call(t, "POST", "/api/v1/polls/"+sid+"/vote", c.rA, map[string]any{"option_ids": []string{optionIDs(soon)[0]}})
	nA, nB := len(inbox(t, c.rA, "")), len(inbox(t, c.rB, ""))
	if cl, rem, err := apolls.Sweep(ctx); err != nil || cl != 0 || rem != 1 {
		t.Fatalf("reminder sweep: %d %d %v", cl, rem, err)
	}
	if len(inbox(t, c.rA, "")) != nA || len(inbox(t, c.rB, "")) != nB+1 {
		t.Fatal("only the flat that has not voted is reminded")
	}
	if _, rem, _ := apolls.Sweep(ctx); rem != 0 {
		t.Fatalf("reminded twice: %d", rem)
	}
	mustExec(t, ctx, `UPDATE polls SET closes_at = now() - interval '1 minute' WHERE id=$1`, sid)
	if r := call(t, "GET", "/api/v1/polls/"+sid, c.rB, nil); r.vstatus() != "closed" || r.data()["results_visible"] != true {
		t.Fatalf("a poll past its time reads as closed at once: %v", r.Body)
	}
	if r := call(t, "POST", "/api/v1/polls/"+sid+"/vote", c.rB, map[string]any{"option_ids": []string{optionIDs(soon)[1]}}); r.Status != 409 {
		t.Fatalf("vote after the deadline: %d", r.Status)
	}
	if cl, _, _ := apolls.Sweep(ctx); cl != 1 {
		t.Fatalf("sweeper closes it for good: %d", cl)
	}

	// switch
	mustExec(t, ctx, `INSERT INTO society_features (society_id, feature_key, enabled) VALUES ($1,'polls',false)`, c.s.ID)
	for name, r := range map[string]resp{"list": call(t, "GET", "/api/v1/polls", c.rA, nil), "office": call(t, "GET", "/api/v1/society/polls", c.s.Admin, nil)} {
		if r.Status != 403 || r.errCode() != "FEATURE_DISABLED" {
			t.Fatalf("%s with the service off: %d %v", name, r.Status, r.Body)
		}
	}
}
