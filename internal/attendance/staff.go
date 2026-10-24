package attendance

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Nexora48/nexora-api/internal/attachment"
	"github.com/Nexora48/nexora-api/internal/pass"
	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/database"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
	"github.com/Nexora48/nexora-api/internal/resident"
)

var (
	ErrStaffNotFound = httpx.NewError(http.StatusNotFound, "NOT_FOUND", "staff member not found")
	ErrPhoneTaken    = httpx.NewError(http.StatusConflict, "PHONE_TAKEN", "a staff member with this phone number already exists")
	staffRoles       = map[string]bool{"housekeeping": true, "gardener": true, "electrician": true, "plumber": true, "supervisor": true, "other": true}
)

type Staff struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"full_name"`
	Phone     string    `json:"phone"`
	Role      string    `json:"role"`
	ShiftFrom string    `json:"shift_from"`
	ShiftTo   string    `json:"shift_to"`
	WorkDays  []int     `json:"work_days"`
	Active    bool      `json:"active"`
	HasPhoto  bool      `json:"has_photo"`
	Inside    bool      `json:"inside"`
	CreatedAt time.Time `json:"created_at"`
}

type StaffInput struct {
	Name, Phone, Role, ShiftFrom, ShiftTo string
	WorkDays                              []int
	Active                                *bool
}

const staffSelect = `SELECT s.id, s.full_name, s.phone, s.role, s.shift_from::text, s.shift_to::text, s.work_days, s.active, s.created_at,
	EXISTS (SELECT 1 FROM attachments a WHERE a.owner_type='staff' AND a.owner_id=s.id),
	EXISTS (SELECT 1 FROM attendance_logs l WHERE l.subject_type='staff' AND l.subject_id=s.id AND l.out_at IS NULL)
	FROM society_staff s `

func scanStaff(row pgx.Row) (*Staff, error) {
	var st Staff
	var days []int16
	err := row.Scan(&st.ID, &st.Name, &st.Phone, &st.Role, &st.ShiftFrom, &st.ShiftTo, &days, &st.Active, &st.CreatedAt, &st.HasPhoto, &st.Inside)
	st.ShiftFrom, st.ShiftTo = st.ShiftFrom[:min(5, len(st.ShiftFrom))], st.ShiftTo[:min(5, len(st.ShiftTo))]
	st.WorkDays = []int{}
	for _, d := range days {
		st.WorkDays = append(st.WorkDays, int(d))
	}
	return &st, err
}

func (in *StaffInput) validate(create bool) error {
	in.Name = strings.Join(strings.Fields(in.Name), " ")
	if l := len([]rune(in.Name)); l < 2 || l > 80 {
		return invalid("name must be 2-80 characters")
	}
	in.Phone = resident.NormalizePhone(in.Phone)
	if !resident.ValidPhone(in.Phone) {
		return httpx.NewError(http.StatusBadRequest, "INVALID_PHONE", "enter a valid 10-digit mobile number")
	}
	if !staffRoles[in.Role] {
		return invalid("role must be housekeeping, gardener, electrician, plumber, supervisor or other")
	}
	if in.ShiftFrom == "" {
		in.ShiftFrom = "09:00"
	}
	if in.ShiftTo == "" {
		in.ShiftTo = "17:00"
	}
	f, ok1 := parseMin(in.ShiftFrom)
	t, ok2 := parseMin(in.ShiftTo)
	if !ok1 || !ok2 || f >= t {
		return invalid("shift times must be HH:MM with the start before the end")
	}
	if in.WorkDays == nil {
		in.WorkDays = []int{1, 2, 3, 4, 5, 6}
	}
	seen := map[int]bool{}
	for _, d := range in.WorkDays {
		if d < 0 || d > 6 || seen[d] {
			return invalid("work days must be unique numbers 0 (Sunday) to 6")
		}
		seen[d] = true
	}
	return nil
}

func days16(d []int) []int16 {
	out := make([]int16, len(d))
	for i, v := range d {
		out[i] = int16(v)
	}
	return out
}

func (s *Service) ListStaff(ctx context.Context, p authctx.Principal, includeInactive bool) ([]*Staff, error) {
	rows, err := s.pool.Query(ctx, staffSelect+`WHERE s.society_id=$1 AND ($2 OR s.active) ORDER BY s.active DESC, s.full_name, s.id`, p.SocietyID, includeInactive)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Staff{}
	for rows.Next() {
		st, err := scanStaff(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

func (s *Service) GetStaff(ctx context.Context, p authctx.Principal, id uuid.UUID) (*Staff, error) {
	st, err := scanStaff(s.pool.QueryRow(ctx, staffSelect+`WHERE s.id=$1 AND s.society_id=$2`, id, p.SocietyID))
	if err != nil {
		return nil, ErrStaffNotFound
	}
	return st, nil
}

func (s *Service) CreateStaff(ctx context.Context, p authctx.Principal, in StaffInput) (*Staff, error) {
	if err := in.validate(true); err != nil {
		return nil, err
	}
	for try := 0; try < 8; try++ {
		code, err := pass.NewCode()
		if err != nil {
			return nil, err
		}
		var id uuid.UUID
		err = s.pool.QueryRow(ctx, `INSERT INTO society_staff (society_id, full_name, phone, role, shift_from, shift_to, work_days, pass_code) VALUES ($1,$2,$3,$4,$5::time,$6::time,$7::smallint[],$8) RETURNING id`,
			p.SocietyID, in.Name, in.Phone, in.Role, in.ShiftFrom, in.ShiftTo, days16(in.WorkDays), code).Scan(&id)
		switch {
		case err == nil:
			s.log(ctx, p, "staff.create", "staff", id, map[string]any{"role": in.Role})
			return s.GetStaff(ctx, p, id)
		case database.IsUnique(err, "society_staff_society_id_phone_key"):
			return nil, ErrPhoneTaken
		case database.IsUnique(err, "society_staff_society_id_pass_code_key"):
			continue
		default:
			return nil, err
		}
	}
	return nil, invalid("could not generate a unique pass code; try again")
}

func (s *Service) UpdateStaff(ctx context.Context, p authctx.Principal, id uuid.UUID, in StaffInput) (*Staff, error) {
	if err := in.validate(false); err != nil {
		return nil, err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE society_staff SET full_name=$3, phone=$4, role=$5, shift_from=$6::time, shift_to=$7::time, work_days=$8::smallint[], active=COALESCE($9, active) WHERE id=$1 AND society_id=$2`,
		id, p.SocietyID, in.Name, in.Phone, in.Role, in.ShiftFrom, in.ShiftTo, days16(in.WorkDays), in.Active)
	if database.IsUnique(err, "society_staff_society_id_phone_key") {
		return nil, ErrPhoneTaken
	}
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrStaffNotFound
	}
	s.log(ctx, p, "staff.update", "staff", id, map[string]any{"active": in.Active})
	return s.GetStaff(ctx, p, id)
}

