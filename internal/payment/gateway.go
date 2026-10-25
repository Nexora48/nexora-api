// Package payment connects billing to a payment gateway: orders, the hosted checkout page, signature-verified
// webhooks and the society's gateway settings (secrets encrypted, write-only).
package payment

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"time"

	"github.com/Nexora48/nexora-api/internal/platform/money"
)

type Creds struct{ KeyID, KeySecret, WebhookSecret string }

// Event is a gateway notification reduced to what billing needs.
type Event struct {
	ID                string
	Type              string // paid | failed | ignore
	ProviderOrderID   string
	ProviderPaymentID string
	AmountPaise       int64
}

type OrderInfo struct {
	ID          string
	ProviderRef string
	AmountPaise int64
	Society     string
	ConfirmURL  string // fake provider only
}

type Gateway interface {
	Name() string
	CreateOrder(ctx context.Context, c Creds, amountPaise int64, receipt string) (string, error)
	CheckoutHTML(o OrderInfo, c Creds) string
	VerifyWebhook(c Creds, body []byte, h http.Header) error
	ParseWebhook(body []byte, h http.Header) (Event, error)
	Ping(ctx context.Context, c Creds) error
}

var errBadSignature = errors.New("invalid webhook signature")

// ── Razorpay ─────────────────────────────────────────────────────────────────

type razorpay struct {
	base string
	http *http.Client
}

func newRazorpay() *razorpay {
	return &razorpay{base: "https://api.razorpay.com/v1", http: &http.Client{Timeout: 15 * time.Second}}
}

func (r *razorpay) Name() string { return "razorpay" }

func (r *razorpay) call(ctx context.Context, c Creds, method, path string, body any) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.base+path, rd)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.KeyID, c.KeySecret)
	req.Header.Set("Content-Type", "application/json")
	res, err := r.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode >= 300 {
		return nil, fmt.Errorf("razorpay %d", res.StatusCode)
	}
	return b, nil
}

func (r *razorpay) CreateOrder(ctx context.Context, c Creds, amount int64, receipt string) (string, error) {
	b, err := r.call(ctx, c, http.MethodPost, "/orders", map[string]any{"amount": amount, "currency": "INR", "receipt": receipt})
	if err != nil {
		return "", err
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.ID == "" {
		return "", errors.New("razorpay: no order id")
	}
	return out.ID, nil
}

func (r *razorpay) Ping(ctx context.Context, c Creds) error {
	_, err := r.call(ctx, c, http.MethodGet, "/orders?count=1", nil)
	return err
}

func (r *razorpay) CheckoutHTML(o OrderInfo, c Creds) string {
	return fmt.Sprintf(`<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Pay</title>
<style>body{font-family:system-ui,sans-serif;margin:0;display:flex;min-height:100vh;align-items:center;justify-content:center;background:#f0f2f5}main{background:#fff;border-radius:16px;padding:28px;max-width:380px;text-align:center;box-shadow:0 2px 12px #0002}button{background:#1877f2;color:#fff;border:0;border-radius:24px;padding:14px 28px;font-size:16px;font-weight:600}</style></head>
<body><main><h2>%s</h2><p>Amount to pay: <b>%s</b></p><button id="b">Pay now</button><p id="m" style="color:#555"></p></main>
<script src="https://checkout.razorpay.com/v1/checkout.js"></script>
<script>
var opts={key:%q,amount:%d,currency:"INR",name:%q,order_id:%q,
 handler:function(){document.getElementById("m").textContent="Payment received. You can close this page and return to the app.";document.getElementById("b").style.display="none"},
 modal:{ondismiss:function(){document.getElementById("m").textContent="Payment not completed."}},theme:{color:"#1877f2"}};
document.getElementById("b").onclick=function(){new Razorpay(opts).open()};
document.getElementById("b").click();
</script></body></html>`, html.EscapeString(o.Society), html.EscapeString(money.Rupees(o.AmountPaise)), c.KeyID, o.AmountPaise, o.Society, o.ProviderRef)
}

func (r *razorpay) VerifyWebhook(c Creds, body []byte, h http.Header) error {
	if c.WebhookSecret == "" {
		return errBadSignature
	}
	m := hmac.New(sha256.New, []byte(c.WebhookSecret))
	m.Write(body)
	want := hex.EncodeToString(m.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(h.Get("X-Razorpay-Signature"))) {
		return errBadSignature
	}
	return nil
}

func (r *razorpay) ParseWebhook(body []byte, h http.Header) (Event, error) {
	var in struct {
		Event   string `json:"event"`
		Payload struct {
			Payment struct {
				Entity struct {
					ID      string `json:"id"`
					OrderID string `json:"order_id"`
					Amount  int64  `json:"amount"`
				} `json:"entity"`
			} `json:"payment"`
			Order struct {
				Entity struct {
					ID         string `json:"id"`
					AmountPaid int64  `json:"amount_paid"`
				} `json:"entity"`
			} `json:"order"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return Event{}, err
	}
	ev := Event{ID: h.Get("X-Razorpay-Event-Id"), Type: "ignore", ProviderOrderID: in.Payload.Payment.Entity.OrderID, ProviderPaymentID: in.Payload.Payment.Entity.ID, AmountPaise: in.Payload.Payment.Entity.Amount}
	if ev.ID == "" {
		sum := sha256.Sum256(body)
		ev.ID = "body-" + hex.EncodeToString(sum[:])
	}
	switch in.Event {
	case "payment.captured":
		ev.Type = "paid"
	case "order.paid":
		ev.Type = "paid"
		if ev.ProviderOrderID == "" {
			ev.ProviderOrderID = in.Payload.Order.Entity.ID
			ev.AmountPaise = in.Payload.Order.Entity.AmountPaid
		}
	case "payment.failed":
		ev.Type = "failed"
	}
	return ev, nil
}

// ── Fake (development and tests) ─────────────────────────────────────────────

type fake struct{}

func (fake) Name() string { return "fake" }

func (fake) CreateOrder(_ context.Context, _ Creds, _ int64, receipt string) (string, error) {
	return "fake_" + receipt, nil
}

func (fake) Ping(context.Context, Creds) error { return nil }

func (fake) CheckoutHTML(o OrderInfo, _ Creds) string {
	return fmt.Sprintf(`<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Test payment</title>
<style>body{font-family:system-ui,sans-serif;margin:0;display:flex;min-height:100vh;align-items:center;justify-content:center;background:#f0f2f5}main{background:#fff;border-radius:16px;padding:28px;max-width:380px;text-align:center;box-shadow:0 2px 12px #0002}button{border:0;border-radius:24px;padding:14px 26px;font-size:16px;font-weight:600;margin:6px;color:#fff}</style></head>
<body><main><h2>%s</h2><p>Test checkout · <b>%s</b></p>
<form method="post" action="%s"><button name="result" value="pay" style="background:#2e9b4e">Pay</button><button name="result" value="fail" style="background:#d93a3a">Fail</button></form>
<p style="color:#777;font-size:13px">Development gateway: no money moves.</p></main></body></html>`, html.EscapeString(o.Society), html.EscapeString(money.Rupees(o.AmountPaise)), html.EscapeString(o.ConfirmURL))
}

func (fake) VerifyWebhook(Creds, []byte, http.Header) error { return nil }

func (fake) ParseWebhook([]byte, http.Header) (Event, error) {
	return Event{}, errors.New("the test gateway has no webhook")
}

// NewRazorpayAt is the Razorpay adapter pointed at another base URL (tests use a local stub server).
func NewRazorpayAt(base string) Gateway {
	return &razorpay{base: base, http: &http.Client{Timeout: 5 * time.Second}}
}
