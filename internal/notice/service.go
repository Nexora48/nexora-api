// Package notice holds the society notice board: drafts, scheduled and published notices, block targeting,
// attachments, read tracking and the push on publication.
package notice

import (
	"context"
	"errors"
	"fmt"
	"github.com/Nexora48/nexora-api/internal/platform/heartbeat"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nexora48/nexora-api/internal/attachment"
	"github.com/Nexora48/nexora-api/internal/audit"
	"github.com/Nexora48/nexora-api/internal/notify"
	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
	"github.com/Nexora48/nexora-api/internal/realtime"
)

const (
	maxPinned      = 3
	maxAttachments = 5
)

var (
	ErrNotFound  = httpx.NewError(http.StatusNotFound, "NOT_FOUND", "notice not found")
	ErrPinLimit  = httpx.NewError(http.StatusConflict, "PIN_LIMIT", "at most 3 notices can be pinned; unpin one first")
	ErrBadState  = httpx.NewError(http.StatusConflict, "INVALID_STATE", "this notice cannot be changed that way")
	categories   = map[string]bool{"general": true, "maintenance": true, "event": true, "emergency": true, "notice": true}
	priorities   = map[string]bool{"normal": true, "important": true, "urgent": true}
	errValidName = func(m string) error { return httpx.NewError(http.StatusBadRequest, "VALIDATION", m) }
)

type Service struct {
	pool  *pgxpool.Pool
	audit *audit.Logger
	files *attachment.Service
	note  *notify.Service
	hub   *realtime.Hub
}

func NewService(pool *pgxpool.Pool, a *audit.Logger, files *attachment.Service, note *notify.Service, hub *realtime.Hub) *Service {
	return &Service{pool: pool, audit: a, files: files, note: note, hub: hub}
}

