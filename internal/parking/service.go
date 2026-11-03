// Package parking is the Parking Management service. The society office creates parking slots (an area plus a number)
// and allocates each to a flat; the residents of that flat see their slots in the resident app. Only the office
// (permission parking.manage) can create, change or allocate slots. The feature key is "parking".
package parking

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nexora48/nexora-api/internal/audit"
	"github.com/Nexora48/nexora-api/internal/notify"
	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
)

var (
	ErrNotFound     = httpx.NewError(http.StatusNotFound, "NOT_FOUND", "parking slot not found")
	ErrExists       = httpx.NewError(http.StatusConflict, "SLOT_EXISTS", "a slot with this area and number already exists")
	ErrAllocated    = httpx.NewError(http.StatusConflict, "SLOT_ALLOCATED", "release this slot before deleting it")
	ErrVisitor      = httpx.NewError(http.StatusConflict, "SLOT_VISITOR", "visitor slots are shared and cannot be allocated to a flat")
	ErrFlatNotFound = httpx.NewError(http.StatusNotFound, "UNIT_NOT_FOUND", "flat not found in this society")
	ErrAlreadyFree  = httpx.NewError(http.StatusConflict, "SLOT_FREE", "this slot is not allocated")
	ErrBulkTooLarge = httpx.NewError(http.StatusBadRequest, "VALIDATION", "create at most 200 slots at a time")
)

func invalid(msg string) *httpx.AppError {
	return httpx.NewError(http.StatusBadRequest, "VALIDATION", msg)
}

type Service struct {
	pool  *pgxpool.Pool
	audit *audit.Logger
	notes *notify.Service
}

func NewService(pool *pgxpool.Pool, a *audit.Logger, notes *notify.Service) *Service {
	return &Service{pool: pool, audit: a, notes: notes}
}

// Slot is one parking slot with, when allocated, the flat and its residents.
type Slot struct {
	ID          uuid.UUID  `json:"id"`
	Area        string     `json:"area"`
	Number      string     `json:"number"`
	Kind        string     `json:"kind"` // car | bike | visitor
	Note        string     `json:"note"`
	Allocated   bool       `json:"allocated"`
	UnitID      *uuid.UUID `json:"unit_id,omitempty"`
	Flat        string     `json:"flat,omitempty"` // "A-101"
	UnitType    string     `json:"unit_type,omitempty"`
	Residents   []string   `json:"residents"`
	AllocatedAt *time.Time `json:"allocated_at,omitempty"`
	AllocatedBy *string    `json:"allocated_by,omitempty"`
}

const slotCols = `p.id, p.area, p.number, p.kind, p.note, p.unit_id, COALESCE(b.code || '-' || u.number, ''), COALESCE(u.unit_type, ''),
	       COALESCE((SELECT array_agg(us.full_name ORDER BY us.full_name) FROM memberships m JOIN users us ON us.id=m.user_id
	                 WHERE m.society_id=p.society_id AND m.unit_id=p.unit_id AND m.status='active' AND m.role_key IN ('resident_owner','resident_tenant')), '{}'),
	       p.allocated_at, ab.full_name`

const slotFrom = `
	FROM parking_slots p
	LEFT JOIN units u ON u.id=p.unit_id LEFT JOIN blocks b ON b.id=u.block_id
	LEFT JOIN memberships am ON am.id=p.allocated_by LEFT JOIN users ab ON ab.id=am.user_id`

func scanSlot(row pgx.Row, total *int) (Slot, error) {
	var s Slot
	var names []string
	dest := []any{&s.ID, &s.Area, &s.Number, &s.Kind, &s.Note, &s.UnitID, &s.Flat, &s.UnitType, &names, &s.AllocatedAt, &s.AllocatedBy}
	if total != nil {
		dest = append(dest, total)
	}
	if err := row.Scan(dest...); err != nil {
		return s, err
	}
	s.Allocated = s.UnitID != nil
	s.Residents = names
	if s.Residents == nil {
		s.Residents = []string{}
	}
	return s, nil
}

