package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"runtime/pprof"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nexora48/nexora-api/internal/auth"
)

const adminPass = "Admin@123"

func periodOf(offset int) string {
	n := time.Now()
	return time.Date(n.Year(), n.Month()+time.Month(offset), 1, 0, 0, 0, 0, time.UTC).Format("2006-01")
}

// seed creates the society, residents, guards, billing history and bulk historical rows.
func seed(ctx context.Context, c *client, pool *pgxpool.Pool, flats, nVisits, nNotes int) *world {
	flats = (flats / 10) * 10
	if flats < 10 {
		flats = 10
	}
	w := &world{society: uuid.New(), adminUser: uuid.New()}
	hash, err := auth.HashPassword(adminPass)
	must(err)
	_, err = pool.Exec(ctx, `INSERT INTO societies (id, name, slug, walkin_photo_required) VALUES ($1,'Load Test Society',$2,false)`, w.society, "lt-"+w.society.String()[:8])
	must(err)
	email := "admin-" + w.society.String()[:8] + "@load.test"
	_, err = pool.Exec(ctx, `INSERT INTO users (id, full_name, phone, email, password_hash) VALUES ($1,'Admin','9000000001',$2,$3)`, w.adminUser, email, hash)
	must(err)
	_, err = pool.Exec(ctx, `INSERT INTO memberships (user_id, society_id, role_key) VALUES ($1,$2,'society_admin')`, w.adminUser, w.society)
	must(err)
	code, b := c.do("", "POST", "/api/v1/auth/login", "", map[string]any{"email": email, "password": adminPass})
	if code != 200 {
		must(fmt.Errorf("admin login: %d %s", code, b))
	}
	w.admin = fmt.Sprint(data(b)["access_token"])

	code, b = c.do("", "POST", "/api/v1/society/blocks", w.admin, map[string]any{"name": "Tower A", "code": "A"})
	if code != 201 {
		must(fmt.Errorf("block: %d %s", code, b))
	}
	w.block = fmt.Sprint(data(b)["id"])
	code, b = c.do("", "POST", "/api/v1/society/blocks/"+w.block+"/units/bulk", w.admin, map[string]any{"floors": flats / 10, "units_per_floor": 10, "unit_type": "2BHK"})
	if code != 201 {
		must(fmt.Errorf("units: %d %s", code, b))
	}
	for page := 1; ; page++ {
		_, b = c.do("", "GET", fmt.Sprintf("/api/v1/society/units?page_size=100&page=%d", page), w.admin, nil)
		var r struct {
			Data []map[string]any `json:"data"`
		}
		_ = json.Unmarshal(b, &r)
		if len(r.Data) == 0 {
			break
		}
		for _, u := range r.Data {
			w.units = append(w.units, fmt.Sprint(u["id"]))
		}
	}

	// residents (an owner per flat and a tenant in every second flat) and guards, created in parallel
	type job struct {
		path string
		body map[string]any
	}
	var jobs []job
	phone := func(i int) string { return fmt.Sprintf("%d", 9300000000+i) }
	n := 0
	for i, u := range w.units {
		jobs = append(jobs, job{"/api/v1/society/residents", map[string]any{"full_name": fmt.Sprintf("Owner %d", i), "phone": phone(n), "unit_id": u, "type": "owner"}})
		n++
		if i%2 == 0 {
			jobs = append(jobs, job{"/api/v1/society/residents", map[string]any{"full_name": fmt.Sprintf("Tenant %d", i), "phone": phone(n), "unit_id": u, "type": "tenant"}})
			n++
		}
	}
	guardPhones := []string{}
	for g := 0; g < 20; g++ {
		p := fmt.Sprintf("%d", 9400000000+g)
		guardPhones = append(guardPhones, p)
		jobs = append(jobs, job{"/api/v1/society/guards", map[string]any{"full_name": fmt.Sprintf("Guard %d", g), "phone": p}})
	}
	var wg sync.WaitGroup
	ch := make(chan job)
	for k := 0; k < 16; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range ch {
				if code, b := c.do("", "POST", j.path, w.admin, j.body); code != 201 {
					must(fmt.Errorf("seed %s: %d %s", j.path, code, b))
				}
			}
		}()
	}
	for _, j := range jobs {
		ch <- j
	}
	close(ch)
	wg.Wait()

	// billing history: one head and last month issued
	c.do("", "POST", "/api/v1/society/billing/heads", w.admin, map[string]any{"name": "Maintenance", "calc": "fixed", "amount_paise": 400000})
	c.do("", "POST", "/api/v1/society/invoices/generate", w.admin, map[string]any{"period": periodOf(-1)})
	c.do("", "POST", "/api/v1/society/invoices/issue", w.admin, map[string]any{"period": periodOf(-1)})

	// bulk history straight into the tables, so list and report queries run against realistic sizes
	_, err = pool.Exec(ctx, `
		WITH u AS (SELECT array_agg(id) a FROM units WHERE society_id=$1)
		INSERT INTO visits (society_id, unit_id, visitor_name, purpose, status, source, expected_from, expected_until, created_at, entered_at, exited_at)
		SELECT $1, u.a[1 + g % array_length(u.a,1)], 'Visitor ' || g, (ARRAY['guest','delivery','cab','service'])[1 + g % 4], 'completed', 'walk_in',
		       now() - (g || ' minutes')::interval, now() - (g || ' minutes')::interval, now() - (g || ' minutes')::interval,
		       now() - (g || ' minutes')::interval, now() - (g || ' minutes')::interval + interval '20 minutes'
		FROM generate_series(1,$2) g, u`, w.society, nVisits)
	must(err)
	_, err = pool.Exec(ctx, `
		WITH r AS (SELECT array_agg(user_id) a FROM memberships WHERE society_id=$1 AND role_key IN ('resident_owner','resident_tenant'))
		INSERT INTO notifications (society_id, user_id, type, title, body, created_at)
		SELECT $1, r.a[1 + g % array_length(r.a,1)], 'visit.arrived', 'Visitor arrived', 'History row ' || g, now() - (g || ' minutes')::interval
		FROM generate_series(1,$2) g, r`, w.society, nNotes)
	must(err)
	_, err = pool.Exec(ctx, `ANALYZE`)
	must(err)

	// sign in the users the steady load needs
	want := *vus
	nRes := want * 70 / 100
	rows, err := pool.Query(ctx, `SELECT u.phone, m.unit_id::text FROM memberships m JOIN users u ON u.id=m.user_id WHERE m.society_id=$1 AND m.role_key IN ('resident_owner','resident_tenant') ORDER BY u.phone LIMIT $2`, w.society, nRes)
	must(err)
	type pu struct{ phone, unit string }
	var rs []pu
	for rows.Next() {
		var x pu
		must(rows.Scan(&x.phone, &x.unit))
		rs = append(rs, x)
	}
	rows.Close()
	w.residents = make([]user, len(rs))
	w.guards = make([]user, len(guardPhones))
	sem := make(chan struct{}, 1) // sequential: each sign-in clears the OTP limiter first
	for i, x := range rs {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, x pu) {
			defer wg.Done()
			defer func() { <-sem }()
			w.residents[i] = user{tok: login(c, x.phone), unit: x.unit}
		}(i, x)
	}
	for i, p := range guardPhones {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, p string) {
			defer wg.Done()
			defer func() { <-sem }()
			w.guards[i] = user{tok: login(c, p)}
		}(i, p)
	}
	wg.Wait()
	for _, u := range append(append([]user{}, w.residents...), w.guards...) {
		if u.tok == "" {
			must(fmt.Errorf("a virtual user could not sign in"))
		}
	}
	return w
}

