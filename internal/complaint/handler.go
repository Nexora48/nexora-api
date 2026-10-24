package complaint

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

func list(w http.ResponseWriter, items []*Complaint, pg pagination.Params, total int) {
	if items == nil {
		items = []*Complaint{}
	}
	httpx.OK(w, items, pg.Meta(total))
}

// ── residents ────────────────────────────────────────────────────────────────

type raiseRequest struct {
	Title       string `json:"title" validate:"required,max=200"`
	Description string `json:"description" validate:"required,max=2000"`
	Category    string `json:"category" validate:"required"`
	Priority    string `json:"priority"`
	Common      bool   `json:"common_area"`
}

func (h *Handler) Raise(w http.ResponseWriter, r *http.Request) {
	var req raiseRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	c, err := h.svc.Raise(r.Context(), principal(r), Input{Title: req.Title, Description: req.Description, Category: req.Category, Priority: req.Priority, Common: req.Common})
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.Created(w, c)
}

func (h *Handler) ListMine(w http.ResponseWriter, r *http.Request) {
	pg := pagination.FromRequest(r)
	g := r.URL.Query().Get("group")
	if g != "" && g != "open" && g != "resolved" {
		httpx.Fail(w, invalid("group must be open or resolved"))
		return
	}
	items, total, err := h.svc.ListMine(r.Context(), principal(r), g, pg.PageSize, pg.Offset())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	list(w, items, pg, total)
}

func (h *Handler) ListCommon(w http.ResponseWriter, r *http.Request) {
	pg := pagination.FromRequest(r)
	items, total, err := h.svc.ListCommon(r.Context(), principal(r), pg.PageSize, pg.Offset())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	list(w, items, pg, total)
}

func (h *Handler) act(fn func(r *http.Request, id uuid.UUID) (*Complaint, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := param(r, "id")
		if err != nil {
			httpx.Fail(w, err)
			return
		}
		c, err := fn(r, id)
		if err != nil {
			httpx.Fail(w, err)
			return
		}
		httpx.OK(w, c, nil)
	}
}

func (h *Handler) GetMine() http.HandlerFunc {
	return h.act(func(r *http.Request, id uuid.UUID) (*Complaint, error) {
		return h.svc.GetMine(r.Context(), principal(r), id)
	})
}

type textRequest struct {
	Text     string `json:"text" validate:"required,max=1000"`
	Internal bool   `json:"internal"`
}

func (h *Handler) Comment(w http.ResponseWriter, r *http.Request) {
	var req textRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	h.act(func(r *http.Request, id uuid.UUID) (*Complaint, error) {
		return h.svc.Comment(r.Context(), principal(r), id, req.Text)
	})(w, r)
}

type reopenRequest struct {
	Reason string `json:"reason" validate:"required,max=600"`
}

func (h *Handler) Reopen(w http.ResponseWriter, r *http.Request) {
	var req reopenRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	h.act(func(r *http.Request, id uuid.UUID) (*Complaint, error) {
		return h.svc.Reopen(r.Context(), principal(r), id, req.Reason)
	})(w, r)
}

type rateRequest struct {
	Rating  int    `json:"rating" validate:"required,min=1,max=5"`
	Comment string `json:"comment" validate:"max=600"`
}

func (h *Handler) Rate(w http.ResponseWriter, r *http.Request) {
	var req rateRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	h.act(func(r *http.Request, id uuid.UUID) (*Complaint, error) {
		return h.svc.Rate(r.Context(), principal(r), id, req.Rating, req.Comment)
	})(w, r)
}

