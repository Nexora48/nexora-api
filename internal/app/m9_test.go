package app_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Nexora48/nexora-api/internal/payment"
)

// periodOf is the YYYY-MM of the month `offset` months from now, in society time.
func periodOf(offset int) string {
	n := time.Now().In(ist)
	return time.Date(n.Year(), n.Month()+time.Month(offset), 1, 0, 0, 0, 0, ist).Format("2006-01")
}

func head(t *testing.T, c community, body map[string]any) resp {
	t.Helper()
	r := call(t, "POST", "/api/v1/society/billing/heads", c.s.Admin, body)
	if r.Status != 201 {
		t.Fatalf("create head: %d %v", r.Status, r.Body)
	}
	return r
}

// fixedHead is one flat-rate charge of the given paise.
func fixedHead(t *testing.T, c community, name string, paise int) {
	head(t, c, map[string]any{"name": name, "calc": "fixed", "amount_paise": paise})
}

func issuePeriod(t *testing.T, c community, period string) {
	t.Helper()
	if g := call(t, "POST", "/api/v1/society/invoices/generate", c.s.Admin, map[string]any{"period": period}); g.Status != 200 {
		t.Fatalf("generate %s: %d %v", period, g.Status, g.Body)
	}
	if i := call(t, "POST", "/api/v1/society/invoices/issue", c.s.Admin, map[string]any{"period": period}); i.Status != 200 {
		t.Fatalf("issue %s: %d %v", period, i.Status, i.Body)
	}
}

func bills(t *testing.T, tok, group string) []map[string]any {
	t.Helper()
	rows, r := callList(t, "/api/v1/bills?group="+group, tok)
	if r.Status != 200 {
		t.Fatalf("bills: %d %v", r.Status, r.Body)
	}
	return rows
}

func num(m map[string]any, k string) int64 { f, _ := m[k].(float64); return int64(f) }

func balance(t *testing.T, tok string) int64 {
	t.Helper()
	r := call(t, "GET", "/api/v1/ledger", tok, nil)
	if r.Status != 200 {
		t.Fatalf("ledger: %d %v", r.Status, r.Body)
	}
	return num(r.data(), "balance_paise")
}

func recordPay(t *testing.T, c community, unit string, paise int, mode, ref string) resp {
	t.Helper()
	return call(t, "POST", "/api/v1/society/payments", c.s.Admin, map[string]any{"unit_id": unit, "amount_paise": paise, "mode": mode, "reference": ref})
}

func postForm(t *testing.T, path string, form url.Values) int {
	t.Helper()
	res, err := http.PostForm(srv.URL+path, form)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	return res.StatusCode
}

func newCommittee(t *testing.T, s soc) string {
	t.Helper()
	ctx := context.Background()
	uid, phone := uuid.New(), nextPhone()
	mustExec(t, ctx, `INSERT INTO users (id, full_name, phone) VALUES ($1,'Committee Member',$2)`, uid, phone)
	mustExec(t, ctx, `INSERT INTO memberships (user_id, society_id, role_key) VALUES ($1,$2,'committee')`, uid, s.ID)
	mr.FlushAll()
	l := otpLogin(t, phone)
	if l.Status != 200 {
		t.Fatalf("committee login: %d %v", l.Status, l.Body)
	}
	return l.str("access_token")
}

func TestInvoiceGenerationPreviewAndIssue(t *testing.T) {
	c := newCommunity(t)
	head(t, c, map[string]any{"name": "Maintenance", "calc": "per_unit_type", "amount_paise": 400000, "rates": []map[string]any{{"unit_type": "2BHK", "amount_paise": 420000}}})
	fixedHead(t, c, "Water", 30000)
	head(t, c, map[string]any{"name": "Parking", "calc": "per_sqft", "rate_paise": 500})
	mustExec(t, context.Background(), `UPDATE units SET area_sqft=1000 WHERE id=$1`, c.units["A101"]) // B101 has no area
	period := periodOf(0)

	if r := call(t, "POST", "/api/v1/society/invoices/generate", c.rA, map[string]any{"period": period}); r.Status != 403 {
		t.Fatalf("resident generates: %d", r.Status)
	}
	if r := call(t, "GET", "/api/v1/society/invoices", c.guard.Token, nil); r.Status != 403 {
		t.Fatalf("guard reads invoices: %d", r.Status)
	}
	if r := call(t, "POST", "/api/v1/society/invoices/generate", c.s.Admin, map[string]any{"period": "2026-13"}); r.Status != 400 {
		t.Fatalf("bad period: %d", r.Status)
	}

	g := call(t, "POST", "/api/v1/society/invoices/generate", c.s.Admin, map[string]any{"period": period})
	d := g.data()
	if g.Status != 200 || num(d, "drafts") != 2 || num(d, "vacant_skipped") != 2 || num(d, "total_paise") != 950000+450000 {
		t.Fatalf("draft summary: %d %v", g.Status, g.Body)
	}
	if w := d["warnings"].([]any); len(w) != 1 || w[0].(map[string]any)["code"] != "MISSING_AREA" || !strings.Contains(w[0].(map[string]any)["flat"].(string), "B-B101") && !strings.Contains(w[0].(map[string]any)["flat"].(string), "B101") {
		t.Fatalf("warnings: %v", w)
	}
	// regenerating a draft does not duplicate it
	if g2 := call(t, "POST", "/api/v1/society/invoices/generate", c.s.Admin, map[string]any{"period": period}); num(g2.data(), "drafts") != 2 {
		t.Fatalf("regenerate: %v", g2.Body)
	}
	if len(bills(t, c.rA, "open")) != 0 {
		t.Fatal("a draft is visible to the resident")
	}

	i := call(t, "POST", "/api/v1/society/invoices/issue", c.s.Admin, map[string]any{"period": period})
	if i.Status != 200 || num(i.data(), "issued") != 2 {
		t.Fatalf("issue: %d %v", i.Status, i.Body)
	}
	mine := bills(t, c.rA, "open")
	if len(mine) != 1 || !regexp.MustCompile(`^INV-\d{4}-000[12]$`).MatchString(mine[0]["number"].(string)) || num(mine[0], "total_paise") != 950000 || len(mine[0]["lines"].([]any)) != 3 {
		t.Fatalf("A101 bill: %v", mine)
	}
	if b := bills(t, c.rB, "open"); len(b) != 1 || num(b[0], "total_paise") != 450000 {
		t.Fatalf("B101 bill: %v", b)
	}
	// issuing again changes nothing; generating again finds both issued
	if i2 := call(t, "POST", "/api/v1/society/invoices/issue", c.s.Admin, map[string]any{"period": period}); num(i2.data(), "issued") != 0 {
		t.Fatalf("second issue: %v", i2.Body)
	}
	if g3 := call(t, "POST", "/api/v1/society/invoices/generate", c.s.Admin, map[string]any{"period": period}); num(g3.data(), "drafts") != 0 || num(g3.data(), "already_issued") != 2 {
		t.Fatalf("after issue: %v", g3.Body)
	}
	if n := inbox(t, c.rA, ""); len(n) == 0 || n[0]["type"] != "bill.issued" {
		t.Fatalf("flat not told: %v", n)
	}
	if bal := balance(t, c.rA); bal != 950000 {
		t.Fatalf("ledger balance %d", bal)
	}
	// an ad hoc charge
	ad := call(t, "POST", "/api/v1/society/invoices", c.s.Admin, map[string]any{"unit_id": c.units["A101"], "label": "Lift damage", "amount_paise": 25000})
	if ad.Status != 201 || ad.data()["kind"] != "adhoc" || num(ad.data(), "total_paise") != 25000 {
		t.Fatalf("ad hoc: %d %v", ad.Status, ad.Body)
	}
	if bal := balance(t, c.rA); bal != 975000 {
		t.Fatalf("ledger after ad hoc %d", bal)
	}
}

