package seed

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nexora48/nexora-api/internal/superadmin"
)

// Dev-only SuperAdmin staff. The TOTP secrets are fixed so `go run ./cmd/totp` can print the code.
const (
	SuperEmail   = "super@nexora.test"
	SuperPass    = "Super@12345"
	SuperTOTP    = "JBSWY3DPEHPK3PXP"
	SupportEmail = "support@nexora.test"
	SupportPass  = "Support@12345"
	SupportTOTP  = "KRSXG5CTMVRXEZLU"
)

// Platform seeds one super_admin and one read-only platform_support user (idempotent).
func Platform(ctx context.Context, pool *pgxpool.Pool, encKey string) error {
	c, err := superadmin.NewCipher(encKey)
	if err != nil {
		return err
	}
	for _, u := range []struct{ email, name, pass, role, totp string }{
		{SuperEmail, "Nexora Super Admin", SuperPass, superadmin.RoleSuper, SuperTOTP},
		{SupportEmail, "Nexora Support", SupportPass, superadmin.RoleSupport, SupportTOTP},
	} {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM platform_users WHERE email=$1`, u.email).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		if _, err := superadmin.CreateUser(ctx, pool, c, u.email, u.name, u.pass, u.role, u.totp); err != nil {
			return err
		}
	}
	return nil
}