type Notice struct {
	ID          uuid.UUID         `json:"id"`
	Title       string            `json:"title"`
	Body        string            `json:"body"`
	Category    string            `json:"category"`
	Priority    string            `json:"priority"`
	Status      string            `json:"status"`
	Pinned      bool              `json:"pinned"`
	AllBlocks   bool              `json:"all_blocks"`
	BlockIDs    []uuid.UUID       `json:"block_ids"`
	BlockCodes  []string          `json:"block_codes"`
	PublishAt   *time.Time        `json:"publish_at,omitempty"`
	PublishedAt *time.Time        `json:"published_at,omitempty"`
	ExpiresAt   *time.Time        `json:"expires_at,omitempty"`
	EditedAt    *time.Time        `json:"edited_at,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	Read        *bool             `json:"read,omitempty"` // residents only
	Attachments []attachment.File `json:"attachments"`
}

type Input struct {
	Title     string
	Body      string
	Category  string
	Priority  string
	Pinned    bool
	AllBlocks bool
	BlockIDs  []uuid.UUID
	Status    string // draft | scheduled | published
	PublishAt *time.Time
	ExpiresAt *time.Time
}

const cols = `n.id, n.title, n.body, n.category, n.priority, n.status, n.pinned, n.all_blocks, n.publish_at, n.published_at, n.expires_at, n.edited_at, n.created_at`

func scanNotice(row pgx.Row, extra ...any) (*Notice, error) {
	var n Notice
	dest := []any{&n.ID, &n.Title, &n.Body, &n.Category, &n.Priority, &n.Status, &n.Pinned, &n.AllBlocks, &n.PublishAt, &n.PublishedAt, &n.ExpiresAt, &n.EditedAt, &n.CreatedAt}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	n.BlockIDs, n.BlockCodes, n.Attachments = []uuid.UUID{}, []string{}, []attachment.File{}
	return &n, nil
}

// fill adds the targeted blocks and the attachments in two queries for the whole page.
func (s *Service) fill(ctx context.Context, soc uuid.UUID, ns []*Notice) error {
	if len(ns) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(ns))
	by := map[uuid.UUID]*Notice{}
	for i, n := range ns {
		ids[i], by[n.ID] = n.ID, n
	}
	rows, err := s.pool.Query(ctx, `SELECT nb.notice_id, b.id, b.code FROM notice_blocks nb JOIN blocks b ON b.id=nb.block_id WHERE nb.notice_id = ANY($1) ORDER BY b.code`, ids)
	if err != nil {
		return err
	}
	for rows.Next() {
		var nid, bid uuid.UUID
		var code string
		if rows.Scan(&nid, &bid, &code) == nil {
			by[nid].BlockIDs = append(by[nid].BlockIDs, bid)
			by[nid].BlockCodes = append(by[nid].BlockCodes, code)
		}
	}
	rows.Close()
	files, err := s.files.ListMany(ctx, soc, attachment.OwnerNotice, ids)
	if err != nil {
		return err
	}
	for id, f := range files {
		by[id].Attachments = f
	}
	return nil
}

func (s *Service) log(ctx context.Context, p authctx.Principal, action string, id uuid.UUID, meta map[string]any) {
	s.audit.Log(ctx, audit.Entry{ActorID: &p.UserID, SocietyID: &p.SocietyID, Action: action, Entity: "notice", EntityID: id.String(), Meta: meta})
}

// ── residents, guards ────────────────────────────────────────────────────────

func (s *Service) unitOf(ctx context.Context, p authctx.Principal) *uuid.UUID {
	var u *uuid.UUID
	_ = s.pool.QueryRow(ctx, `SELECT unit_id FROM memberships WHERE id=$1 AND user_id=$2 AND society_id=$3 AND status='active'`, p.MembershipID, p.UserID, p.SocietyID).Scan(&u)
	return u
}

// visible is the SQL a person may read: published, not expired, for everyone or for the block of their flat.
// Without a flat (guards) every published notice is visible.
const visible = `n.society_id=$1 AND n.status='published' AND (n.expires_at IS NULL OR n.expires_at > now())
	AND (n.all_blocks OR $2::uuid IS NULL OR EXISTS (SELECT 1 FROM notice_blocks nb JOIN units u ON u.block_id=nb.block_id WHERE nb.notice_id=n.id AND u.id=$2::uuid))`

func (s *Service) List(ctx context.Context, p authctx.Principal, unreadOnly bool, limit, offset int) ([]*Notice, int, error) {
	unit := s.unitOf(ctx, p)
	where := visible
	if unreadOnly {
		where += ` AND NOT EXISTS (SELECT 1 FROM notice_reads nr WHERE nr.notice_id=n.id AND nr.user_id=$3)`
	}
	args := []any{p.SocietyID, unit}
	if unreadOnly {
		args = append(args, p.UserID)
	}
	args = append(args, limit, offset)
	rows, err := s.pool.Query(ctx, `SELECT `+cols+`, EXISTS (SELECT 1 FROM notice_reads nr WHERE nr.notice_id=n.id AND nr.user_id=$`+fmt.Sprint(len(args)+1)+`), count(*) OVER()
		FROM notices n WHERE `+where+fmt.Sprintf(` ORDER BY n.pinned DESC, n.published_at DESC, n.id LIMIT $%d OFFSET $%d`, len(args)-1, len(args)), append(args, p.UserID)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*Notice
	total := 0
	for rows.Next() {
		var read bool
		n, err := scanNotice(rows, &read, &total)
		if err != nil {
			return nil, 0, err
		}
		n.Read = &read
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if err := s.fill(ctx, p.SocietyID, out); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

func (s *Service) UnreadCount(ctx context.Context, p authctx.Principal) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM notices n WHERE `+visible+` AND NOT EXISTS (SELECT 1 FROM notice_reads nr WHERE nr.notice_id=n.id AND nr.user_id=$3)`,
		p.SocietyID, s.unitOf(ctx, p), p.UserID).Scan(&n)
	return n, err
}

func (s *Service) get(ctx context.Context, p authctx.Principal, id uuid.UUID) (*Notice, error) {
	var read bool
	n, err := scanNotice(s.pool.QueryRow(ctx, `SELECT `+cols+`, EXISTS (SELECT 1 FROM notice_reads nr WHERE nr.notice_id=n.id AND nr.user_id=$4) FROM notices n WHERE n.id=$3 AND `+visible,
		p.SocietyID, s.unitOf(ctx, p), id, p.UserID), &read)
	if err != nil {
		return nil, err
	}
	n.Read = &read
	if err := s.fill(ctx, p.SocietyID, []*Notice{n}); err != nil {
		return nil, err
	}
	return n, nil
}

// Get returns one notice and marks it read for the caller.
func (s *Service) Get(ctx context.Context, p authctx.Principal, id uuid.UUID) (*Notice, error) {
	n, err := s.get(ctx, p, id)
	if err != nil {
		return nil, err
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO notice_reads (notice_id, user_id, unit_id) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, id, p.UserID, s.unitOf(ctx, p)); err != nil {
		return nil, err
	}
	t := true
	n.Read = &t
	return n, nil
}

