// Package household is resident self-service: profile, emergency contact, family and vehicles of the caller's flat.
package household

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nexora48/nexora-api/internal/audit"
	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/database"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
	"github.com/Nexora48/nexora-api/internal/resident"
)

var (
	ErrNoUnit          = httpx.NewError(http.StatusConflict, "NO_UNIT", "your account is not linked to a flat")
	ErrNotFound        = httpx.NewError(http.StatusNotFound, "NOT_FOUND", "not found")
	ErrUserShared      = httpx.NewError(http.StatusForbidden, "USER_SHARED", "name and email cannot be changed: your number is linked to other societies; contact support")
	ErrEmailTaken      = httpx.NewError(http.StatusConflict, "EMAIL_TAKEN", "this email belongs to another user")
	ErrVehicleExists   = httpx.NewError(http.StatusConflict, "VEHICLE_EXISTS", "a vehicle with this number is already registered in the society")
	ErrAlreadyResident = httpx.NewError(http.StatusConflict, "ALREADY_RESIDENT", "this person is already a resident of this society")
	ErrInvalidPhone    = httpx.NewError(http.StatusBadRequest, "INVALID_PHONE", "enter a valid 10-digit mobile number")
	ErrInvalidVehicle  = httpx.NewError(http.StatusBadRequest, "VALIDATION", "vehicle number must be 4-12 letters/digits")
)

var vehicleRe = regexp.MustCompile(`^[A-Z0-9]{4,12}$`)

// NormalizeVehicle upper-cases and strips spaces/hyphens.
func NormalizeVehicle(s string) string {
	r := strings.NewReplacer(" ", "", "-", "")
	return strings.ToUpper(r.Replace(strings.TrimSpace(s)))
}

type Service struct {
	pool  *pgxpool.Pool
	audit *audit.Logger
}

func NewService(pool *pgxpool.Pool, a *audit.Logger) *Service { return &Service{pool: pool, audit: a} }

func (s *Service) unitOf(ctx context.Context, p authctx.Principal) (uuid.UUID, error) {
	var unit *uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT unit_id FROM memberships WHERE id=$1 AND user_id=$2 AND society_id=$3`,
		p.MembershipID, p.UserID, p.SocietyID).Scan(&unit)
	if err != nil || unit == nil {
		return uuid.Nil, ErrNoUnit
	}
	return *unit, nil
}

func (s *Service) isShared(ctx context.Context, userID, societyID uuid.UUID) (bool, error) {
	var shared bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM memberships WHERE user_id=$1 AND society_id<>$2)`, userID, societyID).Scan(&shared)
	return shared, err
}

// ── Profile & emergency contact ──────────────────────────────────────────────

func (s *Service) UpdateProfile(ctx context.Context, p authctx.Principal, name, email *string) error {
	var curName string
	var curEmail *string
	if err := s.pool.QueryRow(ctx, `SELECT full_name, email FROM users WHERE id=$1`, p.UserID).Scan(&curName, &curEmail); err != nil {
		return err
	}
	nameChange := name != nil && strings.TrimSpace(*name) != curName
	emailChange := email != nil && (curEmail == nil || strings.TrimSpace(*email) != *curEmail)
	if !nameChange && !emailChange {
		return nil
	}
	if shared, err := s.isShared(ctx, p.UserID, p.SocietyID); err != nil {
		return err
	} else if shared {
		return ErrUserShared
	}
	if nameChange {
		if _, err := s.pool.Exec(ctx, `UPDATE users SET full_name=$2, updated_at=now() WHERE id=$1`, p.UserID, strings.TrimSpace(*name)); err != nil {
			return err
		}
	}
	if emailChange {
		var v any
		if t := strings.TrimSpace(*email); t != "" {
			v = t
		}
		if _, err := s.pool.Exec(ctx, `UPDATE users SET email=$2, updated_at=now() WHERE id=$1`, p.UserID, v); err != nil {
			if database.IsUnique(err, "") {
				return ErrEmailTaken
			}
			return err
		}
	}
	s.audit.Log(ctx, audit.Entry{ActorID: &p.UserID, SocietyID: &p.SocietyID, Action: "profile.update", Entity: "user", EntityID: p.UserID.String()})
	return nil
}