type timing struct {
	name string
	d    time.Duration
}

// singleShots times the heavy one-off operations.
func singleShots(ctx context.Context, c *client, w *world, base string) []timing {
	var out []timing
	step := func(name string, f func()) {
		t := time.Now()
		f()
		out = append(out, timing{name, time.Since(t)})
	}
	period := periodOf(0)
	step(fmt.Sprintf("generate invoices for %d flats (draft)", len(w.units)), func() {
		c.do("", "POST", "/api/v1/society/invoices/generate", w.admin, map[string]any{"period": period})
	})
	step(fmt.Sprintf("issue invoices for %d flats", len(w.units)), func() {
		c.do("", "POST", "/api/v1/society/invoices/issue", w.admin, map[string]any{"period": period})
	})
	step("collection report", func() { c.do("", "GET", "/api/v1/society/billing/reports/collection?period="+period, w.admin, nil) })
	step("dues and ageing", func() { c.do("", "GET", "/api/v1/society/billing/dues", w.admin, nil) })
	step("invoices CSV export", func() { c.do("", "GET", "/api/v1/society/invoices/export?period="+period, w.admin, nil) })
	step("visitors CSV export (all history)", func() { c.do("", "GET", "/api/v1/society/visitors/export", w.admin, nil) })
	step("visitor stats", func() { c.do("", "GET", "/api/v1/society/visitors/stats", w.admin, nil) })

	// SOS fan-out: every guard and the office are connected; a resident raises an alert
	var received int64
	var last atomic.Int64
	var conns []*websocket.Conn
	listen := func(tok string) {
		u := "ws" + strings.TrimPrefix(base, "http") + "/api/v1/ws?token=" + url.QueryEscape(tok)
		cn, _, err := websocket.DefaultDialer.Dial(u, nil)
		if err != nil {
			return
		}
		conns = append(conns, cn)
		go func() {
			for {
				_, msg, err := cn.ReadMessage()
				if err != nil {
					return
				}
				if strings.Contains(string(msg), `"sos.raised"`) {
					atomic.AddInt64(&received, 1)
					last.Store(time.Now().UnixNano())
					return
				}
			}
		}()
	}
	listen(w.admin)
	for _, g := range w.guards {
		listen(g.tok)
	}
	time.Sleep(300 * time.Millisecond)
	expect := int64(len(conns))
	start := time.Now()
	code, _ := c.do("", "POST", "/api/v1/sos", w.residents[0].tok, map[string]any{"kind": "medical"})
	deadline := time.Now().Add(10 * time.Second)
	for atomic.LoadInt64(&received) < expect && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	name := fmt.Sprintf("SOS reaches %d connected guards and office (raise returned %d)", expect, code)
	if got := atomic.LoadInt64(&received); got < expect {
		name += fmt.Sprintf(" — ONLY %d received", got)
	}
	out = append(out, timing{name, time.Duration(last.Load() - start.UnixNano())})
	for _, cn := range conns {
		cn.Close()
	}
	return out
}

