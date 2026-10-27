package billing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/money"
)

// settle applies a flat's unallocated payments to its open invoices, oldest due first. It runs inside the caller's
// transaction after anything that adds money (a payment, a waiver) or a new bill.
func (s *Service) settle(ctx context.Context, tx pgx.Tx, soc, unit uuid.UUID) error {
	for {
		var invID uuid.UUID
		var total, paid int64
		err := tx.QueryRow(ctx, `SELECT id, total_paise, paid_paise FROM invoices WHERE unit_id=$1 AND status IN ('issued','partially_paid') ORDER BY due_date, created_at, id LIMIT 1 FOR UPDATE`, unit).
			Scan(&invID, &total, &paid)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var payID uuid.UUID
		var free int64
		err = tx.QueryRow(ctx, `SELECT id, unallocated_paise FROM payments WHERE unit_id=$1 AND status='ok' AND unallocated_paise > 0 ORDER BY created_at, id LIMIT 1 FOR UPDATE`, unit).Scan(&payID, &free)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		amt := money.Min(total-paid, free)
		if _, err := tx.Exec(ctx, `INSERT INTO payment_allocations (payment_id, invoice_id, amount_paise) VALUES ($1,$2,$3)
			ON CONFLICT (payment_id, invoice_id) DO UPDATE SET amount_paise = payment_allocations.amount_paise + EXCLUDED.amount_paise`, payID, invID, amt); err != nil {
			return err
		}
		status := "partially_paid"
		if paid+amt >= total {
			status = "paid"
		}
		if _, err := tx.Exec(ctx, `UPDATE invoices SET paid_paise=paid_paise+$2, status=$3 WHERE id=$1`, invID, amt, status); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE payments SET unallocated_paise=unallocated_paise-$2 WHERE id=$1`, payID, amt); err != nil {
			return err
		}
	}
}

// PaymentIn is everything needed to record money that came in.
type PaymentIn struct {
	Society, Unit uuid.UUID
	AmountPaise   int64
	Mode          string // razorpay | fake | cash | cheque | upi_transfer | bank_transfer | waiver
	Reference     string
	PaidOn        time.Time
	ReceivedBy    *uuid.UUID
	OrderID       *uuid.UUID
	Note          string
	Reason        string // ledger text
}

// CreatePaymentTx records a payment, its receipt (not for waivers), the ledger entry and applies it to the oldest
// dues. It is the only place money is added, for online and office payments alike.
func (s *Service) CreatePaymentTx(ctx context.Context, tx pgx.Tx, in PaymentIn) (uuid.UUID, string, error) {
	if in.AmountPaise <= 0 {
		return uuid.Nil, "", invalid("the amount must be positive")
	}
	var ref, note *string
	if r := strings.TrimSpace(in.Reference); r != "" {
		ref = &r
	}
	if n := strings.TrimSpace(in.Note); n != "" {
		note = &n
	}
	var pid uuid.UUID
	if err := tx.QueryRow(ctx, `INSERT INTO payments (society_id, unit_id, amount_paise, unallocated_paise, mode, reference, paid_on, received_by, order_id, note)
		VALUES ($1,$2,$3,$3,$4,$5,$6,$7,$8,$9) RETURNING id`, in.Society, in.Unit, in.AmountPaise, in.Mode, ref, in.PaidOn, in.ReceivedBy, in.OrderID, note).Scan(&pid); err != nil {
		return uuid.Nil, "", err
	}
	number := ""
	kind, reason := "payment", in.Reason
	if in.Mode == "waiver" {
		kind = "waiver"
	} else {
		n, err := nextNumber(ctx, tx, "receipt_counters", in.Society, in.PaidOn.Format("0601"), "RCP")
		if err != nil {
			return uuid.Nil, "", err
		}
		number = n
		if _, err := tx.Exec(ctx, `INSERT INTO receipts (payment_id, society_id, number) VALUES ($1,$2,$3)`, pid, in.Society, number); err != nil {
			return uuid.Nil, "", err
		}
		if reason == "" {
			reason = "Payment " + number
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger_entries (society_id, unit_id, kind, amount_paise, payment_id, reason, created_by) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		in.Society, in.Unit, kind, -in.AmountPaise, pid, reason, in.ReceivedBy); err != nil {
		return uuid.Nil, "", err
	}
	if err := s.settle(ctx, tx, in.Society, in.Unit); err != nil {
		return uuid.Nil, "", err
	}
	return pid, number, nil
}

// AfterPayment tells the flat and the office about a recorded payment (call after the transaction commits).
func (s *Service) AfterPayment(ctx context.Context, soc, unit, payment uuid.UUID, amount int64, receipt string) {
	s.tellFlat(ctx, soc, unit, "bill.paid", "Payment received", fmt.Sprintf("%s received · receipt %s", money.Rupees(amount), receipt), map[string]any{"type": "payment", "payment_id": payment}, "normal")
	s.publish(ctx, soc, unit, "payment.updated", payment)
}

type Allocation struct {
	InvoiceNumber string `json:"invoice_number"`
	Period        string `json:"period"`
	AmountPaise   int64  `json:"amount_paise"`
}

type Payment struct {
	ID          uuid.UUID    `json:"id"`
	UnitID      uuid.UUID    `json:"unit_id"`
	Flat        string       `json:"flat"`
	AmountPaise int64        `json:"amount_paise"`
	Mode        string       `json:"mode"`
	Reference   *string      `json:"reference,omitempty"`
	PaidOn      string       `json:"paid_on"`
	Status      string       `json:"status"`
	Receipt     *string      `json:"receipt_number,omitempty"`
	Note        *string      `json:"note,omitempty"`
	Allocations []Allocation `json:"allocations,omitempty"`
	CreatedAt   time.Time    `json:"created_at"`
}

const paymentCols = `p.id, p.unit_id, bl.code || '-' || u.number, p.amount_paise, p.mode, p.reference, to_char(p.paid_on,'YYYY-MM-DD'), p.status, r.number, p.note, p.created_at`
const paymentFrom = ` FROM payments p JOIN units u ON u.id=p.unit_id JOIN blocks bl ON bl.id=u.block_id LEFT JOIN receipts r ON r.payment_id=p.id `

func scanPayment(row pgx.Row, extra ...any) (*Payment, error) {
	var x Payment
	if err := row.Scan(append([]any{&x.ID, &x.UnitID, &x.Flat, &x.AmountPaise, &x.Mode, &x.Reference, &x.PaidOn, &x.Status, &x.Receipt, &x.Note, &x.CreatedAt}, extra...)...); err != nil {
		return nil, err
	}
	return &x, nil
}

func (s *Service) allocations(ctx context.Context, id uuid.UUID) ([]Allocation, error) {
	rows, err := s.pool.Query(ctx, `SELECT i.number, i.period, a.amount_paise FROM payment_allocations a JOIN invoices i ON i.id=a.invoice_id WHERE a.payment_id=$1 ORDER BY i.due_date, i.created_at`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Allocation{}
	for rows.Next() {
		var a Allocation
		var n *string
		if err := rows.Scan(&n, &a.Period, &a.AmountPaise); err != nil {
			return nil, err
		}
		if n != nil {
			a.InvoiceNumber = *n
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Service) Payment(ctx context.Context, soc, id uuid.UUID) (*Payment, error) {
	x, err := scanPayment(s.pool.QueryRow(ctx, `SELECT `+paymentCols+paymentFrom+`WHERE p.id=$1 AND p.society_id=$2`, id, soc))
	if err != nil {
		return nil, ErrNotFound
	}
	x.Allocations, err = s.allocations(ctx, id)
	return x, err
}

// RecordPayment is the office entering money received outside the app.
func (s *Service) RecordPayment(ctx context.Context, p authctx.Principal, unit uuid.UUID, amount int64, mode, reference, paidOn, note string) (*Payment, error) {
	switch mode {
	case "cash", "cheque", "upi_transfer", "bank_transfer":
	default:
		return nil, invalid("mode must be cash, cheque, upi_transfer or bank_transfer")
	}
	if amount < 100 || amount > 100_000_000_00 {
		return nil, invalid("the amount must be between ₹1 and ₹10,00,00,000")
	}
	if len([]rune(reference)) > 80 || len([]rune(note)) > 200 {
		return nil, invalid("reference is limited to 80 and the note to 200 characters")
	}
	if (mode == "cheque" || mode == "upi_transfer" || mode == "bank_transfer") && strings.TrimSpace(reference) == "" {
		return nil, invalid("enter the cheque number or transaction reference")
	}
	var one int
	if err := s.pool.QueryRow(ctx, `SELECT 1 FROM units WHERE id=$1 AND society_id=$2`, unit, p.SocietyID).Scan(&one); err != nil {
		return nil, ErrNotFound
	}
	day := s.today(ctx, p.SocietyID)
	if paidOn != "" {
		d, err := time.Parse("2006-01-02", paidOn)
		if err != nil {
			return nil, invalid("paid_on must be YYYY-MM-DD")
		}
		if d.After(day) || d.Before(day.AddDate(0, -6, 0)) {
			return nil, invalid("the payment date must be within the last six months and not in the future")
		}
		day = d
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	pid, number, err := s.CreatePaymentTx(ctx, tx, PaymentIn{Society: p.SocietyID, Unit: unit, AmountPaise: amount, Mode: mode, Reference: reference, PaidOn: day, ReceivedBy: &p.MembershipID, Note: note})
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	s.log(ctx, p, "payment.record", "payment", pid, map[string]any{"amount_paise": amount, "mode": mode, "receipt": number})
	s.AfterPayment(ctx, p.SocietyID, unit, pid, amount, number)
	return s.Payment(ctx, p.SocietyID, pid)
}

// Bounce reverses a cheque payment: the money leaves the invoices it settled and the flat owes it again.
func (s *Service) Bounce(ctx context.Context, p authctx.Principal, id uuid.UUID, reason string) (*Payment, error) {
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
	var mode, status string
	var amount int64
	if err := tx.QueryRow(ctx, `SELECT unit_id, mode, status, amount_paise FROM payments WHERE id=$1 AND society_id=$2 FOR UPDATE`, id, p.SocietyID).Scan(&unit, &mode, &status, &amount); err != nil {
		return nil, ErrNotFound
	}
	if mode != "cheque" || status != "ok" {
		return nil, ErrInvalidState
	}
	rows, err := tx.Query(ctx, `SELECT invoice_id, amount_paise FROM payment_allocations WHERE payment_id=$1`, id)
	if err != nil {
		return nil, err
	}
	type al struct {
		inv uuid.UUID
		amt int64
	}
	var als []al
	for rows.Next() {
		var a al
		if err := rows.Scan(&a.inv, &a.amt); err != nil {
			rows.Close()
			return nil, err
		}
		als = append(als, a)
	}
	rows.Close()
	for _, a := range als {
		if _, err := tx.Exec(ctx, `UPDATE invoices SET paid_paise=paid_paise-$2, status=CASE WHEN paid_paise-$2 <= 0 THEN 'issued' ELSE 'partially_paid' END WHERE id=$1 AND status <> 'void'`, a.inv, a.amt); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM payment_allocations WHERE payment_id=$1`, id); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE payments SET status='reversed', unallocated_paise=0, note=COALESCE(note || ' · ', '') || $2 WHERE id=$1`, id, "Bounced: "+reason); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger_entries (society_id, unit_id, kind, amount_paise, payment_id, reason, created_by) VALUES ($1,$2,'reversal',$3,$4,$5,$6)`,
		p.SocietyID, unit, amount, id, "Cheque bounced: "+reason, p.MembershipID); err != nil {
		return nil, err
	}
	// money that was freed can settle other open invoices only through new payments; nothing else to apply
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	s.log(ctx, p, "payment.bounce", "payment", id, map[string]any{"reason": reason, "amount_paise": amount})
	s.tellFlat(ctx, p.SocietyID, unit, "bill.bounced", "Cheque bounced", fmt.Sprintf("Your cheque of %s was returned: %s", money.Rupees(amount), reason), map[string]any{"type": "payment", "payment_id": id}, "normal")
	s.publish(ctx, p.SocietyID, unit, "payment.updated", id)
	return s.Payment(ctx, p.SocietyID, id)
}

