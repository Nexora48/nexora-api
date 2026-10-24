package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nexora48/nexora-api/internal/platform/httpx"
	"github.com/Nexora48/nexora-api/internal/realtime"
)

// Priority decides the channels: silent = inbox + live badge only, normal = also push (unless muted),
// critical = push that ignores preferences (SOS).
const (
	PrioritySilent   = "silent"
	PriorityNormal   = "normal"
	PriorityCritical = "critical"
)

const maxAttempts = 5

var ErrNotificationNotFound = httpx.NewError(http.StatusNotFound, "NOT_FOUND", "notification not found")

// Notification is one event for one or more people of a society.
type Notification struct {
	SocietyID uuid.UUID
	Users     []uuid.UUID
	Type      string // visit.request, visit.decided, visit.arrived, sos.raised, ...
	Title     string
	Body      string
	Data      map[string]any
	Priority  string
}

// Item is what the inbox returns.
type Item struct {
	ID        uuid.UUID       `json:"id"`
	Type      string          `json:"type"`
	Title     string          `json:"title"`
	Body      string          `json:"body"`
	Data      json.RawMessage `json:"data"`
	Priority  string          `json:"priority"`
	ReadAt    *time.Time      `json:"read_at"`
	CreatedAt time.Time       `json:"created_at"`
}

// Publisher is the realtime hub (nil in tests that do not need it).
type Publisher interface {
	Publish(ctx context.Context, e realtime.Event)
}

type Service struct {
	pool      *pgxpool.Pool
	sender    Sender
	log       *slog.Logger
	pub       Publisher
	retention time.Duration
}

func NewService(pool *pgxpool.Pool, sender Sender, log *slog.Logger) *Service {
	if sender == nil {
		sender = LogSender{Log: log}
	}
	return &Service{pool: pool, sender: sender, log: log, retention: 90 * 24 * time.Hour}
}

func (s *Service) SetPublisher(p Publisher)     { s.pub = p }
func (s *Service) SetRetention(d time.Duration) { s.retention = d }

// Group maps a type to its preference group. SOS has no group of its own that can be muted.
func Group(t string) string {
	switch {
	case strings.HasPrefix(t, "visit."):
		return "visits"
	case strings.HasPrefix(t, "sos."):
		return "sos"
	case strings.HasPrefix(t, "notice."), strings.HasPrefix(t, "poll."):
		return "community"
	case strings.HasPrefix(t, "complaint."):
		return "complaints"
	}
	return "system"
}

func channelFor(t string) string {
	if strings.HasPrefix(t, "sos.") {
		return "sos_alerts"
	}
	return "visitor_requests"
}

// Send stores the notification for each person, updates their open apps live and queues the pushes.
// Pushes are attempted immediately; failures stay in the outbox and are retried by the worker.
func (s *Service) Send(ctx context.Context, n Notification) {
	if n.Priority == "" {
		n.Priority = PriorityNormal
	}
	data, _ := json.Marshal(n.Data)
	if n.Data == nil {
		data = []byte("{}")
	}
	seen := map[uuid.UUID]bool{}
	var outbox []uuid.UUID
	for _, u := range n.Users {
		if seen[u] {
			continue
		}
		seen[u] = true
		var id uuid.UUID
		var at time.Time
		if err := s.pool.QueryRow(ctx, `INSERT INTO notifications (society_id, user_id, type, title, body, data, priority) VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id, created_at`,
			n.SocietyID, u, n.Type, n.Title, n.Body, data, n.Priority).Scan(&id, &at); err != nil {
			s.warn("store notification", err)
			continue
		}
		if s.pub != nil {
			uid := u
			s.pub.Publish(ctx, realtime.Event{Type: "notification.new", SocietyID: n.SocietyID, User: &uid, Data: map[string]any{
				"id": id, "type": n.Type, "title": n.Title, "body": n.Body, "data": n.Data, "priority": n.Priority, "created_at": at}})
		}
		if n.Priority == PrioritySilent || !s.pushAllowed(ctx, u, n) {
			continue
		}
		ids, err := s.enqueuePush(ctx, u, n)
		if err != nil {
			s.warn("queue push", err)
			continue
		}
		outbox = append(outbox, ids...)
	}
	s.process(ctx, outbox)
}

func (s *Service) warn(what string, err error) {
	if s.log != nil {
		s.log.Warn("notify: "+what, "err", err)
	}
}

func (s *Service) pushAllowed(ctx context.Context, user uuid.UUID, n Notification) bool {
	if n.Priority == PriorityCritical {
		return true
	}
	var push bool
	err := s.pool.QueryRow(ctx, `SELECT push FROM notification_prefs WHERE user_id=$1 AND grp=$2`, user, Group(n.Type)).Scan(&push)
	if errors.Is(err, pgx.ErrNoRows) {
		return true
	}
	return err != nil || push
}