func TestAmenityChargesBecomeInvoiceLines(t *testing.T) {
	c := newCommunity(t)
	a := openAmenity(t, c, map[string]any{"name": "Party hall", "fee_amount": 150, "deposit_amount": 200})
	id := a.str("id")
	day := dayString(2)
	b1 := book(t, c.rA, id, slotAt(t, c.rA, id, 2, 10))
	b2 := book(t, c.rA, id, slotAt(t, c.rA, id, 2, 12))
	if b1.Status != 201 || b2.Status != 201 {
		t.Fatalf("book: %d %d", b1.Status, b2.Status)
	}
	if r := call(t, "POST", "/api/v1/amenities/bookings/"+b2.str("id")+"/cancel", c.rA, nil); r.Status != 200 {
		t.Fatalf("cancel: %d", r.Status)
	}
	period := day[:7]
	g := call(t, "POST", "/api/v1/society/invoices/generate", c.s.Admin, map[string]any{"period": period})
	if num(g.data(), "drafts") != 1 || num(g.data(), "total_paise") != 35000 || num(g.data(), "booking_charges") != 1 {
		t.Fatalf("draft: %v", g.Body)
	}
	call(t, "POST", "/api/v1/society/invoices/issue", c.s.Admin, map[string]any{"period": period})
	inv := bills(t, c.rA, "open")
	if len(inv) != 1 || len(inv[0]["lines"].([]any)) != 2 {
		t.Fatalf("bill: %v", inv)
	}
	var status string
	var invoice *string
	_ = pool.QueryRow(context.Background(), `SELECT charge_status, invoice_id::text FROM amenity_bookings WHERE id=$1`, b1.str("id")).Scan(&status, &invoice)
	if status != "billed" || invoice == nil || *invoice != inv[0]["id"].(string) {
		t.Fatalf("booking after issue: %s %v", status, invoice)
	}
	// billed once: another run bills nothing more
	g2 := call(t, "POST", "/api/v1/society/invoices/generate", c.s.Admin, map[string]any{"period": period})
	if num(g2.data(), "drafts") != 0 {
		t.Fatalf("billed twice: %v", g2.Body)
	}
	// voiding makes the charge billable again
	if v := call(t, "POST", "/api/v1/society/invoices/"+inv[0]["id"].(string)+"/void", c.s.Admin, map[string]any{"reason": "Wrong flat"}); v.Status != 200 || v.vstatus() != "void" {
		t.Fatalf("void: %d %v", v.Status, v.Body)
	}
	_ = pool.QueryRow(context.Background(), `SELECT charge_status, invoice_id::text FROM amenity_bookings WHERE id=$1`, b1.str("id")).Scan(&status, &invoice)
	if status != "pending" || invoice != nil {
		t.Fatalf("booking after void: %s %v", status, invoice)
	}
	if bal := balance(t, c.rA); bal != 0 {
		t.Fatalf("balance after void %d", bal)
	}
	if g3 := call(t, "POST", "/api/v1/society/invoices/generate", c.s.Admin, map[string]any{"period": period}); num(g3.data(), "drafts") != 1 {
		t.Fatalf("regenerate after void: %v", g3.Body)
	}
}