// Adjust is the office correcting a flat's balance: a credit (waiver, discount) or a debit (an extra charge).
func (s *Service) Adjust(ctx context.Context, p authctx.Principal, unit uuid.UUID, kind string, amount int64, reason string) error {
	reason = strings.TrimSpace(reason)
	if l := len([]rune(reason)); l < 3 || l > 200 {
		return invalid("give a reason of 3-200 characters")
	}
	if amount < 100 || amount > 100_000_000_00 {
		return invalid("the amount must be between ₹1 and ₹10,00,00,000")
	}
	var one int
	if err := s.pool.QueryRow(ctx, `SELECT 1 FROM units WHERE id=$1 AND society_id=$2`, unit, p.SocietyID).Scan(&one); err != nil {
		return ErrNotFound
	}
	switch kind {
	case "debit":
		_, err := s.AdHoc(ctx, p, unit, reason, amount, "", "Adjustment")
		return err
	case "credit":
		today := s.today(ctx, p.SocietyID) // before the transaction starts: a second connection while one is held can deadlock the pool
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		pid, _, err := s.CreatePaymentTx(ctx, tx, PaymentIn{Society: p.SocietyID, Unit: unit, AmountPaise: amount, Mode: "waiver", PaidOn: today, ReceivedBy: &p.MembershipID, Note: reason, Reason: "Credit: " + reason})
		if err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		s.log(ctx, p, "ledger.credit", "payment", pid, map[string]any{"amount_paise": amount, "reason": reason})
		s.tellFlat(ctx, p.SocietyID, unit, "bill.credit", "Credit added", fmt.Sprintf("%s credited to your account: %s", money.Rupees(amount), reason), map[string]any{"type": "payment", "payment_id": pid}, "normal")
		s.publish(ctx, p.SocietyID, unit, "payment.updated", pid)
		return nil
	}
	return invalid("kind must be credit or debit")
}

