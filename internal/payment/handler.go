package payment

import (
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
	"github.com/Nexora48/nexora-api/internal/platform/metrics"
)

type Handler struct {
	svc        *Service
	publicBase string
}

// NewHandler: publicBase is the address residents' phones reach the API on (empty = taken from the request).
func NewHandler(svc *Service, publicBase string) *Handler {
	return &Handler{svc: svc, publicBase: strings.TrimRight(publicBase, "/")}
}

func principal(r *http.Request) authctx.Principal { p, _ := authctx.From(r.Context()); return p }

func (h *Handler) base(r *http.Request) string {
	if h.publicBase != "" {
		return h.publicBase
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (h *Handler) withURL(r *http.Request, o *Order) *Order {
	o.CheckoutURL = h.base(r) + "/pay/" + o.ID.String() + "?t=" + url.QueryEscape(o.Token)
	return o
}

func (h *Handler) CreateOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AmountPaise *int64 `json:"amount_paise"`
	}
	if r.ContentLength != 0 {
		if err := httpx.Decode(w, r, &req); err != nil {
			httpx.Fail(w, err)
			return
		}
	}
	o, err := h.svc.CreateOrder(r.Context(), principal(r), req.AmountPaise, r.Header.Get("Idempotency-Key"))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.Created(w, h.withURL(r, o))
}

func (h *Handler) Order(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, ErrNotFound)
		return
	}
	o, err := h.svc.MyOrder(r.Context(), principal(r), id)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, o, nil)
}

func page(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func message(w http.ResponseWriter, status int, title, text string) {
	page(w, status, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><body style="font-family:system-ui;text-align:center;padding:48px 20px"><h2>`+title+`</h2><p>`+text+`</p>`)
}

// Checkout serves the hosted payment page.
func (h *Handler) Checkout(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		message(w, http.StatusNotFound, "Not found", "This payment link is not valid.")
		return
	}
	t := r.URL.Query().Get("t")
	confirm := h.base(r) + "/pay/" + id.String() + "/fake?t=" + url.QueryEscape(t)
	body, err := h.svc.Page(r.Context(), id, t, confirm)
	if err != nil {
		code := http.StatusNotFound
		text := "This payment link is not valid."
		if ae, ok := err.(*httpx.AppError); ok {
			code, text = ae.Status, ae.Message
		}
		message(w, code, "Payment", text)
		return
	}
	page(w, http.StatusOK, body)
}

// FakeConfirm is the test gateway's Pay / Fail button.
func (h *Handler) FakeConfirm(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		message(w, http.StatusNotFound, "Not found", "This payment link is not valid.")
		return
	}
	_ = r.ParseForm()
	result := r.PostFormValue("result")
	if err := h.svc.ConfirmFake(r.Context(), id, r.URL.Query().Get("t"), result); err != nil {
		message(w, http.StatusBadRequest, "Payment", "This payment could not be completed.")
		return
	}
	if result == "fail" {
		message(w, http.StatusOK, "Payment failed", "Nothing was charged. Return to the app and try again.")
		return
	}
	message(w, http.StatusOK, "Payment received", "You can close this page and return to the app.")
}

// Razorpay receives the gateway's webhook for a society.
func (h *Handler) Razorpay(w http.ResponseWriter, r *http.Request) {
	soc, err := uuid.Parse(chi.URLParam(r, "society"))
	if err != nil {
		httpx.Fail(w, ErrNotFound)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		httpx.Fail(w, httpx.NewError(http.StatusRequestEntityTooLarge, "TOO_LARGE", "payload too large"))
		return
	}
	if err := h.svc.Webhook(r.Context(), soc, body, r.Header); err != nil {
		metrics.Webhooks.WithLabelValues("rejected").Inc()
		httpx.Fail(w, err)
		return
	}
	metrics.Webhooks.WithLabelValues("accepted").Inc()
	httpx.OK(w, map[string]bool{"ok": true}, nil)
}

// ── office ───────────────────────────────────────────────────────────────────

func (h *Handler) GetGateway(w http.ResponseWriter, r *http.Request) {
	v, err := h.svc.Info(r.Context(), principal(r).SocietyID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string]any{"gateway": v, "webhook_url": h.base(r) + "/api/v1/webhooks/razorpay/" + principal(r).SocietyID.String()}, nil)
}

func (h *Handler) PutGateway(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider      string `json:"provider"`
		KeyID         string `json:"key_id"`
		KeySecret     string `json:"key_secret"`
		WebhookSecret string `json:"webhook_secret"`
		Active        bool   `json:"active"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	v, err := h.svc.Configure(r.Context(), principal(r), GatewayInput{Provider: req.Provider, KeyID: req.KeyID, KeySecret: req.KeySecret, WebhookSecret: req.WebhookSecret, Active: req.Active})
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

func (h *Handler) TestGateway(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.Test(r.Context(), principal(r)); err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string]bool{"ok": true}, nil)
}
