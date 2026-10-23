package user

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("not found")

type Repository struct{ pool *pgxpool.Pool }

func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

const userCols = `id, full_name, phone, email, password_hash, status, must_change_password`

func scanUser(row pgx.Row) (*User, error) {
	var u User
	if err := row.Scan(&u.ID, &u.FullName, &u.Phone, &u.Email, &u.PasswordHash, &u.Status, &u.MustChangePassword); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &u, nil
}

func (r *Repository) FindByPhone(ctx context.Context, phone string) (*User, error) {
	return scanUser(r.pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE phone=$1`, phone))
}

func (r *Repository) FindByEmail(ctx context.Context, email string) (*User, error) {
	return scanUser(r.pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE lower(email)=lower($1)`, email))
}

func (r *Repository) FindByID(ctx context.Context, id uuid.UUID) (*User, error) {
	return scanUser(r.pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id=$1`, id))
}

// SetPassword stores a new hash and clears the temporary-password flag.
func (r *Repository) SetPassword(ctx context.Context, id uuid.UUID, hash string) error {
	_, err := r.pool.Exec(ctx, `UPDATE users SET password_hash=$2, must_change_password=false, updated_at=now() WHERE id=$1`, id, hash)
	return err
}

func (r *Repository) TouchLogin(ctx context.Context, id uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `UPDATE users SET last_login_at=now() WHERE id=$1`, id)
	return err
}

const membershipSelect = `
	SELECT m.id, m.user_id, m.society_id, m.unit_id, m.role_key, m.status, s.name, s.status, m.app_access,
	       COALESCE(m.move_in_date::timestamptz, m.created_at)
	FROM memberships m JOIN societies s ON s.id = m.society_id`

func scanMembership(row pgx.Row) (*Membership, error) {
	var m Membership
	if err := row.Scan(&m.ID, &m.UserID, &m.SocietyID, &m.UnitID, &m.RoleKey, &m.Status,
		&m.SocietyName, &m.SocietyStatus, &m.AppAccess, &m.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &m, nil
}

// ListActiveMemberships returns the user's active memberships (any society status).
func (r *Repository) ListActiveMemberships(ctx context.Context, userID uuid.UUID) ([]Membership, error) {
	rows, err := r.pool.Query(ctx, membershipSelect+` WHERE m.user_id=$1 AND m.status='active' AND m.app_access ORDER BY s.name, m.created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Membership
	for rows.Next() {
		var m Membership
		if err := rows.Scan(&m.ID, &m.UserID, &m.SocietyID, &m.UnitID, &m.RoleKey, &m.Status,
			&m.SocietyName, &m.SocietyStatus, &m.AppAccess, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (r *Repository) GetMembership(ctx context.Context, id uuid.UUID) (*Membership, error) {
	return scanMembership(r.pool.QueryRow(ctx, membershipSelect+` WHERE m.id=$1`, id))
}

// UpsertDevice records the calling device for push notifications (M6).
func (r *Repository) UpsertDevice(ctx context.Context, userID uuid.UUID, deviceID, platform, fcmToken, appVersion string) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO devices (user_id, device_id, platform, fcm_token, app_version)
		VALUES ($1,$2,$3,NULLIF($4,''),$5)
		ON CONFLICT (user_id, device_id) DO UPDATE
		SET platform=EXCLUDED.platform,
		    fcm_token=COALESCE(EXCLUDED.fcm_token, devices.fcm_token),
		    app_version=EXCLUDED.app_version, last_seen_at=now()`,
		userID, deviceID, platform, fcmToken, appVersion)
	return err
}