// ── statements ───────────────────────────────────────────────────────────────

type Entry struct {
	At           time.Time `json:"at"`
	Kind         string    `json:"kind"`
	AmountPaise  int64     `json:"amount_paise"` // positive = owed more, negative = owed less
	Reason       string    `json:"reason"`
	BalancePaise int64     `json:"balance_paise"`
}

type Statement struct {
	Flat         string  `json:"flat"`
	BalancePaise int64   `json:"balance_paise"` // positive = the flat owes, negative = advance credit
	Entries      []Entry `json:"entries"`
}

func (s *Service) statement(ctx context.Context, soc, unit uuid.UUID, limit int) (*Statement, error) {
	st := &Statement{Flat: s.flatLabel(ctx, unit), Entries: []Entry{}}
	rows, err := s.pool.Query(ctx, `SELECT created_at, kind, amount_paise, COALESCE(reason,'') FROM ledger_entries WHERE society_id=$1 AND unit_id=$2 ORDER BY created_at, id`, soc, unit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []Entry
	var run int64
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.At, &e.Kind, &e.AmountPaise, &e.Reason); err != nil {
			return nil, err
		}
		run += e.AmountPaise
		e.BalancePaise = run
		all = append(all, e)
	}
	st.BalancePaise = run
	// newest first, capped
	for i := len(all) - 1; i >= 0 && len(st.Entries) < limit; i-- {
		st.Entries = append(st.Entries, all[i])
	}
	return st, rows.Err()
}