type pushPayload struct {
	Title   string            `json:"title"`
	Body    string            `json:"body"`
	Data    map[string]string `json:"data"`
	Channel string            `json:"channel"`
}

func (s *Service) enqueuePush(ctx context.Context, user uuid.UUID, n Notification) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx, `SELECT fcm_token FROM devices WHERE user_id=$1 AND fcm_token IS NOT NULL AND fcm_token <> ''`, user)
	if err != nil {
		return nil, err
	}
	var tokens []string
	for rows.Next() {
		var t string
		if rows.Scan(&t) == nil {
			tokens = append(tokens, t)
		}
	}
	rows.Close()
	data := map[string]string{}
	for k, v := range n.Data {
		data[k] = fmt.Sprint(v)
	}
	payload, _ := json.Marshal(pushPayload{Title: n.Title, Body: n.Body, Data: data, Channel: channelFor(n.Type)})
	var ids []uuid.UUID
	for _, t := range tokens {
		var id uuid.UUID
		if err := s.pool.QueryRow(ctx, `INSERT INTO notification_outbox (user_id, channel, target, payload) VALUES ($1,'push',$2,$3) RETURNING id`, user, t, payload).Scan(&id); err != nil {
			return ids, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// process sends the given outbox rows; failures back off exponentially (30 s, 60 s, ...) up to 5 attempts.
func (s *Service) process(ctx context.Context, ids []uuid.UUID) {
	if len(ids) == 0 {
		return
	}
	rows, err := s.pool.Query(ctx, `SELECT id, target, payload, attempts FROM notification_outbox WHERE id = ANY($1) AND sent_at IS NULL`, ids)
	if err != nil {
		return
	}
	type row struct {
		id       uuid.UUID
		target   string
		payload  []byte
		attempts int
	}
	var batch []row
	for rows.Next() {
		var r row
		if rows.Scan(&r.id, &r.target, &r.payload, &r.attempts) == nil {
			batch = append(batch, r)
		}
	}
	rows.Close()
	for _, r := range batch {
		var p pushPayload
		_ = json.Unmarshal(r.payload, &p)
		err := s.sender.Send(ctx, r.target, Message{Title: p.Title, Body: p.Body, Data: p.Data, Channel: p.Channel})
		switch {
		case err == nil:
			_, _ = s.pool.Exec(ctx, `UPDATE notification_outbox SET sent_at=now(), attempts=attempts+1, last_error=NULL WHERE id=$1`, r.id)
		case errors.Is(err, ErrUnregistered):
			_, _ = s.pool.Exec(ctx, `UPDATE devices SET fcm_token=NULL WHERE fcm_token=$1`, r.target)
			_, _ = s.pool.Exec(ctx, `UPDATE notification_outbox SET sent_at=now(), last_error='unregistered' WHERE id=$1`, r.id)
		default:
			delay := time.Duration(30<<r.attempts) * time.Second
			_, _ = s.pool.Exec(ctx, `UPDATE notification_outbox SET attempts=attempts+1, last_error=$2, next_at=now()+$3::interval WHERE id=$1`, r.id, err.Error(), fmt.Sprintf("%d seconds", int(delay.Seconds())))
			s.warn("push failed", err)
		}
	}
}

// Retry sends every outbox row that is due (called by the worker).
func (s *Service) Retry(ctx context.Context) {
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`SELECT id FROM notification_outbox WHERE sent_at IS NULL AND attempts < %d AND next_at <= now() ORDER BY next_at LIMIT 100`, maxAttempts))
	if err != nil {
		return
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	s.process(ctx, ids)
}

// Cleanup drops old notifications and delivered outbox rows.
func (s *Service) Cleanup(ctx context.Context) {
	_, _ = s.pool.Exec(ctx, `DELETE FROM notifications WHERE created_at < now() - $1::interval`, fmt.Sprintf("%d seconds", int(s.retention.Seconds())))
	_, _ = s.pool.Exec(ctx, `DELETE FROM notification_outbox WHERE created_at < now() - interval '7 days' AND (sent_at IS NOT NULL OR attempts >= 5)`)
}

// RunWorker retries failed sends every interval and cleans up hourly.
func (s *Service) RunWorker(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Retry(ctx)
			if time.Since(last) > time.Hour {
				s.Cleanup(ctx)
				last = time.Now()
			}
		}
	}
}

// ── inbox ────────────────────────────────────────────────────────────────────

