package resident

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nexora48/nexora-api/internal/audit"
	"github.com/Nexora48/nexora-api/internal/platform/database"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
)

var (
	ErrUnitNotFound    = httpx.NewError(http.StatusNotFound, "UNIT_NOT_FOUND", "flat not found in this society")
	ErrNotFound        = httpx.NewError(http.StatusNotFound, "NOT_FOUND", "resident not found")
	ErrAlreadyResident = httpx.NewError(http.StatusConflict, "ALREADY_RESIDENT", "this person is already a resident of this society")
	ErrUnitRoleTaken   = httpx.NewError(http.StatusConflict, "UNIT_ROLE_TAKEN", "this flat already has an active owner/tenant of that type")
	ErrEmailTaken      = httpx.NewError(http.StatusConflict, "EMAIL_TAKEN", "this email belongs to another user")
	ErrNoHead          = httpx.NewError(http.StatusConflict, "NO_HEAD", "add the flat's owner or tenant before adding family")
	ErrUserShared      = httpx.NewError(http.StatusForbidden, "USER_SHARED", "name and email cannot be changed: this person belongs to other societies too")
	ErrInvalidState    = httpx.NewError(http.StatusConflict, "INVALID_STATE", "this action is not allowed in the resident's current status")
)

var (
	phoneRe = regexp.MustCompile(`^[6-9]\d{9}$`)
	dateRe  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// NormalizePhone strips spaces/punctuation and a +91 / 0 prefix.
func NormalizePhone(s string) string {
	var b strings.Builder
	for _, c := range s {
		if c >= '0' && c <= '9' {
			b.WriteRune(c)
		}
	}
	d := b.String()
	if len(d) == 12 && strings.HasPrefix(d, "91") {
		d = d[2:]
	}
	if len(d) == 11 && strings.HasPrefix(d, "0") {
		d = d[1:]
	}
	return d
}

func ValidPhone(p string) bool { return phoneRe.MatchString(p) }

func validDate(s *string) bool {
	if s == nil || *s == "" {
		return true
	}
	if !dateRe.MatchString(*s) {
		return false
	}
	_, err := time.Parse("2006-01-02", *s)
	return err == nil
}

// nullDate converts "" / nil to SQL NULL (the query casts with ::date).
func nullDate(s *string) any {
	if s == nil || *s == "" {
		return nil
	}
	return *s
}

func nullStr(s *string) any {
	if s == nil || strings.TrimSpace(*s) == "" {
		return nil
	}
	v := strings.TrimSpace(*s)
	return v
}

func last4(s string) string {
	var b []rune
	for _, c := range s {
		if (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') {
			b = append(b, c)
		}
	}
	if len(b) > 4 {
		b = b[len(b)-4:]
	}
	return strings.ToUpper(string(b))
}

type Service struct {
	pool  *pgxpool.Pool
	audit *audit.Logger
}

func NewService(pool *pgxpool.Pool, a *audit.Logger) *Service { return &Service{pool: pool, audit: a} }

const residentRoles = `('resident_owner','resident_tenant','family_member')`

const statusExpr = `CASE WHEN m.status='active' AND u.last_login_at IS NULL THEN 'pending' ELSE m.status END`

// ── List ─────────────────────────────────────────────────────────────────────

func (s *Service) List(ctx context.Context, societyID uuid.UUID, f ListFilter) ([]Resident, int, error) {
	args := []any{societyID}
	where := []string{"m.society_id = $1", "m.role_key IN " + residentRoles}
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, strings.ReplaceAll(cond, "?", "$"+itoa(len(args))))
	}
	if role, ok := typeToRole[f.Type]; ok {
		add("m.role_key = ?", role)
	}
	if f.Status != "" {
		add(statusExpr+" = ?", f.Status)
	}
	if f.UnitID != nil {
		add("m.unit_id = ?", *f.UnitID)
	}
	if f.Query != "" {
		args = append(args, "%"+f.Query+"%")
		n := "$" + itoa(len(args))
		where = append(where, "(u.full_name ILIKE "+n+" OR u.phone ILIKE "+n+" OR u.email ILIKE "+n+" OR un.number ILIKE "+n+")")
	}
	base := `FROM memberships m JOIN users u ON u.id = m.user_id
	         LEFT JOIN units un ON un.id = m.unit_id LEFT JOIN blocks b ON b.id = un.block_id
	         WHERE ` + strings.Join(where, " AND ")
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) `+base, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, f.Limit, f.Offset)
	rows, err := s.pool.Query(ctx, `
		SELECT m.id, u.id, u.full_name, u.phone, u.email, m.unit_id, un.number, b.code, m.role_key, `+statusExpr+`,
		       to_char(m.move_in_date,'YYYY-MM-DD'), m.created_at `+base+
		` ORDER BY m.created_at DESC, m.id LIMIT $`+itoa(len(args)-1)+` OFFSET $`+itoa(len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Resident{}
	for rows.Next() {
		var r Resident
		var role string
		if err := rows.Scan(&r.MembershipID, &r.UserID, &r.FullName, &r.Phone, &r.Email, &r.UnitID, &r.UnitNumber, &r.BlockCode,
			&role, &r.Status, &r.MoveInDate, &r.JoinedAt); err != nil {
			return nil, 0, err
		}
		r.Type = roleToType(role)
		out = append(out, r)
	}
	return out, total, rows.Err()
}

func itoa(n int) string { return strconv.Itoa(n) }

// ── Detail ───────────────────────────────────────────────────────────────────

func (s *Service) Get(ctx context.Context, societyID, membershipID uuid.UUID) (*Detail, error) {
	var d Detail
	var role string
	err := s.pool.QueryRow(ctx, `
		SELECT m.id, u.id, u.full_name, u.phone, u.email, m.unit_id, un.number, b.code, m.role_key, `+statusExpr+`,
		       to_char(m.move_in_date,'YYYY-MM-DD'), m.created_at,
		       to_char(m.agreement_end_date,'YYYY-MM-DD'), to_char(u.date_of_birth,'YYYY-MM-DD'), u.gov_id_type, u.gov_id_last4,
		       m.app_access, m.can_invite_visitors, m.can_pay_maintenance, m.sos_enabled, u.last_login_at,
		       EXISTS (SELECT 1 FROM memberships o WHERE o.user_id = u.id AND o.society_id <> m.society_id)
		FROM memberships m JOIN users u ON u.id = m.user_id
		LEFT JOIN units un ON un.id = m.unit_id LEFT JOIN blocks b ON b.id = un.block_id
		WHERE m.id = $1 AND m.society_id = $2 AND m.role_key IN `+residentRoles, membershipID, societyID).
		Scan(&d.MembershipID, &d.UserID, &d.FullName, &d.Phone, &d.Email, &d.UnitID, &d.UnitNumber, &d.BlockCode, &role, &d.Status,
			&d.MoveInDate, &d.JoinedAt, &d.AgreementEndDate, &d.DateOfBirth, &d.GovIDType, &d.GovIDLast4,
			&d.AppAccess, &d.CanInviteVisitors, &d.CanPayMaintenance, &d.SOSEnabled, &d.LastLoginAt, &d.UserShared)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	d.Type = roleToType(role)
	d.Family, d.Vehicles = []FamilyMember{}, []Vehicle{}

	var ec EmergencyContact
	if err := s.pool.QueryRow(ctx, `SELECT name, phone, relation FROM emergency_contacts WHERE user_id=$1`, d.UserID).
		Scan(&ec.Name, &ec.Phone, &ec.Relation); err == nil {
		d.EmergencyContact = &ec
	}
	if d.UnitID != nil && role != RoleFamily {
		rows, err := s.pool.Query(ctx, `
			SELECT id, name, relation, phone, to_char(date_of_birth,'YYYY-MM-DD'), login_membership_id IS NOT NULL
			FROM family_members WHERE society_id=$1 AND head_membership_id=$2 ORDER BY created_at`, societyID, membershipID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var f FamilyMember
			if err := rows.Scan(&f.ID, &f.Name, &f.Relation, &f.Phone, &f.DateOfBirth, &f.HasLogin); err != nil {
				rows.Close()
				return nil, err
			}
			d.Family = append(d.Family, f)
		}
		rows.Close()
		vr, err := s.pool.Query(ctx, `
			SELECT id, vehicle_type, number, make_model, color, sticker_no
			FROM vehicles WHERE society_id=$1 AND owner_user_id=$2 AND unit_id=$3 ORDER BY created_at`, societyID, d.UserID, *d.UnitID)
		if err != nil {
			return nil, err
		}
		for vr.Next() {
			var v Vehicle
			if err := vr.Scan(&v.ID, &v.VehicleType, &v.Number, &v.MakeModel, &v.Color, &v.StickerNo); err != nil {
				vr.Close()
				return nil, err
			}
			d.Vehicles = append(d.Vehicles, v)
		}
		vr.Close()
	}
	return &d, nil
}

// ── Create ───────────────────────────────────────────────────────────────────

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func (s *Service) Create(ctx context.Context, societyID, actorID uuid.UUID, in CreateInput) (*Detail, error) {
	role, ok := typeToRole[in.Type]
	if !ok {
		return nil, httpx.NewError(http.StatusBadRequest, "VALIDATION", "type must be owner, tenant or family")
	}
	in.Phone = NormalizePhone(in.Phone)
	if !ValidPhone(in.Phone) {
		return nil, httpx.NewError(http.StatusBadRequest, "INVALID_PHONE", "enter a valid 10-digit mobile number")
	}
	for _, d := range []*string{in.MoveInDate, in.AgreementEndDate, in.DateOfBirth} {
		if !validDate(d) {
			return nil, httpx.NewError(http.StatusBadRequest, "VALIDATION", "dates must be YYYY-MM-DD")
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var unitID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM units WHERE id=$1 AND society_id=$2`, in.UnitID, societyID).Scan(&unitID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrUnitNotFound
		}
		return nil, err
	}

	// family needs an owner/tenant head in the same flat
	var headID *uuid.UUID
	if role == RoleFamily {
		var h uuid.UUID
		err := tx.QueryRow(ctx, `
			SELECT id FROM memberships WHERE unit_id=$1 AND society_id=$2 AND status IN ('active','suspended')
			  AND role_key IN ('resident_tenant','resident_owner')
			ORDER BY (role_key='resident_tenant') DESC LIMIT 1`, unitID, societyID).Scan(&h)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNoHead
		}
		if err != nil {
			return nil, err
		}
		headID = &h
	}

	// reuse the person by phone (one person across societies) or create
	var userID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM users WHERE phone=$1`, in.Phone).Scan(&userID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		var last *string
		var gtype *string
		if in.GovIDNumber != nil && strings.TrimSpace(*in.GovIDNumber) != "" {
			l := last4(*in.GovIDNumber)
			last, gtype = &l, in.GovIDType
		}
		err = tx.QueryRow(ctx, `
			INSERT INTO users (full_name, phone, email, date_of_birth, gov_id_type, gov_id_last4)
			VALUES ($1,$2,$3,$4::date,$5,$6) RETURNING id`,
			strings.TrimSpace(in.FullName), in.Phone, nullStr(in.Email), nullDate(in.DateOfBirth), gtype, last).Scan(&userID)
		if database.IsUnique(err, "") {
			return nil, ErrEmailTaken
		}
		if err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	}

	var existing int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM memberships WHERE user_id=$1 AND society_id=$2 AND status IN ('active','suspended')
		  AND role_key IN `+residentRoles, userID, societyID).Scan(&existing); err != nil {
		return nil, err
	}
	if existing > 0 {
		return nil, ErrAlreadyResident
	}

	// re-adding after move-out to the same flat re-activates the old row
	var mid uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM memberships WHERE user_id=$1 AND society_id=$2 AND unit_id=$3 AND status='inactive'`,
		userID, societyID, unitID).Scan(&mid)
	flags := []any{boolOr(in.AppAccess, true), boolOr(in.CanInviteVisitors, true), boolOr(in.CanPayMaintenance, false), boolOr(in.SOSEnabled, true)}
	if err == nil {
		_, err = tx.Exec(ctx, `
			UPDATE memberships SET status='active', ended_at=NULL, role_key=$2, move_in_date=$3::date, agreement_end_date=$4::date,
			       app_access=$5, can_invite_visitors=$6, can_pay_maintenance=$7, sos_enabled=$8 WHERE id=$1`,
			append([]any{mid, role, nullDate(in.MoveInDate), nullDate(in.AgreementEndDate)}, flags...)...)
	} else if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `
			INSERT INTO memberships (user_id, society_id, unit_id, role_key, move_in_date, agreement_end_date,
			                         app_access, can_invite_visitors, can_pay_maintenance, sos_enabled)
			VALUES ($1,$2,$3,$4,$5::date,$6::date,$7,$8,$9,$10) RETURNING id`,
			append([]any{userID, societyID, unitID, role, nullDate(in.MoveInDate), nullDate(in.AgreementEndDate)}, flags...)...).Scan(&mid)
	}
	if database.IsUnique(err, "memberships_unit_role_active_uq") {
		return nil, ErrUnitRoleTaken
	}
	if err != nil {
		return nil, err
	}

	if in.Emergency != nil && in.Emergency.Name != "" && in.Emergency.Phone != "" {
		if _, err := tx.Exec(ctx, `
			INSERT INTO emergency_contacts (user_id, name, phone, relation) VALUES ($1,$2,$3,$4)
			ON CONFLICT (user_id) DO UPDATE SET name=EXCLUDED.name, phone=EXCLUDED.phone, relation=EXCLUDED.relation`,
			userID, in.Emergency.Name, in.Emergency.Phone, in.Emergency.Relation); err != nil {
			return nil, err
		}
	}
	if headID != nil {
		rel := in.Relation
		if rel == "" {
			rel = "Family"
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO family_members (society_id, unit_id, head_membership_id, name, relation, phone, date_of_birth, login_membership_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7::date,$8)`,
			societyID, unitID, *headID, strings.TrimSpace(in.FullName), rel, in.Phone, nullDate(in.DateOfBirth), mid); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	s.audit.Log(ctx, audit.Entry{ActorID: &actorID, SocietyID: &societyID, Action: "resident.create", Entity: "membership",
		EntityID: mid.String(), Meta: map[string]any{"type": in.Type, "unit_id": unitID.String()}})
	return s.Get(ctx, societyID, mid)
}

