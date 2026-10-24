package visitor

import (
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
	"github.com/Nexora48/nexora-api/internal/platform/pagination"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

type createRequest struct {
	VisitorName   string     `json:"visitor_name" validate:"required,min=2,max=80"`
	VisitorPhone  *string    `json:"visitor_phone" validate:"omitempty,max=20"`
	Purpose       string     `json:"purpose" validate:"required,oneof=guest delivery cab service"`
	Company       *string    `json:"company" validate:"omitempty,max=60"`
	VehicleNumber *string    `json:"vehicle_number" validate:"omitempty,max=20"`
	VisitorCount  int        `json:"visitor_count" validate:"omitempty,min=1,max=20"`
	Note          *string    `json:"note" validate:"omitempty,max=200"`
	ExpectedFrom  *time.Time `json:"expected_from"`
	ExpectedUntil *time.Time `json:"expected_until"`
}

type decisionRequest struct {
	Decision string `json:"decision" validate:"required,oneof=approve reject"`
	Reason   string `json:"reason" validate:"max=200"`
}

type verifyRequest struct {
	Code string `json:"code" validate:"omitempty,len=6,numeric"`
	QR   string `json:"qr" validate:"omitempty,max=200"`
}

type walkInRequest struct {
	UnitID        string  `json:"unit_id" validate:"required,uuid"`
	VisitorName   string  `json:"visitor_name" validate:"required,min=2,max=80"`
	VisitorPhone  *string `json:"visitor_phone" validate:"omitempty,max=20"`
	Purpose       string  `json:"purpose" validate:"required,oneof=guest delivery cab service"`
	Company       *string `json:"company" validate:"omitempty,max=60"`
	VehicleNumber *string `json:"vehicle_number" validate:"omitempty,max=20"`
	VisitorCount  int     `json:"visitor_count" validate:"omitempty,min=1,max=20"`
}

func visitID(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return uuid.Nil, ErrNotFound
	}
	return id, nil
}

func principal(r *http.Request) authctx.Principal { p, _ := authctx.From(r.Context()); return p }

// ── resident ─────────────────────────────────────────────────────────────────

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	v, err := h.svc.Create(r.Context(), principal(r), CreateInput(req))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.Created(w, v)
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	pg := pagination.FromRequest(r)
	q := r.URL.Query()
	tab := q.Get("tab")
	if tab != "" && tab != "active" && tab != "history" && tab != "invited" {
		httpx.Fail(w, httpx.NewError(http.StatusBadRequest, "VALIDATION", "invalid tab"))
		return
	}
	rows, total, err := h.svc.List(r.Context(), principal(r), ListFilter{Tab: tab, Purpose: q.Get("purpose"), Limit: pg.PageSize, Offset: pg.Offset()})
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, rows, pg.Meta(total))
}

func (h *Handler) Summary(w http.ResponseWriter, r *http.Request) {
	sm, err := h.svc.Summary(r.Context(), principal(r))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, sm, nil)
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	id, err := visitID(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	v, err := h.svc.Get(r.Context(), principal(r), id)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

func (h *Handler) Cancel(w http.ResponseWriter, r *http.Request) {
	id, err := visitID(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	v, err := h.svc.Cancel(r.Context(), principal(r), id)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

func (h *Handler) Decide(w http.ResponseWriter, r *http.Request) {
	id, err := visitID(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	var req decisionRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	v, err := h.svc.Decide(r.Context(), principal(r), id, req.Decision == "approve", req.Reason)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

// ── gate ─────────────────────────────────────────────────────────────────────

func (h *Handler) Verify(w http.ResponseWriter, r *http.Request) {
	var req verifyRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	if req.Code == "" && req.QR == "" {
		httpx.Fail(w, httpx.NewError(http.StatusBadRequest, "VALIDATION", "send a 6-digit code or a QR payload"))
		return
	}
	res, err := h.svc.Verify(r.Context(), principal(r), req.Code, req.QR)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, res, nil)
}

func (h *Handler) GateUnits(w http.ResponseWriter, r *http.Request) {
	us, err := h.svc.Units(r.Context(), principal(r), r.URL.Query().Get("q"))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, us, nil)
}

func (h *Handler) WalkIn(w http.ResponseWriter, r *http.Request) {
	var req walkInRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	v, err := h.svc.WalkIn(r.Context(), principal(r), WalkInInput{UnitID: uuid.MustParse(req.UnitID), VisitorName: req.VisitorName,
		VisitorPhone: req.VisitorPhone, Purpose: req.Purpose, Company: req.Company, VehicleNumber: req.VehicleNumber, VisitorCount: req.VisitorCount})
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	g := v.ForGate()
	httpx.Created(w, g)
}

func (h *Handler) GateList(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status != "" && status != "waiting" && status != "approved" && status != "inside" {
		httpx.Fail(w, httpx.NewError(http.StatusBadRequest, "VALIDATION", "invalid status"))
		return
	}
	rows, err := h.svc.GateList(r.Context(), principal(r), status)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	out := make([]GateVisit, len(rows))
	for i, v := range rows {
		out[i] = v.ForGate()
	}
	httpx.OK(w, out, nil)
}

func (h *Handler) CheckIn(w http.ResponseWriter, r *http.Request) {
	id, err := visitID(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	v, err := h.svc.CheckIn(r.Context(), principal(r), id)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v.ForGate(), nil)
}

func (h *Handler) CheckOut(w http.ResponseWriter, r *http.Request) {
	id, err := visitID(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	v, err := h.svc.CheckOut(r.Context(), principal(r), id)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v.ForGate(), nil)
}

// ── admin ────────────────────────────────────────────────────────────────────

func adminFilter(r *http.Request) (ListFilter, error) {
	q := r.URL.Query()
	f := ListFilter{Status: q.Get("status"), Purpose: q.Get("purpose"), Query: q.Get("q")}
	valid := map[string]bool{"": true, StatusInvited: true, StatusWaiting: true, StatusApproved: true, StatusInside: true, StatusCompleted: true,
		StatusRejected: true, StatusCancelled: true, StatusExpired: true}
	if !valid[f.Status] {
		return f, httpx.NewError(http.StatusBadRequest, "VALIDATION", "invalid status")
	}
	if v := q.Get("unit_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			return f, httpx.NewError(http.StatusBadRequest, "VALIDATION", "invalid unit_id")
		}
		f.UnitID = &id
	}
	for k, dst := range map[string]**time.Time{"from": &f.From, "to": &f.To} {
		if v := q.Get(k); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return f, httpx.NewError(http.StatusBadRequest, "VALIDATION", "invalid "+k+" (RFC 3339)")
			}
			*dst = &t
		}
	}
	return f, nil
}

func (h *Handler) AdminList(w http.ResponseWriter, r *http.Request) {
	f, err := adminFilter(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	pg := pagination.FromRequest(r)
	f.Limit, f.Offset = pg.PageSize, pg.Offset()
	rows, total, err := h.svc.AdminList(r.Context(), principal(r), f)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, rows, pg.Meta(total))
}

func (h *Handler) AdminStats(w http.ResponseWriter, r *http.Request) {
	st, err := h.svc.AdminStats(r.Context(), principal(r))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, st, nil)
}

func (h *Handler) AdminExport(w http.ResponseWriter, r *http.Request) {
	f, err := adminFilter(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="visitors_%s.csv"`, time.Now().UTC().Format("20060102")))
	if _, err := h.svc.AdminExport(r.Context(), principal(r), f, w); err != nil {
		httpx.Fail(w, err) // only reachable before the first byte is written
	}
}
