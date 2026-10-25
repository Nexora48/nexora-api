package billing

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/money"
)

type Line struct {
	Label       string  `json:"label"`
	AmountPaise int64   `json:"amount_paise"`
	SourceType  *string `json:"source_type,omitempty"`
}

type PaymentRef struct {
	ID          uuid.UUID `json:"id"`
	AmountPaise int64     `json:"amount_paise"`
	Mode        string    `json:"mode"`
	PaidOn      string    `json:"paid_on"`
	Receipt     *string   `json:"receipt_number,omitempty"`
}

type Invoice struct {
	ID           uuid.UUID    `json:"id"`
	Number       *string      `json:"number"`
	UnitID       uuid.UUID    `json:"unit_id"`
	Flat         string       `json:"flat"`
	Period       string       `json:"period"`
	Kind         string       `json:"kind"`
	Status       string       `json:"status"`
	IssuedAt     *time.Time   `json:"issued_at,omitempty"`
	DueDate      string       `json:"due_date"`
	TotalPaise   int64        `json:"total_paise"`
	PaidPaise    int64        `json:"paid_paise"`
	BalancePaise int64        `json:"balance_paise"`
	DaysOverdue  int          `json:"days_overdue"`
	Note         *string      `json:"note,omitempty"`
	VoidReason   *string      `json:"void_reason,omitempty"`
	Lines        []Line       `json:"lines,omitempty"`
	Payments     []PaymentRef `json:"payments,omitempty"`
}

const invoiceCols = `i.id, i.number, i.unit_id, bl.code || '-' || u.number, i.period, i.kind, i.status, i.issued_at, i.due_date, i.total_paise, i.paid_paise, i.note, i.void_reason`
const invoiceFrom = ` FROM invoices i JOIN units u ON u.id=i.unit_id JOIN blocks bl ON bl.id=u.block_id `

func (s *Service) scanInvoice(row pgx.Row, today time.Time, extra ...any) (*Invoice, error) {
	var v Invoice
	var due time.Time
	dest := append([]any{&v.ID, &v.Number, &v.UnitID, &v.Flat, &v.Period, &v.Kind, &v.Status, &v.IssuedAt, &due, &v.TotalPaise, &v.PaidPaise, &v.Note, &v.VoidReason}, extra...)
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	v.DueDate = due.Format("2006-01-02")
	if v.Status == "issued" || v.Status == "partially_paid" {
		v.BalancePaise = v.TotalPaise - v.PaidPaise
		if d := int(today.Sub(time.Date(due.Year(), due.Month(), due.Day(), 0, 0, 0, 0, time.UTC)).Hours() / 24); d > 0 {
			v.DaysOverdue = d
		}
	}
	return &v, nil
}

