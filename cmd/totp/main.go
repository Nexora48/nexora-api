// Command totp prints the current 6-digit codes for the dev SuperAdmin staff (or any base32 secret).
//
//	go run ./cmd/totp                 # super@nexora.test and support@nexora.test
//	go run ./cmd/totp JBSWY3DPEHPK3PXP
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/Nexora48/nexora-api/internal/seed"
	"github.com/Nexora48/nexora-api/internal/superadmin"
)

func main() {
	type entry struct{ label, secret string }
	entries := []entry{{seed.SuperEmail, seed.SuperTOTP}, {seed.SupportEmail, seed.SupportTOTP}}
	if len(os.Args) > 1 {
		entries = []entry{{"secret", os.Args[1]}}
	}
	now := time.Now()
	left := 30 - now.Unix()%30
	for _, e := range entries {
		code, err := superadmin.CurrentTOTP(e.secret, now, 0)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		fmt.Printf("%-22s %s  (valid for ~%ds)\n", e.label, code, left)
	}
}
