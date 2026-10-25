// Package poll runs society polls: one vote per flat, results shown after voting or close, anonymous by default.
package poll

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"github.com/Nexora48/nexora-api/internal/platform/heartbeat"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nexora48/nexora-api/internal/audit"
	"github.com/Nexora48/nexora-api/internal/notify"
	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
	"github.com/Nexora48/nexora-api/internal/realtime"
)

var (
	ErrNotFound   = httpx.NewError(http.StatusNotFound, "NOT_FOUND", "poll not found")
	ErrNoUnit     = httpx.NewError(http.StatusConflict, "NO_UNIT", "your account is not linked to a flat")
	ErrClosed     = httpx.NewError(http.StatusConflict, "POLL_CLOSED", "this poll is closed")
	ErrHasVotes   = httpx.NewError(http.StatusConflict, "HAS_VOTES", "a poll cannot be edited after the first vote; close it and start a new one")
	ErrBadOptions = httpx.NewError(http.StatusBadRequest, "VALIDATION", "choose a valid option")
)

func invalid(m string) error { return httpx.NewError(http.StatusBadRequest, "VALIDATION", m) }

type Service struct {
	pool  *pgxpool.Pool
	audit *audit.Logger
	note  *notify.Service
	hub   *realtime.Hub
}

func NewService(pool *pgxpool.Pool, a *audit.Logger, note *notify.Service, hub *realtime.Hub) *Service {
	return &Service{pool: pool, audit: a, note: note, hub: hub}
}

type Option struct {
	ID       uuid.UUID `json:"id"`
	Label    string    `json:"label"`
	Votes    *int      `json:"votes,omitempty"`   // flats; hidden until results are visible
	Percent  *float64  `json:"percent,omitempty"` // of the flats that voted
	Mine     bool      `json:"mine,omitempty"`
	Position int       `json:"position"`
}

type Voter struct {
	Flat    string   `json:"flat"`
	Options []string `json:"options"`
}

type Poll struct {
	ID             uuid.UUID   `json:"id"`
	Question       string      `json:"question"`
	Multi          bool        `json:"multi"`
	Anonymous      bool        `json:"anonymous"`
	Status         string      `json:"status"` // open | closed
	AllBlocks      bool        `json:"all_blocks"`
	BlockIDs       []uuid.UUID `json:"block_ids"`
	BlockCodes     []string    `json:"block_codes"`
	ClosesAt       *time.Time  `json:"closes_at,omitempty"`
	ClosedAt       *time.Time  `json:"closed_at,omitempty"`
	CreatedAt      time.Time   `json:"created_at"`
	Options        []Option    `json:"options"`
	Voted          bool        `json:"voted"`
	CanVote        bool        `json:"can_vote"`
	ResultsVisible bool        `json:"results_visible"`
	VotedFlats     int         `json:"voted_flats"`
	EligibleFlats  int         `json:"eligible_flats"`
	Voters         []Voter     `json:"voters,omitempty"` // office, non-anonymous polls only
}

const effStatus = `(CASE WHEN p.status='open' AND p.closes_at IS NOT NULL AND p.closes_at <= now() THEN 'closed' ELSE p.status END)`

const cols = `p.id, p.question, p.multi, p.anonymous, ` + effStatus + `, p.all_blocks, p.closes_at, p.closed_at, p.created_at`

