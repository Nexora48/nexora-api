package billing

import (
	"context"
	"fmt"
	"github.com/Nexora48/nexora-api/internal/platform/heartbeat"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Nexora48/nexora-api/internal/platform/database"
	"github.com/Nexora48/nexora-api/internal/platform/money"
)

type openInvoice struct {
	id, soc, unit uuid.UUID
	number        string
	balance       int64
	due, today    time.Time
}

func reminderKind(days int) string {
	switch {
	case days >= 15:
		return "over-15"
	case days >= 7:
		return "over-7"
	case days >= 0:
		return "due-0"
	case days >= -3:
		return "due-3"
	}
	return ""
}

// Sweep sends the due-date reminders (once per kind per invoice) and adds late fees (once per invoice per month).
func (s *Service) Sweep(ctx context.Context) (reminded, lateFees int, err error) {
	rows, err := s.pool.Query(ctx, `SELECT i.id, i.society_id, i.unit_id, COALESCE(i.number,''), i.total_paise-i.paid_paise, i.due_date, (($1::timestamptz) AT TIME ZONE so.timezone)::date
		FROM invoices i JOIN societies so ON so.id=i.society_id
		WHERE i.status IN ('issued','partially_paid') AND i.kind='regular' AND so.status='active'`, s.now())
	if err != nil {
		return 0, 0, err
	}
	var open []openInvoice
	for rows.Next() {
		var o openInvoice
		if err := rows.Scan(&o.id, &o.soc, &o.unit, &o.number, &o.balance, &o.due, &o.today); err != nil {
			rows.Close()
			return 0, 0, err
		}
		open = append(open, o)
	}
	rows.Close()

	settings := map[uuid.UUID]Settings{}
	for _, o := range open {
		days := int(o.today.Sub(time.Date(o.due.Year(), o.due.Month(), o.due.Day(), 0, 0, 0, 0, time.UTC)).Hours() / 24)
		if kind := reminderKind(days); kind != "" {
			tag, err := s.pool.Exec(ctx, `INSERT INTO invoice_reminders (invoice_id, kind) VALUES ($1,$2) ON CONFLICT DO NOTHING`, o.id, kind)
			if err == nil && tag.RowsAffected() == 1 {
				title, body := "Bill due soon", fmt.Sprintf("%s · %s is due on %s", o.number, money.Rupees(o.balance), o.due.Format("2 Jan"))
				if days >= 0 {
					title, body = "Bill due", fmt.Sprintf("%s · %s is due now", o.number, money.Rupees(o.balance))
				}
				if days >= 7 {
					title, body = "Bill overdue", fmt.Sprintf("%s · %s is %d days overdue", o.number, money.Rupees(o.balance), days)
				}
				s.tellFlat(ctx, o.soc, o.unit, "bill.reminder", title, body, map[string]any{"type": "bill", "invoice_id": o.id}, "normal")
				reminded++
			}
		}
		st, ok := settings[o.soc]
		if !ok {
			st, _ = s.Settings(ctx, o.soc)
			settings[o.soc] = st
		}
		if st.LateFeeMode == "none" || days <= st.GraceDays || o.balance <= 0 {
			continue
		}
		fee := st.LateFeeValue
		if st.LateFeeMode == "percent" {
			fee = money.MulBP(o.balance, st.LateFeeValue)
		}
		if fee <= 0 {
			continue
		}
		period := o.today.Format("2006-01")
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return reminded, lateFees, err
		}
		_, num, _, err := s.createInvoiceTx(ctx, tx, o.soc, o.unit, "late_fee", period, o.today, []Line{{Label: "Late fee for " + o.number, AmountPaise: fee}}, "", "late:"+o.id.String()+":"+period, nil)
		if database.IsUnique(err, "invoices_idem_uq") || (err != nil && strings.Contains(err.Error(), "invoices_idem_uq")) {
			_ = tx.Rollback(ctx)
			continue
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return reminded, lateFees, err
		}
		if err := tx.Commit(ctx); err != nil {
			return reminded, lateFees, err
		}
		lateFees++
		s.tellFlat(ctx, o.soc, o.unit, "bill.late_fee", "Late fee added", fmt.Sprintf("%s added (%s) for %s", money.Rupees(fee), num, o.number), map[string]any{"type": "bill"}, "normal")
	}
	return reminded, lateFees, nil
}

func (s *Service) RunSweeper(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			heartbeat.Beat("billing")
			_, _, _ = s.Sweep(ctx)
		}
	}
}
