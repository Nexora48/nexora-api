package amenity

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Nexora48/nexora-api/internal/attachment"
	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/database"
)

// Input is the editable part of an amenity.
type Input struct {
	Name             string
	Description      string
	Capacity         int
	SlotMinutes      int
	MinNoticeMinutes int
	MaxDaysAhead     int
	MaxUpcoming      int
	MaxPerWeek       int
	CancelCutoff     int
	Fee              float64
	Deposit          float64
	RequiresApproval bool
	Active           bool
	Hours            []Hours
}

func parseClock(v string) (int, bool) {
	var h, m int
	if n, err := fmt.Sscanf(v, "%d:%d", &h, &m); err != nil || n != 2 || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

func validate(in *Input) error {
	in.Name, in.Description = strings.TrimSpace(in.Name), strings.TrimSpace(in.Description)
	if l := len([]rune(in.Name)); l < 2 || l > 60 {
		return invalid("name must be 2-60 characters")
	}
	if len([]rune(in.Description)) > 500 {
		return invalid("description is limited to 500 characters")
	}
	if in.Capacity < 1 || in.Capacity > 500 {
		return invalid("capacity must be 1-500")
	}
	switch in.SlotMinutes {
	case 30, 60, 90, 120:
	default:
		return invalid("slot length must be 30, 60, 90 or 120 minutes")
	}
	if in.MinNoticeMinutes < 0 || in.MinNoticeMinutes > 7*24*60 {
		return invalid("minimum notice is out of range")
	}
	if in.MaxDaysAhead < 1 || in.MaxDaysAhead > 90 {
		return invalid("days ahead must be 1-90")
	}
	if in.MaxUpcoming < 1 || in.MaxUpcoming > 20 || in.MaxPerWeek < 1 || in.MaxPerWeek > 50 {
		return invalid("per-flat limits are out of range")
	}
	if in.CancelCutoff < 0 || in.CancelCutoff > 72 {
		return invalid("cancel cut-off must be 0-72 hours")
	}
	if in.Fee < 0 || in.Deposit < 0 || in.Fee > 1e6 || in.Deposit > 1e6 {
		return invalid("fee and deposit must be between 0 and 1,000,000")
	}
	seen := map[int]bool{}
	for _, h := range in.Hours {
		o, ok1 := parseClock(h.Opens)
		c, ok2 := parseClock(h.Closes)
		if h.Weekday < 0 || h.Weekday > 6 || !ok1 || !ok2 || o >= c {
			return invalid("opening hours need a weekday 0-6 and an opening time before the closing time (HH:MM)")
		}
		if seen[h.Weekday] {
			return invalid("one set of hours per weekday")
		}
		seen[h.Weekday] = true
		if c-o < in.SlotMinutes {
			return invalid("a day's opening hours are shorter than one slot")
		}
	}
	return nil
}

func (s *Service) write(ctx context.Context, p authctx.Principal, id *uuid.UUID, in Input) (uuid.UUID, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var aid uuid.UUID
	if id == nil {
		err = tx.QueryRow(ctx, `INSERT INTO amenities (society_id, name, description, capacity, slot_minutes, min_notice_minutes, max_days_ahead, max_upcoming_per_flat, max_per_week_per_flat,
			cancel_cutoff_hours, fee_amount, deposit_amount, requires_approval, active) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) RETURNING id`,
			p.SocietyID, in.Name, in.Description, in.Capacity, in.SlotMinutes, in.MinNoticeMinutes, in.MaxDaysAhead, in.MaxUpcoming, in.MaxPerWeek, in.CancelCutoff, in.Fee, in.Deposit, in.RequiresApproval, in.Active).Scan(&aid)
	} else {
		aid = *id
		var tag int64
		t, e := tx.Exec(ctx, `UPDATE amenities SET name=$3, description=$4, capacity=$5, slot_minutes=$6, min_notice_minutes=$7, max_days_ahead=$8, max_upcoming_per_flat=$9, max_per_week_per_flat=$10,
			cancel_cutoff_hours=$11, fee_amount=$12, deposit_amount=$13, requires_approval=$14, active=$15 WHERE id=$1 AND society_id=$2`,
			aid, p.SocietyID, in.Name, in.Description, in.Capacity, in.SlotMinutes, in.MinNoticeMinutes, in.MaxDaysAhead, in.MaxUpcoming, in.MaxPerWeek, in.CancelCutoff, in.Fee, in.Deposit, in.RequiresApproval, in.Active)
		err = e
		if e == nil {
			tag = t.RowsAffected()
			if tag == 0 {
				return uuid.Nil, ErrNotFound
			}
		}
	}
	if database.IsUnique(err, "amenities_name_uq") {
		return uuid.Nil, ErrNameTaken
	}
	if err != nil {
		return uuid.Nil, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM amenity_hours WHERE amenity_id=$1`, aid); err != nil {
		return uuid.Nil, err
	}
	for _, h := range in.Hours {
		if _, err := tx.Exec(ctx, `INSERT INTO amenity_hours (amenity_id, weekday, opens, closes) VALUES ($1,$2,$3::time,$4::time)`, aid, h.Weekday, h.Opens, h.Closes); err != nil {
			return uuid.Nil, err
		}
	}
	return aid, tx.Commit(ctx)
}

func (s *Service) Create(ctx context.Context, p authctx.Principal, in Input) (*Amenity, error) {
	if err := validate(&in); err != nil {
		return nil, err
	}
	id, err := s.write(ctx, p, nil, in)
	if err != nil {
		return nil, err
	}
	s.log(ctx, p, "amenity.create", id, map[string]any{"name": in.Name})
	return s.get(ctx, p.SocietyID, id)
}

func (s *Service) Update(ctx context.Context, p authctx.Principal, id uuid.UUID, in Input) (*Amenity, error) {
	if err := validate(&in); err != nil {
		return nil, err
	}
	if _, err := s.write(ctx, p, &id, in); err != nil {
		return nil, err
	}
	s.log(ctx, p, "amenity.update", id, map[string]any{"name": in.Name, "active": in.Active})
	return s.get(ctx, p.SocietyID, id)
}

// Deactivate hides an amenity from residents; existing bookings stay (cancel them separately).
func (s *Service) Deactivate(ctx context.Context, p authctx.Principal, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `UPDATE amenities SET active=false WHERE id=$1 AND society_id=$2`, id, p.SocietyID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	s.log(ctx, p, "amenity.deactivate", id, nil)
	return nil
}

func (s *Service) AddBlackout(ctx context.Context, p authctx.Principal, id uuid.UUID, date, reason string) (*Amenity, error) {
	if _, err := s.get(ctx, p.SocietyID, id); err != nil {
		return nil, err
	}
	d, err := time.Parse("2006-01-02", date)
	if err != nil {
		return nil, invalid("date must be YYYY-MM-DD")
	}
	reason = strings.TrimSpace(reason)
	if len([]rune(reason)) > 120 {
		return nil, invalid("the reason is limited to 120 characters")
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO amenity_blackouts (amenity_id, on_date, reason) VALUES ($1,$2,$3) ON CONFLICT (amenity_id, on_date) DO UPDATE SET reason=EXCLUDED.reason`, id, d, reason); err != nil {
		return nil, err
	}
	s.log(ctx, p, "amenity.blackout", id, map[string]any{"date": date})
	return s.get(ctx, p.SocietyID, id)
}

func (s *Service) RemoveBlackout(ctx context.Context, p authctx.Principal, id, bid uuid.UUID) (*Amenity, error) {
	if _, err := s.get(ctx, p.SocietyID, id); err != nil {
		return nil, err
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM amenity_blackouts WHERE id=$1 AND amenity_id=$2`, bid, id); err != nil {
		return nil, err
	}
	return s.get(ctx, p.SocietyID, id)
}

func (s *Service) AddPhoto(ctx context.Context, p authctx.Principal, id uuid.UUID, name string, data []byte) (*attachment.File, error) {
	if _, err := s.get(ctx, p.SocietyID, id); err != nil {
		return nil, err
	}
	f, err := s.files.Save(ctx, p.SocietyID, attachment.OwnerAmenity, id, p.UserID, name, data, maxPhotos)
	if err == nil {
		s.log(ctx, p, "amenity.photo", id, nil)
	}
	return f, err
}

func (s *Service) RemovePhoto(ctx context.Context, p authctx.Principal, id, aid uuid.UUID) error {
	if _, err := s.get(ctx, p.SocietyID, id); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM attachments WHERE id=$1 AND owner_type='amenity' AND owner_id=$2 AND society_id=$3`, aid, id, p.SocietyID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return attachment.ErrNotFound
	}
	return nil
}

// Photo serves an amenity photo to any member of the society.
func (s *Service) Photo(ctx context.Context, p authctx.Principal, id, aid uuid.UUID) ([]byte, *attachment.File, error) {
	if _, err := s.get(ctx, p.SocietyID, id); err != nil {
		return nil, nil, err
	}
	return s.files.Open(ctx, p.SocietyID, attachment.OwnerAmenity, id, aid)
}

// ── bookings desk ────────────────────────────────────────────────────────────

type Filter struct {
	AmenityID *uuid.UUID
	Status    string
	From, To  *time.Time
	Flat      string
	Limit     int
	Offset    int
}

func (s *Service) AdminBookings(ctx context.Context, p authctx.Principal, f Filter) ([]*Booking, int, error) {
	args := []any{p.SocietyID}
	where := `b.society_id=$1`
	add := func(cond string, v any) {
		args = append(args, v)
		where += " AND " + strings.ReplaceAll(cond, "?", fmt.Sprintf("$%d", len(args)))
	}
	if f.AmenityID != nil {
		add("b.amenity_id = ?", *f.AmenityID)
	}
	if f.Status != "" {
		add("b.status = ?", f.Status)
	}
	if f.From != nil {
		add("b.slot_start >= ?", *f.From)
	}
	if f.To != nil {
		add("b.slot_start < ?", *f.To)
	}
	if q := strings.TrimSpace(f.Flat); q != "" {
		add("(bl.code || '-' || u.number) ILIKE ?", "%"+q+"%")
	}
	rows, err := s.pool.Query(ctx, `SELECT `+bookingCols+`, count(*) OVER() `+bookingFrom+`WHERE `+where+
		fmt.Sprintf(` ORDER BY b.slot_start DESC, b.id LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2), append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*Booking
	total := 0
	for rows.Next() {
		b, err := scanBooking(rows, &total)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, b)
	}
	return out, total, rows.Err()
}

func (s *Service) transition(ctx context.Context, p authctx.Principal, id uuid.UUID, from []string, sql string, args ...any) (*Booking, error) {
	b, err := s.booking(ctx, p.SocietyID, id)
	if err != nil {
		return nil, err
	}
	ok := false
	for _, f := range from {
		ok = ok || f == b.Status
	}
	if !ok {
		return nil, ErrInvalidState
	}
	tag, err := s.pool.Exec(ctx, sql, append([]any{id, p.SocietyID, b.Status}, args...)...)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrInvalidState
	}
	nb, err := s.booking(ctx, p.SocietyID, id)
	if err == nil {
		s.updated(ctx, p.SocietyID, nb)
	}
	return nb, err
}

func (s *Service) Approve(ctx context.Context, p authctx.Principal, id uuid.UUID) (*Booking, error) {
	b, err := s.transition(ctx, p, id, []string{"pending"}, `UPDATE amenity_bookings SET status='confirmed', decided_by=$4, updated_at=now() WHERE id=$1 AND society_id=$2 AND status=$3`, p.MembershipID)
	if err != nil {
		return nil, err
	}
	s.log(ctx, p, "booking.approve", id, nil)
	s.tell(ctx, p.SocietyID, b, "booking.confirmed", "Booking confirmed", b.AmenityName+" is yours")
	return b, nil
}

func cleanReason(r string) (string, error) {
	r = strings.TrimSpace(r)
	if l := len([]rune(r)); l < 3 || l > 200 {
		return "", invalid("give a reason of 3-200 characters")
	}
	return r, nil
}

func (s *Service) Reject(ctx context.Context, p authctx.Principal, id uuid.UUID, reason string) (*Booking, error) {
	reason, err := cleanReason(reason)
	if err != nil {
		return nil, err
	}
	b, err := s.transition(ctx, p, id, []string{"pending"}, `UPDATE amenity_bookings SET status='rejected', decided_by=$4, decision_reason=$5, updated_at=now(),
		charge_status=CASE WHEN charge_status='pending' THEN 'waived' ELSE charge_status END WHERE id=$1 AND society_id=$2 AND status=$3`, p.MembershipID, reason)
	if err != nil {
		return nil, err
	}
	s.log(ctx, p, "booking.reject", id, map[string]any{"reason": reason})
	s.tell(ctx, p.SocietyID, b, "booking.rejected", "Booking not approved", b.AmenityName+": "+reason)
	return b, nil
}

func (s *Service) OfficeCancel(ctx context.Context, p authctx.Principal, id uuid.UUID, reason string) (*Booking, error) {
	reason, err := cleanReason(reason)
	if err != nil {
		return nil, err
	}
	b, err := s.transition(ctx, p, id, []string{"pending", "confirmed"}, `UPDATE amenity_bookings SET status='cancelled', cancelled_by='office', cancel_reason=$4, updated_at=now(),
		charge_status=CASE WHEN charge_status='pending' THEN 'waived' ELSE charge_status END WHERE id=$1 AND society_id=$2 AND status=$3`, reason)
	if err != nil {
		return nil, err
	}
	s.log(ctx, p, "booking.office_cancel", id, map[string]any{"reason": reason})
	s.tell(ctx, p.SocietyID, b, "booking.cancelled", "Booking cancelled by the office", b.AmenityName+": "+reason)
	return b, nil
}

// NoShow marks a confirmed booking whose slot has ended as not used.
func (s *Service) NoShow(ctx context.Context, p authctx.Principal, id uuid.UUID) (*Booking, error) {
	b, err := s.booking(ctx, p.SocietyID, id)
	if err != nil {
		return nil, err
	}
	if b.Status != "confirmed" && b.Status != "completed" {
		return nil, ErrInvalidState
	}
	if b.SlotEnd.After(time.Now()) {
		return nil, ErrInvalidState
	}
	if _, err := s.pool.Exec(ctx, `UPDATE amenity_bookings SET status='no_show', updated_at=now() WHERE id=$1 AND society_id=$2 AND status IN ('confirmed','completed')`, id, p.SocietyID); err != nil {
		return nil, err
	}
	s.log(ctx, p, "booking.no_show", id, nil)
	nb, err := s.booking(ctx, p.SocietyID, id)
	if err == nil {
		s.updated(ctx, p.SocietyID, nb)
	}
	return nb, err
}

// ── stats and export ─────────────────────────────────────────────────────────

type UsageRow struct {
	AmenityID   uuid.UUID `json:"amenity_id"`
	Name        string    `json:"name"`
	Bookings    int       `json:"bookings_30d"`
	Utilisation float64   `json:"utilisation_30d"` // booked places / available places, percent
	NoShowRate  float64   `json:"no_show_rate"`    // percent of finished bookings
	PeakHour    *int      `json:"peak_hour,omitempty"`
}

func (s *Service) Stats(ctx context.Context, p authctx.Principal) ([]UsageRow, error) {
	list, err := s.List(ctx, p, true)
	if err != nil {
		return nil, err
	}
	loc := s.loc(ctx, p.SocietyID)
	now := time.Now()
	out := []UsageRow{}
	for _, a := range list {
		r := UsageRow{AmenityID: a.ID, Name: a.Name}
		var finished, noShows int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status IN ('confirmed','completed','no_show')), count(*) FILTER (WHERE status IN ('completed','no_show')), count(*) FILTER (WHERE status='no_show')
			FROM amenity_bookings WHERE amenity_id=$1 AND slot_start >= now() - interval '30 days' AND slot_start < now()`, a.ID).Scan(&r.Bookings, &finished, &noShows); err != nil {
			return nil, err
		}
		if finished > 0 {
			r.NoShowRate = float64(noShows) * 100 / float64(finished)
		}
		places := 0
		for d := 1; d <= 30; d++ {
			day := now.In(loc).AddDate(0, 0, -d)
			local := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
			if !blackedOutOn(a, local.Format("2006-01-02")) {
				places += len(grid(a, local, loc)) * a.Capacity
			}
		}
		if places > 0 {
			var booked int
			_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM amenity_bookings WHERE amenity_id=$1 AND status IN ('confirmed','completed','no_show') AND slot_start >= now() - interval '30 days' AND slot_start < now()`, a.ID).Scan(&booked)
			r.Utilisation = float64(booked) * 100 / float64(places)
		}
		rows, err := s.pool.Query(ctx, `SELECT slot_start FROM amenity_bookings WHERE amenity_id=$1 AND status IN ('confirmed','completed','no_show') AND slot_start >= now() - interval '30 days'`, a.ID)
		if err != nil {
			return nil, err
		}
		perHour := map[int]int{}
		for rows.Next() {
			var t time.Time
			if rows.Scan(&t) == nil {
				perHour[t.In(loc).Hour()]++
			}
		}
		rows.Close()
		best, bestN := -1, 0
		for h, n := range perHour {
			if n > bestN || (n == bestN && h < best) {
				best, bestN = h, n
			}
		}
		if best >= 0 {
			r.PeakHour = &best
		}
		out = append(out, r)
	}
	return out, nil
}

func blackedOutOn(a *Amenity, date string) bool { return blackedOut(a, date) }

func csvSafe(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}

func (s *Service) Export(ctx context.Context, p authctx.Principal, f Filter, w io.Writer) (int, error) {
	f.Limit, f.Offset = 10000, 0
	rows, _, err := s.AdminBookings(ctx, p, f)
	if err != nil {
		return 0, err
	}
	loc := s.loc(ctx, p.SocietyID)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"Amenity", "Date", "From", "To", "Flat", "Booked by", "People", "Status", "Fee", "Deposit", "Charge"})
	for _, b := range rows {
		st, en := b.SlotStart.In(loc), b.SlotEnd.In(loc)
		_ = cw.Write([]string{csvSafe(b.AmenityName), st.Format("2006-01-02"), st.Format("15:04"), en.Format("15:04"), b.Flat, csvSafe(b.BookedBy), fmt.Sprint(b.People), b.Status,
			fmt.Sprintf("%.2f", b.Fee), fmt.Sprintf("%.2f", b.Deposit), b.ChargeStatus})
	}
	cw.Flush()
	s.log(ctx, p, "booking.export", uuid.Nil, map[string]any{"rows": len(rows)})
	return len(rows), cw.Error()
}

// ── sweeper ──────────────────────────────────────────────────────────────────

// Sweep completes finished bookings, expires requests nobody answered and sends the reminders.
func (s *Service) Sweep(ctx context.Context) (completed, expired, reminded int, err error) {
	tag, err := s.pool.Exec(ctx, `UPDATE amenity_bookings SET status='completed', updated_at=now() WHERE status='confirmed' AND slot_end < now()`)
	if err != nil {
		return 0, 0, 0, err
	}
	completed = int(tag.RowsAffected())

	rows, err := s.pool.Query(ctx, `UPDATE amenity_bookings SET status='expired', updated_at=now(), charge_status=CASE WHEN charge_status='pending' THEN 'waived' ELSE charge_status END
		WHERE status='pending' AND slot_start < now() + interval '12 hours' AND (created_at < now() - interval '2 hours' OR slot_start < now() + interval '30 minutes') RETURNING id, society_id`)
	if err != nil {
		return completed, 0, 0, err
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
		if b, err := s.booking(ctx, h.soc, h.id); err == nil {
			s.updated(ctx, h.soc, b)
			s.tell(ctx, h.soc, b, "booking.rejected", "Booking request expired", b.AmenityName+": the office did not answer in time")
			expired++
		}
	}

	rows, err = s.pool.Query(ctx, `UPDATE amenity_bookings SET reminded=true WHERE status='confirmed' AND NOT reminded AND slot_start > now() AND slot_start <= now() + $1::interval RETURNING id, society_id`,
		fmt.Sprintf("%d seconds", int(s.reminderAhead.Seconds())))
	if err != nil {
		return completed, expired, 0, err
	}
	hits = nil
	for rows.Next() {
		var h hit
		if rows.Scan(&h.id, &h.soc) == nil {
			hits = append(hits, h)
		}
	}
	rows.Close()
	for _, h := range hits {
		if b, err := s.booking(ctx, h.soc, h.id); err == nil {
			loc := s.loc(ctx, h.soc)
			s.tell(ctx, h.soc, b, "booking.reminder", "Booking coming up", fmt.Sprintf("%s at %s", b.AmenityName, b.SlotStart.In(loc).Format("3:04 PM")))
			reminded++
		}
	}
	return completed, expired, reminded, nil
}

func (s *Service) RunSweeper(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, _, _, _ = s.Sweep(ctx)
		}
	}
}
