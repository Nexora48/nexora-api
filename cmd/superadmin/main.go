// Command superadmin manages Nexora console staff (there is no public sign-up).
//
//	go run ./cmd/superadmin create-user -email a@nexora.com -name "A B" -role super_admin
//	go run ./cmd/superadmin reset-mfa -email a@nexora.com
//	go run ./cmd/superadmin disable-user -email a@nexora.com
package main

import (
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"math/big"
	"os"

	"github.com/Nexora48/nexora-api/internal/config"
	"github.com/Nexora48/nexora-api/internal/platform/database"
	"github.com/Nexora48/nexora-api/internal/superadmin"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: superadmin create-user|reset-mfa|disable-user [flags]")
		os.Exit(2)
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	email := fs.String("email", "", "staff email")
	name := fs.String("name", "", "full name (create-user)")
	role := fs.String("role", superadmin.RoleSupport, "super_admin | platform_support (create-user)")
	password := fs.String("password", "", "initial password (create-user; generated when empty)")
	_ = fs.Parse(os.Args[2:])
	if *email == "" {
		fail(fmt.Errorf("-email is required"))
	}

	cfg, err := config.Load()
	if err != nil {
		fail(err)
	}
	ctx := context.Background()
	pool, err := database.NewPostgres(ctx, cfg.DatabaseURL)
	if err != nil {
		fail(err)
	}
	defer pool.Close()

	switch cmd {
	case "create-user":
		if *name == "" || (*role != superadmin.RoleSuper && *role != superadmin.RoleSupport) {
			fail(fmt.Errorf("-name and a valid -role are required"))
		}
		pw := *password
		if pw == "" {
			pw = generate()
		}
		c, err := superadmin.NewCipher(cfg.PlatformEncKey)
		if err != nil {
			fail(err)
		}
		if _, err := superadmin.CreateUser(ctx, pool, c, *email, *name, pw, *role, ""); err != nil {
			fail(err)
		}
		fmt.Printf("created %s (%s)\npassword: %s\nThey set up their authenticator app at first sign-in.\n", *email, *role, pw)
	case "reset-mfa":
		tag, err := pool.Exec(ctx, `UPDATE platform_users SET totp_enabled=false, totp_secret_enc=NULL WHERE lower(email)=lower($1)`, *email)
		if err != nil || tag.RowsAffected() == 0 {
			fail(fmt.Errorf("user not found or update failed: %v", err))
		}
		_, _ = pool.Exec(ctx, `DELETE FROM platform_recovery_codes WHERE user_id=(SELECT id FROM platform_users WHERE lower(email)=lower($1))`, *email)
		_, _ = pool.Exec(ctx, `UPDATE platform_refresh_tokens SET revoked_at=now() WHERE revoked_at IS NULL AND user_id=(SELECT id FROM platform_users WHERE lower(email)=lower($1))`, *email)
		fmt.Println("2FA reset; the user enrols a new authenticator at next sign-in")
	case "disable-user":
		tag, err := pool.Exec(ctx, `UPDATE platform_users SET status='disabled' WHERE lower(email)=lower($1)`, *email)
		if err != nil || tag.RowsAffected() == 0 {
			fail(fmt.Errorf("user not found or update failed: %v", err))
		}
		_, _ = pool.Exec(ctx, `UPDATE platform_refresh_tokens SET revoked_at=now() WHERE revoked_at IS NULL AND user_id=(SELECT id FROM platform_users WHERE lower(email)=lower($1))`, *email)
		fmt.Println("user disabled and sessions revoked")
	default:
		fail(fmt.Errorf("unknown command %q", cmd))
	}
}

func generate() string {
	const alpha = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnpqrstuvwxyz23456789"
	b := make([]byte, 14)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alpha))))
		b[i] = alpha[n.Int64()]
	}
	return string(b) + "7a"
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
