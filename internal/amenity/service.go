// Package amenity runs the shared facilities: catalogue and rules (office), slot grids and bookings (residents).
package amenity

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
	"github.com/Nexora48/nexora-api/internal/platform/database"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
	"github.com/Nexora48/nexora-api/internal/realtime"
)

const maxPhotos = 3

func unprocessable(code, msg string) error {
	return httpx.NewError(http.StatusUnprocessableEntity, code, msg)
}
func invalid(msg string) error { return httpx.NewError(http.StatusBadRequest, "VALIDATION", msg) }

var (
	ErrNotFound      = httpx.NewError(http.StatusNotFound, "NOT_FOUND", "amenity not found")
	ErrBookingNone   = httpx.NewError(http.StatusNotFound, "NOT_FOUND", "booking not found")
	ErrNoUnit        = httpx.NewError(http.StatusConflict, "NO_UNIT", "your account is not linked to a flat")
	ErrSlotFull      = httpx.NewError(http.StatusConflict, "SLOT_FULL", "this slot is full")
	ErrAlreadyBooked = httpx.NewError(http.StatusConflict, "ALREADY_BOOKED", "your flat already has this slot")
	ErrInvalidState  = httpx.NewError(http.StatusConflict, "INVALID_STATE", "this booking cannot be changed that way")
	ErrCutoff        = httpx.NewError(http.StatusConflict, "CANCEL_CUTOFF", "it is too late to cancel this booking; please contact the society office")
	ErrNameTaken     = httpx.NewError(http.StatusConflict, "NAME_TAKEN", "an amenity with this name already exists")
	ErrInactive      = unprocessable("INACTIVE", "this amenity is not open for booking")
)

type Service struct {
	pool          *pgxpool.Pool
	audit         *audit.Logger
	files         *attachment.Service
	note          *notify.Service
	hub           *realtime.Hub
	reminderAhead time.Duration
}

func NewService(pool *pgxpool.Pool, a *audit.Logger, files *attachment.Service, note *notify.Service, hub *realtime.Hub, reminderAhead time.Duration) *Service {
	if reminderAhead <= 0 {
		reminderAhead = time.Hour
	}
	return &Service{pool: pool, audit: a, files: files, note: note, hub: hub, reminderAhead: reminderAhead}
}

type Hours struct {
	Weekday int    `json:"weekday"` // 0 = Sunday
	Opens   string `json:"opens"`   // HH:MM
	Closes  string `json:"closes"`
}

type Blackout struct {
	ID     uuid.UUID `json:"id"`
	Date   string    `json:"date"`
	Reason string    `json:"reason"`
}

type Amenity struct {
	ID               uuid.UUID         `json:"id"`
	Name             string            `json:"name"`
	Description      string            `json:"description"`
	Capacity         int               `json:"capacity"`
	SlotMinutes      int               `json:"slot_minutes"`
	MinNoticeMinutes int               `json:"min_notice_minutes"`
	MaxDaysAhead     int               `json:"max_days_ahead"`
	MaxUpcoming      int               `json:"max_upcoming_per_flat"`
	MaxPerWeek       int               `json:"max_per_week_per_flat"`
	CancelCutoff     int               `json:"cancel_cutoff_hours"`
	Fee              float64           `json:"fee_amount"`
	Deposit          float64           `json:"deposit_amount"`
	RequiresApproval bool              `json:"requires_approval"`
	Active           bool              `json:"active"`
	Hours            []Hours           `json:"hours"`
	Blackouts        []Blackout        `json:"blackouts"`
	Photos           []attachment.File `json:"photos"`
}

const amenityCols = `a.id, a.name, a.description, a.capacity, a.slot_minutes, a.min_notice_minutes, a.max_days_ahead, a.max_upcoming_per_flat,
	a.max_per_week_per_flat, a.cancel_cutoff_hours, a.fee_amount::float8, a.deposit_amount::float8, a.requires_approval, a.active`

