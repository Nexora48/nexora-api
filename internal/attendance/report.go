package attendance

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Nexora48/nexora-api/internal/platform/authctx"
)

// Day is one day a person was present.
type Day struct {
	Day     int  `json:"day"`
	Minutes int  `json:"minutes"`
	Late    bool `json:"late"`
	Open    bool `json:"open,omitempty"` // still inside (no check-out yet)
}

type SheetRow struct {
	SubjectType string    `json:"subject_type"`
	SubjectID   uuid.UUID `json:"subject_id"`
	Name        string    `json:"name"`
	Role        string    `json:"role"`
	WorkDays    []int     `json:"work_days,omitempty"` // staff only: lets the screen show absences
	Since       *string   `json:"since,omitempty"`     // staff only: first day on the list (no absences before it)
	Present     int       `json:"present_days"`
	LateDays    int       `json:"late_days"`
	Minutes     int       `json:"minutes"`
	Days        []Day     `json:"days"`
}

type Sheet struct {
	Month string     `json:"month"`
	Days  int        `json:"days_in_month"`
	Rows  []SheetRow `json:"rows"`
}

func (s *Service) monthBounds(ctx context.Context, soc uuid.UUID, month string) (time.Time, time.Time, *time.Location, error) {
	loc := s.loc(ctx, soc)
	if month == "" {
		month = s.nowFn().In(loc).Format("2006-01")
	}
	m, err := time.ParseInLocation("2006-01", month, loc)
	if err != nil {
		return m, m, loc, invalid("month must be YYYY-MM")
	}
	return m, m.AddDate(0, 1, 0), loc, nil
}

// Sheet is the monthly attendance of helpers or staff ("" = both), one row per person who worked or is on the staff list.
func (s *Service) Sheet(ctx context.Context, p authctx.Principal, month, typ string) (*Sheet, error) {
	from, to, loc, err := s.monthBounds(ctx, p.SocietyID, month)
	if err != nil {
		return nil, err
	}
	if typ != "" && typ != "helper" && typ != "staff" {
		return nil, invalid("type must be helper or staff")
	}
	sh := &Sheet{Month: from.Format("2006-01"), Days: int(to.Sub(from).Hours()/24 + 0.5), Rows: []SheetRow{}}
	idx := map[uuid.UUID]*SheetRow{}
	var ordered []*SheetRow
	add := func(r *SheetRow) { ordered = append(ordered, r); idx[r.SubjectID] = r }

	// everyone who has a row: staff always, helpers only when they appeared this month
	if typ == "" || typ == "staff" {
		rows, err := s.pool.Query(ctx, `SELECT id, full_name, role, work_days, to_char(created_at AT TIME ZONE (SELECT timezone FROM societies WHERE id=$1), 'YYYY-MM-DD') FROM society_staff WHERE society_id=$1 AND (active OR EXISTS (SELECT 1 FROM attendance_logs l WHERE l.subject_id=society_staff.id AND l.in_at >= $2 AND l.in_at < $3)) ORDER BY full_name, id`, p.SocietyID, from, to)
		if err != nil {
			return nil, err
		}
		var list []*SheetRow
		for rows.Next() {
			r := &SheetRow{SubjectType: "staff", Days: []Day{}}
			var wd []int16
			var since string
			if err := rows.Scan(&r.SubjectID, &r.Name, &r.Role, &wd, &since); err != nil {
				rows.Close()
				return nil, err
			}
			r.Since = &since
			r.WorkDays = []int{}
			for _, d := range wd {
				r.WorkDays = append(r.WorkDays, int(d))
			}
			list = append(list, r)
		}
		rows.Close()
		for _, r := range list {
			add(r)
		}
	}
	if typ == "" || typ == "helper" {
		rows, err := s.pool.Query(ctx, `SELECT id, full_name, kind FROM helpers h WHERE society_id=$1 AND EXISTS (SELECT 1 FROM attendance_logs l WHERE l.subject_id=h.id AND l.in_at >= $2 AND l.in_at < $3) ORDER BY full_name, id`, p.SocietyID, from, to)
		if err != nil {
			return nil, err
		}
		var list []*SheetRow
		for rows.Next() {
			r := &SheetRow{SubjectType: "helper", Days: []Day{}}
			if err := rows.Scan(&r.SubjectID, &r.Name, &r.Role); err != nil {
				rows.Close()
				return nil, err
			}
			list = append(list, r)
		}
		rows.Close()
		for _, r := range list {
			add(r)
		}
	}

	logs, err := s.pool.Query(ctx, `SELECT subject_id, in_at, out_at, late FROM attendance_logs WHERE society_id=$1 AND in_at >= $2 AND in_at < $3 ORDER BY in_at`, p.SocietyID, from, to)
	if err != nil {
		return nil, err
	}
	defer logs.Close()
	for logs.Next() {
		var sid uuid.UUID
		var in time.Time
		var out *time.Time
		var late bool
		if err := logs.Scan(&sid, &in, &out, &late); err != nil {
			return nil, err
		}
		r := idx[sid]
		if r == nil {
			continue
		}
		d := in.In(loc).Day()
		mins := 0
		if out != nil {
			mins = int(out.Sub(in).Minutes())
		}
		if n := len(r.Days); n > 0 && r.Days[n-1].Day == d { // several visits in a day add up; late if the first one was
			r.Days[n-1].Minutes += mins
			r.Days[n-1].Open = out == nil
		} else {
			r.Days = append(r.Days, Day{Day: d, Minutes: mins, Late: late, Open: out == nil})
			r.Present++
			if late {
				r.LateDays++
			}
		}
		r.Minutes += mins
	}
	if err := logs.Err(); err != nil {
		return nil, err
	}
	for _, r := range ordered {
		sh.Rows = append(sh.Rows, *r)
	}
	return sh, nil
}

