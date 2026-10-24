package visitor

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"math/big"
	"strings"

	"github.com/google/uuid"
)

const qrPrefix = "NXV1:"

// newPassCode returns a random 6-digit OTP (leading zeros allowed).
func newPassCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// qrPayload is derived from the visit id with an HMAC, so it can be re-displayed any time
// without storing a secret, and cannot be forged for another visit.
func (s *Service) qrPayload(id uuid.UUID) string {
	return qrPrefix + id.String() + "." + s.sign(id)
}

func (s *Service) sign(id uuid.UUID) string {
	m := hmac.New(sha256.New, s.qrKey)
	m.Write([]byte(id.String()))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:12])
}

// parseQR validates a scanned payload and returns the visit id.
func (s *Service) parseQR(payload string) (uuid.UUID, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(payload), qrPrefix)
	if !ok {
		return uuid.Nil, false
	}
	idPart, sig, ok := strings.Cut(rest, ".")
	if !ok {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(idPart)
	if err != nil {
		return uuid.Nil, false
	}
	return id, hmac.Equal([]byte(sig), []byte(s.sign(id)))
}
