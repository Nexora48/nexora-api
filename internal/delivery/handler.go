package delivery

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func principal(r *http.Request) authctx.Principal { p, _ := authctx.From(r.Context()); return p }

func (h *Handler) Rules(w http.ResponseWriter, r *http.Request) {
	rules, ex, err := h.svc.Rules(r.Context(), principal(r))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string]any{"rules": rules, "expectations": ex}, nil)
}

type ruleRequest struct {
	Scope     string   `json:"scope" validate:"required"`
	Mode      string   `json:"mode" validate:"required"`
	Companies []string `json:"companies" validate:"max=40,dive,max=60"`
}

func (h *Handler) SetRule(w http.ResponseWriter, r *http.Request) {
	var req ruleRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	rule, err := h.svc.SetRule(r.Context(), principal(r), Rule(req))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, rule, nil)
}

type expectRequest struct {
	Company string `json:"company" validate:"required,max=60"`
	Hours   int    `json:"hours" validate:"omitempty,min=1,max=24"`
}

func (h *Handler) Expect(w http.ResponseWriter, r *http.Request) {
	var req expectRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	e, err := h.svc.Expect(r.Context(), principal(r), req.Company, req.Hours)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.Created(w, e)
}

func (h *Handler) CancelExpectation(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, httpx.NewError(http.StatusNotFound, "NOT_FOUND", "expectation not found"))
		return
	}
	if err := h.svc.CancelExpectation(r.Context(), principal(r), id); err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.NoContent(w)
}
