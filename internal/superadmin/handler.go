package superadmin

import (
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Nexora48/nexora-api/internal/middleware"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
	"github.com/Nexora48/nexora-api/internal/platform/pagination"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func clientIP(r *http.Request) string {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}

func uuidParam(r *http.Request, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		return uuid.Nil, ErrSocietyNotFound
	}
	return id, nil
}

// ── DTOs ─────────────────────────────────────────────────────────────────────

type loginRequest struct {
	Email    string `json:"email" validate:"required,email,max=200"`
	Password string `json:"password" validate:"required,max=200"`
}

type mfaRequest struct {
	MFAToken     string `json:"mfa_token" validate:"required"`
	Code         string `json:"code" validate:"omitempty,len=6,numeric"`
	RecoveryCode string `json:"recovery_code" validate:"omitempty,max=20"`
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token" validate:"required"`
}

type adminDTO struct {
	FullName string `json:"full_name" validate:"required,max=120"`
	Email    string `json:"email" validate:"required,email,max=200"`
	Phone    string `json:"phone" validate:"required,max=20"`
}

type createSocietyRequest struct {
	Name    string   `json:"name" validate:"required,min=2,max=120"`
	Slug    string   `json:"slug" validate:"max=40"`
	Address string   `json:"address" validate:"max=300"`
	City    string   `json:"city" validate:"max=80"`
	State   string   `json:"state" validate:"max=80"`
	Pincode string   `json:"pincode" validate:"max=10"`
	Status  string   `json:"status" validate:"omitempty,oneof=active trial"`
	Admin   adminDTO `json:"admin" validate:"required"`
}

type updateSocietyRequest struct {
	Name    string `json:"name" validate:"required,min=2,max=120"`
	Address string `json:"address" validate:"max=300"`
	City    string `json:"city" validate:"max=80"`
	State   string `json:"state" validate:"max=80"`
	Pincode string `json:"pincode" validate:"max=10"`
}

type suspendRequest struct {
	Reason string `json:"reason" validate:"required,min=3,max=300"`
}

type featureRequest struct {
	Enabled *bool  `json:"enabled" validate:"required"`
	Reason  string `json:"reason" validate:"required,min=3,max=300"`
}

type bulkFeatureRequest struct {
	Features map[string]bool `json:"features" validate:"required,min=1"`
	Reason   string          `json:"reason" validate:"required,min=3,max=300"`
}

// ── Auth ─────────────────────────────────────────────────────────────────────

func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	res, err := h.svc.Login(r.Context(), req.Email, req.Password, clientIP(r))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, res, nil)
}

func (h *Handler) VerifyMFA(w http.ResponseWriter, r *http.Request) {
	var req mfaRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	res, err := h.svc.VerifyMFA(r.Context(), req.MFAToken, req.Code, req.RecoveryCode, clientIP(r), r.UserAgent())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, res, nil)
}

func (h *Handler) Refresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	res, err := h.svc.Refresh(r.Context(), req.RefreshToken, clientIP(r), r.UserAgent())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, res, nil)
}

func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var req refreshRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	if err := h.svc.Logout(r.Context(), p, req.RefreshToken, clientIP(r)); err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.NoContent(w)
}

func (h *Handler) Me(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	u, err := h.svc.Me(r.Context(), p)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, u, nil)
}

// ── Societies ────────────────────────────────────────────────────────────────

func (h *Handler) ListSocieties(w http.ResponseWriter, r *http.Request) {
	pg := pagination.FromRequest(r)
	q := r.URL.Query()
	status := q.Get("status")
	if status != "" && status != "active" && status != "trial" && status != "suspended" {
		httpx.Fail(w, httpx.NewError(http.StatusBadRequest, "VALIDATION", "invalid status"))
		return
	}
	rows, total, err := h.svc.ListSocieties(r.Context(), strings.TrimSpace(q.Get("q")), status, pg.PageSize, pg.Offset())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, rows, pg.Meta(total))
}

