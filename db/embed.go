// Package db embeds SQL migrations so the binary can migrate without external tools.
package db

import "embed"

//go:embed migrations/*.sql
var Migrations embed.FS
