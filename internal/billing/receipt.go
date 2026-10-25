package billing

import (
	"bytes"
	"context"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/google/uuid"

	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/money"
)

type Receipt struct {
	Number      string       `json:"number"`
	IssuedAt    time.Time    `json:"issued_at"`
	Society     string       `json:"society"`
	Flat        string       `json:"flat"`
	AmountPaise int64        `json:"amount_paise"`
	Mode        string       `json:"mode"`
	Reference   *string      `json:"reference,omitempty"`
	PaidOn      string       `json:"paid_on"`
	Status      string       `json:"status"`
	Items       []Allocation `json:"items"`
}

func (s *Service) receipt(ctx context.Context, soc, paymentID uuid.UUID) (*Receipt, error) {
	pay, err := s.Payment(ctx, soc, paymentID)
	if err != nil || pay.Receipt == nil {
		return nil, ErrNotFound
	}
	r := &Receipt{Number: *pay.Receipt, Flat: pay.Flat, AmountPaise: pay.AmountPaise, Mode: pay.Mode, Reference: pay.Reference, PaidOn: pay.PaidOn, Status: pay.Status, Items: pay.Allocations}
	_ = s.pool.QueryRow(ctx, `SELECT rc.issued_at, so.name FROM receipts rc JOIN societies so ON so.id=rc.society_id WHERE rc.payment_id=$1`, paymentID).Scan(&r.IssuedAt, &r.Society)
	return r, nil
}

// MyReceipt is the receipt of one of the caller's flat's payments.
func (s *Service) MyReceipt(ctx context.Context, p authctx.Principal, paymentID uuid.UUID) (*Receipt, error) {
	if _, err := s.MyPayment(ctx, p, paymentID); err != nil {
		return nil, err
	}
	return s.receipt(ctx, p.SocietyID, paymentID)
}

// OfficeReceipt is the receipt of any payment in the society.
func (s *Service) OfficeReceipt(ctx context.Context, p authctx.Principal, paymentID uuid.UUID) (*Receipt, error) {
	return s.receipt(ctx, p.SocietyID, paymentID)
}

var modeLabels = map[string]string{"razorpay": "Online (Razorpay)", "fake": "Online (test)", "cash": "Cash", "cheque": "Cheque", "upi_transfer": "UPI transfer", "bank_transfer": "Bank transfer"}

// PDF renders a receipt as a one-page document.
func (r *Receipt) PDF() ([]byte, error) {
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetTitle("Receipt "+r.Number, false)
	pdf.AddPage()
	pdf.SetFont("Helvetica", "B", 18)
	pdf.CellFormat(0, 10, r.Society, "", 1, "L", false, 0, "")
	pdf.SetFont("Helvetica", "", 11)
	pdf.CellFormat(0, 7, "Payment receipt", "", 1, "L", false, 0, "")
	pdf.Ln(4)
	row := func(k, v string) {
		pdf.SetFont("Helvetica", "B", 11)
		pdf.CellFormat(45, 7, k, "", 0, "L", false, 0, "")
		pdf.SetFont("Helvetica", "", 11)
		pdf.CellFormat(0, 7, v, "", 1, "L", false, 0, "")
	}
	row("Receipt no.", r.Number)
	row("Date", r.PaidOn)
	row("Flat", r.Flat)
	row("Mode", modeLabels[r.Mode])
	if r.Reference != nil {
		row("Reference", *r.Reference)
	}
	row("Amount", "Rs "+money.Rupees(r.AmountPaise)[len("₹"):])
	if r.Status == "reversed" {
		row("Status", "REVERSED")
	}
	pdf.Ln(6)
	pdf.SetFont("Helvetica", "B", 11)
	pdf.CellFormat(60, 8, "Invoice", "B", 0, "L", false, 0, "")
	pdf.CellFormat(40, 8, "Period", "B", 0, "L", false, 0, "")
	pdf.CellFormat(0, 8, "Applied (Rs)", "B", 1, "R", false, 0, "")
	pdf.SetFont("Helvetica", "", 11)
	for _, it := range r.Items {
		pdf.CellFormat(60, 8, it.InvoiceNumber, "", 0, "L", false, 0, "")
		pdf.CellFormat(40, 8, it.Period, "", 0, "L", false, 0, "")
		pdf.CellFormat(0, 8, money.Rupees(it.AmountPaise)[len("₹"):], "", 1, "R", false, 0, "")
	}
	if len(r.Items) == 0 {
		pdf.CellFormat(0, 8, "Held as advance credit for future bills.", "", 1, "L", false, 0, "")
	}
	pdf.Ln(10)
	pdf.SetFont("Helvetica", "I", 9)
	pdf.CellFormat(0, 6, "This is a computer generated receipt and needs no signature.", "", 1, "L", false, 0, "")
	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