func (h *Handler) GetSociety(w http.ResponseWriter, r *http.Request) {
	id, err := uuidParam(r, "id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	d, err := h.svc.GetSociety(r.Context(), id)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, d, nil)
}

func (h *Handler) CreateSociety(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var req createSocietyRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	d, cred, err := h.svc.CreateSociety(r.Context(), p.UserID, CreateSocietyInput{
		Name: req.Name, Slug: req.Slug, Address: req.Address, City: req.City, State: req.State, Pincode: req.Pincode, Status: req.Status,
		Admin: AdminInput{FullName: req.Admin.FullName, Email: req.Admin.Email, Phone: req.Admin.Phone},
	})
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.Created(w, map[string]any{"society": d, "admin_credentials": cred})
}

func (h *Handler) UpdateSociety(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	id, err := uuidParam(r, "id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	var req updateSocietyRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	d, err := h.svc.UpdateSociety(r.Context(), p.UserID, id, UpdateSocietyInput(req))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, d, nil)
}

func (h *Handler) Suspend(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	id, err := uuidParam(r, "id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	var req suspendRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	if err := h.svc.Suspend(r.Context(), p.UserID, id, strings.TrimSpace(req.Reason)); err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.NoContent(w)
}

func (h *Handler) Reactivate(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	id, err := uuidParam(r, "id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if err := h.svc.Reactivate(r.Context(), p.UserID, id); err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.NoContent(w)
}

func (h *Handler) AddAdmin(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	id, err := uuidParam(r, "id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	var req adminDTO
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	cred, err := h.svc.AddAdmin(r.Context(), p.UserID, id, AdminInput(req))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.Created(w, cred)
}

func (h *Handler) ResetAdminPassword(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	sid, err := uuidParam(r, "id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	uid, err := uuidParam(r, "userId")
	if err != nil {
		httpx.Fail(w, ErrAdminNotFound)
		return
	}
	cred, err := h.svc.ResetAdminPassword(r.Context(), p.UserID, sid, uid)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, cred, nil)
}

// ── Services ─────────────────────────────────────────────────────────────────

func (h *Handler) Catalogue(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.Catalogue(r.Context())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, rows, nil)
}

func (h *Handler) SetFeature(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	id, err := uuidParam(r, "id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	var req featureRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	rows, err := h.svc.SetFeatures(r.Context(), p.UserID, id, map[string]bool{chi.URLParam(r, "key"): *req.Enabled}, req.Reason)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, rows, nil)
}

func (h *Handler) BulkFeatures(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	id, err := uuidParam(r, "id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	var req bulkFeatureRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	rows, err := h.svc.SetFeatures(r.Context(), p.UserID, id, req.Features, req.Reason)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, rows, nil)
}

// ── Audit & stats ────────────────────────────────────────────────────────────

func parseTime(v string) *time.Time {
	if v == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, v); err == nil {
			return &t
		}
	}
	return nil
}

func (h *Handler) Audit(w http.ResponseWriter, r *http.Request) {
	pg := pagination.FromRequest(r)
	q := r.URL.Query()
	f := AuditFilter{Action: q.Get("action"), From: parseTime(q.Get("from")), To: parseTime(q.Get("to")), Limit: pg.PageSize, Offset: pg.Offset()}
	if v := q.Get("society_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			httpx.Fail(w, httpx.NewError(http.StatusBadRequest, "VALIDATION", "invalid society_id"))
			return
		}
		f.SocietyID = &id
	}
	if v := q.Get("actor"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			httpx.Fail(w, httpx.NewError(http.StatusBadRequest, "VALIDATION", "invalid actor"))
			return
		}
		f.ActorID = &id
	}
	rows, total, err := h.svc.ListAudit(r.Context(), f)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, rows, pg.Meta(total))
}

func (h *Handler) Stats(w http.ResponseWriter, r *http.Request) {
	st, err := h.svc.Stats(r.Context())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, st, nil)
}

// ── Routes ───────────────────────────────────────────────────────────────────

// Mount registers /superadmin/* on r.
func Mount(r chi.Router, svc *Service, rdb *redis.Client, allowedCIDRs []string) {
	h := NewHandler(svc)
	r.Route("/superadmin", func(r chi.Router) {
		r.Use(IPAllowlist(allowedCIDRs))

		r.Route("/auth", func(r chi.Router) {
			r.With(middleware.RateLimit(rdb, "pf-login", 30, 10*time.Minute)).Post("/login", h.Login)
			r.With(middleware.RateLimit(rdb, "pf-mfa", 60, 10*time.Minute)).Post("/mfa/verify", h.VerifyMFA)
			r.Post("/refresh", h.Refresh)
			r.With(svc.PlatformAuth).Post("/logout", h.Logout)
		})

		r.Group(func(r chi.Router) {
			r.Use(svc.PlatformAuth)
			r.Get("/me", h.Me)
			r.Get("/societies", h.ListSocieties)
			r.Get("/societies/{id}", h.GetSociety)
			r.Get("/features", h.Catalogue)
			r.Get("/audit", h.Audit)
			r.Get("/stats", h.Stats)

			r.Group(func(r chi.Router) { // mutations: super_admin only
				r.Use(RequireSuper)
				r.Post("/societies", h.CreateSociety)
				r.Put("/societies/{id}", h.UpdateSociety)
				r.Post("/societies/{id}/suspend", h.Suspend)
				r.Post("/societies/{id}/reactivate", h.Reactivate)
				r.Post("/societies/{id}/admins", h.AddAdmin)
				r.Post("/societies/{id}/admins/{userId}/reset-password", h.ResetAdminPassword)
				r.Put("/societies/{id}/features/{key}", h.SetFeature)
				r.Post("/societies/{id}/features/bulk", h.BulkFeatures)
			})
		})
	})
}