func scanAmenity(row pgx.Row) (*Amenity, error) {
	var a Amenity
	if err := row.Scan(&a.ID, &a.Name, &a.Description, &a.Capacity, &a.SlotMinutes, &a.MinNoticeMinutes, &a.MaxDaysAhead, &a.MaxUpcoming,
		&a.MaxPerWeek, &a.CancelCutoff, &a.Fee, &a.Deposit, &a.RequiresApproval, &a.Active); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	a.Hours, a.Blackouts, a.Photos = []Hours{}, []Blackout{}, []attachment.File{}
	return &a, nil
}

// fill adds hours, blackouts (from today on) and photos.
func (s *Service) fill(ctx context.Context, soc uuid.UUID, list []*Amenity) error {
	if len(list) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(list))
	by := map[uuid.UUID]*Amenity{}
	for i, a := range list {
		ids[i], by[a.ID] = a.ID, a
	}
	rows, err := s.pool.Query(ctx, `SELECT amenity_id, weekday, to_char(opens,'HH24:MI'), to_char(closes,'HH24:MI') FROM amenity_hours WHERE amenity_id = ANY($1) ORDER BY weekday`, ids)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id uuid.UUID
		var h Hours
		if rows.Scan(&id, &h.Weekday, &h.Opens, &h.Closes) == nil {
			by[id].Hours = append(by[id].Hours, h)
		}
	}
	rows.Close()
	rows, err = s.pool.Query(ctx, `SELECT amenity_id, id, to_char(on_date,'YYYY-MM-DD'), reason FROM amenity_blackouts WHERE amenity_id = ANY($1) AND on_date >= current_date - 1 ORDER BY on_date`, ids)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id uuid.UUID
		var b Blackout
		if rows.Scan(&id, &b.ID, &b.Date, &b.Reason) == nil {
			by[id].Blackouts = append(by[id].Blackouts, b)
		}
	}
	rows.Close()
	files, err := s.files.ListMany(ctx, soc, attachment.OwnerAmenity, ids)
	if err != nil {
		return err
	}
	for id, f := range files {
		by[id].Photos = f
	}
	return nil
}

func (s *Service) loc(ctx context.Context, soc uuid.UUID) *time.Location {
	var tz string
	_ = s.pool.QueryRow(ctx, `SELECT timezone FROM societies WHERE id=$1`, soc).Scan(&tz)
	if l, err := time.LoadLocation(tz); err == nil {
		return l
	}
	return time.FixedZone("IST", 5*3600+1800)
}

func (s *Service) log(ctx context.Context, p authctx.Principal, action string, id uuid.UUID, meta map[string]any) {
	s.audit.Log(ctx, audit.Entry{ActorID: &p.UserID, SocietyID: &p.SocietyID, Action: action, Entity: "amenity", EntityID: id.String(), Meta: meta})
}

func (s *Service) get(ctx context.Context, soc, id uuid.UUID) (*Amenity, error) {
	a, err := scanAmenity(s.pool.QueryRow(ctx, `SELECT `+amenityCols+` FROM amenities a WHERE a.id=$1 AND a.society_id=$2`, id, soc))
	if err != nil {
		return nil, err
	}
	return a, s.fill(ctx, soc, []*Amenity{a})
}

// ── residents ────────────────────────────────────────────────────────────────