// runSteady drives the virtual users: residents, guards and the office, each looping with a short think time.
func runSteady(ctx context.Context, c *client, w *world, rec *recorder, n int, d time.Duration) {
	if os.Getenv("LOADTEST_DUMP") != "" {
		go func() {
			time.Sleep(d / 2)
			f, _ := os.Create(os.Getenv("LOADTEST_DUMP"))
			_ = pprof.Lookup("goroutine").WriteTo(f, 2)
			f.Close()
		}()
	}
	stop := time.Now().Add(d)
	var wg sync.WaitGroup
	think := func() { time.Sleep(time.Duration(50+rand.Intn(150)) * time.Millisecond) }

	resident := func(u user) {
		defer wg.Done()
		calls := []func(){
			func() { c.do("resident: me", "GET", "/api/v1/me", u.tok, nil) },
			func() { c.do("resident: features", "GET", "/api/v1/me/features", u.tok, nil) },
			func() { c.do("resident: notifications", "GET", "/api/v1/me/notifications?page_size=20", u.tok, nil) },
			func() { c.do("resident: unread count", "GET", "/api/v1/me/notifications/unread-count", u.tok, nil) },
			func() { c.do("resident: my visitors", "GET", "/api/v1/visitors", u.tok, nil) },
			func() { c.do("resident: bills", "GET", "/api/v1/bills?group=open", u.tok, nil) },
			func() { c.do("resident: ledger", "GET", "/api/v1/ledger", u.tok, nil) },
			func() { c.do("resident: notices", "GET", "/api/v1/notices", u.tok, nil) },
			func() {
				c.do("resident: pre-approve visitor", "POST", "/api/v1/visitors", u.tok, map[string]any{
					"visitor_name": "Guest", "purpose": "guest", "expected_from": time.Now().Add(time.Hour).Format(time.RFC3339), "expected_until": time.Now().Add(5 * time.Hour).Format(time.RFC3339)})
			},
		}
		for time.Now().Before(stop) {
			calls[rand.Intn(len(calls))]()
			think()
		}
	}
	guard := func(u user) {
		defer wg.Done()
		calls := []func(){
			func() { c.do("guard: dashboard", "GET", "/api/v1/gate/dashboard", u.tok, nil) },
			func() { c.do("guard: inside now", "GET", "/api/v1/gate/inside", u.tok, nil) },
			func() { c.do("guard: visits", "GET", "/api/v1/gate/visits", u.tok, nil) },
			func() { c.do("guard: passes sync", "GET", "/api/v1/gate/passes/sync", u.tok, nil) },
			func() {
				c.do("guard: verify a pass", "POST", "/api/v1/gate/verify", u.tok, map[string]any{"code": fmt.Sprintf("%06d", rand.Intn(1000000))})
			},
			func() { c.do("guard: flat lookup", "GET", "/api/v1/gate/units?q=A1", u.tok, nil) },
		}
		for time.Now().Before(stop) {
			calls[rand.Intn(len(calls))]()
			think()
		}
	}
	office := func() {
		defer wg.Done()
		calls := []func(){
			func() { c.do("office: stats", "GET", "/api/v1/society/stats", w.admin, nil) },
			func() { c.do("office: residents list", "GET", "/api/v1/society/residents?page_size=25", w.admin, nil) },
			func() { c.do("office: units list", "GET", "/api/v1/society/units?page_size=50", w.admin, nil) },
			func() { c.do("office: visitors list", "GET", "/api/v1/society/visitors?page_size=25", w.admin, nil) },
			func() { c.do("office: visitor stats", "GET", "/api/v1/society/visitors/stats", w.admin, nil) },
			func() { c.do("office: invoices list", "GET", "/api/v1/society/invoices?page_size=25", w.admin, nil) },
			func() { c.do("office: dues", "GET", "/api/v1/society/billing/dues", w.admin, nil) },
			func() { c.do("office: complaints", "GET", "/api/v1/society/complaints?page_size=25", w.admin, nil) },
		}
		for time.Now().Before(stop) {
			calls[rand.Intn(len(calls))]()
			think()
		}
	}
	nGuards, nOffice := n*10/100, n*20/100
	for i := 0; i < len(w.residents); i++ {
		wg.Add(1)
		go resident(w.residents[i])
	}
	for i := 0; i < nGuards; i++ {
		wg.Add(1)
		go guard(w.guards[i%len(w.guards)])
	}
	for i := 0; i < nOffice; i++ {
		wg.Add(1)
		go office()
	}
	wg.Wait()
}

