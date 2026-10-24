package pass

import (
	"regexp"
	"testing"

	"github.com/google/uuid"
)

func TestQRRoundTripAndPurposeSeparation(t *testing.T) {
	visitor := NewSigner("secret", "visitor-qr", "NXV1:")
	helper := NewSigner("secret", "helper-qr", "NXH1:")
	id := uuid.New()
	q := visitor.QR(id)
	if got, ok := visitor.Parse(q); !ok || got != id {
		t.Fatalf("round trip: %v %v", got, ok)
	}
	if _, ok := helper.Parse(q); ok {
		t.Fatal("a visitor QR must not parse as a helper QR")
	}
	if _, ok := visitor.Parse(helper.QR(id)); ok {
		t.Fatal("a helper QR must not parse as a visitor QR")
	}
	for _, bad := range []string{"", "garbage", "NXV1:", "NXV1:" + id.String(), "NXV1:" + id.String() + ".AAAA", "NXV1:not-a-uuid.abc", visitor.QR(uuid.New())[:30]} {
		if _, ok := visitor.Parse(bad); ok {
			t.Fatalf("accepted %q", bad)
		}
	}
	if _, ok := NewSigner("other-secret", "visitor-qr", "NXV1:").Parse(q); ok {
		t.Fatal("a QR signed with another secret must not parse")
	}
}

func TestCodesAndHashes(t *testing.T) {
	re := regexp.MustCompile(`^\d{6}$`)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		c, err := NewCode()
		if err != nil || !re.MatchString(c) {
			t.Fatalf("code %q %v", c, err)
		}
		seen[c] = true
	}
	if len(seen) < 150 {
		t.Fatalf("codes are not random enough: %d distinct of 200", len(seen))
	}
	a, b := uuid.New(), uuid.New()
	if CodeHash(a, "123456") == CodeHash(b, "123456") {
		t.Fatal("hashes must differ per society")
	}
	if CodeHash(a, " 123456 ") != CodeHash(a, "123456") {
		t.Fatal("hash ignores surrounding spaces")
	}
	// vector shared with the apps: printf '11111111-1111-1111-1111-111111111111:482913' | shasum -a 256
	if got := CodeHash(uuid.MustParse("11111111-1111-1111-1111-111111111111"), "482913"); got != "b469b16d45cec2ef3fae377bc379587ce5156d9d6a55a757fe9939e6ae98cdc2" {
		t.Fatalf("vector: %s", got)
	}
}