// Filter narrows the office list.
type Filter struct {
	Status string // free | allocated | "" (all)
	Kind   string // car | bike | visitor | ""
	Q      string // matches area, number or flat
}

// List is the office view of the slots, newest areas first sorted by area then number.
func (s *Service) List(ctx context.Context, soc uuid.UUID, f Filter, limit, offset int) ([]Slot, int, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+slotCols+`, count(*) OVER()`+slotFrom+`
		WHERE p.society_id=$1
		  AND ($2 = '' OR ($2='free' AND p.unit_id IS NULL) OR ($2='allocated' AND p.unit_id IS NOT NULL))
		  AND ($3 = '' OR p.kind=$3)
		  AND ($4 = '' OR p.area ILIKE '%'||$4||'%' OR p.number ILIKE '%'||$4||'%' OR (b.code || '-' || u.number) ILIKE '%'||$4||'%')
		ORDER BY lower(p.area), length(p.number), p.number LIMIT $5 OFFSET $6`, soc, f.Status, f.Kind, strings.TrimSpace(f.Q), limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Slot{}
	total := 0
	for rows.Next() {
		sl, err := scanSlot(rows, &total)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, sl)
	}
	return out, total, rows.Err()
}

func (s *Service) get(ctx context.Context, soc, id uuid.UUID) (*Slot, error) {
	sl, err := scanSlot(s.pool.QueryRow(ctx, `SELECT `+slotCols+slotFrom+` WHERE p.society_id=$1 AND p.id=$2`, soc, id), nil)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &sl, nil
}

// Summary is the count tiles at the top of the office page.
type Summary struct {
	Total     int `json:"total"`
	Allocated int `json:"allocated"`
	Free      int `json:"free"`
	Cars      int `json:"cars"`
	Bikes     int `json:"bikes"`
	Visitor   int `json:"visitor"`
}

func (s *Service) Summary(ctx context.Context, soc uuid.UUID) (*Summary, error) {
	var v Summary
	err := s.pool.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE unit_id IS NOT NULL), count(*) FILTER (WHERE unit_id IS NULL),
		       count(*) FILTER (WHERE kind='car'), count(*) FILTER (WHERE kind='bike'), count(*) FILTER (WHERE kind='visitor')
		FROM parking_slots WHERE society_id=$1`, soc).Scan(&v.Total, &v.Allocated, &v.Free, &v.Cars, &v.Bikes, &v.Visitor)
	return &v, err
}

// Input is the editable part of a slot.
type Input struct {
	Area   string `json:"area"`
	Number string `json:"number"`
	Kind   string `json:"kind"`
	Note   string `json:"note"`
}

func clean(in *Input) error {
	in.Area = strings.TrimSpace(in.Area)
	in.Number = strings.TrimSpace(in.Number)
	in.Note = strings.TrimSpace(in.Note)
	if in.Kind == "" {
		in.Kind = "car"
	}
	switch {
	case in.Area == "" || len(in.Area) > 60:
		return invalid("area is required (up to 60 characters), for example Basement 1")
	case in.Number == "" || len(in.Number) > 20:
		return invalid("slot number is required (up to 20 characters)")
	case in.Kind != "car" && in.Kind != "bike" && in.Kind != "visitor":
		return invalid("kind must be car, bike or visitor")
	case len(in.Note) > 200:
		return invalid("note is too long (200 characters at most)")
	}
	return nil
}

func isDup(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

func (s *Service) Create(ctx context.Context, p authctx.Principal, in Input) (*Slot, error) {
	if err := clean(&in); err != nil {
		return nil, err
	}
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `INSERT INTO parking_slots (society_id, area, number, kind, note) VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		p.SocietyID, in.Area, in.Number, in.Kind, in.Note).Scan(&id)
	if isDup(err) {
		return nil, ErrExists
	}
	if err != nil {
		return nil, err
	}
	s.audit.Log(ctx, audit.Entry{ActorID: &p.UserID, SocietyID: &p.SocietyID, Action: "parking.create", Entity: "parking_slot", EntityID: id.String(), Meta: map[string]any{"area": in.Area, "number": in.Number, "kind": in.Kind}})
	return s.get(ctx, p.SocietyID, id)
}

