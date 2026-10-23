package feature

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Service answers "is this product feature enabled for this society?".
// A society_features row overrides features.default_enabled.
type Service struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// Map returns every feature key with its effective state for the society.
func (s *Service) Map(ctx context.Context, societyID uuid.UUID) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT f.key, COALESCE(sf.enabled, f.default_enabled)
		FROM features f
		LEFT JOIN society_features sf ON sf.feature_key = f.key AND sf.society_id = $1`, societyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var k string
		var on bool
		if err := rows.Scan(&k, &on); err != nil {
			return nil, err
		}
		out[k] = on
	}
	return out, rows.Err()
}

// Enabled reports one feature; unknown keys are treated as disabled.
func (s *Service) Enabled(ctx context.Context, societyID uuid.UUID, key string) (bool, error) {
	m, err := s.Map(ctx, societyID)
	if err != nil {
		return false, err
	}
	return m[key], nil
}