func csvSafe(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}

func (s *Service) ExportSheet(ctx context.Context, p authctx.Principal, month, typ string, w io.Writer) error {
	sh, err := s.Sheet(ctx, p, month, typ)
	if err != nil {
		return err
	}
	cw := csv.NewWriter(w)
	head := []string{"Name", "Type", "Role", "Present days", "Late days", "Hours"}
	for d := 1; d <= sh.Days; d++ {
		head = append(head, fmt.Sprint(d))
	}
	_ = cw.Write(head)
	for _, r := range sh.Rows {
		row := []string{csvSafe(r.Name), r.SubjectType, r.Role, fmt.Sprint(r.Present), fmt.Sprint(r.LateDays), fmt.Sprintf("%.1f", float64(r.Minutes)/60)}
		cells := make([]string, sh.Days)
		for _, d := range r.Days {
			c := "P"
			if d.Late {
				c = "L"
			}
			cells[d.Day-1] = c
		}
		_ = cw.Write(append(row, cells...))
	}
	cw.Flush()
	return cw.Error()
}

// Log is the raw record list of one person for a month (the office's detail view).
func (s *Service) Log(ctx context.Context, p authctx.Principal, typ string, id uuid.UUID, month string) ([]Record, error) {
	from, to, _, err := s.monthBounds(ctx, p.SocietyID, month)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id FROM attendance_logs WHERE society_id=$1 AND subject_type=$2 AND subject_id=$3 AND in_at >= $4 AND in_at < $5 ORDER BY in_at DESC`, p.SocietyID, typ, id, from, to)
	if err != nil {
		return nil, err
	}
	var ids []uuid.UUID
	for rows.Next() {
		var rid uuid.UUID
		if rows.Scan(&rid) == nil {
			ids = append(ids, rid)
		}
	}
	rows.Close()
	out := []Record{}
	for _, rid := range ids {
		if r, err := s.record(ctx, p.SocietyID, rid); err == nil {
			out = append(out, *r)
		}
	}
	return out, nil
}

// ManualMark adds a completed attendance record the gate missed (staff.manage only).
func (s *Service) ManualMark(ctx context.Context, p authctx.Principal, typ string, id uuid.UUID, in, out time.Time, note string) (*Record, error) {
	r, err := s.load(ctx, p.SocietyID, typ, id)
	if err != nil {
		return nil, err
	}
	now := s.nowFn()
	if in.IsZero() || out.IsZero() || !out.After(in) || out.After(now) || in.Before(now.AddDate(0, 0, -62)) {
		return nil, invalid("give an arrival and a later departure within the last two months, not in the future")
	}
	note = strings.TrimSpace(note)
	if len([]rune(note)) > 200 {
		return nil, invalid("the note is limited to 200 characters")
	}
	var overlap bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM attendance_logs WHERE subject_type=$1 AND subject_id=$2 AND tstzrange(in_at, COALESCE(out_at, 'infinity'::timestamptz)) && tstzrange($3, $4))`, typ, id, in, out).Scan(&overlap); err != nil {
		return nil, err
	}
	if overlap {
		return nil, ErrAlreadyInside
	}
	local := in.In(s.loc(ctx, p.SocietyID))
	var rid uuid.UUID
	var n *string
	if note != "" {
		n = &note
	}
	if err := s.pool.QueryRow(ctx, `INSERT INTO attendance_logs (society_id, subject_type, subject_id, in_at, out_at, in_by, out_by, source, late, off_hours, note) VALUES ($1,$2,$3,$4,$5,$6,$6,'manual',$7,$8,$9) RETURNING id`,
		p.SocietyID, typ, id, in, out, p.MembershipID, r.late(local), r.offHours(local), n).Scan(&rid); err != nil {
		return nil, err
	}
	s.log(ctx, p, "attendance.manual", typ, id, map[string]any{"in": in, "out": out})
	return s.record(ctx, p.SocietyID, rid)
}