// BulkInput creates a run of numbered slots in one area: prefix + from..to (zero-padded to the width of "to").
type BulkInput struct {
	Area   string `json:"area"`
	Prefix string `json:"prefix"`
	From   int    `json:"from"`
	To     int    `json:"to"`
	Kind   string `json:"kind"`
}

type BulkResult struct {
	Created int `json:"created"`
	Skipped int `json:"skipped"` // numbers that already existed in the area
}

func (s *Service) Bulk(ctx context.Context, p authctx.Principal, in BulkInput) (*BulkResult, error) {
	probe := Input{Area: in.Area, Number: "1", Kind: in.Kind}
	if err := clean(&probe); err != nil {
		return nil, err
	}
	in.Area, in.Kind, in.Prefix = probe.Area, probe.Kind, strings.TrimSpace(in.Prefix)
	if in.From < 1 || in.To < in.From {
		return nil, invalid("'from' must be 1 or more and 'to' must not be below it")
	}
	if in.To-in.From+1 > 200 {
		return nil, ErrBulkTooLarge
	}
	width := len(strconv.Itoa(in.To))
	res := &BulkResult{}
	for n := in.From; n <= in.To; n++ {
		number := fmt.Sprintf("%s%0*d", in.Prefix, width, n)
		if len(number) > 20 {
			return nil, invalid("slot numbers are too long; use a shorter prefix")
		}
		tag, err := s.pool.Exec(ctx, `INSERT INTO parking_slots (society_id, area, number, kind) VALUES ($1,$2,$3,$4) ON CONFLICT DO NOTHING`, p.SocietyID, in.Area, number, in.Kind)
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() == 1 {
			res.Created++
		} else {
			res.Skipped++
		}
	}
	s.audit.Log(ctx, audit.Entry{ActorID: &p.UserID, SocietyID: &p.SocietyID, Action: "parking.bulk", Entity: "parking_slot", EntityID: in.Area, Meta: map[string]any{"area": in.Area, "created": res.Created, "skipped": res.Skipped}})
	return res, nil
}

func (s *Service) Update(ctx context.Context, p authctx.Principal, id uuid.UUID, in Input) (*Slot, error) {
	if err := clean(&in); err != nil {
		return nil, err
	}
	cur, err := s.get(ctx, p.SocietyID, id)
	if err != nil {
		return nil, err
	}
	if cur.Allocated && in.Kind == "visitor" {
		return nil, ErrVisitor
	}
	_, err = s.pool.Exec(ctx, `UPDATE parking_slots SET area=$3, number=$4, kind=$5, note=$6, updated_at=now() WHERE id=$1 AND society_id=$2`, id, p.SocietyID, in.Area, in.Number, in.Kind, in.Note)
	if isDup(err) {
		return nil, ErrExists
	}
	if err != nil {
		return nil, err
	}
	s.audit.Log(ctx, audit.Entry{ActorID: &p.UserID, SocietyID: &p.SocietyID, Action: "parking.update", Entity: "parking_slot", EntityID: id.String(), Meta: map[string]any{"area": in.Area, "number": in.Number, "kind": in.Kind}})
	return s.get(ctx, p.SocietyID, id)
}

