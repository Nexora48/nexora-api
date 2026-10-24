package amenity

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Nexora48/nexora-api/internal/attachment"
	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
	"github.com/Nexora48/nexora-api/internal/platform/pagination"
)

type Handler struct {
	svc   *Service
	files *attachment.Service
}

func NewHandler(svc *Service, files *attachment.Service) *Handler {
	return &Handler{svc: svc, files: files}
}

func principal(r *http.Request) authctx.Principal { p, _ := authctx.From(r.Context()); return p }

func param(r *http.Request, name string) (uuid.UUID, error) {
	v, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		return uuid.Nil, ErrNotFound
	}
	return v, nil
}

func bookings(w http.ResponseWriter, items []*Booking, pg pagination.Params, total int) {
	if items == nil {
		items = []*Booking{}
	}
	httpx.OK(w, items, pg.Meta(total))
}

// ── residents ────────────────────────────────────────────────────────────────

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.List(r.Context(), principal(r), false)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, items, nil)
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	id, err := param(r, "id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	a, err := h.svc.Get(r.Context(), principal(r), id, false)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, a, nil)
}

func (h *Handler) Slots(w http.ResponseWriter, r *http.Request) {
	id, err := param(r, "id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	d, err := h.svc.Slots(r.Context(), principal(r), id, r.URL.Query().Get("date"))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, d, nil)
}

type bookRequest struct {
	SlotStart time.Time `json:"slot_start" validate:"required"`
	People    int       `json:"people"`
	Note      *string   `json:"note"`
}

func (h *Handler) Book(w http.ResponseWriter, r *http.Request) {
	id, err := param(r, "id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	var req bookRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	if req.SlotStart.IsZero() {
		httpx.Fail(w, invalid("slot_start is required"))
		return
	}
	b, err := h.svc.Book(r.Context(), principal(r), id, req.SlotStart, req.People, req.Note)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.Created(w, b)
}

func (h *Handler) Mine(w http.ResponseWriter, r *http.Request) {
	pg := pagination.FromRequest(r)
	g := r.URL.Query().Get("group")
	if g != "" && g != "upcoming" && g != "past" {
		httpx.Fail(w, invalid("group must be upcoming or past"))
		return
	}
	items, total, err := h.svc.MyBookings(r.Context(), principal(r), g, pg.PageSize, pg.Offset())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	bookings(w, items, pg, total)
}

func (h *Handler) Cancel(w http.ResponseWriter, r *http.Request) {
	id, err := param(r, "id")
	if err != nil {
		httpx.Fail(w, ErrBookingNone)
		return
	}
	b, err := h.svc.Cancel(r.Context(), principal(r), id)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, b, nil)
}

func (h *Handler) Photo(w http.ResponseWriter, r *http.Request) {
	id, err := param(r, "id")
	aid, err2 := param(r, "aid")
	if err != nil || err2 != nil {
		httpx.Fail(w, ErrNotFound)
		return
	}
	b, f, err := h.svc.Photo(r.Context(), principal(r), id, aid)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	attachment.Serve(w, f, b)
}

// ── office ───────────────────────────────────────────────────────────────────

type hoursReq struct {
	Weekday int    `json:"weekday"`
	Opens   string `json:"opens"`
	Closes  string `json:"closes"`
}

type amenityRequest struct {
	Name             string     `json:"name"`
	Description      string     `json:"description"`
	Capacity         int        `json:"capacity"`
	SlotMinutes      int        `json:"slot_minutes"`
	MinNoticeMinutes int        `json:"min_notice_minutes"`
	MaxDaysAhead     int        `json:"max_days_ahead"`
	MaxUpcoming      int        `json:"max_upcoming_per_flat"`
	MaxPerWeek       int        `json:"max_per_week_per_flat"`
	CancelCutoff     int        `json:"cancel_cutoff_hours"`
	Fee              float64    `json:"fee_amount"`
	Deposit          float64    `json:"deposit_amount"`
	RequiresApproval bool       `json:"requires_approval"`
	Active           *bool      `json:"active"`
	Hours            []hoursReq `json:"hours"`
}

func (q amenityRequest) input() Input {
	in := Input{Name: q.Name, Description: q.Description, Capacity: q.Capacity, SlotMinutes: q.SlotMinutes, MinNoticeMinutes: q.MinNoticeMinutes,
		MaxDaysAhead: q.MaxDaysAhead, MaxUpcoming: q.MaxUpcoming, MaxPerWeek: q.MaxPerWeek, CancelCutoff: q.CancelCutoff, Fee: q.Fee, Deposit: q.Deposit,
		RequiresApproval: q.RequiresApproval, Active: q.Active == nil || *q.Active}
	for _, h := range q.Hours {
		in.Hours = append(in.Hours, Hours{Weekday: h.Weekday, Opens: h.Opens, Closes: h.Closes})
	}
	return in
}

func (h *Handler) AdminList(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.List(r.Context(), principal(r), true)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, items, nil)
}