func (s *Service) List(ctx context.Context, user, society uuid.UUID, unreadOnly bool, limit, offset int) ([]Item, int, error) {
	where := `user_id=$1 AND society_id=$2`
	if unreadOnly {
		where += ` AND read_at IS NULL`
	}
	rows, err := s.pool.Query(ctx, `SELECT id, type, title, body, data, priority, read_at, created_at, count(*) OVER()
		FROM notifications WHERE `+where+` ORDER BY created_at DESC, id LIMIT $3 OFFSET $4`, user, society, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Item{}
	total := 0
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ID, &it.Type, &it.Title, &it.Body, &it.Data, &it.Priority, &it.ReadAt, &it.CreatedAt, &total); err != nil {
			return nil, 0, err
		}
		out = append(out, it)
	}
	return out, total, rows.Err()
}

func (s *Service) UnreadCount(ctx context.Context, user, society uuid.UUID) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE user_id=$1 AND society_id=$2 AND read_at IS NULL`, user, society).Scan(&n)
	return n, err
}

func (s *Service) MarkRead(ctx context.Context, user, society, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `UPDATE notifications SET read_at=COALESCE(read_at, now()) WHERE id=$1 AND user_id=$2 AND society_id=$3`, id, user, society)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotificationNotFound
	}
	return nil
}

func (s *Service) ReadAll(ctx context.Context, user, society uuid.UUID) (int64, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE notifications SET read_at=now() WHERE user_id=$1 AND society_id=$2 AND read_at IS NULL`, user, society)
	return tag.RowsAffected(), err
}

// ── preferences ──────────────────────────────────────────────────────────────

type Pref struct {
	Group  string `json:"group"`
	Label  string `json:"label"`
	Push   bool   `json:"push"`
	Email  bool   `json:"email"`
	Locked bool   `json:"locked"` // critical: cannot be switched off
}

var prefGroups = []struct{ key, label string }{{"visits", "Visitors & deliveries"}, {"community", "Notices & polls"}, {"complaints", "Complaints"}, {"system", "Society updates"}}

func (s *Service) Prefs(ctx context.Context, user uuid.UUID) ([]Pref, error) {
	out := []Pref{}
	for _, g := range prefGroups {
		p := Pref{Group: g.key, Label: g.label, Push: true}
		_ = s.pool.QueryRow(ctx, `SELECT push, email FROM notification_prefs WHERE user_id=$1 AND grp=$2`, user, g.key).Scan(&p.Push, &p.Email)
		out = append(out, p)
	}
	out = append(out, Pref{Group: "sos", Label: "Emergency alerts", Push: true, Locked: true})
	return out, nil
}

func (s *Service) SetPref(ctx context.Context, user uuid.UUID, group string, push, email bool) error {
	if group == "sos" {
		return httpx.NewError(http.StatusBadRequest, "LOCKED", "emergency alerts cannot be switched off")
	}
	if group != "visits" && group != "system" && group != "community" && group != "complaints" {
		return httpx.NewError(http.StatusBadRequest, "VALIDATION", "unknown group")
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO notification_prefs (user_id, grp, push, email) VALUES ($1,$2,$3,$4)
		ON CONFLICT (user_id, grp) DO UPDATE SET push=EXCLUDED.push, email=EXCLUDED.email`, user, group, push, email)
	return err
}

// ── recipients ───────────────────────────────────────────────────────────────

// UnitResidents are the owner/tenant of a flat (the people who can answer for it).
func (s *Service) UnitResidents(ctx context.Context, society, unit uuid.UUID) []uuid.UUID {
	return s.users(ctx, `SELECT DISTINCT user_id FROM memberships WHERE society_id=$1 AND unit_id=$2 AND status='active' AND role_key IN ('resident_owner','resident_tenant')`, society, unit)
}

// UnitMembers also includes family members.
func (s *Service) UnitMembers(ctx context.Context, society, unit uuid.UUID) []uuid.UUID {
	return s.users(ctx, `SELECT DISTINCT user_id FROM memberships WHERE society_id=$1 AND unit_id=$2 AND status='active' AND role_key IN ('resident_owner','resident_tenant','family_member')`, society, unit)
}

// ByRoles returns every active member of the society with one of the roles.
func (s *Service) ByRoles(ctx context.Context, society uuid.UUID, roles ...string) []uuid.UUID {
	return s.users(ctx, `SELECT DISTINCT user_id FROM memberships WHERE society_id=$1 AND status='active' AND role_key = ANY($2)`, society, roles)
}

func (s *Service) users(ctx context.Context, q string, args ...any) []uuid.UUID {
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out
}
