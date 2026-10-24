package notify

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

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	pg := pagination.FromRequest(r)
	items, total, err := h.svc.List(r.Context(), p.UserID, p.SocietyID, r.URL.Query().Get("unread") == "1", pg.PageSize, pg.Offset())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, items, pg.Meta(total))
}

func (h *Handler) UnreadCount(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	n, err := h.svc.UnreadCount(r.Context(), p.UserID, p.SocietyID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string]int{"unread": n}, nil)
}

func (h *Handler) Read(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, ErrNotificationNotFound)
		return
	}
	p := principal(r)
	if err := h.svc.MarkRead(r.Context(), p.UserID, p.SocietyID, id); err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.NoContent(w)
}

func (h *Handler) ReadAll(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	n, err := h.svc.ReadAll(r.Context(), p.UserID, p.SocietyID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string]int64{"marked": n}, nil)
}

func (h *Handler) Prefs(w http.ResponseWriter, r *http.Request) {
	prefs, err := h.svc.Prefs(r.Context(), principal(r).UserID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, prefs, nil)
}

type prefRequest struct {
	Group string `json:"group" validate:"required"`
	Push  bool   `json:"push"`
	Email bool   `json:"email"`
}

func (h *Handler) SetPref(w http.ResponseWriter, r *http.Request) {
	var req prefRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	if err := h.svc.SetPref(r.Context(), principal(r).UserID, req.Group, req.Push, req.Email); err != nil {
		httpx.Fail(w, err)
		return
	}
	h.Prefs(w, r)
}