func (h *Handler) AdminGet(w http.ResponseWriter, r *http.Request) {
	id, err := param(r, "id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	a, err := h.svc.Get(r.Context(), principal(r), id, true)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, a, nil)
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var req amenityRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	a, err := h.svc.Create(r.Context(), principal(r), req.input())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.Created(w, a)
}

func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	id, err := param(r, "id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	var req amenityRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	a, err := h.svc.Update(r.Context(), principal(r), id, req.input())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, a, nil)
}

func (h *Handler) Deactivate(w http.ResponseWriter, r *http.Request) {
	id, err := param(r, "id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if err := h.svc.Deactivate(r.Context(), principal(r), id); err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string]bool{"ok": true}, nil)
}

func (h *Handler) AddBlackout(w http.ResponseWriter, r *http.Request) {
	id, err := param(r, "id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	var req struct {
		Date   string `json:"date"`
		Reason string `json:"reason"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	a, err := h.svc.AddBlackout(r.Context(), principal(r), id, req.Date, req.Reason)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.Created(w, a)
}

func (h *Handler) RemoveBlackout(w http.ResponseWriter, r *http.Request) {
	id, err := param(r, "id")
	bid, err2 := param(r, "bid")
	if err != nil || err2 != nil {
		httpx.Fail(w, ErrNotFound)
		return
	}
	a, err := h.svc.RemoveBlackout(r.Context(), principal(r), id, bid)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, a, nil)
}

func (h *Handler) AddPhoto(w http.ResponseWriter, r *http.Request) {
	id, err := param(r, "id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	name, data, err := h.files.ReadUpload(w, r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	f, err := h.svc.AddPhoto(r.Context(), principal(r), id, name, data)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.Created(w, f)
}

func (h *Handler) RemovePhoto(w http.ResponseWriter, r *http.Request) {
	id, err := param(r, "id")
	aid, err2 := param(r, "aid")
	if err != nil || err2 != nil {
		httpx.Fail(w, ErrNotFound)
		return
	}
	if err := h.svc.RemovePhoto(r.Context(), principal(r), id, aid); err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string]bool{"ok": true}, nil)
}

func filter(r *http.Request, pg pagination.Params) (Filter, error) {
	q := r.URL.Query()
	f := Filter{Status: q.Get("status"), Flat: q.Get("flat"), Limit: pg.PageSize, Offset: pg.Offset()}
	switch f.Status {
	case "", "pending", "confirmed", "rejected", "cancelled", "completed", "no_show", "expired":
	default:
		return f, invalid("invalid status")
	}
	if v := q.Get("amenity_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			return f, invalid("invalid amenity_id")
		}
		f.AmenityID = &id
	}
	for key, dst := range map[string]**time.Time{"from": &f.From, "to": &f.To} {
		if v := q.Get(key); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return f, invalid(key + " must be an RFC 3339 timestamp")
			}
			*dst = &t
		}
	}
	return f, nil
}

func (h *Handler) AdminBookings(w http.ResponseWriter, r *http.Request) {
	pg := pagination.FromRequest(r)
	f, err := filter(r, pg)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	items, total, err := h.svc.AdminBookings(r.Context(), principal(r), f)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	bookings(w, items, pg, total)
}

func (h *Handler) decide(fn func(r *http.Request, id uuid.UUID, reason string) (*Booking, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := param(r, "id")
		if err != nil {
			httpx.Fail(w, ErrBookingNone)
			return
		}
		var req struct {
			Reason string `json:"reason"`
		}
		if r.ContentLength != 0 {
			if err := httpx.Decode(w, r, &req); err != nil {
				httpx.Fail(w, err)
				return
			}
		}
		b, err := fn(r, id, req.Reason)
		if err != nil {
			httpx.Fail(w, err)
			return
		}
		httpx.OK(w, b, nil)
	}
}

func (h *Handler) Approve() http.HandlerFunc {
	return h.decide(func(r *http.Request, id uuid.UUID, _ string) (*Booking, error) {
		return h.svc.Approve(r.Context(), principal(r), id)
	})
}

func (h *Handler) Reject() http.HandlerFunc {
	return h.decide(func(r *http.Request, id uuid.UUID, reason string) (*Booking, error) {
		return h.svc.Reject(r.Context(), principal(r), id, reason)
	})
}

func (h *Handler) OfficeCancel() http.HandlerFunc {
	return h.decide(func(r *http.Request, id uuid.UUID, reason string) (*Booking, error) {
		return h.svc.OfficeCancel(r.Context(), principal(r), id, reason)
	})
}

func (h *Handler) NoShow() http.HandlerFunc {
	return h.decide(func(r *http.Request, id uuid.UUID, _ string) (*Booking, error) {
		return h.svc.NoShow(r.Context(), principal(r), id)
	})
}

func (h *Handler) Stats(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.Stats(r.Context(), principal(r))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, rows, nil)
}

func (h *Handler) Export(w http.ResponseWriter, r *http.Request) {
	f, err := filter(r, pagination.Params{Page: 1, PageSize: 100})
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="bookings.csv"`)
	if _, err := h.svc.Export(r.Context(), principal(r), f, w); err != nil {
		httpx.Fail(w, err)
	}
}
