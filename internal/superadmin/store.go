package superadmin

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nexora48/nexora-api/internal/auth"
)

var (
	errRefreshNotFound = errors.New("refresh token not found")
	errRefreshReused   = errors.New("refresh token reused")
	errRefreshExpired  = errors.New("refresh token expired")
	errUserNotFound    = errors.New("platform user not found")
)

// storePool is the Postgres pool type used by the console.
type storePool = *pgxpool.Pool

type staff struct {
	ID            uuid.UUID
	Email         string
	FullName      string
	PasswordHash  string
	Role          string
	Status        string
	TOTPSecretEnc *string
	TOTPEnabled   bool
}

type store struct{ pool *pgxpool.Pool }

const staffCols = `id, email, full_name, password_hash, role, status, totp_secret_enc, totp_enabled`

func scanStaff(row pgx.Row) (*staff, error) {
	var u staff
	if err := row.Scan(&u.ID, &u.Email, &u.FullName, &u.PasswordHash, &u.Role, &u.Status, &u.TOTPSecretEnc, &u.TOTPEnabled); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errUserNotFound
		}
		return nil, err
	}
	return &u, nil
}

func (s *store) userByEmail(ctx context.Context, email string) (*staff, error) {
	return scanStaff(s.pool.QueryRow(ctx, `SELECT `+staffCols+` FROM platform_users WHERE lower(email)=lower($1)`, email))
}

func (s *store) userByID(ctx context.Context, id uuid.UUID) (*staff, error) {
	return scanStaff(s.pool.QueryRow(ctx, `SELECT `+staffCols+` FROM platform_users WHERE id=$1`, id))
}

func (s *store) setTOTP(ctx context.Context, id uuid.UUID, enc string, enabled bool) error {
	_, err := s.pool.Exec(ctx, `UPDATE platform_users SET totp_secret_enc=$2, totp_enabled=$3 WHERE id=$1`, id, enc, enabled)
	return err
}

func (s *store) touchLogin(ctx context.Context, id uuid.UUID) {
	_, _ = s.pool.Exec(ctx, `UPDATE platform_users SET last_login_at=now() WHERE id=$1`, id)
}

func (s *store) replaceRecoveryCodes(ctx context.Context, id uuid.UUID, hashes []string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `DELETE FROM platform_recovery_codes WHERE user_id=$1`, id); err != nil {
		return err
	}
	for _, h := range hashes {
		if _, err := tx.Exec(ctx, `INSERT INTO platform_recovery_codes (user_id, code_hash) VALUES ($1,$2)`, id, h); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// useRecoveryCode atomically burns a recovery code; false when unknown or already used.
func (s *store) useRecoveryCode(ctx context.Context, id uuid.UUID, hash string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE platform_recovery_codes SET used_at=now() WHERE user_id=$1 AND code_hash=$2 AND used_at IS NULL`, id, hash)
	return tag.RowsAffected() == 1, err
}

// ── sessions ─────────────────────────────────────────────────────────────────

func (s *store) createRefresh(ctx context.Context, userID, family uuid.UUID, hash string, expires time.Time, ip, ua string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO platform_refresh_tokens (user_id, family_id, token_hash, expires_at, ip, user_agent) VALUES ($1,$2,$3,$4,$5,$6)`,
		userID, family, hash, expires, ip, ua)
	return err
}

// rotate consumes oldHash and stores newHash in the same family; reuse revokes the whole family.
func (s *store) rotate(ctx context.Context, oldHash, newHash string, newExpires time.Time, ip, ua string) (uuid.UUID, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var id, userID, family uuid.UUID
	var expires time.Time
	var revoked *time.Time
	err = tx.QueryRow(ctx, `SELECT id, user_id, family_id, expires_at, revoked_at FROM platform_refresh_tokens WHERE token_hash=$1 FOR UPDATE`, oldHash).
		Scan(&id, &userID, &family, &expires, &revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, errRefreshNotFound
	}
	if err != nil {
		return uuid.Nil, err
	}
	if revoked != nil {
		if _, err := tx.Exec(ctx, `UPDATE platform_refresh_tokens SET revoked_at=now() WHERE family_id=$1 AND revoked_at IS NULL`, family); err != nil {
			return uuid.Nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return uuid.Nil, err
		}
		return userID, errRefreshReused
	}
	if time.Now().After(expires) {
		return uuid.Nil, errRefreshExpired
	}
	var newID uuid.UUID
	if err := tx.QueryRow(ctx, `INSERT INTO platform_refresh_tokens (user_id, family_id, token_hash, expires_at, ip, user_agent) VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		userID, family, newHash, newExpires, ip, ua).Scan(&newID); err != nil {
		return uuid.Nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE platform_refresh_tokens SET revoked_at=now(), replaced_by=$2 WHERE id=$1`, id, newID); err != nil {
		return uuid.Nil, err
	}
	return userID, tx.Commit(ctx)
}

func (s *store) revokeFamilyByHash(ctx context.Context, hash string, userID uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `UPDATE platform_refresh_tokens SET revoked_at=now() WHERE revoked_at IS NULL AND user_id=$2
		AND family_id=(SELECT family_id FROM platform_refresh_tokens WHERE token_hash=$1 AND user_id=$2)`, hash, userID)
	return err
}

func (s *store) revokeAllForUser(ctx context.Context, userID uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `UPDATE platform_refresh_tokens SET revoked_at=now() WHERE user_id=$1 AND revoked_at IS NULL`, userID)
	return err
}

// ── provisioning (CLI, seed, tests) ──────────────────────────────────────────

// CreateUser inserts a staff user. totpSecret may be empty (the user enrols at first sign-in).
func CreateUser(ctx context.Context, pool *pgxpool.Pool, c *Cipher, email, fullName, password, role, totpSecret string) (uuid.UUID, error) {
	hash, err := auth.HashPassword(password)
	if err != nil {
		return uuid.Nil, err
	}
	var enc *string
	enabled := false
	if totpSecret != "" {
		e, err := c.Encrypt(totpSecret)
		if err != nil {
			return uuid.Nil, err
		}
		enc, enabled = &e, true
	}
	var id uuid.UUID
	err = pool.QueryRow(ctx, `INSERT INTO platform_users (email, full_name, password_hash, role, totp_secret_enc, totp_enabled)
		VALUES (lower($1),$2,$3,$4,$5,$6) RETURNING id`, email, fullName, hash, role, enc, enabled).Scan(&id)
	return id, err
}