func (s *Service) GetEmergency(ctx context.Context, userID uuid.UUID) (*resident.EmergencyContact, error) {
	var e resident.EmergencyContact
	err := s.pool.QueryRow(ctx, `SELECT name, phone, relation FROM emergency_contacts WHERE user_id=$1`, userID).Scan(&e.Name, &e.Phone, &e.Relation)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &e, err
}

func (s *Service) SetEmergency(ctx context.Context, userID uuid.UUID, e resident.EmergencyContact) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO emergency_contacts (user_id, name, phone, relation) VALUES ($1,$2,$3,$4)
		ON CONFLICT (user_id) DO UPDATE SET name=EXCLUDED.name, phone=EXCLUDED.phone, relation=EXCLUDED.relation`,
		userID, e.Name, e.Phone, e.Relation)
	return err
}

// ── Household view ───────────────────────────────────────────────────────────

type Member struct {
	Name string `json:"name"`
	Type string `json:"type"` // owner | tenant | family
	Me   bool   `json:"me"`
}

type UnitView struct {
	UnitID       uuid.UUID `json:"unit_id"`
	Number       string    `json:"number"`
	Block        string    `json:"block"`
	Floor        int       `json:"floor"`
	UnitType     string    `json:"unit_type"`
	Members      []Member  `json:"members"`
	VehicleCount int       `json:"vehicle_count"`
	MembersCount int       `json:"members_count"`
}

func (s *Service) Unit(ctx context.Context, p authctx.Principal) (*UnitView, error) {
	unitID, err := s.unitOf(ctx, p)
	if err != nil {
		return nil, err
	}
	v := UnitView{UnitID: unitID, Members: []Member{}}
	if err := s.pool.QueryRow(ctx, `
		SELECT u.number, b.code, u.floor, u.unit_type,
		       (SELECT count(*) FROM vehicles WHERE society_id=u.society_id AND unit_id=u.id)
		FROM units u JOIN blocks b ON b.id=u.block_id WHERE u.id=$1 AND u.society_id=$2`, unitID, p.SocietyID).
		Scan(&v.Number, &v.Block, &v.Floor, &v.UnitType, &v.VehicleCount); err != nil {
		return nil, ErrNotFound
	}
	rows, err := s.pool.Query(ctx, `
		SELECT us.full_name, m.role_key, m.user_id = $3
		FROM memberships m JOIN users us ON us.id=m.user_id
		WHERE m.society_id=$1 AND m.unit_id=$2 AND m.status IN ('active','suspended')
		  AND m.role_key IN ('resident_owner','resident_tenant','family_member')
		ORDER BY m.role_key DESC, us.full_name`, p.SocietyID, unitID, p.UserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var m Member
		var role string
		if err := rows.Scan(&m.Name, &role, &m.Me); err != nil {
			return nil, err
		}
		switch role {
		case resident.RoleOwner:
			m.Type = "owner"
		case resident.RoleTenant:
			m.Type = "tenant"
		default:
			m.Type = "family"
		}
		v.Members = append(v.Members, m)
	}
	v.MembersCount = len(v.Members)
	return &v, rows.Err()
}

// ── Family ───────────────────────────────────────────────────────────────────

type FamilyInput struct {
	Name        string
	Relation    string
	Phone       *string
	DateOfBirth *string
}

func nullDate(s *string) any {
	if s == nil || *s == "" {
		return nil
	}
	return *s
}

func (s *Service) ListFamily(ctx context.Context, p authctx.Principal) ([]resident.FamilyMember, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, relation, phone, to_char(date_of_birth,'YYYY-MM-DD'), login_membership_id IS NOT NULL
		FROM family_members WHERE society_id=$1 AND head_membership_id=$2 ORDER BY created_at`, p.SocietyID, p.MembershipID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []resident.FamilyMember{}
	for rows.Next() {
		var f resident.FamilyMember
		if err := rows.Scan(&f.ID, &f.Name, &f.Relation, &f.Phone, &f.DateOfBirth, &f.HasLogin); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Service) AddFamily(ctx context.Context, p authctx.Principal, in FamilyInput) (*resident.FamilyMember, error) {
	unitID, err := s.unitOf(ctx, p)
	if err != nil {
		return nil, err
	}
	phone := ""
	if in.Phone != nil && strings.TrimSpace(*in.Phone) != "" {
		phone = resident.NormalizePhone(*in.Phone)
		if !resident.ValidPhone(phone) {
			return nil, ErrInvalidPhone
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var loginMem *uuid.UUID
	var phoneArg any
	if phone != "" {
		phoneArg = phone
		var userID uuid.UUID
		err := tx.QueryRow(ctx, `SELECT id FROM users WHERE phone=$1`, phone).Scan(&userID)
		if errors.Is(err, pgx.ErrNoRows) {
			err = tx.QueryRow(ctx, `INSERT INTO users (full_name, phone, date_of_birth) VALUES ($1,$2,$3::date) RETURNING id`,
				strings.TrimSpace(in.Name), phone, nullDate(in.DateOfBirth)).Scan(&userID)
		}
		if err != nil {
			return nil, err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM memberships WHERE user_id=$1 AND society_id=$2 AND status IN ('active','suspended')
			AND role_key IN ('resident_owner','resident_tenant','family_member')`, userID, p.SocietyID).Scan(&n); err != nil {
			return nil, err
		}
		if n > 0 {
			return nil, ErrAlreadyResident
		}
		var mid uuid.UUID
		err = tx.QueryRow(ctx, `SELECT id FROM memberships WHERE user_id=$1 AND society_id=$2 AND unit_id=$3 AND status='inactive'`,
			userID, p.SocietyID, unitID).Scan(&mid)
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE memberships SET status='active', ended_at=NULL, role_key='family_member' WHERE id=$1`, mid)
		} else if errors.Is(err, pgx.ErrNoRows) {
			err = tx.QueryRow(ctx, `INSERT INTO memberships (user_id, society_id, unit_id, role_key) VALUES ($1,$2,$3,'family_member') RETURNING id`,
				userID, p.SocietyID, unitID).Scan(&mid)
		}
		if err != nil {
			return nil, err
		}
		loginMem = &mid
	}
	var f resident.FamilyMember
	err = tx.QueryRow(ctx, `
		INSERT INTO family_members (society_id, unit_id, head_membership_id, name, relation, phone, date_of_birth, login_membership_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7::date,$8)
		RETURNING id, name, relation, phone, to_char(date_of_birth,'YYYY-MM-DD'), login_membership_id IS NOT NULL`,
		p.SocietyID, unitID, p.MembershipID, strings.TrimSpace(in.Name), strings.TrimSpace(in.Relation), phoneArg, nullDate(in.DateOfBirth), loginMem).
		Scan(&f.ID, &f.Name, &f.Relation, &f.Phone, &f.DateOfBirth, &f.HasLogin)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	s.audit.Log(ctx, audit.Entry{ActorID: &p.UserID, SocietyID: &p.SocietyID, Action: "family.add", Entity: "family_member", EntityID: f.ID.String()})
	return &f, nil
}

func (s *Service) UpdateFamily(ctx context.Context, p authctx.Principal, id uuid.UUID, in FamilyInput) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE family_members SET name=$4, relation=$5, date_of_birth=$6::date
		WHERE id=$1 AND society_id=$2 AND head_membership_id=$3`,
		id, p.SocietyID, p.MembershipID, strings.TrimSpace(in.Name), strings.TrimSpace(in.Relation), nullDate(in.DateOfBirth))
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Service) DeleteFamily(ctx context.Context, p authctx.Principal, id uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var login *uuid.UUID
	err = tx.QueryRow(ctx, `DELETE FROM family_members WHERE id=$1 AND society_id=$2 AND head_membership_id=$3 RETURNING login_membership_id`,
		id, p.SocietyID, p.MembershipID).Scan(&login)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if login != nil {
		if _, err := tx.Exec(ctx, `UPDATE memberships SET status='inactive', ended_at=now() WHERE id=$1`, *login); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at=now() WHERE membership_id=$1 AND revoked_at IS NULL`, *login); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.audit.Log(ctx, audit.Entry{ActorID: &p.UserID, SocietyID: &p.SocietyID, Action: "family.remove", Entity: "family_member", EntityID: id.String()})
	return nil
}

