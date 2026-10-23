package app_test

import "github.com/Nexora48/nexora-api/internal/auth"

func hashFor(p string) (string, error) { return auth.HashPassword(p) }
