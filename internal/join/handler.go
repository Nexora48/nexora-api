package join

import (
	"context"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Nexora48/nexora-api/internal/auth"
	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
	"github.com/Nexora48/nexora-api/internal/platform/pagination"
)

// Claimer signs in someone whose request was approved.
type Claimer interface {
	ClaimSession(ctx context.Context, phone string, ci auth.ClientInfo) (*auth.LoginResponse, error)
}

type Handler struct {
	svc    *Service
	tokens *auth.TokenManager
	claim  Claimer
}

func NewHandler(svc *Service, tokens *auth.TokenManager, claim Claimer) *Handler {
	return &Handler{svc: svc, tokens: tokens, claim: claim}
}

type phoneKey struct{}

// Applicant checks the applicant token and puts the verified phone in the request context.
func (h *Handler) Applicant(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || tok == "" {
			httpx.Fail(w, httpx.NewError(http.StatusUnauthorized, "UNAUTHORIZED", "verify your mobile number first"))
			return
		}
		phone, err := h.tokens.ParseApplicant(tok)
		if err != nil {
			httpx.Fail(w, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), phoneKey{}, phone)))
	})
}

func phoneOf(r *http.Request) string { p, _ := r.Context().Value(phoneKey{}).(string); return p }

// ── applicant routes (mounted under /join behind Applicant) ──────────────────

func (h *Handler) Society(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.Lookup(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

func (h *Handler) Request(w http.ResponseWriter, r *http.Request) {
	var req RequestInput
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	v, err := h.svc.Request(r.Context(), phoneOf(r), req)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.Created(w, v)
}

func (h *Handler) Status(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.StatusOf(r.Context(), phoneOf(r))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

func (h *Handler) Cancel(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.Cancel(r.Context(), phoneOf(r)); err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.NoContent(w)
}

type claimReq struct {
	Device *auth.DeviceInfo `json:"device"`
}

// Claim exchanges the applicant token for a normal session once the request is approved.
func (h *Handler) Claim(w http.ResponseWriter, r *http.Request) {
	var req claimReq
	_ = httpx.Decode(w, r, &req)
	resp, err := h.claim.ClaimSession(r.Context(), phoneOf(r), auth.ClientFrom(r, req.Device))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, resp, nil)
}

// ── office routes (mounted under /society behind the permission chain) ───────

func principal(r *http.Request) authctx.Principal { p, _ := authctx.From(r.Context()); return p }

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	pg := pagination.FromRequest(r)
	status := r.URL.Query().Get("status")
	switch status {
	case "", "pending", "approved", "rejected":
	case "all":
		status = ""
	default:
		httpx.Fail(w, invalid("status must be pending, approved, rejected or all"))
		return
	}
	rows, total, err := h.svc.List(r.Context(), principal(r).SocietyID, status, pg.PageSize, pg.Offset())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, rows, pg.Meta(total))
}

func (h *Handler) idParam(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, ErrNotFound)
		return uuid.Nil, false
	}
	return id, true
}

func (h *Handler) Approve(w http.ResponseWriter, r *http.Request) {
	id, ok := h.idParam(w, r)
	if !ok {
		return
	}
	v, err := h.svc.Approve(r.Context(), principal(r), id)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

func (h *Handler) Reject(w http.ResponseWriter, r *http.Request) {
	id, ok := h.idParam(w, r)
	if !ok {
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	_ = httpx.Decode(w, r, &req)
	v, err := h.svc.Reject(r.Context(), principal(r), id, req.Reason)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

func (h *Handler) GetSettings(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.Settings(r.Context(), principal(r).SocietyID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

func (h *Handler) PutSettings(w http.ResponseWriter, r *http.Request) {
	var req SettingsInput
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	v, err := h.svc.UpdateSettings(r.Context(), principal(r), req)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}
