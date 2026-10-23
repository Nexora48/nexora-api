package superadmin

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type CatalogueRow struct {
	Key            string `json:"key"`
	Name           string `json:"name"`
	Description    string `json:"description"`
	DefaultEnabled bool   `json:"default_enabled"`
}

func (s *Service) Catalogue(ctx context.Context) ([]CatalogueRow, error) {
	rows, err := s.store.pool.Query(ctx, `SELECT key, name, description, default_enabled FROM features ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CatalogueRow{}
	for rows.Next() {
		var r CatalogueRow
		if err := rows.Scan(&r.Key, &r.Name, &r.Description, &r.DefaultEnabled); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Service) societyFeatures(ctx context.Context, societyID uuid.UUID) ([]FeatureRow, error) {
	rows, err := s.store.pool.Query(ctx, `
		SELECT f.key, f.name, f.description, f.default_enabled, sf.enabled, COALESCE(sf.enabled, f.default_enabled), sf.updated_at, COALESCE(sf.reason,'')
		FROM features f LEFT JOIN society_features sf ON sf.feature_key=f.key AND sf.society_id=$1 ORDER BY f.name`, societyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FeatureRow{}
	for rows.Next() {
		var r FeatureRow
		if err := rows.Scan(&r.Key, &r.Name, &r.Description, &r.DefaultEnabled, &r.Override, &r.Enabled, &r.UpdatedAt, &r.Reason); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetFeatures applies one or many service switches for a society in one transaction and
// writes a single audit row with the before/after of every change.
func (s *Service) SetFeatures(ctx context.Context, actor, societyID uuid.UUID, changes map[string]bool, reason string) ([]FeatureRow, error) {
	var exists bool
	if err := s.store.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM societies WHERE id=$1)`, societyID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrSocietyNotFound
	}
	tx, err := s.store.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	keys := make([]string, 0, len(changes))
	for k := range changes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	type change struct {
		Feature string `json:"feature"`
		From    bool   `json:"from"`
		To      bool   `json:"to"`
	}
	var done []change
	for _, k := range keys {
		var before bool
		err := tx.QueryRow(ctx, `SELECT COALESCE(sf.enabled, f.default_enabled) FROM features f
			LEFT JOIN society_features sf ON sf.feature_key=f.key AND sf.society_id=$1 WHERE f.key=$2`, societyID, k).Scan(&before)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrFeatureUnknown
		}
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO society_features (society_id, feature_key, enabled, changed_by, reason, updated_at)
			VALUES ($1,$2,$3,$4,$5,now())
			ON CONFLICT (society_id, feature_key) DO UPDATE SET enabled=EXCLUDED.enabled, changed_by=EXCLUDED.changed_by, reason=EXCLUDED.reason, updated_at=now()`,
			societyID, k, changes[k], actor, strings.TrimSpace(reason)); err != nil {
			return nil, err
		}
		if before != changes[k] {
			done = append(done, change{k, before, changes[k]})
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	if len(done) > 0 {
		s.platformLog(ctx, actor, &societyID, "feature.toggle", "society", societyID.String(), map[string]any{"changes": done, "reason": strings.TrimSpace(reason)})
	}
	return s.societyFeatures(ctx, societyID)
}