func (s *Service) StaffPass(ctx context.Context, p authctx.Principal, id uuid.UUID, regenerate bool) (map[string]string, error) {
	var code string
	if err := s.pool.QueryRow(ctx, `SELECT pass_code FROM society_staff WHERE id=$1 AND society_id=$2`, id, p.SocietyID).Scan(&code); err != nil {
		return nil, ErrStaffNotFound
	}
	if regenerate {
		for try := 0; ; try++ {
			c, err := pass.NewCode()
			if err != nil {
				return nil, err
			}
			_, err = s.pool.Exec(ctx, `UPDATE society_staff SET pass_code=$3 WHERE id=$1 AND society_id=$2`, id, p.SocietyID, c)
			if database.IsUnique(err, "") && try < 8 {
				continue
			}
			if err != nil {
				return nil, err
			}
			code = c
			s.log(ctx, p, "staff.new_code", "staff", id, nil)
			break
		}
	}
	return map[string]string{"qr": s.staffQR.QR(id), "code": code}, nil
}

func (s *Service) SetStaffPhoto(ctx context.Context, p authctx.Principal, id uuid.UUID, name string, data []byte) error {
	if _, err := s.GetStaff(ctx, p, id); err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM attachments WHERE owner_type='staff' AND owner_id=$1 AND society_id=$2`, id, p.SocietyID); err != nil {
		return err
	}
	_, err := s.files.Save(ctx, p.SocietyID, attachment.OwnerStaff, id, p.UserID, name, data, 1)
	return err
}
