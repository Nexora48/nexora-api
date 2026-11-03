package parking

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

func (h *Handler) id(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, ErrNotFound)
		return uuid.Nil, false
	}
	return id, true
}

// List: GET /society/parking/slots?status=free|allocated&kind=car|bike|visitor&q=&page=&page_size=
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	pg := pagination.FromRequest(r)
	q := r.URL.Query()
	f := Filter{Status: q.Get("status"), Kind: q.Get("kind"), Q: q.Get("q")}
	if f.Status != "" && f.Status != "free" && f.Status != "allocated" {
		httpx.Fail(w, invalid("status must be free or allocated"))
		return
	}
	if f.Kind != "" && f.Kind != "car" && f.Kind != "bike" && f.Kind != "visitor" {
		httpx.Fail(w, invalid("kind must be car, bike or visitor"))
		return
	}
	rows, total, err := h.svc.List(r.Context(), principal(r).SocietyID, f, pg.PageSize, pg.Offset())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, rows, pg.Meta(total))
}

func (h *Handler) Summary(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.Summary(r.Context(), principal(r).SocietyID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var in Input
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.Fail(w, err)
		return
	}
	v, err := h.svc.Create(r.Context(), principal(r), in)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.Created(w, v)
}

func (h *Handler) Bulk(w http.ResponseWriter, r *http.Request) {
	var in BulkInput
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.Fail(w, err)
		return
	}
	v, err := h.svc.Bulk(r.Context(), principal(r), in)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.Created(w, v)
}

func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	id, ok := h.id(w, r)
	if !ok {
		return
	}
	var in Input
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.Fail(w, err)
		return
	}
	v, err := h.svc.Update(r.Context(), principal(r), id, in)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	id, ok := h.id(w, r)
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), principal(r), id); err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.NoContent(w)
}

func (h *Handler) Allocate(w http.ResponseWriter, r *http.Request) {
	id, ok := h.id(w, r)
	if !ok {
		return
	}
	var in struct {
		UnitID uuid.UUID `json:"unit_id"`
	}
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.Fail(w, err)
		return
	}
	if in.UnitID == uuid.Nil {
		httpx.Fail(w, invalid("unit_id is required"))
		return
	}
	v, err := h.svc.Allocate(r.Context(), principal(r), id, in.UnitID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

func (h *Handler) Release(w http.ResponseWriter, r *http.Request) {
	id, ok := h.id(w, r)
	if !ok {
		return
	}
	v, err := h.svc.Release(r.Context(), principal(r), id)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

// Mine: GET /me/parking, the slots of the caller's flat.
func (h *Handler) Mine(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.Mine(r.Context(), principal(r))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}