func pct(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	i := int(float64(len(d)-1) * p)
	return d[i]
}

// summarize prints and records the per-call table; it returns false when a target is missed.
func summarize(rec *recorder, rep *strings.Builder, p95Limit time.Duration) bool {
	by := map[string][]time.Duration{}
	errs := map[string]int{}
	bad := map[string]int{}
	total, totalErr := 0, 0
	for _, s := range rec.s {
		by[s.name] = append(by[s.name], s.d)
		total++
		if s.err {
			errs[s.name]++
			totalErr++
		}
		if s.code == 429 {
			bad[s.name]++
		}
	}
	names := make([]string, 0, len(by))
	for n := range by {
		names = append(names, n)
	}
	sort.Strings(names)
	ok := true
	fmt.Fprintf(rep, "| call | n | p50 | p95 | p99 | max | 5xx/errors |\n|---|---|---|---|---|---|---|\n")
	fmt.Printf("%-34s %7s %8s %8s %8s %8s %6s\n", "call", "n", "p50", "p95", "p99", "max", "errs")
	for _, n := range names {
		d := by[n]
		sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
		flag := ""
		if pct(d, .95) > p95Limit {
			flag = " ⚠"
			ok = false
		}
		fmt.Fprintf(rep, "| %s | %d | %s | %s%s | %s | %s | %d |\n", n, len(d), pct(d, .5).Round(time.Millisecond), pct(d, .95).Round(time.Millisecond), flag, pct(d, .99).Round(time.Millisecond), d[len(d)-1].Round(time.Millisecond), errs[n])
		fmt.Printf("%-34s %7d %8s %8s %8s %8s %6d%s\n", n, len(d), pct(d, .5).Round(time.Millisecond), pct(d, .95).Round(time.Millisecond), pct(d, .99).Round(time.Millisecond), d[len(d)-1].Round(time.Millisecond), errs[n], flag)
	}
	fmt.Fprintf(rep, "\nTotal %d requests, %d errors (5xx or network).\n", total, totalErr)
	fmt.Printf("total %d requests, %d errors\n", total, totalErr)
	if totalErr > 0 {
		ok = false
	}
	return ok
}

