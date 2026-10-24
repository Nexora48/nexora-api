package gate

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
	"github.com/Nexora48/nexora-api/internal/visitor"
)

type Handler struct {
	svc *Service
	vs  *visitor.Service
}

func NewHandler(svc *Service, vs *visitor.Service) *Handler { return &Handler{svc: svc, vs: vs} }

func principal(r *http.Request) authctx.Principal { p, _ := authctx.From(r.Context()); return p }

func idParam(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return uuid.Nil, ErrGuardNotFound
	}
	return id, nil
}

// ── admin: guards ────────────────────────────────────────────────────────────

type guardRequest struct {
	FullName string `json:"full_name" validate:"required,min=2,max=80"`
	Phone    string `json:"phone" validate:"required,max=20"`
}

type guardPatch struct {
	FullName *string `json:"full_name" validate:"omitempty,min=2,max=80"`
	Status   *string `json:"status" validate:"omitempty,oneof=active suspended"`
}

func (h *Handler) ListGuards(w http.ResponseWriter, r *http.Request) {
	g, err := h.svc.Guards(r.Context(), principal(r).SocietyID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, g, nil)
}

func (h *Handler) CreateGuard(w http.ResponseWriter, r *http.Request) {
	var req guardRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	p := principal(r)
	g, err := h.svc.CreateGuard(r.Context(), p.SocietyID, p.UserID, req.FullName, req.Phone)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.Created(w, g)
}

func (h *Handler) UpdateGuard(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	var req guardPatch
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	p := principal(r)
	g, err := h.svc.UpdateGuard(r.Context(), p.SocietyID, p.UserID, id, req.FullName, req.Status)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, g, nil)
}

func (h *Handler) RemoveGuard(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	p := principal(r)
	if err := h.svc.RemoveGuard(r.Context(), p.SocietyID, p.UserID, id); err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.NoContent(w)
}

type settingsPatch struct {
	WalkinPhotoRequired *bool `json:"walkin_photo_required" validate:"required"`
}

func (h *Handler) GetSettings(w http.ResponseWriter, r *http.Request) {
	st, err := h.svc.Settings(r.Context(), principal(r).SocietyID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, st, nil)
}

func (h *Handler) UpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req settingsPatch
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	p := principal(r)
	st, err := h.svc.UpdateSettings(r.Context(), p.SocietyID, p.UserID, *req.WalkinPhotoRequired)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, st, nil)
}

// ── admin + guard: gates ─────────────────────────────────────────────────────

type gateRequest struct {
	Name string `json:"name" validate:"required,min=2,max=40"`
}

type gatePatch struct {
	Name   *string `json:"name" validate:"omitempty,min=2,max=40"`
	Active *bool   `json:"active"`
}

func (h *Handler) ListGates(w http.ResponseWriter, r *http.Request) {
	g, err := h.svc.Gates(r.Context(), principal(r).SocietyID, false)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, g, nil)
}

// GuardGates is what a guard picks from when starting a shift (active gates only).
func (h *Handler) GuardGates(w http.ResponseWriter, r *http.Request) {
	g, err := h.svc.Gates(r.Context(), principal(r).SocietyID, true)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, g, nil)
}

func (h *Handler) CreateGate(w http.ResponseWriter, r *http.Request) {
	var req gateRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	p := principal(r)
	g, err := h.svc.CreateGate(r.Context(), p.SocietyID, p.UserID, req.Name)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.Created(w, g)
}

func (h *Handler) UpdateGate(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, ErrGateNotFound)
		return
	}
	var req gatePatch
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	p := principal(r)
	g, err := h.svc.UpdateGate(r.Context(), p.SocietyID, p.UserID, id, req.Name, req.Active)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, g, nil)
}

// ── guard: shift, feeds, batch ───────────────────────────────────────────────

type shiftRequest struct {
	GateID string `json:"gate_id" validate:"required,uuid"`
}

func (h *Handler) StartShift(w http.ResponseWriter, r *http.Request) {
	var req shiftRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	p := principal(r)
	sh, err := h.svc.StartShift(r.Context(), p.SocietyID, p.MembershipID, uuid.MustParse(req.GateID))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	h.svc.log(r.Context(), p.SocietyID, p.UserID, "shift.start", "gate", sh.GateID, nil)
	httpx.OK(w, sh, nil)
}

func (h *Handler) EndShift(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if err := h.svc.EndShift(r.Context(), p.SocietyID, p.MembershipID); err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.NoContent(w)
}

func (h *Handler) Shift(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	sh, err := h.svc.CurrentShift(r.Context(), p.SocietyID, p.MembershipID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string]any{"shift": sh}, nil)
}

func (h *Handler) Dashboard(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	d, err := h.vs.Dashboard(r.Context(), p)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	sh, _ := h.svc.CurrentShift(r.Context(), p.SocietyID, p.MembershipID)
	httpx.OK(w, map[string]any{"counts": d, "shift": sh}, nil)
}

func (h *Handler) Inside(w http.ResponseWriter, r *http.Request) {
	v, err := h.vs.Inside(r.Context(), principal(r))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

func (h *Handler) Vehicles(w http.ResponseWriter, r *http.Request) {
	v, err := h.vs.Vehicles(r.Context(), principal(r), r.URL.Query().Get("q"))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

func (h *Handler) PassesSync(w http.ResponseWriter, r *http.Request) {
	v, err := h.vs.PassesSync(r.Context(), principal(r))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string]any{"passes": v, "society_id": principal(r).SocietyID}, nil)
}

func (h *Handler) Batch(w http.ResponseWriter, r *http.Request) {
	var req BatchRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, h.svc.Batch(r.Context(), principal(r), h.vs, req), nil)
}