func (s *Service) lines(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID][]Line, error) {
	out := map[uuid.UUID][]Line{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT invoice_id, label, amount_paise, source_type FROM invoice_lines WHERE invoice_id = ANY($1) ORDER BY ctid`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var l Line
		if err := rows.Scan(&id, &l.Label, &l.AmountPaise, &l.SourceType); err != nil {
			return nil, err
		}
		out[id] = append(out[id], l)
	}
	return out, rows.Err()
}

// ── generation ───────────────────────────────────────────────────────────────

type Warning struct {
	Flat    string `json:"flat"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type DraftSummary struct {
	Period         string    `json:"period"`
	Drafts         int       `json:"drafts"`
	TotalPaise     int64     `json:"total_paise"`
	AlreadyIssued  int       `json:"already_issued"`
	VacantSkipped  int       `json:"vacant_skipped"`
	BookingCharges int       `json:"booking_charges"`
	Warnings       []Warning `json:"warnings"`
}

type draftLine struct {
	head   *uuid.UUID
	label  string
	amount int64
	src    string
	srcID  *uuid.UUID
}

func (s *Service) periodDue(period string, dueDay int) time.Time {
	var y, m int
	fmt.Sscanf(period, "%d-%d", &y, &m)
	return time.Date(y, time.Month(m), dueDay, 0, 0, 0, 0, time.UTC)
}

// Generate computes a draft invoice for every flat for the period. It can be run again until the period is issued;
// flats that already have an issued invoice for it are left alone.
func (s *Service) Generate(ctx context.Context, p authctx.Principal, period string) (*DraftSummary, error) {
	if err := validPeriod(period); err != nil {
		return nil, err
	}
	var y, m int
	fmt.Sscanf(period, "%d-%d", &y, &m)
	if y < 2020 || y > s.now().Year()+2 {
		return nil, invalid("period is out of range")
	}
	st, err := s.Settings(ctx, p.SocietyID)
	if err != nil {
		return nil, err
	}
	heads, err := s.Heads(ctx, p.SocietyID, true)
	if err != nil {
		return nil, err
	}
	var active []*Head
	for _, h := range heads {
		if h.Recurring || (h.OneTimePeriod != nil && *h.OneTimePeriod == period) {
			active = append(active, h)
		}
	}
	loc := s.loc(ctx, p.SocietyID)
	periodEnd := time.Date(y, time.Month(m)+1, 1, 0, 0, 0, 0, loc)

	type unit struct {
		id       uuid.UUID
		label    string
		utype    string
		area     *int
		occupied bool
	}
	rows, err := s.pool.Query(ctx, `SELECT u.id, bl.code || '-' || u.number, u.unit_type, u.area_sqft,
		EXISTS (SELECT 1 FROM memberships m WHERE m.unit_id=u.id AND m.status='active' AND m.role_key IN ('resident_owner','resident_tenant'))
		FROM units u JOIN blocks bl ON bl.id=u.block_id WHERE u.society_id=$1 ORDER BY bl.sort_order, bl.code, u.number`, p.SocietyID)
	if err != nil {
		return nil, err
	}
	var units []unit
	for rows.Next() {
		var u unit
		if err := rows.Scan(&u.id, &u.label, &u.utype, &u.area, &u.occupied); err != nil {
			rows.Close()
			return nil, err
		}
		units = append(units, u)
	}
	rows.Close()

	issued := map[uuid.UUID]bool{}
	ir, err := s.pool.Query(ctx, `SELECT unit_id FROM invoices WHERE society_id=$1 AND period=$2 AND kind='regular' AND status NOT IN ('draft','void')`, p.SocietyID, period)
	if err != nil {
		return nil, err
	}
	for ir.Next() {
		var id uuid.UUID
		if ir.Scan(&id) == nil {
			issued[id] = true
		}
	}
	ir.Close()

	charges := map[uuid.UUID][]draftLine{}
	br, err := s.pool.Query(ctx, `SELECT b.id, b.unit_id, a.name, b.slot_start, b.fee_amount::float8, b.deposit_amount::float8
		FROM amenity_bookings b JOIN amenities a ON a.id=b.amenity_id
		WHERE b.society_id=$1 AND b.charge_status='pending' AND b.status IN ('confirmed','completed','no_show') AND b.slot_start < $2 ORDER BY b.slot_start`, p.SocietyID, periodEnd)
	if err != nil {
		return nil, err
	}
	bookingCharges := 0
	for br.Next() {
		var id, uid uuid.UUID
		var name string
		var start time.Time
		var fee, dep float64
		if err := br.Scan(&id, &uid, &name, &start, &fee, &dep); err != nil {
			br.Close()
			return nil, err
		}
		when := start.In(loc).Format("2 Jan, 3:04 PM")
		bid := id
		if f := int64(math.Round(fee * 100)); f > 0 {
			charges[uid] = append(charges[uid], draftLine{label: fmt.Sprintf("%s booking · %s", name, when), amount: f, src: "booking", srcID: &bid})
			bookingCharges++
		}
		if d := int64(math.Round(dep * 100)); d > 0 {
			charges[uid] = append(charges[uid], draftLine{label: fmt.Sprintf("%s refundable deposit · %s", name, when), amount: d, src: "booking", srcID: &bid})
		}
	}
	br.Close()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `DELETE FROM invoices WHERE society_id=$1 AND period=$2 AND kind='regular' AND status='draft'`, p.SocietyID, period); err != nil {
		return nil, err
	}
	sum := &DraftSummary{Period: period, Warnings: []Warning{}}
	due := s.periodDue(period, st.DueDay)
	for _, u := range units {
		if issued[u.id] {
			sum.AlreadyIssued++
			continue
		}
		var lines []draftLine
		if u.occupied || st.IncludeVacant {
			for _, h := range active {
				hid := h.ID
				var amt int64
				switch h.Calc {
				case "fixed":
					amt = h.AmountPaise
				case "per_unit_type":
					amt = h.AmountPaise
					for _, r := range h.Rates {
						if strings.EqualFold(r.UnitType, u.utype) {
							amt = r.AmountPaise
						}
					}
				case "per_sqft":
					if u.area == nil || *u.area <= 0 {
						sum.Warnings = append(sum.Warnings, Warning{Flat: u.label, Code: "MISSING_AREA", Message: fmt.Sprintf("%s was skipped for %s: the flat has no area", h.Name, u.label)})
						continue
					}
					amt = money.Mul(h.RatePaise, *u.area)
				}
				if amt > 0 {
					lines = append(lines, draftLine{head: &hid, label: h.Name, amount: amt, src: "head"})
				}
			}
		} else {
			sum.VacantSkipped++
		}
		lines = append(lines, charges[u.id]...)
		if len(lines) == 0 {
			continue
		}
		var total int64
		for _, l := range lines {
			total += l.amount
		}
		var iid uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO invoices (society_id, unit_id, period, kind, status, due_date, total_paise, created_by) VALUES ($1,$2,$3,'regular','draft',$4,$5,$6) RETURNING id`,
			p.SocietyID, u.id, period, due, total, p.MembershipID).Scan(&iid); err != nil {
			return nil, err
		}
		for _, l := range lines {
			var src *string
			if l.src == "booking" {
				x := l.src
				src = &x
			}
			if _, err := tx.Exec(ctx, `INSERT INTO invoice_lines (invoice_id, head_id, label, amount_paise, source_type, source_id) VALUES ($1,$2,$3,$4,$5,$6)`, iid, l.head, l.label, l.amount, src, l.srcID); err != nil {
				return nil, err
			}
		}
		sum.Drafts++
		sum.TotalPaise += total
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	sum.BookingCharges = bookingCharges
	s.log(ctx, p, "invoice.generate", "invoice", uuid.Nil, map[string]any{"period": period, "drafts": sum.Drafts, "total_paise": sum.TotalPaise})
	return sum, nil
}

type issuedRef struct {
	id, unit uuid.UUID
	number   string
	total    int64
}

// Issue publishes the drafts of a period. Running it again changes nothing.
func (s *Service) Issue(ctx context.Context, p authctx.Principal, period string) (int, error) {
	if err := validPeriod(period); err != nil {
		return 0, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	rows, err := tx.Query(ctx, `SELECT id, unit_id FROM invoices WHERE society_id=$1 AND period=$2 AND kind='regular' AND status='draft' ORDER BY unit_id FOR UPDATE`, p.SocietyID, period)
	if err != nil {
		return 0, err
	}
	type d struct{ id, unit uuid.UUID }
	var drafts []d
	for rows.Next() {
		var x d
		if err := rows.Scan(&x.id, &x.unit); err != nil {
			rows.Close()
			return 0, err
		}
		drafts = append(drafts, x)
	}
	rows.Close()

	var out []issuedRef
	for _, x := range drafts {
		// a booking cancelled since the draft was computed must not be billed
		lr, err := tx.Query(ctx, `SELECT DISTINCT source_id FROM invoice_lines WHERE invoice_id=$1 AND source_type='booking' AND source_id IS NOT NULL`, x.id)
		if err != nil {
			return 0, err
		}
		var bookings []uuid.UUID
		for lr.Next() {
			var b uuid.UUID
			if lr.Scan(&b) == nil {
				bookings = append(bookings, b)
			}
		}
		lr.Close()
		for _, b := range bookings {
			tag, err := tx.Exec(ctx, `UPDATE amenity_bookings SET charge_status='billed', invoice_id=$2 WHERE id=$1 AND charge_status='pending' AND status IN ('confirmed','completed','no_show')`, b, x.id)
			if err != nil {
				return 0, err
			}
			if tag.RowsAffected() == 0 {
				if _, err := tx.Exec(ctx, `DELETE FROM invoice_lines WHERE invoice_id=$1 AND source_id=$2`, x.id, b); err != nil {
					return 0, err
				}
			}
		}
		var total int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(amount_paise),0) FROM invoice_lines WHERE invoice_id=$1`, x.id).Scan(&total); err != nil {
			return 0, err
		}
		if total == 0 {
			if _, err := tx.Exec(ctx, `DELETE FROM invoices WHERE id=$1`, x.id); err != nil {
				return 0, err
			}
			continue
		}
		num, err := nextNumber(ctx, tx, "invoice_counters", p.SocietyID, ym(period), "INV")
		if err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `UPDATE invoices SET status='issued', number=$2, issued_at=now(), total_paise=$3 WHERE id=$1`, x.id, num, total); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO ledger_entries (society_id, unit_id, kind, amount_paise, invoice_id, reason, created_by) VALUES ($1,$2,'invoice',$3,$4,$5,$6)`,
			p.SocietyID, x.unit, total, x.id, "Invoice "+num, p.MembershipID); err != nil {
			return 0, err
		}
		if err := s.settle(ctx, tx, p.SocietyID, x.unit); err != nil {
			return 0, err
		}
		out = append(out, issuedRef{x.id, x.unit, num, total})
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	s.log(ctx, p, "invoice.issue", "invoice", uuid.Nil, map[string]any{"period": period, "count": len(out)})
	for _, r := range out {
		s.tellFlat(ctx, p.SocietyID, r.unit, "bill.issued", "New bill", fmt.Sprintf("%s · %s for %s", r.number, money.Rupees(r.total), period), map[string]any{"type": "bill", "invoice_id": r.id}, "normal")
		s.publish(ctx, p.SocietyID, r.unit, "invoice.updated", r.id)
	}
	return len(out), nil
}

// createInvoiceTx writes an issued invoice of any kind (ad hoc, late fee) and its ledger entry.
func (s *Service) createInvoiceTx(ctx context.Context, tx pgx.Tx, soc, unit uuid.UUID, kind, period string, due time.Time, lines []Line, note, idem string, by *uuid.UUID) (uuid.UUID, string, int64, error) {
	var total int64
	for _, l := range lines {
		total += l.AmountPaise
	}
	num, err := nextNumber(ctx, tx, "invoice_counters", soc, ym(period), "INV")
	if err != nil {
		return uuid.Nil, "", 0, err
	}
	var idemArg, noteArg *string
	if idem != "" {
		idemArg = &idem
	}
	if note != "" {
		noteArg = &note
	}
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `INSERT INTO invoices (society_id, unit_id, period, kind, number, status, issued_at, due_date, total_paise, note, idem_key, created_by)
		VALUES ($1,$2,$3,$4,$5,'issued',now(),$6,$7,$8,$9,$10) RETURNING id`, soc, unit, period, kind, num, due, total, noteArg, idemArg, by).Scan(&id); err != nil {
		return uuid.Nil, "", 0, err
	}
	for _, l := range lines {
		if _, err := tx.Exec(ctx, `INSERT INTO invoice_lines (invoice_id, label, amount_paise) VALUES ($1,$2,$3)`, id, l.Label, l.AmountPaise); err != nil {
			return uuid.Nil, "", 0, err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger_entries (society_id, unit_id, kind, amount_paise, invoice_id, reason, created_by) VALUES ($1,$2,'invoice',$3,$4,$5,$6)`,
		soc, unit, total, id, "Invoice "+num, by); err != nil {
		return uuid.Nil, "", 0, err
	}
	if err := s.settle(ctx, tx, soc, unit); err != nil {
		return uuid.Nil, "", 0, err
	}
	return id, num, total, nil
}