// List returns the active amenities for residents and every amenity for the office.
func (s *Service) List(ctx context.Context, p authctx.Principal, all bool) ([]*Amenity, error) {
	q := `SELECT ` + amenityCols + ` FROM amenities a WHERE a.society_id=$1`
	if !all {
		q += ` AND a.active`
	}
	rows, err := s.pool.Query(ctx, q+` ORDER BY lower(a.name)`, p.SocietyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Amenity
	for rows.Next() {
		a, err := scanAmenity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	rows.Close()
	return out, s.fill(ctx, p.SocietyID, out)
}

func (s *Service) Get(ctx context.Context, p authctx.Principal, id uuid.UUID, all bool) (*Amenity, error) {
	a, err := s.get(ctx, p.SocietyID, id)
	if err != nil {
		return nil, err
	}
	if !a.Active && !all {
		return nil, ErrNotFound
	}
	return a, nil
}

type Slot struct {
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	Capacity int       `json:"capacity"`
	Taken    int       `json:"taken"`
	Bookable bool      `json:"bookable"`
	Reason   string    `json:"reason,omitempty"` // PAST, TOO_SOON, TOO_FAR, FULL, YOURS, BLACKOUT
}

type Day struct {
	AmenityID    uuid.UUID `json:"amenity_id"`
	Date         string    `json:"date"`
	Open         bool      `json:"open"`
	ClosedReason string    `json:"closed_reason,omitempty"` // CLOSED, BLACKOUT, INACTIVE
	Slots        []Slot    `json:"slots"`
}

// grid generates the slots of one local date from the weekly hours. Slots are built from local wall-clock
// time, so a daylight-saving change can neither duplicate nor skip a slot.
func grid(a *Amenity, d time.Time, loc *time.Location) []Slot {
	for _, h := range a.Hours {
		if h.Weekday != int(d.Weekday()) {
			continue
		}
		var oh, om, ch, cm int
		fmt.Sscanf(h.Opens, "%d:%d", &oh, &om)
		fmt.Sscanf(h.Closes, "%d:%d", &ch, &cm)
		closeMin := ch*60 + cm
		var out []Slot
		for m := oh*60 + om; m+a.SlotMinutes <= closeMin; m += a.SlotMinutes {
			start := time.Date(d.Year(), d.Month(), d.Day(), 0, m, 0, 0, loc)
			if start.Hour()*60+start.Minute() != m { // wall-clock time skipped by a spring-forward change
				continue
			}
			end := start.Add(time.Duration(a.SlotMinutes) * time.Minute)
			out = append(out, Slot{Start: start.UTC(), End: end.UTC(), Capacity: a.Capacity})
		}
		return out
	}
	return nil
}

func (s *Service) parseDate(ctx context.Context, soc uuid.UUID, date string) (time.Time, *time.Location, error) {
	loc := s.loc(ctx, soc)
	d, err := time.ParseInLocation("2006-01-02", date, loc)
	if err != nil {
		return time.Time{}, loc, invalid("date must be YYYY-MM-DD")
	}
	return d, loc, nil
}

func blackedOut(a *Amenity, date string) bool {
	for _, b := range a.Blackouts {
		if b.Date == date {
			return true
		}
	}
	return false
}

// rule says why a slot cannot be booked right now ("" when it can).
func rule(a *Amenity, sl Slot, date string, now time.Time) string {
	switch {
	case blackedOut(a, date):
		return "BLACKOUT"
	case !sl.Start.After(now):
		return "PAST"
	case sl.Start.Before(now.Add(time.Duration(a.MinNoticeMinutes) * time.Minute)):
		return "TOO_SOON"
	case sl.Start.After(now.Add(time.Duration(a.MaxDaysAhead) * 24 * time.Hour)):
		return "TOO_FAR"
	}
	return ""
}

// Slots returns one day's grid with what the caller's flat may book.
func (s *Service) Slots(ctx context.Context, p authctx.Principal, id uuid.UUID, date string) (*Day, error) {
	a, err := s.get(ctx, p.SocietyID, id)
	if err != nil {
		return nil, err
	}
	d, loc, err := s.parseDate(ctx, p.SocietyID, date)
	if err != nil {
		return nil, err
	}
	day := &Day{AmenityID: id, Date: date, Slots: []Slot{}}
	if !a.Active {
		day.ClosedReason = "INACTIVE"
		return day, nil
	}
	slots := grid(a, d, loc)
	if len(slots) == 0 {
		day.ClosedReason = "CLOSED"
		return day, nil
	}
	if blackedOut(a, date) {
		day.ClosedReason = "BLACKOUT"
		return day, nil
	}
	day.Open = true
	unit := s.unitOf(ctx, p)
	taken := map[time.Time]int{}
	mine := map[time.Time]bool{}
	rows, err := s.pool.Query(ctx, `SELECT slot_start, unit_id FROM amenity_bookings WHERE amenity_id=$1 AND status IN ('pending','confirmed') AND slot_start >= $2 AND slot_start < $3`,
		id, slots[0].Start, slots[len(slots)-1].End)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var st time.Time
		var u uuid.UUID
		if rows.Scan(&st, &u) == nil {
			taken[st.UTC()]++
			if unit != nil && u == *unit {
				mine[st.UTC()] = true
			}
		}
	}
	rows.Close()
	now := time.Now()
	for _, sl := range slots {
		sl.Taken = taken[sl.Start]
		switch {
		case mine[sl.Start]:
			sl.Reason = "YOURS"
		case rule(a, sl, date, now) != "":
			sl.Reason = rule(a, sl, date, now)
		case sl.Taken >= sl.Capacity:
			sl.Reason = "FULL"
		default:
			sl.Bookable = true
		}
		day.Slots = append(day.Slots, sl)
	}
	return day, nil
}

func (s *Service) unitOf(ctx context.Context, p authctx.Principal) *uuid.UUID {
	var u *uuid.UUID
	_ = s.pool.QueryRow(ctx, `SELECT unit_id FROM memberships WHERE id=$1 AND user_id=$2 AND society_id=$3 AND status='active'`, p.MembershipID, p.UserID, p.SocietyID).Scan(&u)
	return u
}

type Booking struct {
	ID             uuid.UUID `json:"id"`
	AmenityID      uuid.UUID `json:"amenity_id"`
	AmenityName    string    `json:"amenity_name"`
	UnitID         uuid.UUID `json:"unit_id"`
	Flat           string    `json:"flat"`
	BookedBy       string    `json:"booked_by"`
	SlotStart      time.Time `json:"slot_start"`
	SlotEnd        time.Time `json:"slot_end"`
	Status         string    `json:"status"`
	People         int       `json:"people"`
	Note           *string   `json:"note,omitempty"`
	Fee            float64   `json:"fee_amount"`
	Deposit        float64   `json:"deposit_amount"`
	ChargeStatus   string    `json:"charge_status"`
	DecisionReason *string   `json:"decision_reason,omitempty"`
	CancelledBy    *string   `json:"cancelled_by,omitempty"`
	CancelReason   *string   `json:"cancel_reason,omitempty"`
	CanCancel      bool      `json:"can_cancel,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

const bookingCols = `b.id, b.amenity_id, a.name, b.unit_id, bl.code || '-' || u.number, bu.full_name, b.slot_start, b.slot_end, b.status, b.people, b.note,
	b.fee_amount::float8, b.deposit_amount::float8, b.charge_status, b.decision_reason, b.cancelled_by, b.cancel_reason, b.created_at, a.cancel_cutoff_hours`

const bookingFrom = ` FROM amenity_bookings b JOIN amenities a ON a.id=b.amenity_id JOIN units u ON u.id=b.unit_id JOIN blocks bl ON bl.id=u.block_id
	JOIN memberships bm ON bm.id=b.booked_by_membership_id JOIN users bu ON bu.id=bm.user_id `

func scanBooking(row pgx.Row, extra ...any) (*Booking, error) {
	var b Booking
	var cutoff int
	dest := []any{&b.ID, &b.AmenityID, &b.AmenityName, &b.UnitID, &b.Flat, &b.BookedBy, &b.SlotStart, &b.SlotEnd, &b.Status, &b.People, &b.Note,
		&b.Fee, &b.Deposit, &b.ChargeStatus, &b.DecisionReason, &b.CancelledBy, &b.CancelReason, &b.CreatedAt, &cutoff}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrBookingNone
		}
		return nil, err
	}
	now := time.Now()
	switch b.Status {
	case "pending":
		b.CanCancel = b.SlotStart.After(now)
	case "confirmed":
		b.CanCancel = now.Before(b.SlotStart.Add(-time.Duration(cutoff) * time.Hour))
	}
	return &b, nil
}

func (s *Service) booking(ctx context.Context, soc, id uuid.UUID) (*Booking, error) {
	return scanBooking(s.pool.QueryRow(ctx, `SELECT `+bookingCols+bookingFrom+`WHERE b.id=$1 AND b.society_id=$2`, id, soc))
}

// Book reserves one slot for the caller's flat. The whole decision happens in one transaction under a lock
// per amenity and day, with a unique index behind it, so parallel requests can never oversell a slot.
func (s *Service) Book(ctx context.Context, p authctx.Principal, id uuid.UUID, slotStart time.Time, people int, note *string) (*Booking, error) {
	unit := s.unitOf(ctx, p)
	if unit == nil {
		return nil, ErrNoUnit
	}
	a, err := s.get(ctx, p.SocietyID, id)
	if err != nil {
		return nil, err
	}
	if !a.Active {
		return nil, ErrInactive
	}
	if people <= 0 {
		people = 1
	}
	if people > 100 {
		return nil, invalid("people must be between 1 and 100")
	}
	if note != nil {
		n := strings.TrimSpace(*note)
		if len([]rune(n)) > 200 {
			return nil, invalid("the note is limited to 200 characters")
		}
		if n == "" {
			note = nil
		} else {
			note = &n
		}
	}
	loc := s.loc(ctx, p.SocietyID)
	local := slotStart.In(loc)
	date := local.Format("2006-01-02")
	var slot *Slot
	for _, sl := range grid(a, time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc), loc) {
		if sl.Start.Equal(slotStart.UTC()) {
			c := sl
			slot = &c
		}
	}
	if slot == nil {
		if len(grid(a, time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc), loc)) == 0 {
			return nil, unprocessable("CLOSED", "the amenity is closed on that day")
		}
		return nil, unprocessable("INVALID_SLOT", "choose one of the listed slots")
	}
	switch rule(a, *slot, date, time.Now()) {
	case "BLACKOUT":
		return nil, unprocessable("BLACKOUT", "the amenity is not available on that date")
	case "PAST":
		return nil, unprocessable("PAST", "that slot has already started")
	case "TOO_SOON":
		return nil, unprocessable("OUTSIDE_WINDOW", fmt.Sprintf("bookings need at least %d minutes of notice", a.MinNoticeMinutes))
	case "TOO_FAR":
		return nil, unprocessable("OUTSIDE_WINDOW", fmt.Sprintf("you can book up to %d days ahead", a.MaxDaysAhead))
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, id.String()+":"+date); err != nil {
		return nil, err
	}
	var taken int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM amenity_bookings WHERE amenity_id=$1 AND slot_start=$2 AND status IN ('pending','confirmed')`, id, slot.Start).Scan(&taken); err != nil {
		return nil, err
	}
	if taken >= a.Capacity {
		return nil, ErrSlotFull
	}
	var upcoming, week int
	weekStart := time.Date(local.Year(), local.Month(), local.Day()-(int(local.Weekday())+6)%7, 0, 0, 0, 0, loc)
	if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE slot_end > now()), count(*) FILTER (WHERE slot_start >= $3 AND slot_start < $4)
		FROM amenity_bookings WHERE amenity_id=$1 AND unit_id=$2 AND status IN ('pending','confirmed')`, id, *unit, weekStart.UTC(), weekStart.AddDate(0, 0, 7).UTC()).Scan(&upcoming, &week); err != nil {
		return nil, err
	}
	if upcoming >= a.MaxUpcoming {
		return nil, unprocessable("FLAT_LIMIT", fmt.Sprintf("your flat can hold %d upcoming bookings for this amenity", a.MaxUpcoming))
	}
	if week >= a.MaxPerWeek {
		return nil, unprocessable("FLAT_LIMIT", fmt.Sprintf("your flat can book %d slots a week for this amenity", a.MaxPerWeek))
	}
	status := "confirmed"
	if a.RequiresApproval {
		status = "pending"
	}
	charge := "none"
	if a.Fee+a.Deposit > 0 {
		charge = "pending"
	}
	var bid uuid.UUID
	err = tx.QueryRow(ctx, `INSERT INTO amenity_bookings (society_id, amenity_id, unit_id, booked_by_membership_id, slot_start, slot_end, status, people, note, fee_amount, deposit_amount, charge_status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING id`,
		p.SocietyID, id, *unit, p.MembershipID, slot.Start, slot.End, status, people, note, a.Fee, a.Deposit, charge).Scan(&bid)
	if database.IsUnique(err, "amenity_bookings_flat_slot_uq") {
		return nil, ErrAlreadyBooked
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	s.log(ctx, p, "booking.create", bid, map[string]any{"amenity": a.Name, "status": status})
	b, err := s.booking(ctx, p.SocietyID, bid)
	if err == nil {
		s.updated(ctx, p.SocietyID, b)
		if status == "pending" {
			s.note.Send(ctx, notify.Notification{SocietyID: p.SocietyID, Users: s.note.ByRoles(ctx, p.SocietyID, "society_admin", "committee"), Type: "booking.request",
				Title: "Booking request", Body: fmt.Sprintf("%s for flat %s", a.Name, b.Flat), Data: map[string]any{"type": "booking", "booking_id": b.ID}, Priority: notify.PriorityNormal})
		}
	}
	return b, err
}

// updated tells the flat and the office to refresh (ids and status only).
func (s *Service) updated(ctx context.Context, soc uuid.UUID, b *Booking) {
	if s.hub == nil {
		return
	}
	u := b.UnitID
	s.hub.Publish(ctx, realtime.Event{Type: "booking.updated", SocietyID: soc, Unit: &u, Roles: []string{"society_admin", "committee"},
		Data: map[string]any{"booking_id": b.ID, "amenity_id": b.AmenityID, "status": b.Status}})
}

func (s *Service) tell(ctx context.Context, soc uuid.UUID, b *Booking, typ, title, body string) {
	if s.note == nil {
		return
	}
	s.note.Send(ctx, notify.Notification{SocietyID: soc, Users: s.note.UnitMembers(ctx, soc, b.UnitID), Type: typ, Title: title, Body: body,
		Data: map[string]any{"type": "booking", "booking_id": b.ID}, Priority: notify.PriorityNormal})
}

// MyBookings lists my flat's bookings: group "upcoming" (default) or "past".
func (s *Service) MyBookings(ctx context.Context, p authctx.Principal, group string, limit, offset int) ([]*Booking, int, error) {
	unit := s.unitOf(ctx, p)
	if unit == nil {
		return nil, 0, ErrNoUnit
	}
	where, order := `b.status IN ('pending','confirmed') AND b.slot_end > now()`, `b.slot_start ASC`
	if group == "past" {
		where, order = `NOT (b.status IN ('pending','confirmed') AND b.slot_end > now())`, `b.slot_start DESC`
	}
	rows, err := s.pool.Query(ctx, `SELECT `+bookingCols+`, count(*) OVER() `+bookingFrom+`WHERE b.society_id=$1 AND b.unit_id=$2 AND `+where+` ORDER BY `+order+` LIMIT $3 OFFSET $4`, p.SocietyID, *unit, limit, offset)
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

// Cancel lets any member of the flat cancel before the amenity's cut-off.
func (s *Service) Cancel(ctx context.Context, p authctx.Principal, id uuid.UUID) (*Booking, error) {
	unit := s.unitOf(ctx, p)
	if unit == nil {
		return nil, ErrNoUnit
	}
	b, err := scanBooking(s.pool.QueryRow(ctx, `SELECT `+bookingCols+bookingFrom+`WHERE b.id=$1 AND b.society_id=$2 AND b.unit_id=$3`, id, p.SocietyID, *unit))
	if err != nil {
		return nil, err
	}
	if b.Status != "pending" && b.Status != "confirmed" {
		return nil, ErrInvalidState
	}
	if !b.SlotStart.After(time.Now()) {
		return nil, ErrInvalidState
	}
	if !b.CanCancel {
		return nil, ErrCutoff
	}
	tag, err := s.pool.Exec(ctx, `UPDATE amenity_bookings SET status='cancelled', cancelled_by='flat', updated_at=now(), charge_status=CASE WHEN charge_status='pending' THEN 'waived' ELSE charge_status END
		WHERE id=$1 AND society_id=$2 AND status IN ('pending','confirmed')`, id, p.SocietyID)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrInvalidState
	}
	s.log(ctx, p, "booking.cancel", id, nil)
	b, err = s.booking(ctx, p.SocietyID, id)
	if err == nil {
		s.updated(ctx, p.SocietyID, b)
	}
	return b, err
}
