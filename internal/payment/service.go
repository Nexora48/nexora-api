package payment

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Nexora48/nexora-api/internal/platform/heartbeat"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nexora48/nexora-api/internal/audit"
	"github.com/Nexora48/nexora-api/internal/billing"
	"github.com/Nexora48/nexora-api/internal/notify"
	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
	"github.com/Nexora48/nexora-api/internal/platform/money"
)

const orderTTL = 30 * time.Minute

func conflict(code, msg string) error { return httpx.NewError(http.StatusConflict, code, msg) }
func invalid(msg string) error        { return httpx.NewError(http.StatusBadRequest, "VALIDATION", msg) }

var (
	ErrNotFound      = httpx.NewError(http.StatusNotFound, "NOT_FOUND", "not found")
	ErrNotConfigured = conflict("PAYMENTS_NOT_CONFIGURED", "online payment is not set up for your society; please pay the society office")
	ErrCannotPay     = httpx.NewError(http.StatusForbidden, "CANNOT_PAY", "only the owner (or a tenant the office has allowed) can pay maintenance")
	ErrAmount        = httpx.NewError(http.StatusUnprocessableEntity, "INVALID_AMOUNT", "the amount must be at least ₹1 and not more than what is due")
	ErrBadSignature  = httpx.NewError(http.StatusUnauthorized, "BAD_SIGNATURE", "invalid signature")
)

// Cipher encrypts the gateway secrets at rest.
type Cipher interface {
	Encrypt(string) (string, error)
	Decrypt(string) (string, error)
}

type Service struct {
	pool      *pgxpool.Pool
	bill      *billing.Service
	audit     *audit.Logger
	note      *notify.Service
	cipher    Cipher
	secret    string // derives the checkout page tokens
	allowFake bool
	gateways  map[string]Gateway
	now       func() time.Time
}

func NewService(pool *pgxpool.Pool, bill *billing.Service, a *audit.Logger, note *notify.Service, c Cipher, secret string, allowFake bool) *Service {
	return &Service{pool: pool, bill: bill, audit: a, note: note, cipher: c, secret: secret, allowFake: allowFake, now: time.Now,
		gateways: map[string]Gateway{"razorpay": newRazorpay(), "fake": fake{}}}
}

// SetGateway swaps an adapter (tests point Razorpay at a stub).
func (s *Service) SetGateway(g Gateway) { s.gateways[g.Name()] = g }

func (s *Service) log(ctx context.Context, p authctx.Principal, action, entity string, id uuid.UUID, meta map[string]any) {
	s.audit.Log(ctx, audit.Entry{ActorID: &p.UserID, SocietyID: &p.SocietyID, Action: action, Entity: entity, EntityID: id.String(), Meta: meta})
}

// ── gateway settings (secrets are write-only) ────────────────────────────────

type GatewayInfo struct {
	Provider      string `json:"provider"`
	KeyID         string `json:"key_id"`
	SecretSet     bool   `json:"secret_set"`
	WebhookSet    bool   `json:"webhook_secret_set"`
	Active        bool   `json:"active"`
	FakeAvailable bool   `json:"fake_available"`
}

type GatewayInput struct {
	Provider      string
	KeyID         string
	KeySecret     string // empty keeps the stored one
	WebhookSecret string
	Active        bool
}

func (s *Service) Info(ctx context.Context, soc uuid.UUID) (*GatewayInfo, error) {
	g := &GatewayInfo{Provider: "razorpay", FakeAvailable: s.allowFake}
	var sec, wh *string
	err := s.pool.QueryRow(ctx, `SELECT provider, key_id, key_secret_enc, webhook_secret_enc, active FROM payment_gateway_settings WHERE society_id=$1`, soc).Scan(&g.Provider, &g.KeyID, &sec, &wh, &g.Active)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	g.SecretSet, g.WebhookSet = sec != nil && *sec != "", wh != nil && *wh != ""
	return g, nil
}

