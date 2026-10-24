package blob

import (
	"context"
	"testing"
)

func TestDiskRoundTripAndTraversal(t *testing.T) {
	d, err := NewDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := d.Put(ctx, "soc/1/visits/a.jpg", []byte("img"), "image/jpeg"); err != nil {
		t.Fatal(err)
	}
	b, ct, err := d.Get(ctx, "soc/1/visits/a.jpg")
	if err != nil || string(b) != "img" || ct != "image/jpeg" {
		t.Fatalf("get: %q %s %v", b, ct, err)
	}
	if _, _, err := d.Get(ctx, "soc/1/visits/missing.jpg"); err != ErrNotFound {
		t.Fatalf("missing: %v", err)
	}
	for _, bad := range []string{"../escape.jpg", "a/../../b", "", "/"} {
		if err := d.Put(ctx, bad, []byte("x"), ""); err == nil {
			t.Fatalf("key %q must be rejected", bad)
		}
	}
	if err := d.Delete(ctx, "soc/1/visits/a.jpg"); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(ctx, "soc/1/visits/a.jpg"); err != nil {
		t.Fatalf("delete is idempotent: %v", err)
	}
}