// Attachment serves a file of a notice the caller may read.
func (s *Service) Attachment(ctx context.Context, p authctx.Principal, id, aid uuid.UUID) ([]byte, *attachment.File, error) {
	if _, err := s.get(ctx, p, id); err != nil {
		return nil, nil, err
	}
	return s.files.Open(ctx, p.SocietyID, attachment.OwnerNotice, id, aid)
}

// ── office ───────────────────────────────────────────────────────────────────

func (s *Service) AdminList(ctx context.Context, p authctx.Principal, status string, limit, offset int) ([]*Notice, int, error) {
	args := []any{p.SocietyID}
	where := `n.society_id=$1 AND n.status <> 'archived'`
	if status != "" {
		args = append(args, status)
		where = `n.society_id=$1 AND n.status=$2`
	}
	rows, err := s.pool.Query(ctx, `SELECT `+cols+`, count(*) OVER() FROM notices n WHERE `+where+
		fmt.Sprintf(` ORDER BY n.pinned DESC, COALESCE(n.published_at, n.publish_at, n.created_at) DESC, n.id LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2), append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*Notice
	total := 0
	for rows.Next() {
		n, err := scanNotice(rows, &total)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return out, total, s.fill(ctx, p.SocietyID, out)
}

func (s *Service) adminGet(ctx context.Context, soc, id uuid.UUID) (*Notice, error) {
	n, err := scanNotice(s.pool.QueryRow(ctx, `SELECT `+cols+` FROM notices n WHERE n.id=$1 AND n.society_id=$2 AND n.status <> 'archived'`, id, soc))
	if err != nil {
		return nil, err
	}
	return n, s.fill(ctx, soc, []*Notice{n})
}

func (s *Service) AdminGet(ctx context.Context, p authctx.Principal, id uuid.UUID) (*Notice, error) {
	return s.adminGet(ctx, p.SocietyID, id)
}

func (s *Service) validate(ctx context.Context, soc uuid.UUID, in *Input, existing *Notice) error {
	in.Title, in.Body = strings.TrimSpace(in.Title), strings.TrimSpace(in.Body)
	if l := len([]rune(in.Title)); l < 1 || l > 120 {
		return errValidName("title must be 1-120 characters")
	}
	if l := len([]rune(in.Body)); l < 1 || l > 4000 {
		return errValidName("text must be 1-4000 characters")
	}
	if !categories[in.Category] {
		return errValidName("invalid category")
	}
	if in.Priority == "" {
		in.Priority = "normal"
	}
	if !priorities[in.Priority] {
		return errValidName("invalid priority")
	}
	if in.Status == "" {
		in.Status = "draft"
	}
	switch in.Status {
	case "draft", "scheduled", "published":
	default:
		return errValidName("status must be draft, scheduled or published")
	}
	if in.Status == "scheduled" && (in.PublishAt == nil || !in.PublishAt.After(time.Now())) {
		return errValidName("choose a publish time in the future")
	}
	if in.ExpiresAt != nil {
		from := time.Now()
		if in.PublishAt != nil && in.Status == "scheduled" {
			from = *in.PublishAt
		}
		if !in.ExpiresAt.After(from) {
			return errValidName("the expiry must be after the publish time")
		}
	}
	if !in.AllBlocks {
		if len(in.BlockIDs) == 0 {
			return errValidName("choose at least one block, or send to everyone")
		}
		var n int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM blocks WHERE society_id=$1 AND id = ANY($2)`, soc, in.BlockIDs).Scan(&n); err != nil {
			return err
		}
		if n != len(uniq(in.BlockIDs)) {
			return errValidName("unknown block")
		}
	}
	if in.Pinned && in.Status != "draft" {
		var pinned int
		var self uuid.UUID
		if existing != nil {
			self = existing.ID
		}
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM notices WHERE society_id=$1 AND pinned AND status IN ('published','scheduled') AND id <> $2`, soc, self).Scan(&pinned); err != nil {
			return err
		}
		if pinned >= maxPinned {
			return ErrPinLimit
		}
	}
	return nil
}

func uniq(ids []uuid.UUID) []uuid.UUID {
	seen := map[uuid.UUID]bool{}
	var out []uuid.UUID
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func (s *Service) setBlocks(ctx context.Context, tx pgx.Tx, id uuid.UUID, in Input) error {
	if _, err := tx.Exec(ctx, `DELETE FROM notice_blocks WHERE notice_id=$1`, id); err != nil {
		return err
	}
	if in.AllBlocks {
		return nil
	}
	for _, b := range uniq(in.BlockIDs) {
		if _, err := tx.Exec(ctx, `INSERT INTO notice_blocks (notice_id, block_id) VALUES ($1,$2)`, id, b); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) Create(ctx context.Context, p authctx.Principal, in Input) (*Notice, error) {
	if err := s.validate(ctx, p.SocietyID, &in, nil); err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var id uuid.UUID
	var publishedAt *time.Time
	if in.Status == "published" {
		now := time.Now()
		publishedAt = &now
	}
	var publishAt *time.Time
	if in.Status == "scheduled" {
		publishAt = in.PublishAt
	}
	if err := tx.QueryRow(ctx, `INSERT INTO notices (society_id, title, body, category, priority, status, pinned, all_blocks, publish_at, published_at, expires_at, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING id`,
		p.SocietyID, in.Title, in.Body, in.Category, in.Priority, in.Status, in.Pinned, in.AllBlocks, publishAt, publishedAt, in.ExpiresAt, p.UserID).Scan(&id); err != nil {
		return nil, err
	}
	if err := s.setBlocks(ctx, tx, id, in); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	s.log(ctx, p, "notice.create", id, map[string]any{"status": in.Status})
	n, err := s.adminGet(ctx, p.SocietyID, id)
	if err == nil && in.Status == "published" {
		s.announce(ctx, p.SocietyID, n)
	}
	return n, err
}

func (s *Service) Update(ctx context.Context, p authctx.Principal, id uuid.UUID, in Input) (*Notice, error) {
	cur, err := s.adminGet(ctx, p.SocietyID, id)
	if err != nil {
		return nil, err
	}
	if cur.Status == "published" && in.Status != "published" {
		return nil, ErrBadState // a published notice stays published (archive it instead)
	}
	if in.Status == "" {
		in.Status = cur.Status
	}
	if err := s.validate(ctx, p.SocietyID, &in, cur); err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	publishing := cur.Status != "published" && in.Status == "published"
	var publishAt *time.Time
	if in.Status == "scheduled" {
		publishAt = in.PublishAt
	}
	if _, err := tx.Exec(ctx, `UPDATE notices SET title=$3, body=$4, category=$5, priority=$6, status=$7, pinned=$8, all_blocks=$9, publish_at=$10, expires_at=$11,
		published_at = CASE WHEN $12 THEN now() ELSE published_at END,
		edited_at = CASE WHEN status='published' AND NOT $12 THEN now() ELSE edited_at END
		WHERE id=$1 AND society_id=$2`, id, p.SocietyID, in.Title, in.Body, in.Category, in.Priority, in.Status, in.Pinned, in.AllBlocks, publishAt, in.ExpiresAt, publishing); err != nil {
		return nil, err
	}
	if err := s.setBlocks(ctx, tx, id, in); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	s.log(ctx, p, "notice.update", id, map[string]any{"status": in.Status})
	n, err := s.adminGet(ctx, p.SocietyID, id)
	if err == nil && publishing {
		s.announce(ctx, p.SocietyID, n)
	}
	return n, err
}

// Publish sends a draft or scheduled notice out now.
func (s *Service) Publish(ctx context.Context, p authctx.Principal, id uuid.UUID) (*Notice, error) {
	cur, err := s.adminGet(ctx, p.SocietyID, id)
	if err != nil {
		return nil, err
	}
	if cur.Status == "published" {
		return nil, ErrBadState
	}
	if cur.Pinned {
		var pinned int
		_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM notices WHERE society_id=$1 AND pinned AND status IN ('published','scheduled') AND id <> $2`, p.SocietyID, id).Scan(&pinned)
		if pinned >= maxPinned {
			return nil, ErrPinLimit
		}
	}
	tag, err := s.pool.Exec(ctx, `UPDATE notices SET status='published', published_at=now(), publish_at=NULL WHERE id=$1 AND society_id=$2 AND status IN ('draft','scheduled')`, id, p.SocietyID)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrBadState
	}
	s.log(ctx, p, "notice.publish", id, nil)
	n, err := s.adminGet(ctx, p.SocietyID, id)
	if err == nil {
		s.announce(ctx, p.SocietyID, n)
	}
	return n, err
}

