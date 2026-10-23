package database

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

const (
	codeUniqueViolation = "23505"
	codeFKViolation     = "23503"
)

func pgErr(err error) *pgconn.PgError {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe
	}
	return nil
}

// IsUnique reports a unique violation; constraint "" matches any constraint.
func IsUnique(err error, constraint string) bool {
	pe := pgErr(err)
	return pe != nil && pe.Code == codeUniqueViolation && (constraint == "" || pe.ConstraintName == constraint)
}

// IsForeignKey reports a foreign-key violation (e.g. deleting a referenced row).
func IsForeignKey(err error) bool {
	pe := pgErr(err)
	return pe != nil && pe.Code == codeFKViolation
}