// MyLedger is the caller's flat statement.
func (s *Service) MyLedger(ctx context.Context, p authctx.Principal) (*Statement, error) {
	unit, err := s.unitOf(ctx, p)
	if err != nil {
		return &Statement{Entries: []Entry{}}, nil
	}
	return s.statement(ctx, p.SocietyID, unit, 200)
}

// FlatLedger is the office view of one flat's statement.
func (s *Service) FlatLedger(ctx context.Context, p authctx.Principal, unit uuid.UUID) (*Statement, error) {
	var one int
	if err := s.pool.QueryRow(ctx, `SELECT 1 FROM units WHERE id=$1 AND society_id=$2`, unit, p.SocietyID).Scan(&one); err != nil {
		return nil, ErrNotFound
	}
	return s.statement(ctx, p.SocietyID, unit, 500)
}

// MyPayments lists the flat's payments (the money that came in, newest first).
func (s *Service) MyPayments(ctx context.Context, p authctx.Principal, limit, offset int) ([]*Payment, int, error) {
	unit, err := s.unitOf(ctx, p)
	if err != nil {
		return []*Payment{}, 0, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT `+paymentCols+`, count(*) OVER() `+paymentFrom+`WHERE p.society_id=$1 AND p.unit_id=$2 AND p.mode <> 'waiver' ORDER BY p.created_at DESC LIMIT $3 OFFSET $4`, p.SocietyID, unit, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*Payment
	total := 0
	for rows.Next() {
		x, err := scanPayment(rows, &total)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, x)
	}
	return out, total, rows.Err()
}

// MyPayment reads one payment of the caller's flat.
func (s *Service) MyPayment(ctx context.Context, p authctx.Principal, id uuid.UUID) (*Payment, error) {
	unit, err := s.unitOf(ctx, p)
	if err != nil {
		return nil, ErrNotFound
	}
	x, err := s.Payment(ctx, p.SocietyID, id)
	if err != nil || x.UnitID != unit || x.Mode == "waiver" {
		return nil, ErrNotFound
	}
	return x, nil
}

// Outstanding is what the flat owes on open invoices right now.
func (s *Service) Outstanding(ctx context.Context, unit uuid.UUID) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(sum(total_paise - paid_paise),0) FROM invoices WHERE unit_id=$1 AND status IN ('issued','partially_paid')`, unit).Scan(&n)
	return n, err
}

