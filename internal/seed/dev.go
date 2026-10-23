// Package seed inserts idempotent development data.
package seed

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nexora48/nexora-api/internal/auth"
)

// Fixed IDs so seeds are idempotent and apps/tests can reference them.
const (
	SocietyID     = "11111111-1111-1111-1111-111111111111"
	AdminUserID   = "22222222-2222-2222-2222-222222222221"
	ResidentID    = "22222222-2222-2222-2222-222222222222"
	AdminMemID    = "33333333-3333-3333-3333-333333333331"
	ResidentMem   = "33333333-3333-3333-3333-333333333332"
	AdminEmail    = "admin@sunrise.test"
	AdminPass     = "Admin@123"
	ResidentPhone = "9999999999"
	AdminPhone    = "9999999990"
)

func Dev(ctx context.Context, pool *pgxpool.Pool) error {
	hash, err := auth.HashPassword(AdminPass)
	if err != nil {
		return err
	}
	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO societies (id, name, slug, address, city, state, pincode)
		  VALUES ($1,'Sunrise Heights','sunrise-heights','Andheri West','Mumbai','Maharashtra','400053')
		  ON CONFLICT (id) DO NOTHING`, []any{SocietyID}},
		{`INSERT INTO users (id, full_name, phone, email, password_hash)
		  VALUES ($1,'Anita Sharma',$2,$3,$4) ON CONFLICT (id) DO NOTHING`, []any{AdminUserID, AdminPhone, AdminEmail, hash}},
		{`INSERT INTO users (id, full_name, phone) VALUES ($1,'Rajesh Kumar',$2) ON CONFLICT (id) DO NOTHING`,
			[]any{ResidentID, ResidentPhone}},
		{`INSERT INTO memberships (id, user_id, society_id, role_key) VALUES ($1,$2,$3,'society_admin') ON CONFLICT (id) DO NOTHING`,
			[]any{AdminMemID, AdminUserID, SocietyID}},
		{`INSERT INTO memberships (id, user_id, society_id, role_key) VALUES ($1,$2,$3,'resident_owner') ON CONFLICT (id) DO NOTHING`,
			[]any{ResidentMem, ResidentID, SocietyID}},
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s.sql, s.args...); err != nil {
			return err
		}
	}
	return nil
}
