// Command blobcheck compares the files in the blob store with the keys the database refers to.
//
//	DATABASE_URL=... BLOB_PATH=/var/lib/nexora/blobs go run ./cmd/blobcheck
//
// Exit status 0 = every referenced file exists (orphans are only listed), 1 = something is missing.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/Nexora48/nexora-api/internal/blob"
	"github.com/Nexora48/nexora-api/internal/config"
	"github.com/Nexora48/nexora-api/internal/platform/database"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		panic(err)
	}
	if cfg.BlobPath == "" {
		fmt.Fprintln(os.Stderr, "blobcheck: set BLOB_PATH")
		os.Exit(2)
	}
	ctx := context.Background()
	pool, err := database.NewPostgres(ctx, cfg.DatabaseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "blobcheck:", err)
		os.Exit(2)
	}
	defer pool.Close()
	r, err := blob.Check(ctx, pool, cfg.BlobPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "blobcheck:", err)
		os.Exit(2)
	}
	fmt.Printf("referenced %d, on disk %d, missing %d, orphans %d\n", r.Referenced, r.Present, len(r.Missing), len(r.Orphans))
	for _, k := range r.Missing {
		fmt.Println("MISSING", k)
	}
	for i, k := range r.Orphans {
		if i == 50 {
			fmt.Printf("... and %d more orphans\n", len(r.Orphans)-50)
			break
		}
		fmt.Println("orphan ", k)
	}
	if !r.OK() {
		os.Exit(1)
	}
}