// ── Vehicles ─────────────────────────────────────────────────────────────────

type VehicleInput struct {
	VehicleType string
	Number      string
	MakeModel   string
	Color       string
	StickerNo   *string
}

func (s *Service) ListVehicles(ctx context.Context, p authctx.Principal) ([]resident.Vehicle, error) {
	unitID, err := s.unitOf(ctx, p)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, vehicle_type, number, make_model, color, sticker_no FROM vehicles
		WHERE society_id=$1 AND unit_id=$2 ORDER BY created_at`, p.SocietyID, unitID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []resident.Vehicle{}
	for rows.Next() {
		var v resident.Vehicle
		if err := rows.Scan(&v.ID, &v.VehicleType, &v.Number, &v.MakeModel, &v.Color, &v.StickerNo); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Service) AddVehicle(ctx context.Context, p authctx.Principal, in VehicleInput) (*resident.Vehicle, error) {
	unitID, err := s.unitOf(ctx, p)
	if err != nil {
		return nil, err
	}
	num := NormalizeVehicle(in.Number)
	if !vehicleRe.MatchString(num) {
		return nil, ErrInvalidVehicle
	}
	var v resident.Vehicle
	err = s.pool.QueryRow(ctx, `
		INSERT INTO vehicles (society_id, unit_id, owner_user_id, vehicle_type, number, make_model, color, sticker_no)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING id, vehicle_type, number, make_model, color, sticker_no`,
		p.SocietyID, unitID, p.UserID, in.VehicleType, num, strings.TrimSpace(in.MakeModel), strings.TrimSpace(in.Color), in.StickerNo).
		Scan(&v.ID, &v.VehicleType, &v.Number, &v.MakeModel, &v.Color, &v.StickerNo)
	if database.IsUnique(err, "") {
		return nil, ErrVehicleExists
	}
	if err != nil {
		return nil, err
	}
	s.audit.Log(ctx, audit.Entry{ActorID: &p.UserID, SocietyID: &p.SocietyID, Action: "vehicle.add", Entity: "vehicle", EntityID: v.ID.String()})
	return &v, nil
}

func (s *Service) UpdateVehicle(ctx context.Context, p authctx.Principal, id uuid.UUID, in VehicleInput) error {
	unitID, err := s.unitOf(ctx, p)
	if err != nil {
		return err
	}
	num := NormalizeVehicle(in.Number)
	if !vehicleRe.MatchString(num) {
		return ErrInvalidVehicle
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE vehicles SET vehicle_type=$4, number=$5, make_model=$6, color=$7, sticker_no=$8
		WHERE id=$1 AND society_id=$2 AND unit_id=$3`,
		id, p.SocietyID, unitID, in.VehicleType, num, strings.TrimSpace(in.MakeModel), strings.TrimSpace(in.Color), in.StickerNo)
	if database.IsUnique(err, "") {
		return ErrVehicleExists
	}
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Service) DeleteVehicle(ctx context.Context, p authctx.Principal, id uuid.UUID) error {
	unitID, err := s.unitOf(ctx, p)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM vehicles WHERE id=$1 AND society_id=$2 AND unit_id=$3`, id, p.SocietyID, unitID)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}
