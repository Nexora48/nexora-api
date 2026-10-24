package sos

import (
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

func principal(r *http.Request) authctx.Principal { p, _ := authctx.From(r.Context()); return p }

func sosID(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return uuid.Nil, ErrNotFound
	}
	return id, nil
}

type raiseRequest struct {
	Kind string  `json:"kind" validate:"required"`
	Note *string `json:"note" validate:"omitempty,max=300"`
	Test bool    `json:"test"`
}

func (h *Handler) Raise(w http.ResponseWriter, r *http.Request) {
	var req raiseRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	a, err := h.svc.Raise(r.Context(), principal(r), req.Kind, req.Note, req.Test)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if a.Merged {
		httpx.OK(w, a, nil)
		return
	}
	httpx.Created(w, a)
}

func (h *Handler) act(fn func(r *http.Request, id uuid.UUID) (*Alert, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := sosID(r)
		if err != nil {
			httpx.Fail(w, err)
			return
		}
		a, err := fn(r, id)
		if err != nil {
			httpx.Fail(w, err)
			return
		}
		httpx.OK(w, a, nil)
	}
}

func (h *Handler) Cancel() http.HandlerFunc {
	return h.act(func(r *http.Request, id uuid.UUID) (*Alert, error) {
		return h.svc.Cancel(r.Context(), principal(r), id)
	})
}

func (h *Handler) Acknowledge() http.HandlerFunc {
	return h.act(func(r *http.Request, id uuid.UUID) (*Alert, error) {
		return h.svc.Acknowledge(r.Context(), principal(r), id)
	})
}

type resolveRequest struct {
	Outcome string `json:"outcome" validate:"max=300"`
}

func (h *Handler) Resolve(w http.ResponseWriter, r *http.Request) {
	var req resolveRequest
	if r.ContentLength != 0 {
		if err := httpx.Decode(w, r, &req); err != nil {
			httpx.Fail(w, err)
			return
		}
	}
	h.act(func(r *http.Request, id uuid.UUID) (*Alert, error) {
		return h.svc.Resolve(r.Context(), principal(r), id, req.Outcome)
	})(w, r)
}

func (h *Handler) Active(w http.ResponseWriter, r *http.Request) {
	a, err := h.svc.Active(r.Context(), principal(r))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, a, nil)
}

func (h *Handler) Mine(w http.ResponseWriter, r *http.Request) {
	pg := pagination.FromRequest(r)
	a, total, err := h.svc.Mine(r.Context(), principal(r), pg.PageSize, pg.Offset())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, a, pg.Meta(total))
}

func filter(r *http.Request, pg pagination.Params) (Filter, error) {
	q := r.URL.Query()
	f := Filter{Status: q.Get("status"), Kind: q.Get("kind"), Limit: pg.PageSize, Offset: pg.Offset()}
	switch f.Status {
	case "", StatusRaised, StatusAcknowledged, StatusResolved, StatusCancelled:
	default:
		return f, httpx.NewError(http.StatusBadRequest, "VALIDATION", "invalid status")
	}
	if f.Kind != "" {
		if _, ok := kinds[f.Kind]; !ok {
			return f, ErrBadKind
		}
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
	pg := pagination.FromRequest(r)
	f, err := filter(r, pg)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	a, total, err := h.svc.AdminList(r.Context(), principal(r), f)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, a, pg.Meta(total))
}

func (h *Handler) AdminGet() http.HandlerFunc {
	return h.act(func(r *http.Request, id uuid.UUID) (*Alert, error) {
		return h.svc.AdminGet(r.Context(), principal(r), id)
	})
}

func (h *Handler) AdminExport(w http.ResponseWriter, r *http.Request) {
	f, err := filter(r, pagination.Params{Page: 1, PageSize: 100})
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if !isOffice(principal(r).Role) {
		httpx.Fail(w, ErrAdminOnly)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="sos-log.csv"`)
	if _, err := h.svc.AdminExport(r.Context(), principal(r), f, w); err != nil {
		httpx.Fail(w, err)
	}
}