// AdHoc raises a one-off invoice for a single flat (a damage charge, a penalty).
func (s *Service) AdHoc(ctx context.Context, p authctx.Principal, unit uuid.UUID, label string, amount int64, dueDate, note string) (*Invoice, error) {
	label = strings.TrimSpace(label)
	if l := len([]rune(label)); l < 2 || l > 100 {
		return nil, invalid("describe the charge in 2-100 characters")
	}
	if amount < 100 || amount > 100_000_000_00 {
		return nil, invalid("the amount must be between ₹1 and ₹10,00,00,000")
	}
	var one int
	if err := s.pool.QueryRow(ctx, `SELECT 1 FROM units WHERE id=$1 AND society_id=$2`, unit, p.SocietyID).Scan(&one); err != nil {
		return nil, ErrNotFound
	}
	now := s.now().In(s.loc(ctx, p.SocietyID))
	due := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 7)
	if dueDate != "" {
		d, err := time.Parse("2006-01-02", dueDate)
		if err != nil {
			return nil, invalid("due date must be YYYY-MM-DD")
		}
		due = d
	}
	period := now.Format("2006-01")
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	id, num, total, err := s.createInvoiceTx(ctx, tx, p.SocietyID, unit, "adhoc", period, due, []Line{{Label: label, AmountPaise: amount}}, strings.TrimSpace(note), "", &p.MembershipID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	s.log(ctx, p, "invoice.adhoc", "invoice", id, map[string]any{"amount_paise": amount, "label": label})
	s.tellFlat(ctx, p.SocietyID, unit, "bill.issued", "New charge", fmt.Sprintf("%s · %s: %s", num, money.Rupees(total), label), map[string]any{"type": "bill", "invoice_id": id}, "normal")
	s.publish(ctx, p.SocietyID, unit, "invoice.updated", id)
	return s.Invoice(ctx, p.SocietyID, id)
}

