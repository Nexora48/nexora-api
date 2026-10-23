package auth

import (
	"net"
	"net/http"

	"github.com/google/uuid"

	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func client(r *http.Request, dev *DeviceInfo) ClientInfo {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	return ClientInfo{IP: ip, UserAgent: r.UserAgent(), Device: dev}
}

func (h *Handler) RequestOTP(w http.ResponseWriter, r *http.Request) {
	var req OTPRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	if err := h.svc.RequestOTP(r.Context(), req.Phone); err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.NoContent(w)
}

func (h *Handler) VerifyOTP(w http.ResponseWriter, r *http.Request) {
	var req OTPVerifyRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	resp, err := h.svc.VerifyOTP(r.Context(), req.Phone, req.OTP, client(r, req.Device))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, resp, nil)
}

func (h *Handler) PasswordLogin(w http.ResponseWriter, r *http.Request) {
	var req PasswordLoginRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	resp, err := h.svc.PasswordLogin(r.Context(), req.Email, req.Password, client(r, req.Device))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, resp, nil)
}

func (h *Handler) SelectSociety(w http.ResponseWriter, r *http.Request) {
	var req SelectSocietyRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	resp, err := h.svc.SelectSociety(r.Context(), req.SelectionToken, uuid.MustParse(req.MembershipID), client(r, req.Device))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, resp, nil)
}

func (h *Handler) Refresh(w http.ResponseWriter, r *http.Request) {
	var req RefreshRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	resp, err := h.svc.Refresh(r.Context(), req.RefreshToken, client(r, nil))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, resp, nil)
}

func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	p, ok := authctx.From(r.Context())
	if !ok {
		httpx.Fail(w, ErrTokenInvalid)
		return
	}
	var req LogoutRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	if err := h.svc.Logout(r.Context(), p, req.RefreshToken, client(r, nil).IP); err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.NoContent(w)
}
