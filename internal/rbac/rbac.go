package rbac

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Service resolves role → permissions from the DB with a short in-memory cache.
type Service struct {
	pool     *pgxpool.Pool
	ttl      time.Duration
	mu       sync.RWMutex
	cache    map[string]map[string]struct{}
	loadedAt time.Time
}

func New(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, ttl: 5 * time.Minute}
}

func (s *Service) load(ctx context.Context) (map[string]map[string]struct{}, error) {
	s.mu.RLock()
	if s.cache != nil && time.Since(s.loadedAt) < s.ttl {
		defer s.mu.RUnlock()
		return s.cache, nil
	}
	s.mu.RUnlock()

	rows, err := s.pool.Query(ctx, `SELECT role_key, permission_key FROM role_permissions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]map[string]struct{}{}
	for rows.Next() {
		var role, perm string
		if err := rows.Scan(&role, &perm); err != nil {
			return nil, err
		}
		if m[role] == nil {
			m[role] = map[string]struct{}{}
		}
		m[role][perm] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.cache, s.loadedAt = m, time.Now()
	s.mu.Unlock()
	return m, nil
}

func (s *Service) Has(ctx context.Context, role, perm string) (bool, error) {
	m, err := s.load(ctx)
	if err != nil {
		return false, err
	}
	_, ok := m[role][perm]
	return ok, nil
}

// List returns the sorted permission keys of a role.
func (s *Service) List(ctx context.Context, role string) ([]string, error) {
	m, err := s.load(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(m[role]))
	for p := range m[role] {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}