// Void cancels an issued invoice nobody has paid; amenity charges on it become billable again.
func (s *Service) Void(ctx context.Context, p authctx.Principal, id uuid.UUID, reason string) (*Invoice, error) {
	reason = strings.TrimSpace(reason)
	if l := len([]rune(reason)); l < 3 || l > 200 {
		return nil, invalid("give a reason of 3-200 characters")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var unit uuid.UUID
	var status string
	var total int64
	var number *string
	if err := tx.QueryRow(ctx, `SELECT unit_id, status, total_paise, number FROM invoices WHERE id=$1 AND society_id=$2 FOR UPDATE`, id, p.SocietyID).Scan(&unit, &status, &total, &number); err != nil {
		return nil, ErrNotFound
	}
	var paid int64
	_ = tx.QueryRow(ctx, `SELECT COALESCE(sum(amount_paise),0) FROM payment_allocations WHERE invoice_id=$1`, id).Scan(&paid)
	if (status != "issued" && status != "draft") || paid > 0 {
		return nil, ErrInvalidState
	}
	if _, err := tx.Exec(ctx, `UPDATE invoices SET status='void', voided_by=$2, void_reason=$3 WHERE id=$1`, id, p.MembershipID, reason); err != nil {
		return nil, err
	}
	if status == "issued" {
		if _, err := tx.Exec(ctx, `INSERT INTO ledger_entries (society_id, unit_id, kind, amount_paise, invoice_id, reason, created_by) VALUES ($1,$2,'void',$3,$4,$5,$6)`,
			p.SocietyID, unit, -total, id, "Void: "+reason, p.MembershipID); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE amenity_bookings SET charge_status='pending', invoice_id=NULL WHERE invoice_id=$1 AND charge_status='billed'`, id); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	s.log(ctx, p, "invoice.void", "invoice", id, map[string]any{"reason": reason})
	s.publish(ctx, p.SocietyID, unit, "invoice.updated", id)
	return s.Invoice(ctx, p.SocietyID, id)
}

// ── reads ────────────────────────────────────────────────────────────────────

func (s *Service) Invoice(ctx context.Context, soc, id uuid.UUID) (*Invoice, error) {
	v, err := s.scanInvoice(s.pool.QueryRow(ctx, `SELECT `+invoiceCols+invoiceFrom+`WHERE i.id=$1 AND i.society_id=$2`, id, soc), s.today(ctx, soc))
	if err != nil {
		return nil, ErrNotFound
	}
	ls, err := s.lines(ctx, []uuid.UUID{id})
	if err != nil {
		return nil, err
	}
	v.Lines = ls[id]
	pr, err := s.pool.Query(ctx, `SELECT p.id, a.amount_paise, p.mode, to_char(p.paid_on,'YYYY-MM-DD'), r.number FROM payment_allocations a JOIN payments p ON p.id=a.payment_id
		LEFT JOIN receipts r ON r.payment_id=p.id WHERE a.invoice_id=$1 ORDER BY p.created_at`, id)
	if err != nil {
		return nil, err
	}
	defer pr.Close()
	for pr.Next() {
		var x PaymentRef
		if err := pr.Scan(&x.ID, &x.AmountPaise, &x.Mode, &x.PaidOn, &x.Receipt); err != nil {
			return nil, err
		}
		v.Payments = append(v.Payments, x)
	}
	return v, pr.Err()
}

// MyInvoice reads an invoice of the caller's own flat (drafts are never visible to residents).
func (s *Service) MyInvoice(ctx context.Context, p authctx.Principal, id uuid.UUID) (*Invoice, error) {
	unit, err := s.unitOf(ctx, p)
	if err != nil {
		return nil, ErrNotFound
	}
	v, err := s.Invoice(ctx, p.SocietyID, id)
	if err != nil || v.UnitID != unit || v.Status == "draft" {
		return nil, ErrNotFound
	}
	return v, nil
}

// MyBills lists the flat's invoices: group "open" (default) or "paid" (paid and void).
func (s *Service) MyBills(ctx context.Context, p authctx.Principal, group string, limit, offset int) ([]*Invoice, int, error) {
	unit, err := s.unitOf(ctx, p)
	if err != nil {
		return []*Invoice{}, 0, nil
	}
	cond := `i.status IN ('issued','partially_paid')`
	order := `i.due_date, i.created_at`
	if group == "paid" {
		cond, order = `i.status IN ('paid','void')`, `i.created_at DESC`
	}
	return s.list(ctx, p.SocietyID, `i.unit_id=$2 AND `+cond, []any{unit}, order, limit, offset)
}

func (s *Service) list(ctx context.Context, soc uuid.UUID, where string, args []any, order string, limit, offset int) ([]*Invoice, int, error) {
	all := append([]any{soc}, args...)
	rows, err := s.pool.Query(ctx, `SELECT `+invoiceCols+`, count(*) OVER() `+invoiceFrom+`WHERE i.society_id=$1 AND `+where+
		fmt.Sprintf(` ORDER BY %s LIMIT $%d OFFSET $%d`, order, len(all)+1, len(all)+2), append(all, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	today := s.today(ctx, soc)
	var out []*Invoice
	total := 0
	for rows.Next() {
		v, err := s.scanInvoice(rows, today, &total)
		if err != nil {
			rows.Close()
			return nil, 0, err
		}
		out = append(out, v)
	}
	rows.Close()
	ids := make([]uuid.UUID, len(out))
	for i, v := range out {
		ids[i] = v.ID
	}
	ls, err := s.lines(ctx, ids)
	if err != nil {
		return nil, 0, err
	}
	for _, v := range out {
		v.Lines = ls[v.ID]
	}
	return out, total, nil
}

type InvoiceFilter struct {
	Period, Status, Kind, Block, Flat string
	Limit, Offset                     int
}

// AdminInvoices lists invoices for the office with filters.
func (s *Service) AdminInvoices(ctx context.Context, p authctx.Principal, f InvoiceFilter) ([]*Invoice, int, error) {
	where := `true`
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		where += " AND " + strings.ReplaceAll(cond, "?", fmt.Sprintf("$%d", len(args)+1))
	}
	if f.Period != "" {
		add("i.period = ?", f.Period)
	}
	switch f.Status {
	case "":
	case "open":
		where += ` AND i.status IN ('issued','partially_paid')`
	case "overdue":
		where += ` AND i.status IN ('issued','partially_paid') AND i.due_date < ` + fmt.Sprintf("$%d", len(args)+2)
		args = append(args, s.today(ctx, p.SocietyID))
	default:
		add("i.status = ?", f.Status)
	}
	if f.Kind != "" {
		add("i.kind = ?", f.Kind)
	}
	if f.Block != "" {
		add("bl.code = ?", f.Block)
	}
	if q := strings.TrimSpace(f.Flat); q != "" {
		add("(bl.code || '-' || u.number) ILIKE ?", "%"+q+"%")
	}
	return s.list(ctx, p.SocietyID, where, args, `i.created_at DESC, i.id`, f.Limit, f.Offset)
}
