package visitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Nexora48/nexora-api/internal/platform/authctx"
)

func verifyKey(p authctx.Principal) string { return "visitor:verify:fail:" + p.UserID.String() }

// Verify looks a pass up by OTP or QR inside the guard's own society. It never changes state.
func (s *Service) Verify(ctx context.Context, p authctx.Principal, code, qr string) (*VerifyResult, error) {
	if n, _ := s.rdb.Get(ctx, verifyKey(p)).Int(); n >= verifyMaxFails {
		return nil, ErrVerifyLocked
	}
	notFound := func() (*VerifyResult, error) {
		n, _ := s.rdb.Incr(ctx, verifyKey(p)).Result()
		if n == 1 {
			s.rdb.Expire(ctx, verifyKey(p), verifyLockWindow)
		}
		return &VerifyResult{Reason: "NOT_FOUND"}, nil
	}

	var v *Visit
	var err error
	switch {
	case strings.TrimSpace(qr) != "":
		id, ok := s.parseQR(qr)
		if !ok {
			return notFound()
		}
		v, err = s.one(ctx, p.SocietyID, id, nil)
	default:
		// open passes win; otherwise the most recent visit that used the code (to explain why it fails)
		q := s.selectCols() + fromJoin + ` WHERE v.society_id=$1 AND v.pass_code=$2 ORDER BY (v.status='invited') DESC, v.created_at DESC LIMIT 1`
		v, err = s.scan(s.pool.QueryRow(ctx, q, p.SocietyID, strings.TrimSpace(code)))
	}
	if errors.Is(err, ErrNotFound) {
		return notFound()
	}
	if err != nil {
		return nil, err
	}
	s.rdb.Del(ctx, verifyKey(p))

	g := v.ForGate()
	now := time.Now()
	switch v.Status {
	case StatusInvited:
		if now.Before(v.ExpectedFrom.Add(-earlyGrace)) {
			return &VerifyResult{Reason: "NOT_YET_VALID", Visit: &g}, nil
		}
		return &VerifyResult{Valid: true, Visit: &g}, nil
	case StatusApproved:
		return &VerifyResult{Valid: true, Visit: &g}, nil
	case StatusExpired:
		return &VerifyResult{Reason: "EXPIRED", Visit: &g}, nil
	case StatusInside, StatusCompleted:
		return &VerifyResult{Reason: "ALREADY_USED", Visit: &g}, nil
	case StatusCancelled:
		return &VerifyResult{Reason: "CANCELLED", Visit: &g}, nil
	case StatusRejected:
		return &VerifyResult{Reason: "REJECTED", Visit: &g}, nil
	default: // waiting
		return &VerifyResult{Reason: "NOT_APPROVED", Visit: &g}, nil
	}
}

