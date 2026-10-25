package billing

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
	"github.com/Nexora48/nexora-api/internal/platform/pagination"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func principal(r *http.Request) authctx.Principal { p, _ := authctx.From(r.Context()); return p }

func param(r *http.Request, name string) (uuid.UUID, error) {
	v, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		return uuid.Nil, ErrNotFound
	}
	return v, nil
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := httpx.Decode(w, r, dst); err != nil {
		httpx.Fail(w, err)
		return false
	}
	return true
}

func ok(w http.ResponseWriter, v any) { httpx.OK(w, v, nil) }

func fail(w http.ResponseWriter, err error) { httpx.Fail(w, err) }

func invoices(w http.ResponseWriter, items []*Invoice, pg pagination.Params, total int) {
	if items == nil {
		items = []*Invoice{}
	}
	httpx.OK(w, items, pg.Meta(total))
}

func payments(w http.ResponseWriter, items []*Payment, pg pagination.Params, total int) {
	if items == nil {
		items = []*Payment{}
	}
	httpx.OK(w, items, pg.Meta(total))
}

// ── residents ────────────────────────────────────────────────────────────────

func (h *Handler) Summary(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.MySummary(r.Context(), principal(r))
	if err != nil {
		fail(w, err)
		return
	}
	ok(w, v)
}

func (h *Handler) Bills(w http.ResponseWriter, r *http.Request) {
	pg := pagination.FromRequest(r)
	g := r.URL.Query().Get("group")
	if g != "" && g != "open" && g != "paid" {
		fail(w, invalid("group must be open or paid"))
		return
	}
	items, total, err := h.svc.MyBills(r.Context(), principal(r), g, pg.PageSize, pg.Offset())
	if err != nil {
		fail(w, err)
		return
	}
	invoices(w, items, pg, total)
}

func (h *Handler) Bill(w http.ResponseWriter, r *http.Request) {
	id, err := param(r, "id")
	if err != nil {
		fail(w, err)
		return
	}
	v, err := h.svc.MyInvoice(r.Context(), principal(r), id)
	if err != nil {
		fail(w, err)
		return
	}
	ok(w, v)
}

func (h *Handler) Ledger(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.MyLedger(r.Context(), principal(r))
	if err != nil {
		fail(w, err)
		return
	}
	ok(w, v)
}

func (h *Handler) Payments(w http.ResponseWriter, r *http.Request) {
	pg := pagination.FromRequest(r)
	items, total, err := h.svc.MyPayments(r.Context(), principal(r), pg.PageSize, pg.Offset())
	if err != nil {
		fail(w, err)
		return
	}
	payments(w, items, pg, total)
}

