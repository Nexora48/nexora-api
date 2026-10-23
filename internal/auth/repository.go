package auth

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	errRefreshNotFound = errors.New("refresh token not found")
	errRefreshReused   = errors.New("refresh token reused")
	errRefreshExpired  = errors.New("refresh token expired")
)

type Repository struct{ pool *pgxpool.Pool }

func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

type refreshInfo struct {
	UserID       uuid.UUID
	MembershipID uuid.UUID
	FamilyID     uuid.UUID
	DeviceID     string
}

// CreateRefresh stores a new refresh token (hash only) in a family.
func (r *Repository) CreateRefresh(ctx context.Context, userID, membershipID, familyID uuid.UUID,
	hash string, expires time.Time, deviceID, ip, ua string) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO refresh_tokens (user_id, membership_id, family_id, token_hash, expires_at, device_id, ip, user_agent)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, userID, membershipID, familyID, hash, expires, deviceID, ip, ua)
	return err
}

// Rotate atomically consumes oldHash and stores newHash in the same family.
// Presenting an already-revoked token revokes the whole family (theft signal).
func (r *Repository) Rotate(ctx context.Context, oldHash, newHash string, newExpires time.Time, ip, ua string) (*refreshInfo, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var id uuid.UUID
	var info refreshInfo
	var expires time.Time
	var revoked *time.Time
	var device *string
	err = tx.QueryRow(ctx, `
		SELECT id, user_id, membership_id, family_id, expires_at, revoked_at, device_id
		FROM refresh_tokens WHERE token_hash=$1 FOR UPDATE`, oldHash).
		Scan(&id, &info.UserID, &info.MembershipID, &info.FamilyID, &expires, &revoked, &device)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errRefreshNotFound
	}
	if err != nil {
		return nil, err
	}
	if device != nil {
		info.DeviceID = *device
	}
	if revoked != nil {
		if _, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at=now() WHERE family_id=$1 AND revoked_at IS NULL`, info.FamilyID); err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return &info, errRefreshReused
	}
	if time.Now().After(expires) {
		return nil, errRefreshExpired
	}
	var newID uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO refresh_tokens (user_id, membership_id, family_id, token_hash, expires_at, device_id, ip, user_agent)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`,
		info.UserID, info.MembershipID, info.FamilyID, newHash, newExpires, info.DeviceID, ip, ua).Scan(&newID)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at=now(), replaced_by=$2 WHERE id=$1`, id, newID); err != nil {
		return nil, err
	}
	return &info, tx.Commit(ctx)
}

// RevokeFamily revokes every live token in the family of the given token hash.
func (r *Repository) RevokeFamilyByHash(ctx context.Context, hash string, userID uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE refresh_tokens SET revoked_at=now()
		WHERE revoked_at IS NULL AND user_id=$2
		  AND family_id = (SELECT family_id FROM refresh_tokens WHERE token_hash=$1 AND user_id=$2)`, hash, userID)
	return err
}

// RevokeAllForUser revokes every live refresh token of the user (all devices, all societies).
func (r *Repository) RevokeAllForUser(ctx context.Context, userID uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `UPDATE refresh_tokens SET revoked_at=now() WHERE user_id=$1 AND revoked_at IS NULL`, userID)
	return err
}

func (r *Repository) RevokeFamily(ctx context.Context, familyID uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `UPDATE refresh_tokens SET revoked_at=now() WHERE family_id=$1 AND revoked_at IS NULL`, familyID)
	return err
}
