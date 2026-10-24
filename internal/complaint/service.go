// Package complaint is the helpdesk: residents raise complaints for their flat, the office triages, assigns
// and resolves them, and both sides follow one timeline.
package complaint

import (
	"context"
	"errors"
	"fmt"
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

const maxPhotos = 3

var (
	ErrNoUnit       = httpx.NewError(http.StatusConflict, "NO_UNIT", "your account is not linked to a flat")
	ErrNotFound     = httpx.NewError(http.StatusNotFound, "NOT_FOUND", "complaint not found")
	ErrNotAllowed   = httpx.NewError(http.StatusForbidden, "FORBIDDEN", "only the person who raised it or the owner/tenant of the flat can do this")
	ErrInvalidState = httpx.NewError(http.StatusConflict, "INVALID_STATE", "this complaint cannot be changed that way")
	ErrReopenWindow = httpx.NewError(http.StatusConflict, "REOPEN_WINDOW_OVER", "a complaint can only be reopened within a week of being resolved")
	ErrAlreadyRated = httpx.NewError(http.StatusConflict, "ALREADY_RATED", "you have already rated this complaint")

	categories = map[string]bool{"plumbing": true, "electrical": true, "lift": true, "parking": true, "security": true, "cleaning": true, "noise": true, "other": true}
	priorities = map[string]bool{"low": true, "medium": true, "high": true, "urgent": true}
)

func invalid(msg string) error { return httpx.NewError(http.StatusBadRequest, "VALIDATION", msg) }

type Service struct {
	pool       *pgxpool.Pool
	audit      *audit.Logger
	files      *attachment.Service
	note       *notify.Service
	hub        *realtime.Hub
	reopenFor  time.Duration
	closeAfter time.Duration
}

func NewService(pool *pgxpool.Pool, a *audit.Logger, files *attachment.Service, note *notify.Service, hub *realtime.Hub, reopenFor, closeAfter time.Duration) *Service {
	if reopenFor <= 0 {
		reopenFor = 7 * 24 * time.Hour
	}
	if closeAfter <= 0 {
		closeAfter = 7 * 24 * time.Hour
	}
	return &Service{pool: pool, audit: a, files: files, note: note, hub: hub, reopenFor: reopenFor, closeAfter: closeAfter}
}

type Event struct {
	ID          uuid.UUID `json:"id"`
	Type        string    `json:"type"` // raised, assigned, status, priority, comment, reopened, rated, photo, auto_closed
	Body        string    `json:"body"`
	Internal    bool      `json:"internal,omitempty"`
	Actor       string    `json:"actor"`
	ActorOffice bool      `json:"actor_office"`
	CreatedAt   time.Time `json:"created_at"`
}

type Complaint struct {
	ID                 uuid.UUID         `json:"id"`
	Code               string            `json:"code"`
	Title              string            `json:"title"`
	Description        string            `json:"description"`
	Category           string            `json:"category"`
	Priority           string            `json:"priority"`
	Status             string            `json:"status"`
	Visibility         string            `json:"visibility"`
	UnitID             *uuid.UUID        `json:"unit_id,omitempty"`
	Flat               *string           `json:"flat,omitempty"`
	RaisedBy           *string           `json:"raised_by,omitempty"`
	AssignedMembership *uuid.UUID        `json:"assigned_membership_id,omitempty"`
	AssignedTo         *string           `json:"assigned_to,omitempty"`
	AssignedVendor     *string           `json:"assigned_vendor,omitempty"`
	TargetDate         *string           `json:"target_date,omitempty"`
	Overdue            bool              `json:"overdue"`
	ResolvedAt         *time.Time        `json:"resolved_at,omitempty"`
	ClosedAt           *time.Time        `json:"closed_at,omitempty"`
	Rating             *int              `json:"rating,omitempty"`
	RatingComment      *string           `json:"rating_comment,omitempty"`
	CreatedAt          time.Time         `json:"created_at"`
	UpdatedAt          time.Time         `json:"updated_at"`
	CanReopen          bool              `json:"can_reopen,omitempty"`
	CanRate            bool              `json:"can_rate,omitempty"`
	Photos             []attachment.File `json:"photos"`
	Events             []Event           `json:"events,omitempty"`
}

const overdueSQL = `(c.status IN ('open','assigned','in_progress') AND c.created_at + (CASE c.priority
	WHEN 'urgent' THEN interval '24 hours' WHEN 'high' THEN interval '3 days' WHEN 'medium' THEN interval '7 days' ELSE interval '14 days' END) < now())`

const selectCols = `SELECT c.id, c.seq, c.title, c.description, c.category, c.priority, c.status, c.visibility, c.unit_id, b.code || '-' || u.number, ru.full_name,
		c.assigned_membership_id, au.full_name, c.assigned_vendor, to_char(c.target_date, 'YYYY-MM-DD'), ` + overdueSQL + `,
		c.resolved_at, c.closed_at, c.rating::int, c.rating_comment, c.created_at, c.updated_at `

const fromJoin = `FROM complaints c JOIN units u ON u.id=c.unit_id JOIN blocks b ON b.id=u.block_id
	JOIN memberships rm ON rm.id=c.raised_by_membership_id JOIN users ru ON ru.id=rm.user_id
	LEFT JOIN memberships am ON am.id=c.assigned_membership_id LEFT JOIN users au ON au.id=am.user_id `

func code(seq int) string { return fmt.Sprintf("CMP-%04d", seq) }

func scan(row pgx.Row, extra ...any) (*Complaint, error) {
	var c Complaint
	var seq int
	var unit uuid.UUID
	var flat, raised string
	dest := []any{&c.ID, &seq, &c.Title, &c.Description, &c.Category, &c.Priority, &c.Status, &c.Visibility, &unit, &flat, &raised,
		&c.AssignedMembership, &c.AssignedTo, &c.AssignedVendor, &c.TargetDate, &c.Overdue, &c.ResolvedAt, &c.ClosedAt, &c.Rating, &c.RatingComment, &c.CreatedAt, &c.UpdatedAt}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	c.Code, c.UnitID, c.Flat, c.RaisedBy = code(seq), &unit, &flat, &raised
	c.Photos = []attachment.File{}
	return &c, nil
}

func (s *Service) log(ctx context.Context, p authctx.Principal, action string, id uuid.UUID, meta map[string]any) {
	s.audit.Log(ctx, audit.Entry{ActorID: &p.UserID, SocietyID: &p.SocietyID, Action: action, Entity: "complaint", EntityID: id.String(), Meta: meta})
}

func isOffice(role string) bool { return role == "society_admin" || role == "committee" }

// ── events, photos, notifications ────────────────────────────────────────────

func (s *Service) addEvent(ctx context.Context, soc, complaint uuid.UUID, actor *uuid.UUID, typ, body string, internal bool) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO complaint_events (complaint_id, society_id, actor_membership_id, type, body, internal) VALUES ($1,$2,$3,$4,$5,$6)`, complaint, soc, actor, typ, body, internal)
	if err == nil {
		_, _ = s.pool.Exec(ctx, `UPDATE complaints SET updated_at=now() WHERE id=$1`, complaint)
	}
	return err
}

// timeline returns the events; residents never get internal notes and see "Society office" instead of names.
func (s *Service) timeline(ctx context.Context, id uuid.UUID, office bool) ([]Event, error) {
	q := `SELECT e.id, e.type, e.body, e.internal, COALESCE(u.full_name, 'System'), COALESCE(m.role_key IN ('society_admin','committee'), false), e.created_at
		FROM complaint_events e LEFT JOIN memberships m ON m.id=e.actor_membership_id LEFT JOIN users u ON u.id=m.user_id WHERE e.complaint_id=$1`
	if !office {
		q += ` AND NOT e.internal`
	}
	rows, err := s.pool.Query(ctx, q+` ORDER BY e.created_at, e.id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.Type, &e.Body, &e.Internal, &e.Actor, &e.ActorOffice, &e.CreatedAt); err != nil {
			return nil, err
		}
		if !office && e.ActorOffice {
			e.Actor = "Society office"
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Service) tell(ctx context.Context, soc uuid.UUID, c *Complaint, toFlat bool, typ, title, body string) {
	if s.hub != nil {
		ev := realtime.Event{Type: "complaint.updated", SocietyID: soc, Roles: []string{"society_admin", "committee"}, Data: map[string]any{"complaint_id": c.ID, "status": c.Status}}
		if c.UnitID != nil {
			ev.Unit = c.UnitID
		}
		s.hub.Publish(ctx, ev)
	}
	if s.note == nil || typ == "" {
		return
	}
	var users []uuid.UUID
	if toFlat && c.UnitID != nil {
		users = s.note.UnitMembers(ctx, soc, *c.UnitID)
	} else {
		users = s.note.ByRoles(ctx, soc, "society_admin", "committee")
	}
	s.note.Send(ctx, notify.Notification{SocietyID: soc, Users: users, Type: typ, Title: title, Body: body,
		Data: map[string]any{"type": "complaint", "complaint_id": c.ID}, Priority: notify.PriorityNormal})
}

