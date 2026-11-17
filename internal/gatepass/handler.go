package gatepass

import (
	"net/http"

	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func principal(r *http.Request) authctx.Principal { p, _ := authctx.From(r.Context()); return p }

// Mine: GET /me/gate-pass — a fresh QR for the signed-in resident.
func (h *Handler) Mine(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.Mine(r.Context(), principal(r))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

type scanInput struct {
	QR string `json:"qr"`
}

func (h *Handler) read(w http.ResponseWriter, r *http.Request) (string, bool) {
	var in scanInput
	if err := httpx.Decode(w, r, &in); err != nil {
		return "", false
	}
	if in.QR == "" {
		httpx.Fail(w, ErrBadPass)
		return "", false
	}
	return in.QR, true
}

// Verify: POST /gate/residents/verify {qr} — who is this and what would the scan log; nothing is written.
func (h *Handler) Verify(w http.ResponseWriter, r *http.Request) {
	qr, ok := h.read(w, r)
	if !ok {
		return
	}
	res, err := h.svc.Verify(r.Context(), principal(r), qr)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, res, nil)
}

// Entry: POST /gate/residents/entry {qr} — logs the entry or exit.
func (h *Handler) Entry(w http.ResponseWriter, r *http.Request) {
	qr, ok := h.read(w, r)
	if !ok {
		return
	}
	res, err := h.svc.Record(r.Context(), principal(r), qr)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, res, nil)
}