func (s *Service) Archive(ctx context.Context, p authctx.Principal, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `UPDATE notices SET status='archived', pinned=false WHERE id=$1 AND society_id=$2 AND status <> 'archived'`, id, p.SocietyID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	s.log(ctx, p, "notice.archive", id, nil)
	return nil
}

func (s *Service) AddAttachment(ctx context.Context, p authctx.Principal, id uuid.UUID, name string, data []byte) (*attachment.File, error) {
	if _, err := s.adminGet(ctx, p.SocietyID, id); err != nil {
		return nil, err
	}
	f, err := s.files.Save(ctx, p.SocietyID, attachment.OwnerNotice, id, p.UserID, name, data, maxAttachments)
	if err == nil {
		s.log(ctx, p, "notice.attach", id, map[string]any{"file": f.Filename})
	}
	return f, err
}

func (s *Service) RemoveAttachment(ctx context.Context, p authctx.Principal, id, aid uuid.UUID) error {
	if _, err := s.adminGet(ctx, p.SocietyID, id); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM attachments WHERE id=$1 AND owner_type='notice' AND owner_id=$2 AND society_id=$3`, aid, id, p.SocietyID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return attachment.ErrNotFound
	}
	return nil
}

// AdminAttachment lets the office open any file of its own notices (drafts included).
func (s *Service) AdminAttachment(ctx context.Context, p authctx.Principal, id, aid uuid.UUID) ([]byte, *attachment.File, error) {
	if _, err := s.adminGet(ctx, p.SocietyID, id); err != nil {
		return nil, nil, err
	}
	return s.files.Open(ctx, p.SocietyID, attachment.OwnerNotice, id, aid)
}

type Stats struct {
	ReadFlats  int `json:"read_flats"`
	TotalFlats int `json:"total_flats"`
}

// Stats counts flats, never names: how many of the targeted flats opened the notice.
func (s *Service) Stats(ctx context.Context, p authctx.Principal, id uuid.UUID) (*Stats, error) {
	n, err := s.adminGet(ctx, p.SocietyID, id)
	if err != nil {
		return nil, err
	}
	var st Stats
	if err := s.pool.QueryRow(ctx, `SELECT count(DISTINCT unit_id) FROM notice_reads WHERE notice_id=$1 AND unit_id IS NOT NULL`, id).Scan(&st.ReadFlats); err != nil {
		return nil, err
	}
	err = s.pool.QueryRow(ctx, `SELECT count(DISTINCT m.unit_id) FROM memberships m JOIN units u ON u.id=m.unit_id
		WHERE m.society_id=$1 AND m.status='active' AND m.role_key IN ('resident_owner','resident_tenant','family_member')
		  AND ($2 OR u.block_id IN (SELECT block_id FROM notice_blocks WHERE notice_id=$3))`, p.SocietyID, n.AllBlocks, id).Scan(&st.TotalFlats)
	return &st, err
}

// ── publication ──────────────────────────────────────────────────────────────

// announce tells the targeted residents (push + inbox) and every open app of the society.
func (s *Service) announce(ctx context.Context, soc uuid.UUID, n *Notice) {
	if s.hub != nil {
		s.hub.Publish(ctx, realtime.Event{Type: "notice.published", SocietyID: soc, All: true, Data: map[string]any{"notice_id": n.ID}})
	}
	if s.note == nil {
		return
	}
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT m.user_id FROM memberships m JOIN units u ON u.id=m.unit_id
		WHERE m.society_id=$1 AND m.status='active' AND m.role_key IN ('resident_owner','resident_tenant','family_member')
		  AND ($2 OR u.block_id IN (SELECT block_id FROM notice_blocks WHERE notice_id=$3))`, soc, n.AllBlocks, n.ID)
	if err != nil {
		return
	}
	var users []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if rows.Scan(&id) == nil {
			users = append(users, id)
		}
	}
	rows.Close()
	title := "New notice"
	if n.Priority == "urgent" {
		title = "Urgent notice"
	}
	s.note.Send(ctx, notify.Notification{SocietyID: soc, Users: users, Type: "notice.new", Title: title, Body: n.Title,
		Data: map[string]any{"type": "notice", "notice_id": n.ID}, Priority: notify.PriorityNormal})
}

// Sweep publishes scheduled notices whose time has come (idempotent, safe on several instances).
func (s *Service) Sweep(ctx context.Context) (int, error) {
	rows, err := s.pool.Query(ctx, `UPDATE notices SET status='published', published_at=now() WHERE status='scheduled' AND publish_at <= now() RETURNING id, society_id`)
	if err != nil {
		return 0, err
	}
	type hit struct{ id, soc uuid.UUID }
	var hits []hit
	for rows.Next() {
		var h hit
		if rows.Scan(&h.id, &h.soc) == nil {
			hits = append(hits, h)
		}
	}
	rows.Close()
	for _, h := range hits {
		if n, err := s.adminGet(ctx, h.soc, h.id); err == nil {
			s.announce(ctx, h.soc, n)
		}
	}
	return len(hits), rows.Err()
}

func (s *Service) RunSweeper(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			heartbeat.Beat("notices")
			_, _ = s.Sweep(ctx)
		}
	}
}
