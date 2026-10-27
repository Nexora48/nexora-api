package blob

import (
	"context"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Report compares the keys the database refers to with the files in the disk store.
type Report struct {
	Referenced int      `json:"referenced"` // keys named by attachments, visit photos, helper and staff photos
	Present    int      `json:"present"`    // files on disk
	Missing    []string `json:"missing"`    // referenced but not on disk (data loss: restore the blobs)
	Orphans    []string `json:"orphans"`    // on disk but not referenced (safe to delete after review)
}

// OK is true when every referenced file exists.
func (r *Report) OK() bool { return len(r.Missing) == 0 }

// referenced tables and columns that hold blob keys
var keyColumns = [][2]string{{"attachments", "blob_key"}, {"visits", "photo_key"}, {"helpers", "photo_key"}, {"society_staff", "photo_key"}}

// Check lists every blob key in the database and every file under root and compares them. It changes nothing.
func Check(ctx context.Context, pool *pgxpool.Pool, root string) (*Report, error) {
	want := map[string]bool{}
	for _, tc := range keyColumns {
		rows, err := pool.Query(ctx, `SELECT `+tc[1]+` FROM `+tc[0]+` WHERE `+tc[1]+` IS NOT NULL AND `+tc[1]+` <> ''`)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				rows.Close()
				return nil, err
			}
			want[k] = true
		}
		rows.Close()
	}
	have := map[string]bool{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		have[strings.ReplaceAll(rel, string(filepath.Separator), "/")] = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	r := &Report{Referenced: len(want), Present: len(have), Missing: []string{}, Orphans: []string{}}
	for k := range want {
		if !have[k] {
			r.Missing = append(r.Missing, k)
		}
	}
	for k := range have {
		if !want[k] {
			r.Orphans = append(r.Orphans, k)
		}
	}
	sort.Strings(r.Missing)
	sort.Strings(r.Orphans)
	return r, nil
}