// ── Update ───────────────────────────────────────────────────────────────────

func (s *Service) Update(ctx context.Context, societyID, actorID, membershipID uuid.UUID, in UpdateInput) (*Detail, error) {
	for _, d := range []*string{in.MoveInDate, in.AgreementEndDate, in.DateOfBirth} {
		if !validDate(d) {
			return nil, httpx.NewError(http.StatusBadRequest, "VALIDATION", "dates must be YYYY-MM-DD")
		}
	}
	cur, err := s.Get(ctx, societyID, membershipID)
	if err != nil {
		return nil, err
	}
	nameChange := in.FullName != nil && strings.TrimSpace(*in.FullName) != cur.FullName
	emailChange := in.Email != nil && (cur.Email == nil || strings.TrimSpace(*in.Email) != *cur.Email)
	if cur.UserShared && (nameChange || emailChange) {
		return nil, ErrUserShared
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if nameChange {
		if _, err := tx.Exec(ctx, `UPDATE users SET full_name=$2, updated_at=now() WHERE id=$1`, cur.UserID, strings.TrimSpace(*in.FullName)); err != nil {
			return nil, err
		}
	}
	if emailChange {
		_, err := tx.Exec(ctx, `UPDATE users SET email=$2, updated_at=now() WHERE id=$1`, cur.UserID, nullStr(in.Email))
		if database.IsUnique(err, "") {
			return nil, ErrEmailTaken
		}
		if err != nil {
			return nil, err
		}
	}
	if in.DateOfBirth != nil {
		if _, err := tx.Exec(ctx, `UPDATE users SET date_of_birth=$2::date WHERE id=$1`, cur.UserID, nullDate(in.DateOfBirth)); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE memberships SET
		  move_in_date        = CASE WHEN $3::boolean THEN $4::date ELSE move_in_date END,
		  agreement_end_date  = CASE WHEN $5::boolean THEN $6::date ELSE agreement_end_date END,
		  app_access          = COALESCE($7,  app_access),
		  can_invite_visitors = COALESCE($8,  can_invite_visitors),
		  can_pay_maintenance = COALESCE($9,  can_pay_maintenance),
		  sos_enabled         = COALESCE($10, sos_enabled)
		WHERE id=$1 AND society_id=$2`,
		membershipID, societyID, in.MoveInDate != nil, nullDate(in.MoveInDate), in.AgreementEndDate != nil, nullDate(in.AgreementEndDate),
		in.AppAccess, in.CanInviteVisitors, in.CanPayMaintenance, in.SOSEnabled); err != nil {
		return nil, err
	}
	if in.Emergency != nil {
		if in.Emergency.Name == "" && in.Emergency.Phone == "" {
			_, err = tx.Exec(ctx, `DELETE FROM emergency_contacts WHERE user_id=$1`, cur.UserID)
		} else {
			_, err = tx.Exec(ctx, `
				INSERT INTO emergency_contacts (user_id, name, phone, relation) VALUES ($1,$2,$3,$4)
				ON CONFLICT (user_id) DO UPDATE SET name=EXCLUDED.name, phone=EXCLUDED.phone, relation=EXCLUDED.relation`,
				cur.UserID, in.Emergency.Name, in.Emergency.Phone, in.Emergency.Relation)
		}
		if err != nil {
			return nil, err
		}
	}
	if in.AppAccess != nil && !*in.AppAccess {
		if err := revokeTokens(ctx, tx, membershipID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	s.audit.Log(ctx, audit.Entry{ActorID: &actorID, SocietyID: &societyID, Action: "resident.update", Entity: "membership", EntityID: membershipID.String()})
	return s.Get(ctx, societyID, membershipID)
}