func (h *Handler) receipt(office, pdf bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := param(r, "id")
		if err != nil {
			fail(w, err)
			return
		}
		var rc *Receipt
		if office {
			rc, err = h.svc.OfficeReceipt(r.Context(), principal(r), id)
		} else {
			rc, err = h.svc.MyReceipt(r.Context(), principal(r), id)
		}
		if err != nil {
			fail(w, err)
			return
		}
		if !pdf {
			ok(w, rc)
			return
		}
		b, err := rc.PDF()
		if err != nil {
			fail(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Disposition", `attachment; filename="`+rc.Number+`.pdf"`)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(b)
	}
}

func (h *Handler) Receipt() http.HandlerFunc    { return h.receipt(false, false) }
func (h *Handler) ReceiptPDF() http.HandlerFunc { return h.receipt(false, true) }

// ── office ───────────────────────────────────────────────────────────────────

func (h *Handler) GetSettings(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.Settings(r.Context(), principal(r).SocietyID)
	if err != nil {
		fail(w, err)
		return
	}
	ok(w, v)
}

func (h *Handler) PutSettings(w http.ResponseWriter, r *http.Request) {
	var in Settings
	if !decode(w, r, &in) {
		return
	}
	v, err := h.svc.SetSettings(r.Context(), principal(r), in)
	if err != nil {
		fail(w, err)
		return
	}
	ok(w, v)
}

type headRequest struct {
	Name          string `json:"name"`
	Calc          string `json:"calc"`
	AmountPaise   int64  `json:"amount_paise"`
	RatePaise     int64  `json:"rate_paise"`
	Recurring     *bool  `json:"recurring"`
	OneTimePeriod string `json:"one_time_period"`
	Active        *bool  `json:"active"`
	Rates         []Rate `json:"rates"`
}

func (q headRequest) input() HeadInput {
	return HeadInput{Name: q.Name, Calc: q.Calc, AmountPaise: q.AmountPaise, RatePaise: q.RatePaise, Recurring: q.Recurring == nil || *q.Recurring,
		OneTimePeriod: q.OneTimePeriod, Active: q.Active == nil || *q.Active, Rates: q.Rates}
}

func (h *Handler) Heads(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.Heads(r.Context(), principal(r).SocietyID, false)
	if err != nil {
		fail(w, err)
		return
	}
	ok(w, v)
}

func (h *Handler) CreateHead(w http.ResponseWriter, r *http.Request) {
	var q headRequest
	if !decode(w, r, &q) {
		return
	}
	v, err := h.svc.CreateHead(r.Context(), principal(r), q.input())
	if err != nil {
		fail(w, err)
		return
	}
	httpx.Created(w, v)
}

func (h *Handler) UpdateHead(w http.ResponseWriter, r *http.Request) {
	id, err := param(r, "id")
	if err != nil {
		fail(w, err)
		return
	}
	var q headRequest
	if !decode(w, r, &q) {
		return
	}
	v, err := h.svc.UpdateHead(r.Context(), principal(r), id, q.input())
	if err != nil {
		fail(w, err)
		return
	}
	ok(w, v)
}

func (h *Handler) DeleteHead(w http.ResponseWriter, r *http.Request) {
	id, err := param(r, "id")
	if err != nil {
		fail(w, err)
		return
	}
	if err := h.svc.DeactivateHead(r.Context(), principal(r), id); err != nil {
		fail(w, err)
		return
	}
	ok(w, map[string]bool{"ok": true})
}

type periodRequest struct {
	Period string `json:"period"`
}

func (h *Handler) Generate(w http.ResponseWriter, r *http.Request) {
	var q periodRequest
	if !decode(w, r, &q) {
		return
	}
	v, err := h.svc.Generate(r.Context(), principal(r), q.Period)
	if err != nil {
		fail(w, err)
		return
	}
	ok(w, v)
}

func (h *Handler) Issue(w http.ResponseWriter, r *http.Request) {
	var q periodRequest
	if !decode(w, r, &q) {
		return
	}
	n, err := h.svc.Issue(r.Context(), principal(r), q.Period)
	if err != nil {
		fail(w, err)
		return
	}
	ok(w, map[string]int{"issued": n})
}

func (h *Handler) AdHoc(w http.ResponseWriter, r *http.Request) {
	var q struct {
		UnitID      string `json:"unit_id"`
		Label       string `json:"label"`
		AmountPaise int64  `json:"amount_paise"`
		DueDate     string `json:"due_date"`
		Note        string `json:"note"`
	}
	if !decode(w, r, &q) {
		return
	}
	unit, err := uuid.Parse(q.UnitID)
	if err != nil {
		fail(w, invalid("unit_id must be a UUID"))
		return
	}
	v, err := h.svc.AdHoc(r.Context(), principal(r), unit, q.Label, q.AmountPaise, q.DueDate, q.Note)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.Created(w, v)
}

func (h *Handler) AdminInvoices(w http.ResponseWriter, r *http.Request) {
	pg := pagination.FromRequest(r)
	q := r.URL.Query()
	f := InvoiceFilter{Period: q.Get("period"), Status: q.Get("status"), Kind: q.Get("kind"), Block: q.Get("block"), Flat: q.Get("flat"), Limit: pg.PageSize, Offset: pg.Offset()}
	if f.Period != "" {
		if err := validPeriod(f.Period); err != nil {
			fail(w, err)
			return
		}
	}
	switch f.Status {
	case "", "draft", "open", "overdue", "issued", "partially_paid", "paid", "void":
	default:
		fail(w, invalid("invalid status"))
		return
	}
	items, total, err := h.svc.AdminInvoices(r.Context(), principal(r), f)
	if err != nil {
		fail(w, err)
		return
	}
	invoices(w, items, pg, total)
}

func (h *Handler) AdminInvoice(w http.ResponseWriter, r *http.Request) {
	id, err := param(r, "id")
	if err != nil {
		fail(w, err)
		return
	}
	v, err := h.svc.Invoice(r.Context(), principal(r).SocietyID, id)
	if err != nil {
		fail(w, err)
		return
	}
	ok(w, v)
}

func (h *Handler) Void(w http.ResponseWriter, r *http.Request) {
	id, err := param(r, "id")
	if err != nil {
		fail(w, err)
		return
	}
	var q struct {
		Reason string `json:"reason"`
	}
	if !decode(w, r, &q) {
		return
	}
	v, err := h.svc.Void(r.Context(), principal(r), id, q.Reason)
	if err != nil {
		fail(w, err)
		return
	}
	ok(w, v)
}

func (h *Handler) RecordPayment(w http.ResponseWriter, r *http.Request) {
	var q struct {
		UnitID      string `json:"unit_id"`
		AmountPaise int64  `json:"amount_paise"`
		Mode        string `json:"mode"`
		Reference   string `json:"reference"`
		PaidOn      string `json:"paid_on"`
		Note        string `json:"note"`
	}
	if !decode(w, r, &q) {
		return
	}
	unit, err := uuid.Parse(q.UnitID)
	if err != nil {
		fail(w, invalid("unit_id must be a UUID"))
		return
	}
	v, err := h.svc.RecordPayment(r.Context(), principal(r), unit, q.AmountPaise, q.Mode, q.Reference, q.PaidOn, q.Note)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.Created(w, v)
}

func (h *Handler) AdminPayments(w http.ResponseWriter, r *http.Request) {
	pg := pagination.FromRequest(r)
	var unit *uuid.UUID
	if v := r.URL.Query().Get("unit_id"); v != "" {
		u, err := uuid.Parse(v)
		if err != nil {
			fail(w, invalid("invalid unit_id"))
			return
		}
		unit = &u
	}
	items, total, err := h.svc.AdminPayments(r.Context(), principal(r), unit, pg.PageSize, pg.Offset())
	if err != nil {
		fail(w, err)
		return
	}
	payments(w, items, pg, total)
}

func (h *Handler) Bounce(w http.ResponseWriter, r *http.Request) {
	id, err := param(r, "id")
	if err != nil {
		fail(w, err)
		return
	}
	var q struct {
		Reason string `json:"reason"`
	}
	if !decode(w, r, &q) {
		return
	}
	v, err := h.svc.Bounce(r.Context(), principal(r), id, q.Reason)
	if err != nil {
		fail(w, err)
		return
	}
	ok(w, v)
}

func (h *Handler) OfficeReceipt() http.HandlerFunc    { return h.receipt(true, false) }
func (h *Handler) OfficeReceiptPDF() http.HandlerFunc { return h.receipt(true, true) }

func (h *Handler) Adjust(w http.ResponseWriter, r *http.Request) {
	var q struct {
		UnitID      string `json:"unit_id"`
		Kind        string `json:"kind"`
		AmountPaise int64  `json:"amount_paise"`
		Reason      string `json:"reason"`
	}
	if !decode(w, r, &q) {
		return
	}
	unit, err := uuid.Parse(q.UnitID)
	if err != nil {
		fail(w, invalid("unit_id must be a UUID"))
		return
	}
	if err := h.svc.Adjust(r.Context(), principal(r), unit, q.Kind, q.AmountPaise, q.Reason); err != nil {
		fail(w, err)
		return
	}
	ok(w, map[string]bool{"ok": true})
}

func (h *Handler) FlatLedger(w http.ResponseWriter, r *http.Request) {
	id, err := param(r, "id")
	if err != nil {
		fail(w, err)
		return
	}
	v, err := h.svc.FlatLedger(r.Context(), principal(r), id)
	if err != nil {
		fail(w, err)
		return
	}
	ok(w, v)
}

func (h *Handler) Dues(w http.ResponseWriter, r *http.Request) {
	pg := pagination.FromRequest(r)
	q := r.URL.Query()
	rows, total, ag, err := h.svc.Dues(r.Context(), principal(r), DuesFilter{Block: q.Get("block"), Flat: q.Get("flat"), OverdueOnly: q.Get("overdue") == "1", Limit: pg.PageSize, Offset: pg.Offset()})
	if err != nil {
		fail(w, err)
		return
	}
	httpx.OK(w, map[string]any{"rows": rows, "ageing": ag}, pg.Meta(total))
}

func (h *Handler) Remind(w http.ResponseWriter, r *http.Request) {
	var q struct {
		UnitIDs []string `json:"unit_ids"`
	}
	if r.ContentLength != 0 && !decode(w, r, &q) {
		return
	}
	var ids []uuid.UUID
	for _, s := range q.UnitIDs {
		u, err := uuid.Parse(s)
		if err != nil {
			fail(w, invalid("unit_ids must be UUIDs"))
			return
		}
		ids = append(ids, u)
	}
	n, err := h.svc.Remind(r.Context(), principal(r), ids)
	if err != nil {
		fail(w, err)
		return
	}
	ok(w, map[string]int{"sent": n})
}

func (h *Handler) Collection(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.Collection(r.Context(), principal(r), r.URL.Query().Get("period"))
	if err != nil {
		fail(w, err)
		return
	}
	ok(w, v)
}

func (h *Handler) ExportInvoices(w http.ResponseWriter, r *http.Request) {
	period := r.URL.Query().Get("period")
	if err := validPeriod(period); err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="invoices-`+period+`.csv"`)
	if _, err := h.svc.ExportInvoices(r.Context(), principal(r), period, w); err != nil {
		fail(w, err)
	}
}

func (h *Handler) ExportDefaulters(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="defaulters.csv"`)
	if err := h.svc.ExportDefaulters(r.Context(), principal(r), w); err != nil {
		fail(w, err)
	}
}