func TestPaymentsAreAppliedOldestFirstAndExcessBecomesCredit(t *testing.T) {
	c := newCommunity(t)
	fixedHead(t, c, "Maintenance", 500000)
	issuePeriod(t, c, periodOf(-1))
	issuePeriod(t, c, periodOf(0))
	unit := c.units["A101"]

	p1 := recordPay(t, c, unit, 300000, "cash", "")
	if p1.Status != 201 || !regexp.MustCompile(`^RCP-\d{4}-0001$`).MatchString(p1.str("receipt_number")) {
		t.Fatalf("first payment: %d %v", p1.Status, p1.Body)
	}
	open := bills(t, c.rA, "open")
	if len(open) != 2 || open[0]["status"] != "partially_paid" || num(open[0], "paid_paise") != 300000 || open[1]["status"] != "issued" {
		t.Fatalf("after partial: %v", open)
	}
	p2 := recordPay(t, c, unit, 800000, "upi_transfer", "UPI123")
	if p2.Status != 201 || !strings.HasSuffix(p2.str("receipt_number"), "-0002") {
		t.Fatalf("second payment: %d %v", p2.Status, p2.Body)
	}
	if len(bills(t, c.rA, "open")) != 0 || len(bills(t, c.rA, "paid")) != 2 {
		t.Fatal("both invoices should be paid")
	}
	if bal := balance(t, c.rA); bal != -100000 {
		t.Fatalf("advance credit not shown: %d", bal)
	}
	if s := call(t, "GET", "/api/v1/bills/summary", c.rA, nil); num(s.data(), "credit_paise") != 100000 || num(s.data(), "outstanding_paise") != 0 {
		t.Fatalf("summary: %v", s.Body)
	}
	// the credit is applied to the next bill automatically
	issuePeriod(t, c, periodOf(1))
	nxt := bills(t, c.rA, "open")
	if len(nxt) != 1 || nxt[0]["status"] != "partially_paid" || num(nxt[0], "paid_paise") != 100000 {
		t.Fatalf("credit not applied: %v", nxt)
	}
	if bal := balance(t, c.rA); bal != 400000 {
		t.Fatalf("balance %d", bal)
	}

	// receipts: JSON and PDF for the flat, nothing for other flats, and the office can fetch both
	pays, _ := callList(t, "/api/v1/payments", c.rA)
	if len(pays) != 2 {
		t.Fatalf("history: %v", pays)
	}
	id := p2.str("id")
	if r := call(t, "GET", "/api/v1/payments/"+id+"/receipt", c.rA, nil); r.Status != 200 || r.str("number") != p2.str("receipt_number") || len(r.data()["items"].([]any)) != 3 { // two invoices plus the advance credit applied to the next bill
		t.Fatalf("receipt: %d %v", r.Status, r.Body)
	}
	code, ct, body := rawGet(t, "/api/v1/payments/"+id+"/receipt.pdf", c.rA)
	if code != 200 || ct != "application/pdf" || !bytes.HasPrefix(body, []byte("%PDF")) {
		t.Fatalf("pdf: %d %s", code, ct)
	}
	if r := call(t, "GET", "/api/v1/payments/"+id+"/receipt", c.rB, nil); r.Status != 404 {
		t.Fatalf("another flat reads the receipt: %d", r.Status)
	}
	if code, _, body := rawGet(t, "/api/v1/society/payments/"+id+"/receipt.pdf", c.s.Admin); code != 200 || !bytes.HasPrefix(body, []byte("%PDF")) {
		t.Fatalf("office pdf: %d", code)
	}
	if n := inbox(t, c.rA, ""); len(n) == 0 {
		t.Fatal("no notifications")
	}
	// validation
	if r := recordPay(t, c, unit, 50, "cash", ""); r.Status != 400 {
		t.Fatalf("tiny amount: %d", r.Status)
	}
	if r := recordPay(t, c, unit, 10000, "cheque", ""); r.Status != 400 {
		t.Fatalf("cheque without number: %d", r.Status)
	}
	if r := recordPay(t, c, unit, 10000, "razorpay", "x"); r.Status != 400 {
		t.Fatalf("office recording an online payment: %d", r.Status)
	}
}

func TestChequeBounceVoidAndAdjustments(t *testing.T) {
	c := newCommunity(t)
	fixedHead(t, c, "Maintenance", 500000)
	period := periodOf(0)
	issuePeriod(t, c, period)
	unit := c.units["A101"]
	inv := bills(t, c.rA, "open")[0]["id"].(string)

	p := recordPay(t, c, unit, 500000, "cheque", "000123")
	if p.Status != 201 || len(bills(t, c.rA, "open")) != 0 {
		t.Fatalf("cheque: %d %v", p.Status, p.Body)
	}
	if v := call(t, "POST", "/api/v1/society/invoices/"+inv+"/void", c.s.Admin, map[string]any{"reason": "oops"}); v.Status != 409 {
		t.Fatalf("void a paid invoice: %d", v.Status)
	}
	if b := call(t, "POST", "/api/v1/society/payments/"+p.str("id")+"/bounce", c.s.Admin, map[string]any{"reason": "x"}); b.Status != 400 {
		t.Fatalf("bounce needs a reason: %d", b.Status)
	}
	b := call(t, "POST", "/api/v1/society/payments/"+p.str("id")+"/bounce", c.s.Admin, map[string]any{"reason": "Insufficient funds"})
	if b.Status != 200 || b.data()["status"] != "reversed" {
		t.Fatalf("bounce: %d %v", b.Status, b.Body)
	}
	open := bills(t, c.rA, "open")
	if len(open) != 1 || open[0]["status"] != "issued" || num(open[0], "paid_paise") != 0 {
		t.Fatalf("invoice after bounce: %v", open)
	}
	if bal := balance(t, c.rA); bal != 500000 {
		t.Fatalf("balance after bounce %d", bal)
	}
	if again := call(t, "POST", "/api/v1/society/payments/"+p.str("id")+"/bounce", c.s.Admin, map[string]any{"reason": "again"}); again.Status != 409 {
		t.Fatalf("bounce twice: %d", again.Status)
	}
	cash := recordPay(t, c, unit, 10000, "cash", "")
	if r := call(t, "POST", "/api/v1/society/payments/"+cash.str("id")+"/bounce", c.s.Admin, map[string]any{"reason": "not a cheque"}); r.Status != 409 {
		t.Fatalf("bounce cash: %d", r.Status)
	}

	// a waiver reduces what is owed and settles the invoice partly
	if r := call(t, "POST", "/api/v1/society/ledger/adjust", c.s.Admin, map[string]any{"unit_id": unit, "kind": "credit", "amount_paise": 100000, "reason": "Festival discount"}); r.Status != 200 {
		t.Fatalf("credit: %d %v", r.Status, r.Body)
	}
	if bal := balance(t, c.rA); bal != 500000-10000-100000 {
		t.Fatalf("balance after waiver %d", bal)
	}
	// a debit is an ad hoc invoice
	if r := call(t, "POST", "/api/v1/society/ledger/adjust", c.s.Admin, map[string]any{"unit_id": unit, "kind": "debit", "amount_paise": 25000, "reason": "Damage to gate"}); r.Status != 200 {
		t.Fatalf("debit: %d %v", r.Status, r.Body)
	}
	if bal := balance(t, c.rA); bal != 500000-10000-100000+25000 {
		t.Fatalf("balance after debit %d", bal)
	}
	if r := call(t, "POST", "/api/v1/society/ledger/adjust", c.s.Admin, map[string]any{"unit_id": unit, "kind": "credit", "amount_paise": 100000, "reason": "x"}); r.Status != 400 {
		t.Fatalf("short reason: %d", r.Status)
	}
	// the statement lists everything, newest first, and the resident sees the waiver as an entry but not as a payment
	st := call(t, "GET", "/api/v1/society/billing/ledger/"+unit, c.s.Admin, nil)
	if st.Status != 200 || len(st.data()["entries"].([]any)) < 6 {
		t.Fatalf("statement: %v", st.Body)
	}
	if h, _ := callList(t, "/api/v1/payments", c.rA); len(h) != 2 { // cheque (reversed) + cash, no waiver
		t.Fatalf("payment history: %v", h)
	}
	// void an unpaid invoice (the debit invoice is partly settled by credit? no: use a fresh ad hoc one in another flat)
	ad := call(t, "POST", "/api/v1/society/invoices", c.s.Admin, map[string]any{"unit_id": c.units["B101"], "label": "Fine", "amount_paise": 5000})
	if v := call(t, "POST", "/api/v1/society/invoices/"+ad.str("id")+"/void", c.s.Admin, map[string]any{"reason": "Raised by mistake"}); v.Status != 200 || balance(t, c.rB) != 500000 { // only the regular invoice remains
		t.Fatalf("void ad hoc: %d", v.Status)
	}
}

