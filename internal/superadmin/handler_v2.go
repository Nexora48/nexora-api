package superadmin

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Nexora48/nexora-api/internal/platform/httpx"
	"github.com/Nexora48/nexora-api/internal/usage"
)

// M10 routes: plans, analytics, audit v2, staff, broadcast, health, app versions.

// mountReads registers read routes (any platform user).
func (h *Handler) mountReads(r chi.Router) {
	r.Get("/plans", h.ListPlans)
	r.Get("/plans/{id}", h.GetPlan)
	r.Get("/plans/{id}/societies", h.PlanSocieties)
	r.Get("/analytics/metrics", h.Metrics)
	r.Get("/analytics/overview", h.AnalyticsOverview)
	r.Get("/analytics/services", h.AnalyticsServices)
	r.Get("/analytics/timeseries", h.AnalyticsTimeseries)
	r.Get("/analytics/top", h.AnalyticsTop)
	r.Get("/societies/{id}/usage", h.SocietyUsage)
	r.Get("/audit/{id}", h.AuditGet)
	r.Get("/audit/export.csv", h.AuditExport)
	r.Get("/audit/settings", h.AuditSettingsGet)
	r.Get("/broadcasts", h.ListBroadcasts)
	r.Get("/health", h.Health)
	r.Get("/app-versions", h.ListAppVersions)
}

// mountWrites registers mutations (super_admin only).
func (h *Handler) mountWrites(r chi.Router) {
	r.Post("/plans", h.CreatePlan)
	r.Put("/plans/{id}", h.UpdatePlan)
	r.Post("/plans/{id}/archive", h.ArchivePlan)
	r.Put("/societies/{id}/plan", h.AssignPlan)
	r.Put("/audit/settings", h.AuditSettingsPut)
	r.Get("/staff", h.ListStaff)
	r.Post("/staff", h.InviteStaff)
	r.Put("/staff/{id}", h.UpdateStaff)
	r.Post("/staff/{id}/reinvite", h.ReinviteStaff)
	r.Post("/staff/{id}/reset-mfa", h.ResetStaffMFA)
	r.Post("/broadcasts", h.CreateBroadcast)
	r.Post("/broadcasts/{id}/cancel", h.CancelBroadcast)
	r.Put("/app-versions/{app}", h.PutAppVersion)
}