// Summary is the resident's dues card.
type Summary struct {
	OutstandingPaise int64   `json:"outstanding_paise"`
	OverduePaise     int64   `json:"overdue_paise"`
	OpenInvoices     int     `json:"open_invoices"`
	NextDue          *string `json:"next_due,omitempty"`
	CreditPaise      int64   `json:"credit_paise"`
	CanPay           bool    `json:"can_pay"`
}

func (s *Service) MySummary(ctx context.Context, p authctx.Principal) (*Summary, error) {
	unit, err := s.unitOf(ctx, p)
	if err != nil {
		return &Summary{}, nil
	}
	sm := &Summary{}
	var next *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(sum(total_paise-paid_paise),0), COALESCE(sum(total_paise-paid_paise) FILTER (WHERE due_date < $2),0), count(*), min(due_date)
		FROM invoices WHERE unit_id=$1 AND status IN ('issued','partially_paid')`, unit, s.today(ctx, p.SocietyID)).Scan(&sm.OutstandingPaise, &sm.OverduePaise, &sm.OpenInvoices, &next); err != nil {
		return nil, err
	}
	if next != nil {
		d := next.Format("2006-01-02")
		sm.NextDue = &d
	}
	_ = s.pool.QueryRow(ctx, `SELECT COALESCE(sum(unallocated_paise),0) FROM payments WHERE unit_id=$1 AND status='ok'`, unit).Scan(&sm.CreditPaise)
	sm.CanPay = s.CanPay(ctx, p)
	return sm, nil
}

// CanPay: owners always, tenants only when the office enabled it for them; family never.
func (s *Service) CanPay(ctx context.Context, p authctx.Principal) bool {
	var role string
	var flag bool
	if err := s.pool.QueryRow(ctx, `SELECT role_key, can_pay_maintenance FROM memberships WHERE id=$1 AND user_id=$2 AND society_id=$3 AND status='active'`, p.MembershipID, p.UserID, p.SocietyID).Scan(&role, &flag); err != nil {
		return false
	}
	return role == "resident_owner" || (role == "resident_tenant" && flag)
}

// AdminPayments lists payments for the office (newest first), optionally for one flat.
func (s *Service) AdminPayments(ctx context.Context, p authctx.Principal, unit *uuid.UUID, limit, offset int) ([]*Payment, int, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+paymentCols+`, count(*) OVER() `+paymentFrom+`WHERE p.society_id=$1 AND ($2::uuid IS NULL OR p.unit_id=$2) ORDER BY p.created_at DESC LIMIT $3 OFFSET $4`, p.SocietyID, unit, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*Payment
	total := 0
	for rows.Next() {
		x, err := scanPayment(rows, &total)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, x)
	}
	return out, total, rows.Err()
}

// UnitsByNumber maps "A101"-style unit numbers to ids (used to seed demo data).
func (s *Service) UnitsByNumber(ctx context.Context, soc uuid.UUID) (map[string]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx, `SELECT number, id FROM units WHERE society_id=$1`, soc)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]uuid.UUID{}
	for rows.Next() {
		var n string
		var id uuid.UUID
		if err := rows.Scan(&n, &id); err != nil {
			return nil, err
		}
		out[n] = id
	}
	return out, rows.Err()
}

// InvoiceTotal is the total of a flat's regular invoice for a period (0 when there is none).
func (s *Service) InvoiceTotal(ctx context.Context, unit uuid.UUID, period string) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(sum(total_paise),0) FROM invoices WHERE unit_id=$1 AND period=$2 AND kind='regular' AND status <> 'void'`, unit, period).Scan(&n)
	return n, err
}
