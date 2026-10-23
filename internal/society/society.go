package society

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("society not found")

const (
	StatusActive    = "active"
	StatusTrial     = "trial"
	StatusSuspended = "suspended"
)

type Society struct {
	ID     uuid.UUID
	Name   string
	Slug   string
	Status string
}

func (s Society) IsUsable() bool { return s.Status != StatusSuspended }

type Repository struct{ pool *pgxpool.Pool }

func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

func (r *Repository) Get(ctx context.Context, id uuid.UUID) (*Society, error) {
	var s Society
	err := r.pool.QueryRow(ctx, `SELECT id, name, slug, status FROM societies WHERE id=$1`, id).
		Scan(&s.ID, &s.Name, &s.Slug, &s.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &s, err
}