// Delete removes a slot that is not allocated.
func (s *Service) Delete(ctx context.Context, p authctx.Principal, id uuid.UUID) error {
	cur, err := s.get(ctx, p.SocietyID, id)
	if err != nil {
		return err
	}
	if cur.Allocated {
		return ErrAllocated
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM parking_slots WHERE id=$1 AND society_id=$2 AND unit_id IS NULL`, id, p.SocietyID); err != nil {
		return err
	}
	s.audit.Log(ctx, audit.Entry{ActorID: &p.UserID, SocietyID: &p.SocietyID, Action: "parking.delete", Entity: "parking_slot", EntityID: id.String(), Meta: map[string]any{"area": cur.Area, "number": cur.Number}})
	return nil
}

func label(sl *Slot) string { return sl.Area + " · " + sl.Number }

// Allocate gives a slot to a flat. A slot that is already allocated moves to the new flat.
func (s *Service) Allocate(ctx context.Context, p authctx.Principal, id, unit uuid.UUID) (*Slot, error) {
	cur, err := s.get(ctx, p.SocietyID, id)
	if err != nil {
		return nil, err
	}
	if cur.Kind == "visitor" {
		return nil, ErrVisitor
	}
	var ok bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM units WHERE id=$1 AND society_id=$2)`, unit, p.SocietyID).Scan(&ok); err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrFlatNotFound
	}
	if _, err := s.pool.Exec(ctx, `UPDATE parking_slots SET unit_id=$3, allocated_by=$4, allocated_at=now(), updated_at=now() WHERE id=$1 AND society_id=$2`, id, p.SocietyID, unit, p.MembershipID); err != nil {
		return nil, err
	}
	out, err := s.get(ctx, p.SocietyID, id)
	if err != nil {
		return nil, err
	}
	s.audit.Log(ctx, audit.Entry{ActorID: &p.UserID, SocietyID: &p.SocietyID, Action: "parking.allocate", Entity: "parking_slot", EntityID: id.String(), Meta: map[string]any{"slot": label(out), "flat": out.Flat}})
	// the old flat (if the slot moved) and the new flat find out in their inbox; no push sound for either
	if cur.UnitID != nil && *cur.UnitID != unit {
		s.tell(ctx, p.SocietyID, *cur.UnitID, "parking.released", "Parking slot removed", label(out)+" is no longer allocated to your flat.")
	}
	s.tell(ctx, p.SocietyID, unit, "parking.allocated", "Parking slot allocated", label(out)+" is now allocated to your flat "+out.Flat+".")
	return out, nil
}

// Release takes a slot back from its flat.
func (s *Service) Release(ctx context.Context, p authctx.Principal, id uuid.UUID) (*Slot, error) {
	cur, err := s.get(ctx, p.SocietyID, id)
	if err != nil {
		return nil, err
	}
	if !cur.Allocated {
		return nil, ErrAlreadyFree
	}
	if _, err := s.pool.Exec(ctx, `UPDATE parking_slots SET unit_id=NULL, allocated_by=NULL, allocated_at=NULL, updated_at=now() WHERE id=$1 AND society_id=$2`, id, p.SocietyID); err != nil {
		return nil, err
	}
	s.audit.Log(ctx, audit.Entry{ActorID: &p.UserID, SocietyID: &p.SocietyID, Action: "parking.release", Entity: "parking_slot", EntityID: id.String(), Meta: map[string]any{"slot": label(cur), "flat": cur.Flat}})
	s.tell(ctx, p.SocietyID, *cur.UnitID, "parking.released", "Parking slot removed", label(cur)+" is no longer allocated to your flat.")
	return s.get(ctx, p.SocietyID, id)
}

// tell puts a silent inbox note in front of everyone who lives in the flat.
func (s *Service) tell(ctx context.Context, soc, unit uuid.UUID, typ, title, body string) {
	if s.notes == nil {
		return
	}
	users := s.notes.UnitMembers(ctx, soc, unit)
	if len(users) == 0 {
		return
	}
	s.notes.Send(ctx, notify.Notification{SocietyID: soc, Users: users, Type: typ, Title: title, Body: body, Priority: notify.PrioritySilent})
}

// Mine is what a resident sees: the slots allocated to their flat.
func (s *Service) Mine(ctx context.Context, p authctx.Principal) ([]Slot, error) {
	var unit *uuid.UUID
	if err := s.pool.QueryRow(ctx, `SELECT unit_id FROM memberships WHERE id=$1 AND society_id=$2`, p.MembershipID, p.SocietyID).Scan(&unit); err != nil {
		return nil, err
	}
	out := []Slot{}
	if unit == nil {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT `+slotCols+slotFrom+` WHERE p.society_id=$1 AND p.unit_id=$2 ORDER BY p.kind, lower(p.area), length(p.number), p.number`, p.SocietyID, *unit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		sl, err := scanSlot(rows, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, sl)
	}
	return out, rows.Err()
}