func (s *Service) loadPhotos(ctx context.Context, soc uuid.UUID, c *Complaint) error {
	f, err := s.files.List(ctx, soc, attachment.OwnerComplaint, c.ID)
	c.Photos = f
	return err
}

// ── residents ────────────────────────────────────────────────────────────────

type member struct {
	unit uuid.UUID
	role string
	id   uuid.UUID
}

func (s *Service) member(ctx context.Context, p authctx.Principal) (*member, error) {
	var u *uuid.UUID
	if err := s.pool.QueryRow(ctx, `SELECT unit_id FROM memberships WHERE id=$1 AND user_id=$2 AND society_id=$3 AND status='active'`, p.MembershipID, p.UserID, p.SocietyID).Scan(&u); err != nil || u == nil {
		return nil, ErrNoUnit
	}
	return &member{unit: *u, role: p.Role, id: p.MembershipID}, nil
}

type Input struct {
	Title       string
	Description string
	Category    string
	Priority    string
	Common      bool
}

func (s *Service) Raise(ctx context.Context, p authctx.Principal, in Input) (*Complaint, error) {
	m, err := s.member(ctx, p)
	if err != nil {
		return nil, err
	}
	in.Title, in.Description = strings.TrimSpace(in.Title), strings.TrimSpace(in.Description)
	if l := len([]rune(in.Title)); l < 3 || l > 100 {
		return nil, invalid("title must be 3-100 characters")
	}
	if l := len([]rune(in.Description)); l < 5 || l > 1000 {
		return nil, invalid("describe the problem in 5-1000 characters")
	}
	if !categories[in.Category] {
		return nil, invalid("invalid category")
	}
	if in.Priority == "" {
		in.Priority = "medium"
	}
	if !priorities[in.Priority] {
		return nil, invalid("invalid priority")
	}
	vis := "flat"
	if in.Common {
		vis = "common"
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var seq int
	if err := tx.QueryRow(ctx, `INSERT INTO complaint_counters (society_id, last) VALUES ($1,1) ON CONFLICT (society_id) DO UPDATE SET last=complaint_counters.last+1 RETURNING last`, p.SocietyID).Scan(&seq); err != nil {
		return nil, err
	}
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `INSERT INTO complaints (society_id, seq, unit_id, raised_by_membership_id, title, description, category, priority, visibility) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`,
		p.SocietyID, seq, m.unit, p.MembershipID, in.Title, in.Description, in.Category, in.Priority, vis).Scan(&id); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO complaint_events (complaint_id, society_id, actor_membership_id, type, body) VALUES ($1,$2,$3,'raised','')`, id, p.SocietyID, p.MembershipID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	s.log(ctx, p, "complaint.raise", id, map[string]any{"category": in.Category, "priority": in.Priority})
	c, err := s.mine(ctx, p, id)
	if err == nil {
		s.tell(ctx, p.SocietyID, c, false, "complaint.new", "New complaint", code(seq)+": "+in.Title)
	}
	return c, err
}

// ListMine is my flat's complaints; group is "open", "resolved" or "" for all.
func (s *Service) ListMine(ctx context.Context, p authctx.Principal, group string, limit, offset int) ([]*Complaint, int, error) {
	m, err := s.member(ctx, p)
	if err != nil {
		return nil, 0, err
	}
	where := `c.society_id=$1 AND c.unit_id=$2`
	switch group {
	case "open":
		where += ` AND c.status IN ('open','assigned','in_progress')`
	case "resolved":
		where += ` AND c.status IN ('resolved','closed')`
	}
	rows, err := s.pool.Query(ctx, selectCols+`, count(*) OVER() `+fromJoin+`WHERE `+where+` ORDER BY c.created_at DESC, c.id LIMIT $3 OFFSET $4`, p.SocietyID, m.unit, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*Complaint
	total := 0
	for rows.Next() {
		c, err := scan(rows, &total)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, c)
	}
	return out, total, rows.Err()
}

// ListCommon is the public board of common-area complaints (no flat, no names, no photos).
func (s *Service) ListCommon(ctx context.Context, p authctx.Principal, limit, offset int) ([]*Complaint, int, error) {
	rows, err := s.pool.Query(ctx, selectCols+`, count(*) OVER() `+fromJoin+`WHERE c.society_id=$1 AND c.visibility='common' ORDER BY c.created_at DESC, c.id LIMIT $2 OFFSET $3`, p.SocietyID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*Complaint
	total := 0
	for rows.Next() {
		c, err := scan(rows, &total)
		if err != nil {
			return nil, 0, err
		}
		c.Flat, c.RaisedBy, c.UnitID, c.Description, c.RatingComment = nil, nil, nil, "", nil
		c.AssignedMembership, c.AssignedTo, c.AssignedVendor, c.Rating = nil, nil, nil, nil
		out = append(out, c)
	}
	return out, total, rows.Err()
}

func (s *Service) mine(ctx context.Context, p authctx.Principal, id uuid.UUID) (*Complaint, error) {
	m, err := s.member(ctx, p)
	if err != nil {
		return nil, err
	}
	c, err := scan(s.pool.QueryRow(ctx, selectCols+fromJoin+`WHERE c.id=$1 AND c.society_id=$2 AND c.unit_id=$3`, id, p.SocietyID, m.unit))
	if err != nil {
		return nil, err
	}
	if err := s.loadPhotos(ctx, p.SocietyID, c); err != nil {
		return nil, err
	}
	if c.Events, err = s.timeline(ctx, id, false); err != nil {
		return nil, err
	}
	c.CanReopen = c.Status == "resolved" && c.ResolvedAt != nil && time.Since(*c.ResolvedAt) <= s.reopenFor && s.mayAct(ctx, p, m, id)
	c.CanRate = (c.Status == "resolved" || c.Status == "closed") && c.Rating == nil && s.mayAct(ctx, p, m, id)
	return c, nil
}

func (s *Service) GetMine(ctx context.Context, p authctx.Principal, id uuid.UUID) (*Complaint, error) {
	return s.mine(ctx, p, id)
}

// mayAct: the person who raised it, or the owner/tenant of the flat (family members only watch).
func (s *Service) mayAct(ctx context.Context, p authctx.Principal, m *member, id uuid.UUID) bool {
	if m.role == "resident_owner" || m.role == "resident_tenant" {
		return true
	}
	var raiser uuid.UUID
	return s.pool.QueryRow(ctx, `SELECT raised_by_membership_id FROM complaints WHERE id=$1 AND society_id=$2`, id, p.SocietyID).Scan(&raiser) == nil && raiser == p.MembershipID
}

func (s *Service) actor(ctx context.Context, p authctx.Principal, id uuid.UUID) (*member, *Complaint, error) {
	c, err := s.mine(ctx, p, id)
	if err != nil {
		return nil, nil, err
	}
	m, _ := s.member(ctx, p)
	if !s.mayAct(ctx, p, m, id) {
		return nil, nil, ErrNotAllowed
	}
	return m, c, nil
}

func (s *Service) AddPhoto(ctx context.Context, p authctx.Principal, id uuid.UUID, name string, data []byte) (*attachment.File, error) {
	_, c, err := s.actor(ctx, p, id)
	if err != nil {
		return nil, err
	}
	if c.Status == "closed" {
		return nil, ErrInvalidState
	}
	f, err := s.files.Save(ctx, p.SocietyID, attachment.OwnerComplaint, id, p.UserID, name, data, maxPhotos)
	if err != nil {
		return nil, err
	}
	_ = s.addEvent(ctx, p.SocietyID, id, &p.MembershipID, "photo", f.ID.String(), false)
	s.log(ctx, p, "complaint.photo", id, nil)
	return f, nil
}

func (s *Service) Comment(ctx context.Context, p authctx.Principal, id uuid.UUID, text string) (*Complaint, error) {
	_, c, err := s.actor(ctx, p, id)
	if err != nil {
		return nil, err
	}
	text = strings.TrimSpace(text)
	if l := len([]rune(text)); l < 1 || l > 500 {
		return nil, invalid("write 1-500 characters")
	}
	if c.Status == "closed" {
		return nil, ErrInvalidState
	}
	if err := s.addEvent(ctx, p.SocietyID, id, &p.MembershipID, "comment", text, false); err != nil {
		return nil, err
	}
	c, err = s.mine(ctx, p, id)
	if err == nil {
		s.tell(ctx, p.SocietyID, c, false, "complaint.comment", "Complaint update", code(seqOf(c))+": new comment from the resident")
	}
	return c, err
}

func seqOf(c *Complaint) int {
	var n int
	fmt.Sscanf(strings.TrimPrefix(c.Code, "CMP-"), "%d", &n)
	return n
}

func (s *Service) Reopen(ctx context.Context, p authctx.Principal, id uuid.UUID, reason string) (*Complaint, error) {
	_, c, err := s.actor(ctx, p, id)
	if err != nil {
		return nil, err
	}
	reason = strings.TrimSpace(reason)
	if l := len([]rune(reason)); l < 3 || l > 300 {
		return nil, invalid("tell the office why it is not fixed (3-300 characters)")
	}
	if c.Status != "resolved" {
		return nil, ErrInvalidState
	}
	if c.ResolvedAt == nil || time.Since(*c.ResolvedAt) > s.reopenFor {
		return nil, ErrReopenWindow
	}
	tag, err := s.pool.Exec(ctx, `UPDATE complaints SET status='open', resolved_at=NULL, updated_at=now() WHERE id=$1 AND society_id=$2 AND status='resolved'`, id, p.SocietyID)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrInvalidState
	}
	_ = s.addEvent(ctx, p.SocietyID, id, &p.MembershipID, "reopened", reason, false)
	s.log(ctx, p, "complaint.reopen", id, nil)
	c, err = s.mine(ctx, p, id)
	if err == nil {
		s.tell(ctx, p.SocietyID, c, false, "complaint.reopened", "Complaint reopened", c.Code+": "+c.Title)
	}
	return c, err
}

func (s *Service) Rate(ctx context.Context, p authctx.Principal, id uuid.UUID, rating int, comment string) (*Complaint, error) {
	_, c, err := s.actor(ctx, p, id)
	if err != nil {
		return nil, err
	}
	if rating < 1 || rating > 5 {
		return nil, invalid("rating must be 1 to 5")
	}
	comment = strings.TrimSpace(comment)
	if len([]rune(comment)) > 300 {
		return nil, invalid("comment is limited to 300 characters")
	}
	if c.Status != "resolved" && c.Status != "closed" {
		return nil, ErrInvalidState
	}
	tag, err := s.pool.Exec(ctx, `UPDATE complaints SET rating=$3, rating_comment=NULLIF($4,'') WHERE id=$1 AND society_id=$2 AND rating IS NULL`, id, p.SocietyID, rating, comment)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrAlreadyRated
	}
	_ = s.addEvent(ctx, p.SocietyID, id, &p.MembershipID, "rated", fmt.Sprintf("%d/5 %s", rating, comment), false)
	return s.mine(ctx, p, id)
}

func (s *Service) Attachment(ctx context.Context, p authctx.Principal, id, aid uuid.UUID) ([]byte, *attachment.File, error) {
	if _, err := s.mine(ctx, p, id); err != nil {
		return nil, nil, err
	}
	return s.files.Open(ctx, p.SocietyID, attachment.OwnerComplaint, id, aid)
}
