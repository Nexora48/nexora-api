package complaint

import (
	"context"
	"encoding/csv"
	"fmt"
	"github.com/Nexora48/nexora-api/internal/platform/heartbeat"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Nexora48/nexora-api/internal/attachment"
	"github.com/Nexora48/nexora-api/internal/notify"
	"github.com/Nexora48/nexora-api/internal/platform/authctx"
)

const maxExport = 10000

type Filter struct {
	Status   string
	Category string
	Priority string
	Overdue  bool
	Query    string
	Block    string // block code
	Limit    int
	Offset   int
}

func (s *Service) where(soc uuid.UUID, f Filter) (string, []any) {
	args := []any{soc}
	where := `c.society_id=$1`
	add := func(cond string, v any) {
		args = append(args, v)
		where += " AND " + strings.ReplaceAll(cond, "?", fmt.Sprintf("$%d", len(args)))
	}
	switch f.Status {
	case "":
	case "active":
		where += ` AND c.status IN ('open','assigned','in_progress')`
	default:
		add("c.status = ?", f.Status)
	}
	if f.Category != "" {
		add("c.category = ?", f.Category)
	}
	if f.Priority != "" {
		add("c.priority = ?", f.Priority)
	}
	if f.Overdue {
		where += ` AND ` + overdueSQL
	}
	if f.Block != "" {
		add("upper(b.code) = upper(?)", f.Block)
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		args = append(args, "%"+q+"%")
		n := fmt.Sprintf("$%d", len(args))
		where += " AND (c.title ILIKE " + n + " OR ('CMP-' || lpad(c.seq::text, 4, '0')) ILIKE " + n + " OR u.number ILIKE " + n + " OR ru.full_name ILIKE " + n + ")"
	}
	return where, args
}

func (s *Service) AdminList(ctx context.Context, p authctx.Principal, f Filter) ([]*Complaint, int, error) {
	where, args := s.where(p.SocietyID, f)
	rows, err := s.pool.Query(ctx, selectCols+`, count(*) OVER() `+fromJoin+`WHERE `+where+
		fmt.Sprintf(` ORDER BY c.created_at DESC, c.id LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2), append(args, f.Limit, f.Offset)...)
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

func (s *Service) adminGet(ctx context.Context, soc, id uuid.UUID) (*Complaint, error) {
	c, err := scan(s.pool.QueryRow(ctx, selectCols+fromJoin+`WHERE c.id=$1 AND c.society_id=$2`, id, soc))
	if err != nil {
		return nil, err
	}
	if err := s.loadPhotos(ctx, soc, c); err != nil {
		return nil, err
	}
	c.Events, err = s.timeline(ctx, id, true)
	return c, err
}

func (s *Service) AdminGet(ctx context.Context, p authctx.Principal, id uuid.UUID) (*Complaint, error) {
	return s.adminGet(ctx, p.SocietyID, id)
}

type Stats struct {
	Open       int            `json:"open"`
	Assigned   int            `json:"assigned"`
	InProgress int            `json:"in_progress"`
	Resolved   int            `json:"resolved"`
	Closed     int            `json:"closed"`
	Overdue    int            `json:"overdue"`
	AvgRating  float64        `json:"avg_rating"`
	Rated      int            `json:"rated"`
	ByCategory map[string]int `json:"by_category"`
}

func (s *Service) Stats(ctx context.Context, p authctx.Principal) (*Stats, error) {
	st := Stats{ByCategory: map[string]int{}}
	err := s.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE c.status='open'), count(*) FILTER (WHERE c.status='assigned'), count(*) FILTER (WHERE c.status='in_progress'),
		count(*) FILTER (WHERE c.status='resolved'), count(*) FILTER (WHERE c.status='closed'), count(*) FILTER (WHERE `+overdueSQL+`),
		COALESCE(avg(c.rating), 0)::float8, count(c.rating)
		FROM complaints c WHERE c.society_id=$1`, p.SocietyID).Scan(&st.Open, &st.Assigned, &st.InProgress, &st.Resolved, &st.Closed, &st.Overdue, &st.AvgRating, &st.Rated)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT category, count(*) FROM complaints WHERE society_id=$1 GROUP BY category`, p.SocietyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var cat string
		var n int
		if rows.Scan(&cat, &n) == nil {
			st.ByCategory[cat] = n
		}
	}
	return &st, rows.Err()
}

type OfficeMember struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	Role string    `json:"role"`
}

// OfficeMembers feeds the assignee picker.
func (s *Service) OfficeMembers(ctx context.Context, p authctx.Principal) ([]OfficeMember, error) {
	rows, err := s.pool.Query(ctx, `SELECT m.id, u.full_name, m.role_key FROM memberships m JOIN users u ON u.id=m.user_id
		WHERE m.society_id=$1 AND m.status='active' AND m.role_key IN ('society_admin','committee') ORDER BY lower(u.full_name)`, p.SocietyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OfficeMember{}
	for rows.Next() {
		var m OfficeMember
		if err := rows.Scan(&m.ID, &m.Name, &m.Role); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

type AssignInput struct {
	MembershipID *uuid.UUID
	Vendor       string
	TargetDate   *time.Time
}

func (s *Service) Assign(ctx context.Context, p authctx.Principal, id uuid.UUID, in AssignInput) (*Complaint, error) {
	c, err := s.adminGet(ctx, p.SocietyID, id)
	if err != nil {
		return nil, err
	}
	if c.Status == "closed" || c.Status == "resolved" {
		return nil, ErrInvalidState
	}
	in.Vendor = strings.TrimSpace(in.Vendor)
	if in.MembershipID == nil && in.Vendor == "" {
		return nil, invalid("choose a member of the office or enter a vendor")
	}
	if len([]rune(in.Vendor)) > 120 {
		return nil, invalid("vendor details are limited to 120 characters")
	}
	who := in.Vendor
	if in.MembershipID != nil {
		err := s.pool.QueryRow(ctx, `SELECT u.full_name FROM memberships m JOIN users u ON u.id=m.user_id WHERE m.id=$1 AND m.society_id=$2 AND m.status='active' AND m.role_key IN ('society_admin','committee')`,
			*in.MembershipID, p.SocietyID).Scan(&who)
		if err != nil {
			return nil, invalid("the assignee must be an active member of the society office")
		}
	}
	var target any
	if in.TargetDate != nil {
		if in.TargetDate.Before(time.Now().Add(-24 * time.Hour)) {
			return nil, invalid("the target date is in the past")
		}
		target = in.TargetDate.Format("2006-01-02")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE complaints SET assigned_membership_id=$3, assigned_vendor=NULLIF($4,''), target_date=$5::date, status=CASE WHEN status='open' THEN 'assigned' ELSE status END, updated_at=now()
		WHERE id=$1 AND society_id=$2`, id, p.SocietyID, in.MembershipID, in.Vendor, target); err != nil {
		return nil, err
	}
	text := "Assigned to " + who
	if in.MembershipID != nil && in.Vendor != "" {
		text += " (vendor: " + in.Vendor + ")"
	}
	if target != nil {
		text += ", target " + target.(string)
	}
	_ = s.addEvent(ctx, p.SocietyID, id, &p.MembershipID, "assigned", text, false)
	s.log(ctx, p, "complaint.assign", id, map[string]any{"to": who})
	c, err = s.adminGet(ctx, p.SocietyID, id)
	if err == nil {
		s.tell(ctx, p.SocietyID, c, true, "complaint.assigned", "Complaint assigned", c.Code+" is being handled by "+who)
	}
	return c, err
}

