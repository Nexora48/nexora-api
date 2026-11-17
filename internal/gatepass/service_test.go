package gatepass

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func newTestService() *Service { return NewService(nil, nil, "test-secret") }

func TestQRRoundTripAndTamper(t *testing.T) {
	s := newTestService()
	id := uuid.New()
	exp := time.Now().Add(TTL).Truncate(time.Second)
	qr := s.qr(id, exp)
	if !strings.HasPrefix(qr, "NXR1:") {
		t.Fatalf("prefix: %s", qr)
	}
	gotID, gotExp, ok := s.parse(qr)
	if !ok || gotID != id || !gotExp.Equal(exp) {
		t.Fatalf("parse: %v %v %v", gotID, gotExp, ok)
	}
	// changing the expiry (to keep an old pass alive) breaks the signature
	parts := strings.Split(strings.TrimPrefix(qr, "NXR1:"), ".")
	forged := "NXR1:" + parts[0] + ".9999999999." + parts[2]
	if _, _, ok := s.parse(forged); ok {
		t.Fatal("forged expiry accepted")
	}
	// another server secret cannot read it
	if _, _, ok := NewService(nil, nil, "other-secret").parse(qr); ok {
		t.Fatal("foreign secret accepted")
	}
	// other pass kinds are not resident passes
	for _, bad := range []string{"", "NXV1:" + parts[0] + "." + parts[2], "NXR1:", "NXR1:abc.1.x", qr + ".extra"} {
		if _, _, ok := s.parse(bad); ok {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestExpiredPassIsRefusedBeforeAnyLookup(t *testing.T) {
	s := newTestService()
	old := s.qr(uuid.New(), time.Now().Add(-time.Minute))
	res, _, _, err := s.check(t.Context(), uuid.New(), old) // the pool is nil: an expired pass must not reach the database
	if err != nil || res.Valid || res.Reason != "EXPIRED" {
		t.Fatalf("expired: %v %+v", err, res)
	}
	res, _, _, err = s.check(t.Context(), uuid.New(), "NXR1:garbage")
	if err != nil || res.Valid || res.Reason != "INVALID" {
		t.Fatalf("garbage: %v %+v", err, res)
	}
}
