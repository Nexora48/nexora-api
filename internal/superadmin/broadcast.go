package superadmin

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Nexora48/nexora-api/internal/notify"
	"github.com/Nexora48/nexora-api/internal/platform/heartbeat"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
)

// Broadcast: a notice from Nexora to the office of every society, a plan or chosen societies
// (optionally to residents too). Recipients are resolved once, then delivered in batches.

const (
	broadcastsPerDay = 5
	defaultBatch     = 200
)

var (
	ErrBroadcastLimit    = httpx.NewError(http.StatusTooManyRequests, "BROADCAST_LIMIT", "at most 5 broadcasts can be sent per day")
	ErrNoRecipients      = httpx.NewError(http.StatusUnprocessableEntity, "NO_RECIPIENTS", "no one matches this audience")
	ErrBroadcastNotFound = httpx.NewError(http.StatusNotFound, "NOT_FOUND", "broadcast not found")
	ErrNotCancellable    = httpx.NewError(http.StatusConflict, "NOT_CANCELLABLE", "delivery has already started")
)

type Audience struct {
	All        bool        `json:"all"`
	PlanIDs    []uuid.UUID `json:"plan_ids"`
	SocietyIDs []uuid.UUID `json:"society_ids"`
}

type Broadcast struct {
	ID               uuid.UUID  `json:"id"`
	Title            string     `json:"title"`
	Body             string     `json:"body"`
	Link             *string    `json:"link,omitempty"`
	Audience         Audience   `json:"audience"`
	IncludeResidents bool       `json:"include_residents"`
	Status           string     `json:"status"`
	Recipients       int        `json:"recipients"`
	SentCount        int        `json:"sent_count"`
	CreatedBy        *uuid.UUID `json:"created_by"`
	SenderName       *string    `json:"sender_name,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	FinishedAt       *time.Time `json:"finished_at,omitempty"`
}

type BroadcastInput struct {
	Title            string   `json:"title"`
	Body             string   `json:"body"`
	Link             string   `json:"link"`
	Audience         Audience `json:"audience"`
	IncludeResidents bool     `json:"include_residents"`
	Preview          bool     `json:"preview"` // only count the recipients
}

// SetNotifier connects the notification pipeline (broadcasts are delivered through it).
func (s *Service) SetNotifier(n *notify.Service) { s.notifier = n }

// SetBroadcastBatch changes how many people are notified per pass (tests).
func (s *Service) SetBroadcastBatch(n int) { s.batch = n }

const bcSelect = `SELECT b.id, b.title, b.body, b.link, b.audience, b.include_residents, b.status, b.recipients, b.sent_count, b.created_by, pu.full_name, b.created_at, b.finished_at
	FROM platform_broadcasts b LEFT JOIN platform_users pu ON pu.id=b.created_by`

func scanBroadcast(row interface{ Scan(...any) error }) (*Broadcast, error) {
	var b Broadcast
	var aud []byte
	if err := row.Scan(&b.ID, &b.Title, &b.Body, &b.Link, &aud, &b.IncludeResidents, &b.Status, &b.Recipients, &b.SentCount, &b.CreatedBy, &b.SenderName, &b.CreatedAt, &b.FinishedAt); err != nil {
		return nil, err
	}
	_ = json.Unmarshal(aud, &b.Audience)
	return &b, nil
}

func (s *Service) ListBroadcasts(ctx context.Context) ([]Broadcast, error) {
	rows, err := s.store.pool.Query(ctx, bcSelect+` ORDER BY b.created_at DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Broadcast{}
	for rows.Next() {
		b, err := scanBroadcast(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

// audienceSQL picks the societies and the roles that receive a broadcast.
func audienceWhere(a Audience, residents bool) (string, []any, error) {
	modes := 0
	if a.All {
		modes++
	}
	if len(a.PlanIDs) > 0 {
		modes++
	}
	if len(a.SocietyIDs) > 0 {
		modes++
	}
	if modes != 1 {
		return "", nil, httpx.NewError(http.StatusBadRequest, "VALIDATION", "choose one audience: all societies, plans or selected societies")
	}
	roles := []string{"society_admin"}
	if residents {
		roles = append(roles, "committee", "resident_owner", "resident_tenant", "family_member")
	}
	args := []any{roles}
	where := `m.status='active' AND m.role_key = ANY($1) AND so.status IN ('active','trial')`
	switch {
	case len(a.PlanIDs) > 0:
		args = append(args, a.PlanIDs)
		where += ` AND EXISTS (SELECT 1 FROM society_subscriptions sub WHERE sub.society_id=so.id AND sub.plan_id = ANY($2))`
	case len(a.SocietyIDs) > 0:
		args = append(args, a.SocietyIDs)
		where += ` AND so.id = ANY($2)`
	}
	return where, args, nil
}

func (s *Service) CreateBroadcast(ctx context.Context, actor uuid.UUID, in BroadcastInput) (*Broadcast, int, error) {
	in.Title, in.Body, in.Link = strings.TrimSpace(in.Title), strings.TrimSpace(in.Body), strings.TrimSpace(in.Link)
	if in.Title == "" || len(in.Title) > 120 || in.Body == "" || len(in.Body) > 2000 {
		return nil, 0, httpx.NewError(http.StatusBadRequest, "VALIDATION", "a title (120 characters) and a message (2000 characters) are required")
	}
	if in.Link != "" && !strings.HasPrefix(in.Link, "https://") {
		return nil, 0, httpx.NewError(http.StatusBadRequest, "VALIDATION", "a link must start with https://")
	}
	where, args, err := audienceWhere(in.Audience, in.IncludeResidents)
	if err != nil {
		return nil, 0, err
	}
	var count int
	if err := s.store.pool.QueryRow(ctx, `SELECT count(DISTINCT m.user_id) FROM memberships m JOIN societies so ON so.id=m.society_id WHERE `+where, args...).Scan(&count); err != nil {
		return nil, 0, err
	}
	if in.Preview {
		return nil, count, nil
	}
	if count == 0 {
		return nil, 0, ErrNoRecipients
	}
	var today int
	if err := s.store.pool.QueryRow(ctx, `SELECT count(*) FROM platform_broadcasts WHERE created_at > now() - interval '24 hours'`).Scan(&today); err != nil {
		return nil, 0, err
	}
	if today >= broadcastsPerDay {
		return nil, 0, ErrBroadcastLimit
	}
	aud, _ := json.Marshal(in.Audience)
	tx, err := s.store.pool.Begin(ctx)
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var link *string
	if in.Link != "" {
		link = &in.Link
	}
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `INSERT INTO platform_broadcasts (title, body, link, audience, include_residents, recipients, created_by) VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		in.Title, in.Body, link, aud, in.IncludeResidents, count, actor).Scan(&id); err != nil {
		return nil, 0, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO platform_broadcast_targets (broadcast_id, user_id, society_id)
		SELECT DISTINCT ON (m.user_id) $`+strconv.Itoa(len(args)+1)+`, m.user_id, m.society_id FROM memberships m JOIN societies so ON so.id=m.society_id WHERE `+where+` ORDER BY m.user_id, m.id`,
		append(args, id)...); err != nil {
		return nil, 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, 0, err
	}
	b, err := scanBroadcast(s.store.pool.QueryRow(ctx, bcSelect+` WHERE b.id=$1`, id))
	if err != nil {
		return nil, 0, err
	}
	s.platformLog(ctx, actor, nil, "broadcast.create", "broadcast", id.String(), map[string]any{"title": in.Title, "recipients": count, "audience": in.Audience, "include_residents": in.IncludeResidents})
	return b, count, nil
}

func (s *Service) CancelBroadcast(ctx context.Context, actor, id uuid.UUID) (*Broadcast, error) {
	tag, err := s.store.pool.Exec(ctx, `UPDATE platform_broadcasts SET status='cancelled', finished_at=now() WHERE id=$1 AND status='queued'`, id)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		var one int
		if err := s.store.pool.QueryRow(ctx, `SELECT 1 FROM platform_broadcasts WHERE id=$1`, id).Scan(&one); err != nil {
			return nil, ErrBroadcastNotFound
		}
		return nil, ErrNotCancellable
	}
	s.platformLog(ctx, actor, nil, "broadcast.cancel", "broadcast", id.String(), nil)
	return scanBroadcast(s.store.pool.QueryRow(ctx, bcSelect+` WHERE b.id=$1`, id))
}

// DeliverBroadcasts sends the next batch of every open broadcast and returns how many people were notified.
func (s *Service) DeliverBroadcasts(ctx context.Context) (int, error) {
	if s.notifier == nil {
		return 0, nil
	}
	batch := s.batch
	if batch <= 0 {
		batch = defaultBatch
	}
	rows, err := s.store.pool.Query(ctx, `SELECT id, title, body, link FROM platform_broadcasts WHERE status IN ('queued','sending') ORDER BY created_at`)
	if err != nil {
		return 0, err
	}
	type open struct {
		id          uuid.UUID
		title, body string
		link        *string
	}
	var list []open
	for rows.Next() {
		var o open
		if err := rows.Scan(&o.id, &o.title, &o.body, &o.link); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, o)
	}
	rows.Close()
	sent := 0
	for _, o := range list {
		tx, err := s.store.pool.Begin(ctx)
		if err != nil {
			return sent, err
		}
		// the first batch closes the cancel window
		if _, err := tx.Exec(ctx, `UPDATE platform_broadcasts SET status='sending' WHERE id=$1 AND status='queued'`, o.id); err != nil {
			_ = tx.Rollback(ctx)
			return sent, err
		}
		var st string
		if err := tx.QueryRow(ctx, `SELECT status FROM platform_broadcasts WHERE id=$1`, o.id).Scan(&st); err != nil || st != "sending" {
			_ = tx.Rollback(ctx)
			continue
		}
		trs, err := tx.Query(ctx, `SELECT user_id, society_id FROM platform_broadcast_targets WHERE broadcast_id=$1 AND sent_at IS NULL ORDER BY user_id LIMIT $2 FOR UPDATE SKIP LOCKED`, o.id, batch)
		if err != nil {
			_ = tx.Rollback(ctx)
			return sent, err
		}
		bySociety := map[uuid.UUID][]uuid.UUID{}
		var users []uuid.UUID
		for trs.Next() {
			var u, so uuid.UUID
			if err := trs.Scan(&u, &so); err != nil {
				trs.Close()
				_ = tx.Rollback(ctx)
				return sent, err
			}
			bySociety[so] = append(bySociety[so], u)
			users = append(users, u)
		}
		trs.Close()
		if len(users) > 0 {
			if _, err := tx.Exec(ctx, `UPDATE platform_broadcast_targets SET sent_at=now() WHERE broadcast_id=$1 AND user_id = ANY($2)`, o.id, users); err != nil {
				_ = tx.Rollback(ctx)
				return sent, err
			}
		}
		var left int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM platform_broadcast_targets WHERE broadcast_id=$1 AND sent_at IS NULL`, o.id).Scan(&left); err != nil {
			_ = tx.Rollback(ctx)
			return sent, err
		}
		if _, err := tx.Exec(ctx, `UPDATE platform_broadcasts SET sent_count = sent_count + $2, status = CASE WHEN $3 = 0 THEN 'sent' ELSE 'sending' END,
			finished_at = CASE WHEN $3 = 0 THEN now() ELSE NULL END WHERE id=$1`, o.id, len(users), left); err != nil {
			_ = tx.Rollback(ctx)
			return sent, err
		}
		if err := tx.Commit(ctx); err != nil {
			return sent, err
		}
		data := map[string]any{"broadcast_id": o.id.String()}
		if o.link != nil {
			data["link"] = *o.link
		}
		for so, us := range bySociety {
			s.notifier.Send(ctx, notify.Notification{SocietyID: so, Users: us, Type: "system.broadcast", Title: o.title, Body: o.body, Data: data})
		}
		sent += len(users)
	}
	return sent, nil
}

func (s *Service) RunBroadcasts(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := s.DeliverBroadcasts(ctx); err == nil {
				heartbeat.Beat("broadcasts")
			}
		}
	}
}