// allowed lists the statuses the office may move to from each state.
var allowed = map[string][]string{
	"open":        {"in_progress", "resolved"},
	"assigned":    {"in_progress", "resolved"},
	"in_progress": {"resolved"},
	"resolved":    {"closed"},
}

func (s *Service) SetStatus(ctx context.Context, p authctx.Principal, id uuid.UUID, status, note string) (*Complaint, error) {
	c, err := s.adminGet(ctx, p.SocietyID, id)
	if err != nil {
		return nil, err
	}
	note = strings.TrimSpace(note)
	if len([]rune(note)) > 300 {
		return nil, invalid("the note is limited to 300 characters")
	}
	ok := false
	for _, to := range allowed[c.Status] {
		ok = ok || to == status
	}
	if !ok {
		return nil, ErrInvalidState
	}
	tag, err := s.pool.Exec(ctx, `UPDATE complaints SET status=$3, updated_at=now(),
		resolved_at = CASE WHEN $3='resolved' THEN now() ELSE resolved_at END, closed_at = CASE WHEN $3='closed' THEN now() ELSE closed_at END
		WHERE id=$1 AND society_id=$2 AND status=$4`, id, p.SocietyID, status, c.Status)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrInvalidState
	}
	body := status
	if note != "" {
		body += ": " + note
	}
	_ = s.addEvent(ctx, p.SocietyID, id, &p.MembershipID, "status", body, false)
	s.log(ctx, p, "complaint.status", id, map[string]any{"from": c.Status, "to": status})
	c, err = s.adminGet(ctx, p.SocietyID, id)
	if err == nil {
		label := map[string]string{"in_progress": "is in progress", "resolved": "was resolved", "closed": "was closed"}[status]
		s.tell(ctx, p.SocietyID, c, true, "complaint.status", "Complaint update", c.Code+" "+label)
	}
	return c, err
}

func (s *Service) SetPriority(ctx context.Context, p authctx.Principal, id uuid.UUID, priority string) (*Complaint, error) {
	c, err := s.adminGet(ctx, p.SocietyID, id)
	if err != nil {
		return nil, err
	}
	if !priorities[priority] {
		return nil, invalid("invalid priority")
	}
	if c.Status == "closed" {
		return nil, ErrInvalidState
	}
	if _, err := s.pool.Exec(ctx, `UPDATE complaints SET priority=$3, updated_at=now() WHERE id=$1 AND society_id=$2`, id, p.SocietyID, priority); err != nil {
		return nil, err
	}
	_ = s.addEvent(ctx, p.SocietyID, id, &p.MembershipID, "priority", c.Priority+" → "+priority, false)
	s.log(ctx, p, "complaint.priority", id, map[string]any{"to": priority})
	return s.adminGet(ctx, p.SocietyID, id)
}