func (h *Handler) Photo(w http.ResponseWriter, r *http.Request) {
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

func (h *Handler) Attachment(w http.ResponseWriter, r *http.Request) {
	id, err := param(r, "id")
	aid, err2 := param(r, "aid")
	if err != nil || err2 != nil {
		httpx.Fail(w, ErrNotFound)
		return
	}
	b, f, err := h.svc.Attachment(r.Context(), principal(r), id, aid)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	attachment.Serve(w, f, b)
}

// ── office ───────────────────────────────────────────────────────────────────

func filter(r *http.Request, pg pagination.Params) (Filter, error) {
	q := r.URL.Query()
	f := Filter{Status: q.Get("status"), Category: q.Get("category"), Priority: q.Get("priority"), Overdue: q.Get("overdue") == "1", Query: q.Get("q"), Block: q.Get("block"), Limit: pg.PageSize, Offset: pg.Offset()}
	switch f.Status {
	case "", "active", "open", "assigned", "in_progress", "resolved", "closed":
	default:
		return f, invalid("invalid status")
	}
	if f.Category != "" && !categories[f.Category] {
		return f, invalid("invalid category")
	}
	if f.Priority != "" && !priorities[f.Priority] {
		return f, invalid("invalid priority")
	}
	return f, nil
}

func (h *Handler) AdminList(w http.ResponseWriter, r *http.Request) {
	pg := pagination.FromRequest(r)
	f, err := filter(r, pg)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	items, total, err := h.svc.AdminList(r.Context(), principal(r), f)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	list(w, items, pg, total)
}

func (h *Handler) AdminGet() http.HandlerFunc {
	return h.act(func(r *http.Request, id uuid.UUID) (*Complaint, error) {
		return h.svc.AdminGet(r.Context(), principal(r), id)
	})
}

func (h *Handler) Stats(w http.ResponseWriter, r *http.Request) {
	st, err := h.svc.Stats(r.Context(), principal(r))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, st, nil)
}

func (h *Handler) OfficeMembers(w http.ResponseWriter, r *http.Request) {
	m, err := h.svc.OfficeMembers(r.Context(), principal(r))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, m, nil)
}

type assignRequest struct {
	MembershipID *uuid.UUID `json:"membership_id"`
	Vendor       string     `json:"vendor" validate:"max=200"`
	TargetDate   *string    `json:"target_date"`
}

func (h *Handler) Assign(w http.ResponseWriter, r *http.Request) {
	var req assignRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	in := AssignInput{MembershipID: req.MembershipID, Vendor: req.Vendor}
	if req.TargetDate != nil && *req.TargetDate != "" {
		t, err := time.Parse("2006-01-02", *req.TargetDate)
		if err != nil {
			httpx.Fail(w, invalid("target_date must be YYYY-MM-DD"))
			return
		}
		in.TargetDate = &t
	}
	h.act(func(r *http.Request, id uuid.UUID) (*Complaint, error) {
		return h.svc.Assign(r.Context(), principal(r), id, in)
	})(w, r)
}

type statusRequest struct {
	Status string `json:"status" validate:"required"`
	Note   string `json:"note" validate:"max=600"`
}

func (h *Handler) SetStatus(w http.ResponseWriter, r *http.Request) {
	var req statusRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	h.act(func(r *http.Request, id uuid.UUID) (*Complaint, error) {
		return h.svc.SetStatus(r.Context(), principal(r), id, req.Status, req.Note)
	})(w, r)
}

type priorityRequest struct {
	Priority string `json:"priority" validate:"required"`
}

func (h *Handler) SetPriority(w http.ResponseWriter, r *http.Request) {
	var req priorityRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	h.act(func(r *http.Request, id uuid.UUID) (*Complaint, error) {
		return h.svc.SetPriority(r.Context(), principal(r), id, req.Priority)
	})(w, r)
}

func (h *Handler) OfficeComment(w http.ResponseWriter, r *http.Request) {
	var req textRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	h.act(func(r *http.Request, id uuid.UUID) (*Complaint, error) {
		return h.svc.OfficeComment(r.Context(), principal(r), id, req.Text, req.Internal)
	})(w, r)
}

func (h *Handler) AdminAttachment(w http.ResponseWriter, r *http.Request) {
	id, err := param(r, "id")
	aid, err2 := param(r, "aid")
	if err != nil || err2 != nil {
		httpx.Fail(w, ErrNotFound)
		return
	}
	b, f, err := h.svc.AdminAttachment(r.Context(), principal(r), id, aid)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	attachment.Serve(w, f, b)
}

func (h *Handler) Export(w http.ResponseWriter, r *http.Request) {
	f, err := filter(r, pagination.Params{Page: 1, PageSize: 100})
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="complaints.csv"`)
	if _, err := h.svc.Export(r.Context(), principal(r), f, w); err != nil {
		httpx.Fail(w, err)
	}
}