// Units lets the guard find a flat for a walk-in (no resident names are exposed).
func (s *Service) Units(ctx context.Context, p authctx.Principal, q string) ([]GateUnit, error) {
	args := []any{p.SocietyID}
	where := "u.society_id=$1"
	if q = strings.TrimSpace(q); q != "" {
		args = append(args, "%"+q+"%")
		where += " AND u.number ILIKE $2"
	}
	rows, err := s.pool.Query(ctx, `
		SELECT u.id, u.number, b.code,
		       EXISTS (SELECT 1 FROM memberships m WHERE m.unit_id=u.id AND m.status='active' AND m.role_key IN ('resident_owner','resident_tenant'))
		FROM units u JOIN blocks b ON b.id=u.block_id WHERE `+where+` ORDER BY b.code, u.number LIMIT 20`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GateUnit{}
	for rows.Next() {
		var g GateUnit
		if err := rows.Scan(&g.ID, &g.Number, &g.BlockCode, &g.Occupied); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// WalkIn registers a visitor at the gate; the flat's residents must approve.
func (s *Service) WalkIn(ctx context.Context, p authctx.Principal, in WalkInInput) (*Visit, error) {
	var ok bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM units WHERE id=$1 AND society_id=$2)`, in.UnitID, p.SocietyID).Scan(&ok); err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrUnitNotFound
	}
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM memberships WHERE unit_id=$1 AND society_id=$2 AND status='active' AND role_key IN ('resident_owner','resident_tenant'))`,
		in.UnitID, p.SocietyID).Scan(&ok); err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNoResident
	}
	phone, err := cleanPhone(in.VisitorPhone)
	if err != nil {
		return nil, err
	}
	vehicle, err := cleanVehicle(in.VehicleNumber)
	if err != nil {
		return nil, err
	}
	count := in.VisitorCount
	if count == 0 {
		count = 1
	}
	now := time.Now()
	var needPhoto bool
	_ = s.pool.QueryRow(ctx, `SELECT walkin_photo_required FROM societies WHERE id=$1`, p.SocietyID).Scan(&needPhoto)
	needPhoto = needPhoto && s.blobs != nil

	// the flat's own delivery/cab rule may let the visitor in without asking
	var dec RuleDecision
	if s.rules != nil && (in.Purpose == "delivery" || in.Purpose == "cab") {
		if d, err := s.rules.Resolve(ctx, p.SocietyID, in.UnitID, in.Purpose, cleanText(in.Company)); err == nil {
			dec = d
		}
	}
	auto := dec.Mode == "auto_approve" || dec.Mode == "leave_at_gate"
	var handover *string
	if dec.Mode == "leave_at_gate" {
		h := "leave_at_gate"
		handover = &h
	}
	if auto {
		needPhoto = false // nobody is waiting on it; the photo can still be added
	}
	status := StatusWaiting
	if auto {
		status = StatusApproved
	}
	var id uuid.UUID
	err = s.pool.QueryRow(ctx, `
		INSERT INTO visits (society_id, unit_id, created_by_membership_id, visitor_name, visitor_phone, purpose, company, vehicle_number,
		                    visitor_count, source, status, expected_from, expected_until, awaiting_photo, gate_id, decided_by_rule, handover, decided_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'walk_in',$12,$10,$11,$13,
		        (SELECT gate_id FROM guard_shifts WHERE membership_id=$3 AND ended_at IS NULL), $14, $15, CASE WHEN $14 THEN now() END) RETURNING id`,
		p.SocietyID, in.UnitID, p.MembershipID, strings.TrimSpace(in.VisitorName), phone, in.Purpose, cleanText(in.Company), vehicle, count,
		now, now.Add(walkInWindow), status, needPhoto, auto, handover).Scan(&id)
	if err != nil {
		return nil, err
	}
	if dec.ExpectationID != nil {
		_ = s.rules.Consume(ctx, *dec.ExpectationID)
	}
	s.logv(ctx, p, "visit.walk_in", id, map[string]any{"unit_id": in.UnitID.String(), "purpose": in.Purpose, "auto": dec.Mode})
	v, err := s.one(ctx, p.SocietyID, id, nil)
	if err == nil {
		switch {
		case auto:
			s.emit(ctx, "visit.arrived", v)
		case !needPhoto: // with a required photo the resident is asked once the photo is in
			s.emit(ctx, "visit.requested", v)
		}
	}
	return v, err
}

// GateList is the guard's live feed (waiting / approved / inside).
func (s *Service) GateList(ctx context.Context, p authctx.Principal, status string) ([]Visit, error) {
	where := []string{"v.society_id=$1", "v.created_at > now() - interval '48 hours'"}
	args := []any{p.SocietyID}
	e := s.eff()
	if status == "" {
		where = append(where, e+" IN ('waiting','approved','inside')")
	} else {
		args = append(args, status)
		where = append(where, fmt.Sprintf("%s = $%d", e, len(args)))
	}
	out, _, err := s.many(ctx, where, args, "v.created_at DESC, v.id", 100, 0)
	return out, err
}

func (s *Service) CheckIn(ctx context.Context, p authctx.Principal, id uuid.UUID) (*Visit, error) {
	tag, err := s.pool.Exec(ctx, fmt.Sprintf(`
		UPDATE visits SET status='inside', entered_at=now(), checked_in_by=$3, updated_at=now(),
		       gate_id=COALESCE((SELECT gate_id FROM guard_shifts WHERE membership_id=$3 AND ended_at IS NULL), gate_id)
		WHERE id=$1 AND society_id=$2 AND (
		      (status='invited'  AND now() BETWEEN expected_from - interval '%d seconds' AND expected_until)
		   OR (status='approved' AND expected_until >= now()))`, int(earlyGrace.Seconds())), id, p.SocietyID, p.MembershipID)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		v, err := s.one(ctx, p.SocietyID, id, nil)
		if err != nil {
			return nil, err
		}
		if v.Status == StatusInvited { // still invited: only the window can be wrong
			return nil, passNotValid("NOT_YET_VALID")
		}
		return nil, invalidState(v.Status)
	}
	s.logv(ctx, p, "visit.check_in", id, nil)
	v, err := s.one(ctx, p.SocietyID, id, nil)
	s.emit(ctx, "visit.checked_in", v)
	return v, err
}

func (s *Service) CheckOut(ctx context.Context, p authctx.Principal, id uuid.UUID) (*Visit, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE visits SET status='completed', exited_at=now(), checked_out_by=$3, updated_at=now()
		WHERE id=$1 AND society_id=$2 AND status='inside'`, id, p.SocietyID, p.MembershipID)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, s.classify(ctx, p.SocietyID, id, nil)
	}
	s.logv(ctx, p, "visit.check_out", id, nil)
	v, err := s.one(ctx, p.SocietyID, id, nil)
	s.emit(ctx, "visit.checked_out", v)
	return v, err
}