// OfficeComment adds a note; internal notes are never shown to residents and never notify them.
func (s *Service) OfficeComment(ctx context.Context, p authctx.Principal, id uuid.UUID, text string, internal bool) (*Complaint, error) {
	c, err := s.adminGet(ctx, p.SocietyID, id)
	if err != nil {
		return nil, err
	}
	text = strings.TrimSpace(text)
	if l := len([]rune(text)); l < 1 || l > 500 {
		return nil, invalid("write 1-500 characters")
	}
	if err := s.addEvent(ctx, p.SocietyID, id, &p.MembershipID, "comment", text, internal); err != nil {
		return nil, err
	}
	c, err = s.adminGet(ctx, p.SocietyID, id)
	if err == nil && !internal {
		s.tell(ctx, p.SocietyID, c, true, "complaint.comment", "Complaint update", c.Code+": the office replied")
	}
	return c, err
}

func (s *Service) AdminAttachment(ctx context.Context, p authctx.Principal, id, aid uuid.UUID) ([]byte, *attachment.File, error) {
	if _, err := s.adminGet(ctx, p.SocietyID, id); err != nil {
		return nil, nil, err
	}
	return s.files.Open(ctx, p.SocietyID, attachment.OwnerComplaint, id, aid)
}

func csvSafe(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}

func ts(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func (s *Service) Export(ctx context.Context, p authctx.Principal, f Filter, w io.Writer) (int, error) {
	f.Limit, f.Offset = maxExport, 0
	rows, _, err := s.AdminList(ctx, p, f)
	if err != nil {
		return 0, err
	}
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"ID", "Title", "Category", "Priority", "Status", "Overdue", "Flat", "Raised by", "Assigned to", "Vendor", "Target date", "Rating", "Created", "Resolved"})
	for _, c := range rows {
		rating := ""
		if c.Rating != nil {
			rating = fmt.Sprint(*c.Rating)
		}
		_ = cw.Write([]string{c.Code, csvSafe(c.Title), c.Category, c.Priority, c.Status, fmt.Sprint(c.Overdue), str(c.Flat), csvSafe(str(c.RaisedBy)),
			csvSafe(str(c.AssignedTo)), csvSafe(str(c.AssignedVendor)), str(c.TargetDate), rating, ts(&c.CreatedAt), ts(c.ResolvedAt)})
	}
	cw.Flush()
	s.log(ctx, p, "complaint.export", uuid.Nil, map[string]any{"rows": len(rows)})
	return len(rows), cw.Error()
}

// ── sweepers ─────────────────────────────────────────────────────────────────

// AutoClose closes complaints that stayed resolved for the whole reopen window.
func (s *Service) AutoClose(ctx context.Context) (int, error) {
	rows, err := s.pool.Query(ctx, `UPDATE complaints SET status='closed', closed_at=now(), updated_at=now()
		WHERE status='resolved' AND resolved_at < now() - $1::interval RETURNING id, society_id`, fmt.Sprintf("%d seconds", int(s.closeAfter.Seconds())))
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
		_ = s.addEvent(ctx, h.soc, h.id, nil, "auto_closed", "Closed automatically after a week without a reply", false)
		if c, err := s.adminGet(ctx, h.soc, h.id); err == nil {
			s.tell(ctx, h.soc, c, true, "", "", "")
		}
	}
	return len(hits), rows.Err()
}

// Digest tells each society's office once a day how many complaints are overdue.
func (s *Service) Digest(ctx context.Context) (int, error) {
	rows, err := s.pool.Query(ctx, `SELECT c.society_id, count(*) FROM complaints c WHERE `+overdueSQL+` GROUP BY c.society_id`)
	if err != nil {
		return 0, err
	}
	type hit struct {
		soc uuid.UUID
		n   int
	}
	var hits []hit
	for rows.Next() {
		var h hit
		if rows.Scan(&h.soc, &h.n) == nil {
			hits = append(hits, h)
		}
	}
	rows.Close()
	sent := 0
	for _, h := range hits {
		tag, err := s.pool.Exec(ctx, `UPDATE complaint_counters SET last_digest=current_date WHERE society_id=$1 AND (last_digest IS NULL OR last_digest < current_date)`, h.soc)
		if err != nil || tag.RowsAffected() == 0 {
			continue // already told today
		}
		if s.note != nil {
			word := "complaints are"
			if h.n == 1 {
				word = "complaint is"
			}
			s.note.Send(ctx, notify.Notification{SocietyID: h.soc, Users: s.note.ByRoles(ctx, h.soc, "society_admin", "committee"), Type: "complaint.overdue",
				Title: "Overdue complaints", Body: fmt.Sprintf("%d %s past the target time.", h.n, word),
				Data: map[string]any{"type": "complaint_overdue"}, Priority: notify.PriorityNormal})
		}
		sent++
	}
	return sent, nil
}

func (s *Service) RunSweeper(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			heartbeat.Beat("complaints")
			_, _ = s.AutoClose(ctx)
			_, _ = s.Digest(ctx)
		}
	}
}