func scanPoll(row pgx.Row) (*Poll, error) {
	var p Poll
	if err := row.Scan(&p.ID, &p.Question, &p.Multi, &p.Anonymous, &p.Status, &p.AllBlocks, &p.ClosesAt, &p.ClosedAt, &p.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	p.BlockIDs, p.BlockCodes, p.Options = []uuid.UUID{}, []string{}, []Option{}
	return &p, nil
}

func (s *Service) log(ctx context.Context, p authctx.Principal, action string, id uuid.UUID, meta map[string]any) {
	s.audit.Log(ctx, audit.Entry{ActorID: &p.UserID, SocietyID: &p.SocietyID, Action: action, Entity: "poll", EntityID: id.String(), Meta: meta})
}

func isOffice(role string) bool { return role == "society_admin" || role == "committee" }

type who struct {
	unit   *uuid.UUID
	office bool
}

func (s *Service) who(ctx context.Context, p authctx.Principal) who {
	var u *uuid.UUID
	_ = s.pool.QueryRow(ctx, `SELECT unit_id FROM memberships WHERE id=$1 AND user_id=$2 AND society_id=$3 AND status='active'`, p.MembershipID, p.UserID, p.SocietyID).Scan(&u)
	return who{unit: u, office: isOffice(p.Role)}
}

// audience: everyone, or a flat whose block was chosen. Office (and people without a flat) see all.
const audience = `(p.all_blocks OR $2::uuid IS NULL OR EXISTS (SELECT 1 FROM poll_blocks pb JOIN units u ON u.block_id=pb.block_id WHERE pb.poll_id=p.id AND u.id=$2::uuid))`

// hydrate fills options, blocks, turnout and what this person may see.
func (s *Service) hydrate(ctx context.Context, soc uuid.UUID, w who, role string, polls []*Poll) error {
	for _, pl := range polls {
		// blocks
		rows, err := s.pool.Query(ctx, `SELECT b.id, b.code FROM poll_blocks pb JOIN blocks b ON b.id=pb.block_id WHERE pb.poll_id=$1 ORDER BY b.code`, pl.ID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id uuid.UUID
			var c string
			if rows.Scan(&id, &c) == nil {
				pl.BlockIDs, pl.BlockCodes = append(pl.BlockIDs, id), append(pl.BlockCodes, c)
			}
		}
		rows.Close()

		// my vote, turnout
		mine := map[uuid.UUID]bool{}
		if w.unit != nil {
			r2, err := s.pool.Query(ctx, `SELECT option_id FROM poll_votes WHERE poll_id=$1 AND unit_id=$2`, pl.ID, *w.unit)
			if err != nil {
				return err
			}
			for r2.Next() {
				var id uuid.UUID
				if r2.Scan(&id) == nil {
					mine[id] = true
				}
			}
			r2.Close()
		}
		pl.Voted = len(mine) > 0
		if err := s.pool.QueryRow(ctx, `SELECT count(DISTINCT unit_id) FROM poll_votes WHERE poll_id=$1`, pl.ID).Scan(&pl.VotedFlats); err != nil {
			return err
		}
		if err := s.pool.QueryRow(ctx, `SELECT count(DISTINCT m.unit_id) FROM memberships m JOIN units u ON u.id=m.unit_id
			WHERE m.society_id=$1 AND m.status='active' AND m.role_key IN ('resident_owner','resident_tenant')
			  AND ($2 OR u.block_id IN (SELECT block_id FROM poll_blocks WHERE poll_id=$3))`, soc, pl.AllBlocks, pl.ID).Scan(&pl.EligibleFlats); err != nil {
			return err
		}
		pl.ResultsVisible = w.office || pl.Voted || pl.Status == "closed"
		pl.CanVote = pl.Status == "open" && w.unit != nil && (role == "resident_owner" || role == "resident_tenant")

		// options with counts when visible
		r3, err := s.pool.Query(ctx, `SELECT o.id, o.label, o.position, (SELECT count(DISTINCT v.unit_id) FROM poll_votes v WHERE v.option_id=o.id) FROM poll_options o WHERE o.poll_id=$1 ORDER BY o.position`, pl.ID)
		if err != nil {
			return err
		}
		for r3.Next() {
			var o Option
			var n int
			if r3.Scan(&o.ID, &o.Label, &o.Position, &n) != nil {
				continue
			}
			o.Mine = mine[o.ID]
			if pl.ResultsVisible {
				v := n
				o.Votes = &v
				if pl.VotedFlats > 0 {
					pc := float64(n) * 100 / float64(pl.VotedFlats)
					o.Percent = &pc
				} else {
					z := 0.0
					o.Percent = &z
				}
			}
			pl.Options = append(pl.Options, o)
		}
		r3.Close()
	}
	return nil
}

// ── residents ────────────────────────────────────────────────────────────────

func (s *Service) List(ctx context.Context, p authctx.Principal) ([]*Poll, error) {
	w := s.who(ctx, p)
	rows, err := s.pool.Query(ctx, `SELECT `+cols+` FROM polls p WHERE p.society_id=$1 AND p.status <> 'archived' AND `+audience+`
		ORDER BY (`+effStatus+`='open') DESC, p.created_at DESC, p.id LIMIT 50`, p.SocietyID, w.unit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Poll
	for rows.Next() {
		pl, err := scanPoll(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, pl)
	}
	rows.Close()
	return out, s.hydrate(ctx, p.SocietyID, w, p.Role, out)
}

func (s *Service) Get(ctx context.Context, p authctx.Principal, id uuid.UUID) (*Poll, error) {
	w := s.who(ctx, p)
	pl, err := scanPoll(s.pool.QueryRow(ctx, `SELECT `+cols+` FROM polls p WHERE p.id=$3 AND p.society_id=$1 AND p.status <> 'archived' AND `+audience, p.SocietyID, w.unit, id))
	if err != nil {
		return nil, err
	}
	return pl, s.hydrate(ctx, p.SocietyID, w, p.Role, []*Poll{pl})
}

// Vote records the flat's vote (replacing an earlier one) while the poll is open.
func (s *Service) Vote(ctx context.Context, p authctx.Principal, id uuid.UUID, optionIDs []uuid.UUID) (*Poll, error) {
	pl, err := s.Get(ctx, p, id)
	if err != nil {
		return nil, err
	}
	w := s.who(ctx, p)
	if w.unit == nil {
		return nil, ErrNoUnit
	}
	if pl.Status != "open" {
		return nil, ErrClosed
	}
	ids := map[uuid.UUID]bool{}
	for _, o := range optionIDs {
		ids[o] = true
	}
	if len(ids) == 0 || (!pl.Multi && len(ids) != 1) {
		return nil, ErrBadOptions
	}
	valid := map[uuid.UUID]bool{}
	for _, o := range pl.Options {
		valid[o.ID] = true
	}
	for o := range ids {
		if !valid[o] {
			return nil, ErrBadOptions
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `DELETE FROM poll_votes WHERE poll_id=$1 AND unit_id=$2`, id, *w.unit); err != nil {
		return nil, err
	}
	for o := range ids {
		if _, err := tx.Exec(ctx, `INSERT INTO poll_votes (poll_id, unit_id, option_id, user_id) VALUES ($1,$2,$3,$4)`, id, *w.unit, o, p.UserID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	s.log(ctx, p, "poll.vote", id, nil) // who voted for what is not logged, only that a flat voted
	if s.hub != nil {
		s.hub.Publish(ctx, realtime.Event{Type: "poll.updated", SocietyID: p.SocietyID, All: true, Data: map[string]any{"poll_id": id}})
	}
	return s.Get(ctx, p, id)
}

// ── office ───────────────────────────────────────────────────────────────────

type Input struct {
	Question  string
	Options   []string
	Multi     bool
	Anonymous bool
	AllBlocks bool
	BlockIDs  []uuid.UUID
	ClosesAt  *time.Time
}

func (s *Service) validate(ctx context.Context, soc uuid.UUID, in *Input) error {
	in.Question = strings.TrimSpace(in.Question)
	if l := len([]rune(in.Question)); l < 3 || l > 200 {
		return invalid("the question must be 3-200 characters")
	}
	var opts []string
	seen := map[string]bool{}
	for _, o := range in.Options {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		if len([]rune(o)) > 80 {
			return invalid("an option is limited to 80 characters")
		}
		if seen[strings.ToLower(o)] {
			return invalid("options must be different")
		}
		seen[strings.ToLower(o)] = true
		opts = append(opts, o)
	}
	if len(opts) < 2 || len(opts) > 8 {
		return invalid("give 2 to 8 options")
	}
	in.Options = opts
	if in.ClosesAt != nil && (!in.ClosesAt.After(time.Now()) || in.ClosesAt.After(time.Now().AddDate(0, 0, 90))) {
		return invalid("the closing time must be in the future and within 90 days")
	}
	if !in.AllBlocks {
		if len(in.BlockIDs) == 0 {
			return invalid("choose at least one block, or open the poll to everyone")
		}
		uniq := map[uuid.UUID]bool{}
		for _, b := range in.BlockIDs {
			uniq[b] = true
		}
		var n int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM blocks WHERE society_id=$1 AND id = ANY($2)`, soc, in.BlockIDs).Scan(&n); err != nil {
			return err
		}
		if n != len(uniq) {
			return invalid("unknown block")
		}
	}
	return nil
}

func (s *Service) write(ctx context.Context, tx pgx.Tx, id uuid.UUID, in Input) error {
	if _, err := tx.Exec(ctx, `DELETE FROM poll_options WHERE poll_id=$1`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM poll_blocks WHERE poll_id=$1`, id); err != nil {
		return err
	}
	for i, o := range in.Options {
		if _, err := tx.Exec(ctx, `INSERT INTO poll_options (poll_id, label, position) VALUES ($1,$2,$3)`, id, o, i); err != nil {
			return err
		}
	}
	if !in.AllBlocks {
		seen := map[uuid.UUID]bool{}
		for _, b := range in.BlockIDs {
			if seen[b] {
				continue
			}
			seen[b] = true
			if _, err := tx.Exec(ctx, `INSERT INTO poll_blocks (poll_id, block_id) VALUES ($1,$2)`, id, b); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) Create(ctx context.Context, p authctx.Principal, in Input) (*Poll, error) {
	if err := s.validate(ctx, p.SocietyID, &in); err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `INSERT INTO polls (society_id, question, multi, anonymous, all_blocks, closes_at, created_by) VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		p.SocietyID, in.Question, in.Multi, in.Anonymous, in.AllBlocks, in.ClosesAt, p.UserID).Scan(&id); err != nil {
		return nil, err
	}
	if err := s.write(ctx, tx, id, in); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	s.log(ctx, p, "poll.create", id, nil)
	s.announce(ctx, p.SocietyID, id, in.Question, in.AllBlocks)
	return s.AdminGet(ctx, p, id)
}

// announce pushes "new poll" to the people who can answer (and see) it.
func (s *Service) announce(ctx context.Context, soc, id uuid.UUID, question string, all bool) {
	if s.hub != nil {
		s.hub.Publish(ctx, realtime.Event{Type: "poll.updated", SocietyID: soc, All: true, Data: map[string]any{"poll_id": id}})
	}
	if s.note == nil {
		return
	}
	s.note.Send(ctx, notify.Notification{SocietyID: soc, Users: s.audienceUsers(ctx, soc, id, all, false), Type: "poll.new", Title: "New poll", Body: question,
		Data: map[string]any{"type": "poll", "poll_id": id}, Priority: notify.PriorityNormal})
}

// audienceUsers: owners and tenants (the ones who vote) of the audience; notVoted limits it to flats without a vote.
func (s *Service) audienceUsers(ctx context.Context, soc, id uuid.UUID, all, notVoted bool) []uuid.UUID {
	q := `SELECT DISTINCT m.user_id FROM memberships m JOIN units u ON u.id=m.unit_id
		WHERE m.society_id=$1 AND m.status='active' AND m.role_key IN ('resident_owner','resident_tenant')
		  AND ($2 OR u.block_id IN (SELECT block_id FROM poll_blocks WHERE poll_id=$3))`
	if notVoted {
		q += ` AND NOT EXISTS (SELECT 1 FROM poll_votes v WHERE v.poll_id=$3 AND v.unit_id=m.unit_id)`
	}
	rows, err := s.pool.Query(ctx, q, soc, all, id)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var u uuid.UUID
		if rows.Scan(&u) == nil {
			out = append(out, u)
		}
	}
	return out
}

func (s *Service) AdminList(ctx context.Context, p authctx.Principal) ([]*Poll, error) {
	w := who{office: true}
	rows, err := s.pool.Query(ctx, `SELECT `+cols+` FROM polls p WHERE p.society_id=$1 AND p.status <> 'archived' AND $2::uuid IS NULL
		ORDER BY (`+effStatus+`='open') DESC, p.created_at DESC, p.id LIMIT 100`, p.SocietyID, nil)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Poll
	for rows.Next() {
		pl, err := scanPoll(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, pl)
	}
	rows.Close()
	return out, s.hydrate(ctx, p.SocietyID, w, p.Role, out)
}

// AdminGet adds the per-flat list for polls that are not anonymous.
func (s *Service) AdminGet(ctx context.Context, p authctx.Principal, id uuid.UUID) (*Poll, error) {
	pl, err := scanPoll(s.pool.QueryRow(ctx, `SELECT `+cols+` FROM polls p WHERE p.id=$1 AND p.society_id=$2 AND p.status <> 'archived'`, id, p.SocietyID))
	if err != nil {
		return nil, err
	}
	if err := s.hydrate(ctx, p.SocietyID, who{office: true}, p.Role, []*Poll{pl}); err != nil {
		return nil, err
	}
	if !pl.Anonymous {
		rows, err := s.pool.Query(ctx, `SELECT b.code || '-' || u.number, array_agg(o.label ORDER BY o.position) FROM poll_votes v
			JOIN units u ON u.id=v.unit_id JOIN blocks b ON b.id=u.block_id JOIN poll_options o ON o.id=v.option_id WHERE v.poll_id=$1 GROUP BY 1 ORDER BY 1`, id)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var v Voter
			if rows.Scan(&v.Flat, &v.Options) == nil {
				pl.Voters = append(pl.Voters, v)
			}
		}
	}
	return pl, nil
}

func (s *Service) Update(ctx context.Context, p authctx.Principal, id uuid.UUID, in Input) (*Poll, error) {
	cur, err := s.AdminGet(ctx, p, id)
	if err != nil {
		return nil, err
	}
	if cur.Status != "open" {
		return nil, ErrClosed
	}
	if cur.VotedFlats > 0 {
		return nil, ErrHasVotes
	}
	if err := s.validate(ctx, p.SocietyID, &in); err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `UPDATE polls SET question=$3, multi=$4, anonymous=$5, all_blocks=$6, closes_at=$7, reminder_sent=false WHERE id=$1 AND society_id=$2`,
		id, p.SocietyID, in.Question, in.Multi, in.Anonymous, in.AllBlocks, in.ClosesAt); err != nil {
		return nil, err
	}
	if err := s.write(ctx, tx, id, in); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	s.log(ctx, p, "poll.update", id, nil)
	return s.AdminGet(ctx, p, id)
}

func (s *Service) Close(ctx context.Context, p authctx.Principal, id uuid.UUID) (*Poll, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE polls SET status='closed', closed_at=now() WHERE id=$1 AND society_id=$2 AND status='open'`, id, p.SocietyID)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		if _, err := s.AdminGet(ctx, p, id); err != nil {
			return nil, err
		}
		return nil, ErrClosed
	}
	s.log(ctx, p, "poll.close", id, nil)
	if s.hub != nil {
		s.hub.Publish(ctx, realtime.Event{Type: "poll.updated", SocietyID: p.SocietyID, All: true, Data: map[string]any{"poll_id": id}})
	}
	return s.AdminGet(ctx, p, id)
}

func (s *Service) Archive(ctx context.Context, p authctx.Principal, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `UPDATE polls SET status='archived' WHERE id=$1 AND society_id=$2 AND status <> 'archived'`, id, p.SocietyID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	s.log(ctx, p, "poll.archive", id, nil)
	return nil
}

func csvSafe(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}

// Export writes option totals, and a per-flat table when the poll is not anonymous.
func (s *Service) Export(ctx context.Context, p authctx.Principal, id uuid.UUID, w io.Writer) error {
	pl, err := s.AdminGet(ctx, p, id)
	if err != nil {
		return err
	}
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"Poll", csvSafe(pl.Question)})
	_ = cw.Write([]string{"Status", pl.Status})
	_ = cw.Write([]string{"Turnout", fmt.Sprintf("%d of %d flats", pl.VotedFlats, pl.EligibleFlats)})
	_ = cw.Write(nil)
	_ = cw.Write([]string{"Option", "Flats", "Percent"})
	for _, o := range pl.Options {
		v, pc := 0, 0.0
		if o.Votes != nil {
			v = *o.Votes
		}
		if o.Percent != nil {
			pc = *o.Percent
		}
		_ = cw.Write([]string{csvSafe(o.Label), fmt.Sprint(v), fmt.Sprintf("%.1f", pc)})
	}
	if !pl.Anonymous {
		_ = cw.Write(nil)
		_ = cw.Write([]string{"Flat", "Voted for"})
		for _, v := range pl.Voters {
			_ = cw.Write([]string{v.Flat, csvSafe(strings.Join(v.Options, "; "))})
		}
	}
	cw.Flush()
	s.log(ctx, p, "poll.export", id, nil)
	return cw.Error()
}

// ── sweepers ─────────────────────────────────────────────────────────────────

// Sweep closes polls whose time is up and reminds flats that have not voted a day before they close.
func (s *Service) Sweep(ctx context.Context) (closed, reminded int, err error) {
	tag, err := s.pool.Exec(ctx, `UPDATE polls SET status='closed', closed_at=COALESCE(closes_at, now()) WHERE status='open' AND closes_at IS NOT NULL AND closes_at <= now()`)
	if err != nil {
		return 0, 0, err
	}
	closed = int(tag.RowsAffected())
	rows, err := s.pool.Query(ctx, `UPDATE polls SET reminder_sent=true WHERE status='open' AND NOT reminder_sent AND closes_at IS NOT NULL AND closes_at > now() AND closes_at <= now() + interval '24 hours'
		RETURNING id, society_id, question, all_blocks`)
	if err != nil {
		return closed, 0, err
	}
	type hit struct {
		id, soc uuid.UUID
		q       string
		all     bool
	}
	var hits []hit
	for rows.Next() {
		var h hit
		if rows.Scan(&h.id, &h.soc, &h.q, &h.all) == nil {
			hits = append(hits, h)
		}
	}
	rows.Close()
	for _, h := range hits {
		users := s.audienceUsers(ctx, h.soc, h.id, h.all, true)
		if s.note != nil && len(users) > 0 {
			s.note.Send(ctx, notify.Notification{SocietyID: h.soc, Users: users, Type: "poll.reminder", Title: "Poll closes soon", Body: h.q,
				Data: map[string]any{"type": "poll", "poll_id": h.id}, Priority: notify.PriorityNormal})
		}
		reminded++
	}
	return closed, reminded, nil
}

func (s *Service) RunSweeper(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			heartbeat.Beat("polls")
			_, _, _ = s.Sweep(ctx)
		}
	}
}
