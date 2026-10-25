package seed

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Nexora48/nexora-api/internal/billing"
	"github.com/Nexora48/nexora-api/internal/platform/authctx"
)

// DemoBilling gives the demo society charge heads, three months of invoices and a believable payment history:
// the demo flat (A101) paid the oldest month and owes the last two. It goes through the billing service so the
// ledger, receipts and numbers are exactly what the real flow produces. Idempotent.
func DemoBilling(ctx context.Context, b *billing.Service) error {
	soc := uuid.MustParse(SocietyID)
	p := authctx.Principal{UserID: uuid.MustParse(AdminUserID), SocietyID: soc, MembershipID: uuid.MustParse(AdminMemID), Role: "society_admin"}
	heads, err := b.Heads(ctx, soc, false)
	if err != nil || len(heads) > 0 {
		return err
	}
	if _, err := b.SetSettings(ctx, p, billing.Settings{DueDay: 10, GraceDays: 5, LateFeeMode: "flat", LateFeeValue: 10000}); err != nil {
		return err
	}
	for _, h := range []billing.HeadInput{
		{Name: "Maintenance", Calc: "per_unit_type", AmountPaise: 420000, Recurring: true, Active: true, Rates: []billing.Rate{{UnitType: "2BHK", AmountPaise: 420000}, {UnitType: "3BHK", AmountPaise: 520000}}},
		{Name: "Sinking fund", Calc: "fixed", AmountPaise: 50000, Recurring: true, Active: true},
		{Name: "Water charges", Calc: "fixed", AmountPaise: 30000, Recurring: true, Active: true},
	} {
		if _, err := b.CreateHead(ctx, p, h); err != nil {
			return fmt.Errorf("demo billing head: %w", err)
		}
	}
	now := time.Now()
	period := func(back int) string {
		return time.Date(now.Year(), now.Month()-time.Month(back), 1, 0, 0, 0, 0, time.UTC).Format("2006-01")
	}
	for _, back := range []int{2, 1, 0} {
		if _, err := b.Generate(ctx, p, period(back)); err != nil {
			return fmt.Errorf("demo billing generate: %w", err)
		}
		if _, err := b.Issue(ctx, p, period(back)); err != nil {
			return fmt.Errorf("demo billing issue: %w", err)
		}
	}
	// who paid what (flat number -> months back that are settled, oldest first)
	units := map[string]uuid.UUID{}
	rows, err := b.UnitsByNumber(ctx, soc)
	if err != nil {
		return err
	}
	for k, v := range rows {
		units[k] = v
	}
	plan := []struct {
		flat string
		pays []int
		mode string
	}{
		{"A101", []int{2}, "bank_transfer"},
		{"A102", []int{2, 1, 0}, "upi_transfer"},
		{"A103", []int{2, 1}, "cash"},
		{"A104", []int{2, 1, 0}, "bank_transfer"},
		{"B101", []int{2, 1}, "cash"},
		{"B102", []int{2}, "upi_transfer"},
		{"B201", []int{2, 1, 0}, "bank_transfer"},
		{"C101", []int{2, 1}, "upi_transfer"},
	}
	for _, x := range plan {
		unit, ok := units[x.flat]
		if !ok {
			continue
		}
		due, err := b.Outstanding(ctx, unit)
		if err != nil {
			return err
		}
		_ = due
		for _, back := range x.pays {
			amount, err := b.InvoiceTotal(ctx, unit, period(back))
			if err != nil || amount <= 0 {
				continue
			}
			ref := ""
			if x.mode != "cash" {
				ref = fmt.Sprintf("REF%s%d", x.flat, back)
			}
			if _, err := b.RecordPayment(ctx, p, unit, amount, x.mode, ref, "", "Demo data"); err != nil {
				return fmt.Errorf("demo billing payment: %w", err)
			}
		}
	}
	_, _, err = b.Sweep(ctx)
	return err
}