// probe runs every kind of request once, one at a time, against an API whose pool has a single connection.
// A request that holds a connection and asks for a second one waits forever there, so a timeout names a
// code path that can deadlock the pool under real concurrency.
func probe(c *client, w *world) []string {
	r, g := w.residents[0], w.guards[0]
	type call struct {
		name, method, path, tok string
		body                    any
	}
	calls := []call{
		{"resident me", "GET", "/api/v1/me", r.tok, nil}, {"resident features", "GET", "/api/v1/me/features", r.tok, nil},
		{"resident notifications", "GET", "/api/v1/me/notifications", r.tok, nil}, {"resident unread", "GET", "/api/v1/me/notifications/unread-count", r.tok, nil},
		{"resident visitors", "GET", "/api/v1/visitors", r.tok, nil}, {"resident bills", "GET", "/api/v1/bills?group=open", r.tok, nil},
		{"resident bills summary", "GET", "/api/v1/bills/summary", r.tok, nil}, {"resident ledger", "GET", "/api/v1/ledger", r.tok, nil},
		{"resident payments", "GET", "/api/v1/payments", r.tok, nil}, {"resident notices", "GET", "/api/v1/notices", r.tok, nil},
		{"resident polls", "GET", "/api/v1/polls", r.tok, nil}, {"resident complaints", "GET", "/api/v1/complaints", r.tok, nil},
		{"resident amenities", "GET", "/api/v1/amenities", r.tok, nil}, {"resident helpers", "GET", "/api/v1/helpers", r.tok, nil},
		{"resident family", "GET", "/api/v1/me/family", r.tok, nil}, {"resident unit", "GET", "/api/v1/me/unit", r.tok, nil},
		{"resident sos", "GET", "/api/v1/me/sos", r.tok, nil},
		{"pre-approve visitor", "POST", "/api/v1/visitors", r.tok, map[string]any{"visitor_name": "Guest", "purpose": "guest",
			"expected_from": time.Now().Add(time.Hour).Format(time.RFC3339), "expected_until": time.Now().Add(5 * time.Hour).Format(time.RFC3339)}},
		{"raise complaint", "POST", "/api/v1/complaints", r.tok, map[string]any{"title": "Leak", "description": "Water drips from the pipe.", "category": "plumbing"}},
		{"guard dashboard", "GET", "/api/v1/gate/dashboard", g.tok, nil}, {"guard inside", "GET", "/api/v1/gate/inside", g.tok, nil},
		{"guard visits", "GET", "/api/v1/gate/visits", g.tok, nil}, {"guard passes sync", "GET", "/api/v1/gate/passes/sync", g.tok, nil},
		{"guard verify", "POST", "/api/v1/gate/verify", g.tok, map[string]any{"code": "123456"}},
		{"guard units", "GET", "/api/v1/gate/units?q=A1", g.tok, nil}, {"guard shift", "GET", "/api/v1/gate/shift", g.tok, nil},
		{"guard attendance inside", "GET", "/api/v1/attendance/inside", g.tok, nil},
	}
	for _, p := range []string{"stats", "residents", "units", "visitors", "visitors/stats", "invoices", "billing/dues", "billing/heads", "billing/settings",
		"billing/reports/collection", "billing/gateway", "complaints", "complaints/stats", "notices", "polls", "amenities", "amenities/stats", "bookings", "helpers", "staff",
		"attendance", "guards", "gates", "contacts", "sos", "payments", "apartments", "blocks", "plan", "office-members", "invoices/export", "visitors/export",
		"complaints/export", "attendance/export", "billing/defaulters/export", "bookings/export", "sos/export"} {
		calls = append(calls, call{"office " + p, "GET", "/api/v1/society/" + p, w.admin, nil})
	}
	calls = append(calls,
		call{"office generate", "POST", "/api/v1/society/invoices/generate", w.admin, map[string]any{"period": periodOf(2)}},
		call{"office issue", "POST", "/api/v1/society/invoices/issue", w.admin, map[string]any{"period": periodOf(2)}},
		call{"sos raise", "POST", "/api/v1/sos", r.tok, map[string]any{"kind": "medical"}})
	var hung []string
	for _, k := range calls {
		start := time.Now()
		code, _ := c.do("", k.method, k.path, k.tok, k.body)
		if code == 0 && time.Since(start) >= c.http.Timeout-100*time.Millisecond {
			hung = append(hung, k.method+" "+k.path+"  ("+k.name+")")
		}
	}
	return hung
}