func (h *Handler) ListPlans(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.ListPlans(r.Context())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

func (h *Handler) GetPlan(w http.ResponseWriter, r *http.Request) {
	id, err := uuidParam(r, "id")
	if err != nil {
		httpx.Fail(w, ErrPlanNotFound)
		return
	}
	v, err := h.svc.GetPlan(r.Context(), id)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

func (h *Handler) PlanSocieties(w http.ResponseWriter, r *http.Request) {
	id, err := uuidParam(r, "id")
	if err != nil {
		httpx.Fail(w, ErrPlanNotFound)
		return
	}
	v, err := h.svc.PlanSocieties(r.Context(), id)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

func (h *Handler) CreatePlan(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var req PlanInput
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	v, err := h.svc.CreatePlan(r.Context(), p.UserID, req)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.Created(w, v)
}

func (h *Handler) UpdatePlan(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	id, err := uuidParam(r, "id")
	if err != nil {
		httpx.Fail(w, ErrPlanNotFound)
		return
	}
	var req PlanInput
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	v, err := h.svc.UpdatePlan(r.Context(), p.UserID, id, req)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

func (h *Handler) ArchivePlan(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	id, err := uuidParam(r, "id")
	if err != nil {
		httpx.Fail(w, ErrPlanNotFound)
		return
	}
	v, err := h.svc.ArchivePlan(r.Context(), p.UserID, id)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

func (h *Handler) AssignPlan(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	id, err := uuidParam(r, "id")
	if err != nil {
		httpx.Fail(w, ErrSocietyNotFound)
		return
	}
	var req AssignInput
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	v, err := h.svc.AssignPlan(r.Context(), p.UserID, id, req)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

func rangeParam(r *http.Request) int {
	n, _ := strconv.Atoi(r.URL.Query().Get("range"))
	return n
}

func (h *Handler) Metrics(w http.ResponseWriter, _ *http.Request) { httpx.OK(w, usage.Catalogue, nil) }

func (h *Handler) AnalyticsOverview(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.Overview(r.Context(), rangeParam(r))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

func (h *Handler) AnalyticsServices(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.Adoption(r.Context(), rangeParam(r))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

func (h *Handler) AnalyticsTimeseries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var society *uuid.UUID
	if v := q.Get("society_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			httpx.Fail(w, httpx.NewError(http.StatusBadRequest, "VALIDATION", "invalid society_id"))
			return
		}
		society = &id
	}
	v, err := h.svc.Timeseries(r.Context(), q.Get("metric"), rangeParam(r), society)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

func (h *Handler) AnalyticsTop(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.Top(r.Context(), r.URL.Query().Get("metric"), rangeParam(r))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

func (h *Handler) SocietyUsage(w http.ResponseWriter, r *http.Request) {
	id, err := uuidParam(r, "id")
	if err != nil {
		httpx.Fail(w, ErrSocietyNotFound)
		return
	}
	v, err := h.svc.SocietyUsage(r.Context(), id)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

func (h *Handler) AuditSettingsGet(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.AuditSettings(r.Context())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

func (h *Handler) AuditSettingsPut(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var req AuditSettings
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	v, err := h.svc.SetAuditSettings(r.Context(), p.UserID, req)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

func (h *Handler) ListStaff(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.ListStaff(r.Context())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

func (h *Handler) InviteStaff(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var req InviteInput
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	v, err := h.svc.InviteStaff(r.Context(), p.UserID, req)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.Created(w, v)
}

func (h *Handler) staffID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuidParam(r, "id")
	if err != nil {
		httpx.Fail(w, ErrStaffNotFound)
		return uuid.Nil, false
	}
	return id, true
}

func (h *Handler) UpdateStaff(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	id, ok := h.staffID(w, r)
	if !ok {
		return
	}
	var req StaffUpdate
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	v, err := h.svc.UpdateStaff(r.Context(), p.UserID, id, req)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

func (h *Handler) ReinviteStaff(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	id, ok := h.staffID(w, r)
	if !ok {
		return
	}
	v, err := h.svc.ReinviteStaff(r.Context(), p.UserID, id)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

func (h *Handler) ResetStaffMFA(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	id, ok := h.staffID(w, r)
	if !ok {
		return
	}
	v, err := h.svc.ResetStaffMFA(r.Context(), p.UserID, id)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

type acceptReq struct {
	Token    string `json:"token" validate:"required"`
	Password string `json:"password" validate:"required"`
}

// AcceptInvite is public: the one-time token is the credential.
func (h *Handler) AcceptInvite(w http.ResponseWriter, r *http.Request) {
	var req acceptReq
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	if err := h.svc.AcceptInvite(r.Context(), req.Token, req.Password); err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.NoContent(w)
}

func (h *Handler) ListBroadcasts(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.ListBroadcasts(r.Context())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

func (h *Handler) CreateBroadcast(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var req BroadcastInput
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	b, n, err := h.svc.CreateBroadcast(r.Context(), p.UserID, req)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if req.Preview {
		httpx.OK(w, map[string]any{"recipients": n}, nil)
		return
	}
	httpx.Created(w, b)
}

func (h *Handler) CancelBroadcast(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	id, err := uuidParam(r, "id")
	if err != nil {
		httpx.Fail(w, ErrBroadcastNotFound)
		return
	}
	v, err := h.svc.CancelBroadcast(r.Context(), p.UserID, id)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	httpx.OK(w, h.svc.Health(r.Context()), nil)
}

func (h *Handler) ListAppVersions(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.ListAppVersions(r.Context())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

func (h *Handler) PutAppVersion(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var req AppVersion
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	req.App = chi.URLParam(r, "app")
	v, err := h.svc.SetAppVersion(r.Context(), p.UserID, req)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

// AppConfig is public: GET /app-config?app=living&version=1.2.0 (fails open for unknown input).
func (h *Handler) AppConfig(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	httpx.OK(w, AppConfigFor(r.Context(), h.svc.store.pool, q.Get("app"), q.Get("version")), nil)
}

// AppConfigHandler is the public GET /app-config used by the Living, Admin and Get apps at start-up.
func AppConfigHandler(svc *Service) http.HandlerFunc { return NewHandler(svc).AppConfig }
