package user

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
	"github.com/Nexora48/nexora-api/internal/society"
)

type PermissionLister interface {
	List(ctx context.Context, role string) ([]string, error)
}

type FeatureMapper interface {
	Map(ctx context.Context, societyID uuid.UUID) (map[string]bool, error)
}

type Handler struct {
	users     *Repository
	societies *society.Repository
	perms     PermissionLister
	features  FeatureMapper
}

func NewHandler(users *Repository, societies *society.Repository, perms PermissionLister, features FeatureMapper) *Handler {
	return &Handler{users: users, societies: societies, perms: perms, features: features}
}

type meResponse struct {
	User struct {
		ID       string  `json:"id"`
		FullName string  `json:"full_name"`
		Phone    string  `json:"phone"`
		Email    *string `json:"email,omitempty"`
	} `json:"user"`
	Membership struct {
		ID     string  `json:"id"`
		Role   string  `json:"role"`
		UnitID *string `json:"unit_id,omitempty"`
	} `json:"membership"`
	Society struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"society"`
	Permissions []string        `json:"permissions"`
	Features    map[string]bool `json:"features"`
}

// Me returns the caller's profile, active membership, permissions and features.
func (h *Handler) Me(w http.ResponseWriter, r *http.Request) {
	p, _ := authctx.From(r.Context())
	ctx := r.Context()

	u, err := h.users.FindByID(ctx, p.UserID)
	if err != nil {
		httpx.Fail(w, httpx.NewError(http.StatusUnauthorized, "UNAUTHORIZED", "user not found"))
		return
	}
	m, err := h.users.GetMembership(ctx, p.MembershipID)
	if err != nil || m.UserID != p.UserID || m.Status != "active" {
		httpx.Fail(w, httpx.NewError(http.StatusForbidden, "FORBIDDEN", "membership no longer active"))
		return
	}
	s, err := h.societies.Get(ctx, p.SocietyID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	perms, err := h.perms.List(ctx, m.RoleKey)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	feats, err := h.features.Map(ctx, p.SocietyID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}

	var resp meResponse
	resp.User.ID, resp.User.FullName, resp.User.Phone, resp.User.Email = u.ID.String(), u.FullName, u.Phone, u.Email
	resp.Membership.ID, resp.Membership.Role = m.ID.String(), m.RoleKey
	if m.UnitID != nil {
		s := m.UnitID.String()
		resp.Membership.UnitID = &s
	}
	resp.Society.ID, resp.Society.Name, resp.Society.Status = s.ID.String(), s.Name, s.Status
	resp.Permissions, resp.Features = perms, feats
	httpx.OK(w, resp, nil)
}

// Features returns only the feature map (cheap refresh for the apps).
func (h *Handler) Features(w http.ResponseWriter, r *http.Request) {
	p, _ := authctx.From(r.Context())
	feats, err := h.features.Map(r.Context(), p.SocietyID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, feats, nil)
}

type deviceRequest struct {
	DeviceID   string `json:"device_id" validate:"required,max=100"`
	Platform   string `json:"platform" validate:"max=20"`
	AppVersion string `json:"app_version" validate:"max=30"`
	FCMToken   string `json:"fcm_token" validate:"max=500"`
}

// RegisterDevice stores/updates the caller's device (push token).
func (h *Handler) RegisterDevice(w http.ResponseWriter, r *http.Request) {
	p, _ := authctx.From(r.Context())
	var req deviceRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	if err := h.users.UpsertDevice(r.Context(), p.UserID, req.DeviceID, req.Platform, req.FCMToken, req.AppVersion); err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.NoContent(w)
}
