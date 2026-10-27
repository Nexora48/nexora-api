package app_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Nexora48/nexora-api/internal/blob"
)

func pgBin(t *testing.T) string {
	t.Helper()
	for _, dir := range []string{os.Getenv("PG_BIN"), "/opt/homebrew/opt/postgresql@16/bin", "/usr/local/opt/postgresql@16/bin", "/usr/lib/postgresql/16/bin", "/usr/lib/postgresql/17/bin"} {
		if dir == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "pg_dump")); err == nil {
			return dir
		}
	}
	if p, err := exec.LookPath("pg_dump"); err == nil {
		return filepath.Dir(p)
	}
	t.Skip("pg_dump and psql of the server's major version (16) are not installed (set PG_BIN)")
	return ""
}

// run executes a script from scripts/ with extra environment variables and returns its combined output.
func run(t *testing.T, script string, env map[string]string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(filepath.Join("..", "..", "scripts", script), args...)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// The restore drill: back up a populated database, restore it into a scratch one, compare.
func TestBackupAndRestoreDrill(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	bin := pgBin(t)
	c := newCommunity(t)
	populate(t, c)

	dir := t.TempDir()
	admin := strings.Replace(testDBURL, "/nexora?", "/postgres?", 1)
	env := map[string]string{"PG_BIN": bin, "DATABASE_URL": testDBURL, "ADMIN_URL": admin, "BACKUP_DIR": dir, "BACKUP_HEARTBEAT_FILE": filepath.Join(dir, "backup.last")}

	if out, err := run(t, "backup.sh", env); err != nil {
		t.Fatalf("backup: %v\n%s", err, out)
	}
	dumps, _ := filepath.Glob(filepath.Join(dir, "nexora-*.dump"))
	if len(dumps) != 1 {
		t.Fatalf("expected one dump, got %v", dumps)
	}
	if _, err := os.Stat(filepath.Join(dir, "backup.last")); err != nil {
		t.Fatal("the heartbeat file was not written")
	}
	if _, err := os.Stat(dumps[0] + ".sha256"); err != nil {
		t.Fatal("no checksum written")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".*.partial")); len(left) != 0 {
		t.Fatalf("partial files left behind: %v", left)
	}

	out, err := run(t, "restore-drill.sh", env)
	if err != nil || !strings.Contains(out, "drill: OK") {
		t.Fatalf("drill: %v\n%s", err, out)
	}

	// a damaged dump is refused before anything is restored
	bad := filepath.Join(dir, "nexora-20200101-000000.dump")
	data, _ := os.ReadFile(dumps[0])
	_ = os.WriteFile(bad, data[:len(data)/2], 0o644)
	_ = os.WriteFile(bad+".sha256", []byte(strings.Repeat("0", 64)+"  nexora-20200101-000000.dump\n"), 0o644)
	if out, err := run(t, "restore.sh", map[string]string{"PG_BIN": bin, "TARGET_URL": admin}, bad); err == nil || !strings.Contains(out, "checksum mismatch") {
		t.Fatalf("a damaged dump must be refused: %v\n%s", err, out)
	}
	// restoring into a database that already has tables needs FORCE
	if out, err := run(t, "restore.sh", map[string]string{"PG_BIN": bin, "TARGET_URL": testDBURL}, dumps[0]); err == nil || !strings.Contains(out, "FORCE=1") {
		t.Fatalf("restore over a populated database must be refused: %v\n%s", err, out)
	}
}

func TestBackupRetentionKeepsDailyWeeklyAndMonthly(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	bin := pgBin(t)
	dir := t.TempDir()
	// a year of daily dumps, named like the real ones
	for d := 0; d < 400; d++ {
		stamp := strings.NewReplacer("-", "").Replace(timeAgo(d)) + "-020000"
		_ = os.WriteFile(filepath.Join(dir, "nexora-"+stamp+".dump"), []byte("x"), 0o644)
	}
	env := map[string]string{"PG_BIN": bin, "DATABASE_URL": testDBURL, "BACKUP_DIR": dir, "KEEP_DAILY": "14", "KEEP_WEEKLY": "8", "KEEP_MONTHLY": "12"}
	if out, err := run(t, "backup.sh", env); err != nil {
		t.Fatalf("backup: %v\n%s", err, out)
	}
	left, _ := filepath.Glob(filepath.Join(dir, "nexora-*.dump"))
	// 14 newest (incl. today's real one) + up to 8 Sundays + up to 12 firsts, with overlaps
	if len(left) < 20 || len(left) > 14+8+12 {
		t.Fatalf("retention kept %d dumps", len(left))
	}
}

func timeAgo(days int) string {
	return time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
}

func TestBlobCheckFindsMissingAndOrphans(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	c := newCommunity(t)
	dir := t.TempDir()
	ctx := context.Background()
	put := func(key string) {
		p := filepath.Join(dir, filepath.FromSlash(key))
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte("x"), 0o644)
	}
	att := func(key string) {
		mustExec(t, ctx, `INSERT INTO attachments (id, society_id, owner_type, owner_id, blob_key, content_type, size_bytes, filename) VALUES (gen_random_uuid(),$1,'notice',gen_random_uuid(),$2,'image/jpeg',1,'a.jpg')`, c.s.ID, key)
	}
	base := "chk/" + c.s.ID.String()
	att(base + "/ok.jpg")
	put(base + "/ok.jpg")
	att(base + "/lost.jpg")  // in the database, not on disk
	put(base + "/stray.jpg") // on disk, not in the database

	// other tests leave rows that point nowhere, so look only at this society's keys
	r, err := blob.Check(ctx, pool, dir)
	if err != nil {
		t.Fatal(err)
	}
	has := func(list []string, key string) bool {
		for _, k := range list {
			if k == key {
				return true
			}
		}
		return false
	}
	if !has(r.Missing, base+"/lost.jpg") || has(r.Missing, base+"/ok.jpg") {
		t.Fatalf("missing: %v", r.Missing)
	}
	if !has(r.Orphans, base+"/stray.jpg") || has(r.Orphans, base+"/ok.jpg") {
		t.Fatalf("orphans: %v", r.Orphans)
	}
	if r.OK() {
		t.Fatal("a missing file must fail the check")
	}
}