func (s *Service) Configure(ctx context.Context, p authctx.Principal, in GatewayInput) (*GatewayInfo, error) {
	switch in.Provider {
	case "razorpay":
	case "fake":
		if !s.allowFake {
			return nil, invalid("the test gateway is not available here")
		}
	default:
		return nil, invalid("provider must be razorpay")
	}
	in.KeyID = strings.TrimSpace(in.KeyID)
	if len(in.KeyID) > 80 || len(in.KeySecret) > 120 || len(in.WebhookSecret) > 120 {
		return nil, invalid("a key is too long")
	}
	cur, _ := s.creds(ctx, p.SocietyID)
	secret, webhook := in.KeySecret, in.WebhookSecret
	if secret == "" {
		secret = cur.KeySecret
	}
	if webhook == "" {
		webhook = cur.WebhookSecret
	}
	if in.Provider == "razorpay" && in.Active && (in.KeyID == "" || secret == "" || webhook == "") {
		return nil, invalid("an active Razorpay setup needs the key id, the key secret and the webhook secret")
	}
	encSecret, err := s.cipher.Encrypt(secret)
	if err != nil {
		return nil, err
	}
	encWebhook, err := s.cipher.Encrypt(webhook)
	if err != nil {
		return nil, err
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO payment_gateway_settings (society_id, provider, key_id, key_secret_enc, webhook_secret_enc, active, updated_at) VALUES ($1,$2,$3,$4,$5,$6,now())
		ON CONFLICT (society_id) DO UPDATE SET provider=EXCLUDED.provider, key_id=EXCLUDED.key_id, key_secret_enc=EXCLUDED.key_secret_enc, webhook_secret_enc=EXCLUDED.webhook_secret_enc, active=EXCLUDED.active, updated_at=now()`,
		p.SocietyID, in.Provider, in.KeyID, encSecret, encWebhook, in.Active); err != nil {
		return nil, err
	}
	// never log the secrets, only that they changed
	s.log(ctx, p, "gateway.configure", "gateway", p.SocietyID, map[string]any{"provider": in.Provider, "active": in.Active, "secret_changed": in.KeySecret != "", "webhook_changed": in.WebhookSecret != ""})
	return s.Info(ctx, p.SocietyID)
}

// creds returns the society's decrypted credentials.
func (s *Service) creds(ctx context.Context, soc uuid.UUID) (Creds, error) {
	var c Creds
	var sec, wh *string
	if err := s.pool.QueryRow(ctx, `SELECT key_id, key_secret_enc, webhook_secret_enc FROM payment_gateway_settings WHERE society_id=$1`, soc).Scan(&c.KeyID, &sec, &wh); err != nil {
		return c, err
	}
	if sec != nil && *sec != "" {
		c.KeySecret, _ = s.cipher.Decrypt(*sec)
	}
	if wh != nil && *wh != "" {
		c.WebhookSecret, _ = s.cipher.Decrypt(*wh)
	}
	return c, nil
}

// Test checks the stored credentials against the gateway.
func (s *Service) Test(ctx context.Context, p authctx.Principal) error {
	g, c, err := s.resolve(ctx, p.SocietyID)
	if err != nil {
		return err
	}
	if err := g.Ping(ctx, c); err != nil {
		return conflict("GATEWAY_ERROR", "the gateway did not accept these credentials")
	}
	return nil
}

// resolve picks the gateway for a society: its active setup, else the test gateway where that is allowed.
func (s *Service) resolve(ctx context.Context, soc uuid.UUID) (Gateway, Creds, error) {
	var provider string
	var active bool
	err := s.pool.QueryRow(ctx, `SELECT provider, active FROM payment_gateway_settings WHERE society_id=$1`, soc).Scan(&provider, &active)
	if err == nil && active {
		if provider == "fake" && !s.allowFake {
			return nil, Creds{}, ErrNotConfigured
		}
		c, _ := s.creds(ctx, soc)
		return s.gateways[provider], c, nil
	}
	if s.allowFake {
		return s.gateways["fake"], Creds{}, nil
	}
	return nil, Creds{}, ErrNotConfigured
}

// ── orders ───────────────────────────────────────────────────────────────────

type Order struct {
	ID          uuid.UUID  `json:"id"`
	AmountPaise int64      `json:"amount_paise"`
	Status      string     `json:"status"` // created | paid | failed | expired
	Provider    string     `json:"provider"`
	ExpiresAt   time.Time  `json:"expires_at"`
	PaymentID   *uuid.UUID `json:"payment_id,omitempty"`
	Token       string     `json:"-"`
	CheckoutURL string     `json:"checkout_url,omitempty"`
}

func (s *Service) token(id uuid.UUID) string {
	m := hmac.New(sha256.New, []byte(s.secret))
	m.Write([]byte("pay:" + id.String()))
	return hex.EncodeToString(m.Sum(nil))[:40]
}

func (s *Service) tokenOK(id uuid.UUID, t string) bool {
	return hmac.Equal([]byte(t), []byte(s.token(id)))
}

const orderCols = `id, society_id, unit_id, amount_paise, provider, provider_order_id, status, expires_at, payment_id`

type orderRow struct {
	Order
	Society, Unit uuid.UUID
	ProviderRef   *string
}

func scanOrder(row pgx.Row) (*orderRow, error) {
	var o orderRow
	if err := row.Scan(&o.ID, &o.Society, &o.Unit, &o.AmountPaise, &o.Provider, &o.ProviderRef, &o.Status, &o.ExpiresAt, &o.PaymentID); err != nil {
		return nil, err
	}
	return &o, nil
}

// CreateOrder starts an online payment for the caller's flat. The same Idempotency-Key returns the same order.
func (s *Service) CreateOrder(ctx context.Context, p authctx.Principal, amount *int64, idem string) (*Order, error) {
	if !s.bill.CanPay(ctx, p) {
		return nil, ErrCannotPay
	}
	var unit uuid.UUID
	if err := s.pool.QueryRow(ctx, `SELECT unit_id FROM memberships WHERE id=$1 AND status='active'`, p.MembershipID).Scan(&unit); err != nil {
		return nil, billing.ErrNoUnit
	}
	idem = strings.TrimSpace(idem)
	if len(idem) > 80 {
		return nil, invalid("the idempotency key is too long")
	}
	if idem != "" {
		if o, err := scanOrder(s.pool.QueryRow(ctx, `SELECT `+orderCols+` FROM payment_orders WHERE society_id=$1 AND idem_key=$2`, p.SocietyID, idem)); err == nil && o.Unit == unit {
			o.Token = s.token(o.ID)
			return &o.Order, nil
		}
	}
	due, err := s.bill.Outstanding(ctx, unit)
	if err != nil {
		return nil, err
	}
	if due <= 0 {
		return nil, billing.ErrNothingDue
	}
	amt := due
	if amount != nil {
		amt = *amount
	}
	if amt < 100 || amt > due {
		return nil, ErrAmount
	}
	gw, creds, err := s.resolve(ctx, p.SocietyID)
	if err != nil {
		return nil, err
	}
	id := uuid.New()
	ref, err := gw.CreateOrder(ctx, creds, amt, id.String())
	if err != nil {
		return nil, conflict("GATEWAY_ERROR", "the payment gateway is not reachable right now; please try again")
	}
	sum := sha256.Sum256([]byte(s.token(id)))
	var idemArg *string
	if idem != "" {
		idemArg = &idem
	}
	exp := s.now().Add(orderTTL)
	if _, err := s.pool.Exec(ctx, `INSERT INTO payment_orders (id, society_id, unit_id, amount_paise, provider, provider_order_id, token_hash, idem_key, created_by, expires_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		id, p.SocietyID, unit, amt, gw.Name(), ref, hex.EncodeToString(sum[:]), idemArg, p.MembershipID, exp); err != nil {
		return nil, err
	}
	s.log(ctx, p, "payment.order", "order", id, map[string]any{"amount_paise": amt, "provider": gw.Name()})
	return &Order{ID: id, AmountPaise: amt, Status: "created", Provider: gw.Name(), ExpiresAt: exp, Token: s.token(id)}, nil
}

// MyOrder reads an order of the caller's flat for polling; unpaid orders past their time read as expired.
func (s *Service) MyOrder(ctx context.Context, p authctx.Principal, id uuid.UUID) (*Order, error) {
	var unit uuid.UUID
	if err := s.pool.QueryRow(ctx, `SELECT unit_id FROM memberships WHERE id=$1 AND status='active'`, p.MembershipID).Scan(&unit); err != nil {
		return nil, ErrNotFound
	}
	o, err := scanOrder(s.pool.QueryRow(ctx, `SELECT `+orderCols+` FROM payment_orders WHERE id=$1 AND society_id=$2`, id, p.SocietyID))
	if err != nil || o.Unit != unit {
		return nil, ErrNotFound
	}
	if o.Status == "created" && o.ExpiresAt.Before(s.now()) {
		o.Status = "expired"
	}
	return &o.Order, nil
}

// ── hosted checkout ──────────────────────────────────────────────────────────

// Page returns the checkout HTML for a pending order, if the token matches.
func (s *Service) Page(ctx context.Context, id uuid.UUID, token, confirmURL string) (string, error) {
	if !s.tokenOK(id, token) {
		return "", ErrNotFound
	}
	o, err := scanOrder(s.pool.QueryRow(ctx, `SELECT `+orderCols+` FROM payment_orders WHERE id=$1`, id))
	if err != nil {
		return "", ErrNotFound
	}
	if o.Status == "paid" {
		return "", conflict("ALREADY_PAID", "this payment is already complete")
	}
	if o.ExpiresAt.Before(s.now()) {
		return "", conflict("EXPIRED", "this payment link has expired; start again from the app")
	}
	gw, ok := s.gateways[o.Provider]
	if !ok {
		return "", ErrNotFound
	}
	var name string
	_ = s.pool.QueryRow(ctx, `SELECT name FROM societies WHERE id=$1`, o.Society).Scan(&name)
	c, _ := s.creds(ctx, o.Society)
	ref := ""
	if o.ProviderRef != nil {
		ref = *o.ProviderRef
	}
	return gw.CheckoutHTML(OrderInfo{ID: o.ID.String(), ProviderRef: ref, AmountPaise: o.AmountPaise, Society: name, ConfirmURL: confirmURL}, c), nil
}

// ConfirmFake is the test gateway's Pay / Fail button: it feeds the same event path a real webhook uses.
func (s *Service) ConfirmFake(ctx context.Context, id uuid.UUID, token, result string) error {
	if !s.allowFake || !s.tokenOK(id, token) {
		return ErrNotFound
	}
	o, err := scanOrder(s.pool.QueryRow(ctx, `SELECT `+orderCols+` FROM payment_orders WHERE id=$1`, id))
	if err != nil || o.Provider != "fake" || o.ProviderRef == nil {
		return ErrNotFound
	}
	ev := Event{ID: "fake-" + id.String() + "-" + result, Type: "paid", ProviderOrderID: *o.ProviderRef, ProviderPaymentID: "fakepay_" + id.String()[:8], AmountPaise: o.AmountPaise}
	if result == "fail" {
		ev.Type = "failed"
	} else if result != "pay" {
		return invalid("result must be pay or fail")
	}
	raw, _ := json.Marshal(ev)
	return s.HandleEvent(ctx, o.Society, "fake", ev, raw)
}

// ── webhooks ─────────────────────────────────────────────────────────────────

// Webhook verifies and processes a Razorpay notification for a society.
func (s *Service) Webhook(ctx context.Context, soc uuid.UUID, body []byte, h http.Header) error {
	var provider string
	if err := s.pool.QueryRow(ctx, `SELECT provider FROM payment_gateway_settings WHERE society_id=$1`, soc).Scan(&provider); err != nil || provider != "razorpay" {
		return ErrNotFound
	}
	creds, err := s.creds(ctx, soc)
	if err != nil {
		return ErrNotFound
	}
	gw := s.gateways["razorpay"]
	if err := gw.VerifyWebhook(creds, body, h); err != nil {
		return ErrBadSignature
	}
	ev, err := gw.ParseWebhook(body, h)
	if err != nil {
		return invalid("unreadable event")
	}
	return s.HandleEvent(ctx, soc, "razorpay", ev, body)
}

// HandleEvent applies one gateway event exactly once (events are de-duplicated by id; an order is paid only once).
// If processing fails the event record is removed so the gateway's retry is not mistaken for a replay.
func (s *Service) HandleEvent(ctx context.Context, soc uuid.UUID, provider string, ev Event, raw []byte) error {
	fresh, err := s.handle(ctx, soc, provider, ev, raw)
	if err != nil && fresh {
		_, _ = s.pool.Exec(context.WithoutCancel(ctx), `DELETE FROM payment_events WHERE provider=$1 AND event_id=$2 AND NOT processed`, provider, ev.ID)
	}
	return err
}

func (s *Service) handle(ctx context.Context, soc uuid.UUID, provider string, ev Event, raw []byte) (fresh bool, err error) {
	tag, err := s.pool.Exec(ctx, `INSERT INTO payment_events (provider, event_id, society_id, payload) VALUES ($1,$2,$3,$4) ON CONFLICT DO NOTHING`, provider, ev.ID, soc, raw)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 || ev.Type == "ignore" {
		return false, nil // replay, or an event we do not act on
	}
	done := func(note string) {
		_, _ = s.pool.Exec(ctx, `UPDATE payment_events SET processed=true, note=NULLIF($3,'') WHERE provider=$1 AND event_id=$2`, provider, ev.ID, note)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return true, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	o, err := scanOrder(tx.QueryRow(ctx, `SELECT `+orderCols+` FROM payment_orders WHERE provider=$1 AND provider_order_id=$2 AND society_id=$3 FOR UPDATE`, provider, ev.ProviderOrderID, soc))
	if errors.Is(err, pgx.ErrNoRows) {
		_ = tx.Rollback(ctx) // release the connection before done() asks the pool for another
		done("unknown order")
		return true, nil
	}
	if err != nil {
		return true, err
	}
	if ev.Type == "failed" {
		if _, err := tx.Exec(ctx, `UPDATE payment_orders SET status='failed' WHERE id=$1 AND status='created'`, o.ID); err != nil {
			return true, err
		}
		if err := tx.Commit(ctx); err != nil {
			return true, err
		}
		done("")
		return true, nil
	}
	if o.Status == "paid" {
		_ = tx.Rollback(ctx)
		done("already paid")
		return true, nil
	}
	if ev.AmountPaise != o.AmountPaise {
		if _, err := tx.Exec(ctx, `UPDATE payment_orders SET note=$2 WHERE id=$1`, o.ID, fmt.Sprintf("amount mismatch: order %d, gateway %d", o.AmountPaise, ev.AmountPaise)); err != nil {
			return true, err
		}
		if err := tx.Commit(ctx); err != nil {
			return true, err
		}
		done("amount mismatch")
		s.alertOffice(ctx, soc, o, ev)
		return true, nil
	}
	loc := time.UTC
	var tz string
	_ = tx.QueryRow(ctx, `SELECT timezone FROM societies WHERE id=$1`, soc).Scan(&tz)
	if l, e := time.LoadLocation(tz); e == nil {
		loc = l
	}
	n := s.now().In(loc)
	paidOn := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
	pid, receipt, err := s.bill.CreatePaymentTx(ctx, tx, billing.PaymentIn{Society: soc, Unit: o.Unit, AmountPaise: o.AmountPaise, Mode: provider, Reference: ev.ProviderPaymentID, PaidOn: paidOn, OrderID: &o.ID, Reason: "Online payment"})
	if err != nil {
		return true, err
	}
	if _, err := tx.Exec(ctx, `UPDATE payment_orders SET status='paid', paid_at=now(), payment_id=$2 WHERE id=$1`, o.ID, pid); err != nil {
		return true, err
	}
	if _, err := tx.Exec(ctx, `UPDATE payment_events SET processed=true WHERE provider=$1 AND event_id=$2`, provider, ev.ID); err != nil {
		return true, err
	}
	if err := tx.Commit(ctx); err != nil {
		return true, err
	}
	s.bill.AfterPayment(ctx, soc, o.Unit, pid, o.AmountPaise, receipt)
	return true, nil
}

func (s *Service) alertOffice(ctx context.Context, soc uuid.UUID, o *orderRow, ev Event) {
	s.audit.Log(ctx, audit.Entry{SocietyID: &soc, Action: "payment.mismatch", Entity: "order", EntityID: o.ID.String(), Meta: map[string]any{"order_paise": o.AmountPaise, "gateway_paise": ev.AmountPaise, "payment": ev.ProviderPaymentID}})
	if s.note == nil {
		return
	}
	users := s.note.ByRoles(ctx, soc, "society_admin")
	if len(users) > 0 {
		s.note.Send(ctx, notify.Notification{SocietyID: soc, Users: users, Type: "bill.mismatch", Title: "Payment needs attention",
			Body: fmt.Sprintf("A gateway payment of %s does not match its order of %s", money.Rupees(ev.AmountPaise), money.Rupees(o.AmountPaise)), Data: map[string]any{"type": "bill"}, Priority: notify.PriorityNormal})
	}
}

// ExpireOrders marks unpaid orders past their time as expired.
func (s *Service) ExpireOrders(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE payment_orders SET status='expired' WHERE status IN ('created','failed') AND expires_at < now()`)
	return tag.RowsAffected(), err
}

func (s *Service) RunSweeper(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			heartbeat.Beat("payments")
			_, _ = s.ExpireOrders(ctx)
		}
	}
}