func orderURL(t *testing.T, o resp) string {
	t.Helper()
	u, err := url.Parse(o.str("checkout_url"))
	if err != nil || o.str("checkout_url") == "" {
		t.Fatalf("no checkout url: %v", o.Body)
	}
	return u.RequestURI()
}

func paymentCount(t *testing.T, unit string) int {
	t.Helper()
	var n int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM payments WHERE unit_id=$1 AND status='ok'`, unit).Scan(&n)
	return n
}

func TestOnlinePaymentWithTheTestGateway(t *testing.T) {
	c := newCommunity(t)
	fixedHead(t, c, "Maintenance", 500000)
	issuePeriod(t, c, periodOf(0))
	unit := c.units["A101"]

	if r := call(t, "POST", "/api/v1/payments/orders", c.famA, nil); r.Status != 403 {
		t.Fatalf("family pays: %d", r.Status)
	}
	tenant, _ := newResident(t, c.s, c.units["A102"], "tenant")
	if r := call(t, "POST", "/api/v1/payments/orders", tenant, nil); r.Status != 403 || r.errCode() != "CANNOT_PAY" {
		t.Fatalf("tenant without permission: %d %v", r.Status, r.Body)
	}
	if r := call(t, "POST", "/api/v1/payments/orders", c.rA, map[string]any{"amount_paise": 50}); r.Status != 422 {
		t.Fatalf("below ₹1: %d", r.Status)
	}
	if r := call(t, "POST", "/api/v1/payments/orders", c.rA, map[string]any{"amount_paise": 600000}); r.Status != 422 {
		t.Fatalf("above what is due: %d", r.Status)
	}

	o := call(t, "POST", "/api/v1/payments/orders", c.rA, map[string]any{"amount_paise": 200000})
	if o.Status != 201 || o.str("status") != "created" || num(o.data(), "amount_paise") != 200000 {
		t.Fatalf("order: %d %v", o.Status, o.Body)
	}
	path := orderURL(t, o)
	if code, _, body := rawGet(t, "/pay/"+o.str("id")+"?t=wrong", ""); code != 404 || bytes.Contains(body, []byte(`value="pay"`)) {
		t.Fatalf("wrong token: %d", code)
	}
	code, ct, page := rawGet(t, path, "")
	if code != 200 || !strings.HasPrefix(ct, "text/html") || !bytes.Contains(page, []byte(`value="pay"`)) || !bytes.Contains(page, []byte("₹2,000")) {
		t.Fatalf("hosted page: %d %s", code, ct)
	}
	// opening the page does not pay anything; neither does asking for the status
	if paymentCount(t, unit) != 0 || call(t, "GET", "/api/v1/payments/orders/"+o.str("id"), c.rA, nil).str("status") != "created" {
		t.Fatal("order must stay pending until the gateway confirms")
	}
	// another flat cannot see the order
	if r := call(t, "GET", "/api/v1/payments/orders/"+o.str("id"), c.rB, nil); r.Status != 404 {
		t.Fatalf("foreign order: %d", r.Status)
	}
	fake := "/pay/" + o.str("id") + "/fake?t=" + url.QueryEscape(strings.SplitN(path, "t=", 2)[1])
	if st := postForm(t, fake, url.Values{"result": {"pay"}}); st != 200 {
		t.Fatalf("pay: %d", st)
	}
	if st := postForm(t, fake, url.Values{"result": {"pay"}}); st != 200 { // a double click
		t.Fatalf("pay twice: %d", st)
	}
	if paymentCount(t, unit) != 1 {
		t.Fatalf("expected one payment, got %d", paymentCount(t, unit))
	}
	got := call(t, "GET", "/api/v1/payments/orders/"+o.str("id"), c.rA, nil)
	if got.str("status") != "paid" || got.data()["payment_id"] == nil {
		t.Fatalf("order status: %v", got.Body)
	}
	open := bills(t, c.rA, "open")
	if len(open) != 1 || num(open[0], "paid_paise") != 200000 || open[0]["status"] != "partially_paid" {
		t.Fatalf("invoice after payment: %v", open)
	}
	if r := call(t, "GET", "/api/v1/payments/"+got.str("payment_id")+"/receipt", c.rA, nil); r.Status != 200 || r.data()["mode"] != "fake" {
		t.Fatalf("receipt: %d %v", r.Status, r.Body)
	}

	// idempotent order creation and the failure path
	h := func(key string) resp {
		return callWithHeader(t, "POST", "/api/v1/payments/orders", c.rA, map[string]any{"amount_paise": 100000}, map[string]string{"Idempotency-Key": key})
	}
	a, b := h("retry-1"), h("retry-1")
	if a.Status != 201 || a.str("id") != b.str("id") || h("retry-2").str("id") == a.str("id") {
		t.Fatalf("idempotency: %v %v", a.Body, b.Body)
	}
	failPath := "/pay/" + a.str("id") + "/fake?t=" + url.QueryEscape(strings.SplitN(orderURL(t, a), "t=", 2)[1])
	postForm(t, failPath, url.Values{"result": {"fail"}})
	if s := call(t, "GET", "/api/v1/payments/orders/"+a.str("id"), c.rA, nil).str("status"); s != "failed" || paymentCount(t, unit) != 1 {
		t.Fatalf("failed order: %s", s)
	}
	postForm(t, failPath, url.Values{"result": {"pay"}}) // the same order can still be completed after a failed attempt
	if paymentCount(t, unit) != 2 {
		t.Fatalf("retry after failure: %d", paymentCount(t, unit))
	}
	// nothing left to pay once settled
	recordPay(t, c, unit, 200000, "cash", "")
	if r := call(t, "POST", "/api/v1/payments/orders", c.rA, nil); r.Status != 409 || r.errCode() != "NOTHING_DUE" {
		t.Fatalf("nothing due: %d %v", r.Status, r.Body)
	}
	// expiry
	issuePeriod(t, c, periodOf(1))
	e := call(t, "POST", "/api/v1/payments/orders", c.rA, nil)
	mustExec(t, context.Background(), `UPDATE payment_orders SET expires_at = now() - interval '1 minute' WHERE id=$1`, e.str("id"))
	if s := call(t, "GET", "/api/v1/payments/orders/"+e.str("id"), c.rA, nil).str("status"); s != "expired" {
		t.Fatalf("expired order reads %s", s)
	}
	if code, _, _ := rawGet(t, orderURL(t, e), ""); code != 409 {
		t.Fatalf("expired page: %d", code)
	}
}

func callWithHeader(t *testing.T, method, path, token string, body any, headers map[string]string) resp {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		rd = bytes.NewReader([]byte(jsonString(body)))
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, srv.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	return decodeResp(res)
}

func signed(secret string, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return hex.EncodeToString(m.Sum(nil))
}

func webhook(t *testing.T, society, secret, eventID, body string) int {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/api/v1/webhooks/razorpay/"+society, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Razorpay-Signature", signed(secret, body))
	req.Header.Set("X-Razorpay-Event-Id", eventID)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	return res.StatusCode
}

func captured(order, payment string, amount int) string {
	return fmt.Sprintf(`{"event":"payment.captured","payload":{"payment":{"entity":{"id":%q,"order_id":%q,"amount":%d}}}}`, payment, order, amount)
}

func TestRazorpayWebhookIsVerifiedAndIdempotent(t *testing.T) {
	var n int
	var mu sync.Mutex
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		id := n
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"order_%s_%d"}`, strings.ReplaceAll(t.Name(), "/", ""), id)
	}))
	defer stub.Close()
	apay.SetGateway(payment.NewRazorpayAt(stub.URL))

	c := newCommunity(t)
	fixedHead(t, c, "Maintenance", 500000)
	issuePeriod(t, c, periodOf(-1))
	issuePeriod(t, c, periodOf(0))
	unit := c.units["A101"]

	// a society without a gateway still uses the test gateway in dev/test; configure Razorpay
	if r := call(t, "PUT", "/api/v1/society/billing/gateway", c.rA, map[string]any{"provider": "razorpay"}); r.Status != 403 {
		t.Fatalf("resident configures: %d", r.Status)
	}
	if r := call(t, "PUT", "/api/v1/society/billing/gateway", c.s.Admin, map[string]any{"provider": "razorpay", "key_id": "rzp_test_abc", "active": true}); r.Status != 400 {
		t.Fatalf("active without secrets: %d", r.Status)
	}
	put := call(t, "PUT", "/api/v1/society/billing/gateway", c.s.Admin, map[string]any{"provider": "razorpay", "key_id": "rzp_test_abc", "key_secret": "SECRET-KEY-111", "webhook_secret": "whsec-222", "active": true})
	if put.Status != 200 || put.data()["secret_set"] != true || put.data()["webhook_secret_set"] != true {
		t.Fatalf("configure: %d %v", put.Status, put.Body)
	}
	get := call(t, "GET", "/api/v1/society/billing/gateway", c.s.Admin, nil)
	if raw := fmt.Sprint(get.Body, put.Body); strings.Contains(raw, "SECRET-KEY-111") || strings.Contains(raw, "whsec-222") {
		t.Fatalf("a secret leaked into a response: %v", raw)
	}
	var stored, audit string
	_ = pool.QueryRow(context.Background(), `SELECT key_secret_enc FROM payment_gateway_settings WHERE society_id=$1`, c.s.ID).Scan(&stored)
	_ = pool.QueryRow(context.Background(), `SELECT COALESCE(string_agg(meta::text, ' '),'') FROM audit_logs WHERE society_id=$1`, c.s.ID).Scan(&audit)
	if strings.Contains(stored, "SECRET-KEY-111") || strings.Contains(audit, "SECRET-KEY-111") || strings.Contains(audit, "whsec-222") {
		t.Fatal("a secret is stored or logged in clear")
	}
	// keeping the secrets when only a flag changes
	if r := call(t, "PUT", "/api/v1/society/billing/gateway", c.s.Admin, map[string]any{"provider": "razorpay", "key_id": "rzp_test_abc", "active": true}); r.Status != 200 || r.data()["secret_set"] != true {
		t.Fatalf("keep secrets: %d %v", r.Status, r.Body)
	}

	society := c.s.ID.String()
	orderRef := func(id string) string {
		var ref string
		_ = pool.QueryRow(context.Background(), `SELECT provider_order_id FROM payment_orders WHERE id=$1`, id).Scan(&ref)
		return ref
	}
	o1 := call(t, "POST", "/api/v1/payments/orders", c.rA, map[string]any{"amount_paise": 200000})
	if o1.Status != 201 || o1.str("provider") != "razorpay" {
		t.Fatalf("order: %d %v", o1.Status, o1.Body)
	}
	ref1 := orderRef(o1.str("id"))
	ev := captured(ref1, "pay_1", 200000)

	if st := webhook(t, society, "wrong-secret", "evt-bad", ev); st != 401 {
		t.Fatalf("bad signature: %d", st)
	}
	if paymentCount(t, unit) != 0 {
		t.Fatal("a forged webhook created a payment")
	}
	if st := webhook(t, society, "whsec-222", "evt-1", ev); st != 200 || paymentCount(t, unit) != 1 {
		t.Fatalf("captured: %d, payments %d", st, paymentCount(t, unit))
	}
	if st := webhook(t, society, "whsec-222", "evt-1", ev); st != 200 || paymentCount(t, unit) != 1 { // replay
		t.Fatalf("replay: %d, payments %d", st, paymentCount(t, unit))
	}
	paid := fmt.Sprintf(`{"event":"order.paid","payload":{"order":{"entity":{"id":%q,"amount_paid":200000}}}}`, ref1)
	if st := webhook(t, society, "whsec-222", "evt-2", paid); st != 200 || paymentCount(t, unit) != 1 { // second event for the same order
		t.Fatalf("order.paid after captured: %d, payments %d", st, paymentCount(t, unit))
	}
	if s := call(t, "GET", "/api/v1/payments/orders/"+o1.str("id"), c.rA, nil).str("status"); s != "paid" {
		t.Fatalf("order status %s", s)
	}
	if code, _, _ := rawGet(t, orderURL(t, o1), ""); code != 409 {
		t.Fatalf("paid order page: %d", code)
	}

	// a different amount than ordered is flagged, not applied
	o2 := call(t, "POST", "/api/v1/payments/orders", c.rA, map[string]any{"amount_paise": 300000})
	ref2 := orderRef(o2.str("id"))
	if st := webhook(t, society, "whsec-222", "evt-3", captured(ref2, "pay_2", 100000)); st != 200 || paymentCount(t, unit) != 1 {
		t.Fatalf("mismatch: %d, payments %d", st, paymentCount(t, unit))
	}
	var note *string
	var status string
	_ = pool.QueryRow(context.Background(), `SELECT note, status FROM payment_orders WHERE id=$1`, o2.str("id")).Scan(&note, &status)
	if note == nil || !strings.Contains(*note, "mismatch") || status != "created" {
		t.Fatalf("mismatch not flagged: %v %s", note, status)
	}
	// an unknown order is ignored
	if st := webhook(t, society, "whsec-222", "evt-4", captured("order_unknown", "pay_3", 100)); st != 200 || paymentCount(t, unit) != 1 {
		t.Fatalf("unknown order: %d", st)
	}
	// another society's webhook cannot be used with this society's secret and orders
	other := newCommunity(t)
	if st := webhook(t, other.s.ID.String(), "whsec-222", "evt-5", captured(ref2, "pay_4", 300000)); st == 200 {
		t.Fatalf("the other society accepted a webhook: %d", st)
	}

	// duplicates arriving in parallel, with the same and with different event ids, settle exactly once
	o3 := call(t, "POST", "/api/v1/payments/orders", c.rA, map[string]any{"amount_paise": 100000})
	ref3 := orderRef(o3.str("id"))
	var wg sync.WaitGroup
	var serverErrors atomic.Int64
	for i := 0; i < 50; i++ { // a storm: 50 deliveries, seven distinct event ids
		wg.Add(1)
		go func() {
			defer wg.Done()
			if st := webhook(t, society, "whsec-222", fmt.Sprintf("evt-par-%d", i%7), captured(ref3, "pay_par", 100000)); st >= 500 {
				serverErrors.Add(1)
			}
		}()
	}
	wg.Wait()
	if serverErrors.Load() != 0 {
		t.Fatalf("%d webhook deliveries failed with a 5xx", serverErrors.Load())
	}
	if paymentCount(t, unit) != 2 {
		t.Fatalf("parallel webhooks produced %d payments, want 2 in total", paymentCount(t, unit))
	}
	var linked int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM payments WHERE order_id=$1`, o3.str("id")).Scan(&linked)
	if linked != 1 {
		t.Fatalf("order paid %d times", linked)
	}
	if bal := balance(t, c.rA); bal != 1000000-200000-100000 {
		t.Fatalf("ledger balance %d", bal)
	}
}

func TestDueRemindersLateFeesAndDefaulters(t *testing.T) {
	c := newCommunity(t)
	fixedHead(t, c, "Maintenance", 500000)
	if r := call(t, "PUT", "/api/v1/society/billing/settings", c.s.Admin, map[string]any{"due_day": 10, "grace_days": 5, "late_fee_mode": "flat", "late_fee_value": 10000, "include_vacant": false}); r.Status != 200 {
		t.Fatalf("settings: %d %v", r.Status, r.Body)
	}
	if r := call(t, "PUT", "/api/v1/society/billing/settings", c.s.Admin, map[string]any{"due_day": 31, "grace_days": 5, "late_fee_mode": "none"}); r.Status != 400 {
		t.Fatalf("due day 31: %d", r.Status)
	}
	if r := call(t, "PUT", "/api/v1/society/billing/settings", c.rA, map[string]any{"due_day": 10, "grace_days": 5, "late_fee_mode": "none"}); r.Status != 403 {
		t.Fatalf("resident edits settings: %d", r.Status)
	}
	period := periodOf(0)
	issuePeriod(t, c, period)
	due := time.Date(time.Now().In(ist).Year(), time.Now().In(ist).Month(), 10, 9, 0, 0, 0, ist)
	t.Cleanup(func() { abill.SetClock(time.Now) })
	at := func(d time.Time) { abill.SetClock(func() time.Time { return d }) }
	count := func(typ string) int {
		n := 0
		for _, x := range inbox(t, c.rA, "") {
			if x["type"] == typ {
				n++
			}
		}
		return n
	}
	sweep := func() {
		if _, _, err := abill.Sweep(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	at(due.AddDate(0, 0, -2))
	sweep()
	sweep()
	if count("bill.reminder") != 1 {
		t.Fatalf("pre-due reminder sent %d times", count("bill.reminder"))
	}
	at(due.AddDate(0, 0, 1))
	sweep()
	if count("bill.reminder") != 2 || count("bill.late_fee") != 0 {
		t.Fatalf("due-day: %d reminders, %d late fees", count("bill.reminder"), count("bill.late_fee"))
	}
	at(due.AddDate(0, 0, 8))
	sweep()
	sweep()
	if count("bill.reminder") != 3 || count("bill.late_fee") != 1 {
		t.Fatalf("overdue: %d reminders, %d late fees", count("bill.reminder"), count("bill.late_fee"))
	}
	fees, _ := callList(t, "/api/v1/society/invoices?kind=late_fee", c.s.Admin)
	if len(fees) != 2 || num(fees[0], "total_paise") != 10000 { // one per unpaid flat
		t.Fatalf("late fee invoices: %v", fees)
	}
	if bal := balance(t, c.rA); bal != 510000 {
		t.Fatalf("balance with late fee %d", bal)
	}

	// dues, ageing, defaulters
	d := call(t, "GET", "/api/v1/society/billing/dues?overdue=1", c.s.Admin, nil)
	rows := d.data()["rows"].([]any)
	if d.Status != 200 || len(rows) != 2 { // A101 and B101 both unpaid
		t.Fatalf("dues: %d %v", d.Status, d.Body)
	}
	first := rows[0].(map[string]any)
	if first["bucket"] != "1-30" || num(first, "days_overdue") != 8 {
		t.Fatalf("ageing row: %v", first)
	}
	if ag := d.data()["ageing"].(map[string]any); num(ag, "d1_30_paise") < 1000000 {
		t.Fatalf("ageing totals: %v", ag)
	}
	code, ct, csv := rawGet(t, "/api/v1/society/billing/defaulters/export", c.s.Admin)
	if code != 200 || !strings.HasPrefix(ct, "text/csv") || !strings.Contains(string(csv), "Flat,Residents,Outstanding") {
		t.Fatalf("defaulter csv: %d %s", code, csv)
	}
	if r := call(t, "POST", "/api/v1/society/billing/remind", c.s.Admin, map[string]any{"unit_ids": []string{c.units["A101"]}}); r.Status != 200 || num(r.data(), "sent") != 1 {
		t.Fatalf("remind: %d %v", r.Status, r.Body)
	}
	if r := call(t, "POST", "/api/v1/society/billing/remind", c.s.Admin, map[string]any{"unit_ids": []string{c.units["A101"]}}); num(r.data(), "sent") != 0 {
		t.Fatalf("reminded twice in 24 h: %v", r.Body)
	}
	// paying clears the flat from the defaulters
	recordPay(t, c, c.units["A101"], 510000, "cash", "")
	d2 := call(t, "GET", "/api/v1/society/billing/dues?overdue=1", c.s.Admin, nil)
	if len(d2.data()["rows"].([]any)) != 1 {
		t.Fatalf("defaulters after payment: %v", d2.Body)
	}
	// a month later the fee is added again (once per month) and the reminder escalates
	at(due.AddDate(0, 0, 40))
	sweep()
	sweep()
	fees, _ = callList(t, "/api/v1/society/invoices?kind=late_fee", c.s.Admin)
	var forB int
	for _, f := range fees {
		if strings.Contains(f["flat"].(string), "B101") {
			forB++
		}
	}
	if forB != 2 {
		t.Fatalf("B101 late fees after two months: %d", forB)
	}
}

func TestBillingIsolationRolesAndSwitch(t *testing.T) {
	c1, c2 := newCommunity(t), newCommunity(t)
	fixedHead(t, c1, "Maintenance", 500000)
	issuePeriod(t, c1, periodOf(0))
	inv := bills(t, c1.rA, "open")[0]["id"].(string)
	pay := recordPay(t, c1, c1.units["A101"], 100000, "cash", "")

	if r := call(t, "GET", "/api/v1/bills/"+inv, c2.rA, nil); r.Status != 404 {
		t.Fatalf("other society reads a bill: %d", r.Status)
	}
	if r := call(t, "GET", "/api/v1/bills/"+inv, c1.rB, nil); r.Status != 404 {
		t.Fatalf("other flat reads a bill: %d", r.Status)
	}
	if r := call(t, "GET", "/api/v1/society/invoices/"+inv, c2.s.Admin, nil); r.Status != 404 {
		t.Fatalf("other society's admin reads an invoice: %d", r.Status)
	}
	if r := call(t, "GET", "/api/v1/payments/"+pay.str("id")+"/receipt", c1.rB, nil); r.Status != 404 {
		t.Fatalf("other flat reads a receipt: %d", r.Status)
	}
	if r := call(t, "GET", "/api/v1/bills", c1.famA, nil); r.Status != 403 {
		t.Fatalf("family reads bills: %d", r.Status)
	}
	if r := call(t, "GET", "/api/v1/bills", c1.guard.Token, nil); r.Status != 403 {
		t.Fatalf("guard reads bills: %d", r.Status)
	}
	if r := call(t, "POST", "/api/v1/society/payments", c1.rA, map[string]any{"unit_id": c1.units["A101"], "amount_paise": 100000, "mode": "cash"}); r.Status != 403 {
		t.Fatalf("resident records a payment: %d", r.Status)
	}

	// the committee reads but cannot change anything
	com := newCommittee(t, c1.s)
	if r := call(t, "GET", "/api/v1/society/invoices", com, nil); r.Status != 200 {
		t.Fatalf("committee reads invoices: %d", r.Status)
	}
	if r := call(t, "GET", "/api/v1/society/billing/reports/collection", com, nil); r.Status != 200 {
		t.Fatalf("committee reads the report: %d", r.Status)
	}
	for _, p := range []struct{ m, path string }{{"POST", "/api/v1/society/invoices/generate"}, {"POST", "/api/v1/society/payments"}, {"PUT", "/api/v1/society/billing/settings"}, {"GET", "/api/v1/society/billing/gateway"}, {"POST", "/api/v1/society/ledger/adjust"}} {
		if r := call(t, p.m, p.path, com, map[string]any{}); r.Status != 403 {
			t.Fatalf("committee %s %s: %d", p.m, p.path, r.Status)
		}
	}

	// the switch removes billing for residents and the office
	mustExec(t, context.Background(), `INSERT INTO society_features (society_id, feature_key, enabled) VALUES ($1,'payments',false)`, c1.s.ID)
	if r := call(t, "GET", "/api/v1/bills", c1.rA, nil); r.Status != 403 || r.errCode() != "FEATURE_DISABLED" {
		t.Fatalf("switched off (resident): %d %v", r.Status, r.Body)
	}
	if r := call(t, "GET", "/api/v1/society/invoices", c1.s.Admin, nil); r.Status != 403 {
		t.Fatalf("switched off (office): %d", r.Status)
	}
	if r := call(t, "POST", "/api/v1/payments/orders", c1.rA, nil); r.Status != 403 {
		t.Fatalf("switched off (orders): %d", r.Status)
	}
}

func TestCollectionReportAndExport(t *testing.T) {
	c := newCommunity(t)
	fixedHead(t, c, "Maintenance", 500000)
	period := periodOf(0)
	issuePeriod(t, c, period)
	recordPay(t, c, c.units["A101"], 500000, "cash", "")
	recordPay(t, c, c.units["B101"], 250000, "cash", "")
	r := call(t, "GET", "/api/v1/society/billing/reports/collection?period="+period, c.s.Admin, nil)
	d := r.data()
	if r.Status != 200 || num(d, "billed_paise") != 1000000 || num(d, "paid_paise") != 750000 || num(d, "flats_billed") != 2 || num(d, "flats_paid") != 1 || d["rate"].(float64) != 75 {
		t.Fatalf("collection: %d %v", r.Status, r.Body)
	}
	if num(d, "outstanding_paise") != 250000 || num(d, "collected_in_month_paise") != 750000 {
		t.Fatalf("outstanding / collected: %v", d)
	}
	if bb := d["by_block"].([]any); len(bb) != 2 || num(bb[0].(map[string]any), "paid_paise") != 500000 {
		t.Fatalf("by block: %v", bb)
	}
	if s := d["series"].([]any); len(s) != 6 || num(s[5].(map[string]any), "billed_paise") != 1000000 {
		t.Fatalf("series: %v", s)
	}
	code, ct, csv := rawGet(t, "/api/v1/society/invoices/export?period="+period, c.s.Admin)
	if code != 200 || !strings.HasPrefix(ct, "text/csv") || !strings.Contains(string(csv), "INV-") || !strings.Contains(string(csv), "5000.00") {
		t.Fatalf("export: %d %s", code, csv)
	}
	if code, _, _ := rawGet(t, "/api/v1/society/invoices/export?period=bad", c.s.Admin); code != 400 {
		t.Fatalf("export bad period: %d", code)
	}
	// a one-time head applies to its period only
	head(t, c, map[string]any{"name": "Festival", "calc": "fixed", "amount_paise": 20000, "recurring": false, "one_time_period": periodOf(1)})
	if g := call(t, "POST", "/api/v1/society/invoices/generate", c.s.Admin, map[string]any{"period": periodOf(1)}); num(g.data(), "total_paise") != 2*(500000+20000) {
		t.Fatalf("one-time head in its period: %v", g.Body)
	}
	if g := call(t, "POST", "/api/v1/society/invoices/generate", c.s.Admin, map[string]any{"period": periodOf(2)}); num(g.data(), "total_paise") != 2*500000 {
		t.Fatalf("one-time head leaked: %v", g.Body)
	}
	// a head name is unique
	if r := call(t, "POST", "/api/v1/society/billing/heads", c.s.Admin, map[string]any{"name": "maintenance", "calc": "fixed", "amount_paise": 100}); r.Status != 409 {
		t.Fatalf("duplicate head: %d", r.Status)
	}
}

func TestPlatformSubscriptionInvoices(t *testing.T) {
	c := newCommunity(t) // 4 flats
	tok := superToken(t)
	id := c.s.ID.String()
	period := periodOf(0)
	if r := sa(t, "PUT", "/societies/"+id+"/subscription", tok, map[string]any{"rate_per_unit_paise": 5000, "flat_fee_paise": 100000, "billing_day": 1, "status": "active", "starts_on": "2020-01-01"}); r.Status != 200 || num(r.data(), "next_amount_paise") != 5000*4+100000 || num(r.data(), "units") != 4 {
		t.Fatalf("subscription: %d %v", r.Status, r.Body)
	}
	if r := sa(t, "PUT", "/societies/"+id+"/subscription", tok, map[string]any{"rate_per_unit_paise": -1, "billing_day": 1, "status": "active", "starts_on": "2020-01-01"}); r.Status != 400 {
		t.Fatalf("negative rate: %d", r.Status)
	}
	if r := call(t, "GET", saBase+"/societies/"+id+"/subscription", c.rA, nil); r.Status == 200 {
		t.Fatalf("a resident token reached the platform API")
	}
	g := sa(t, "POST", "/billing/invoices/generate", tok, map[string]any{"period": period})
	if g.Status != 200 || num(g.data(), "created") < 1 {
		t.Fatalf("generate: %d %v", g.Status, g.Body)
	}
	if g2 := sa(t, "POST", "/billing/invoices/generate", tok, map[string]any{"period": period}); num(g2.data(), "created") != 0 {
		t.Fatalf("generated twice: %v", g2.Body)
	}
	rows, r := callList(t, saBase+"/billing/invoices?society_id="+id, tok)
	if r.Status != 200 || len(rows) != 1 || num(rows[0], "amount_paise") != 120000 || rows[0]["status"] != "issued" {
		t.Fatalf("invoices: %d %v", r.Status, rows)
	}
	inv := rows[0]["id"].(string)
	if p := sa(t, "POST", "/billing/invoices/"+inv+"/paid", tok, map[string]any{"reference": ""}); p.Status != 400 {
		t.Fatalf("paid without reference: %d", p.Status)
	}
	if p := sa(t, "POST", "/billing/invoices/"+inv+"/paid", tok, map[string]any{"reference": "NEFT-8841"}); p.Status != 200 || p.vstatus() != "paid" {
		t.Fatalf("mark paid: %d %v", p.Status, p.Body)
	}
	if p := sa(t, "POST", "/billing/invoices/"+inv+"/paid", tok, map[string]any{"reference": "NEFT-8841"}); p.Status != 409 {
		t.Fatalf("paid twice: %d", p.Status)
	}
	if r := sa(t, "POST", "/billing/invoices/generate", tok, map[string]any{"period": "nope"}); r.Status != 400 {
		t.Fatalf("bad period: %d", r.Status)
	}
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func decodeResp(res *http.Response) resp {
	var m map[string]any
	_ = json.NewDecoder(res.Body).Decode(&m)
	return resp{res.StatusCode, m}
}
