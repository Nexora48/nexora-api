package visitor

import (
	"github.com/google/uuid"

	"github.com/Nexora48/nexora-api/internal/pass"
)

const qrPrefix = "NXV1:"

// newPassCode returns a random 6-digit OTP (leading zeros allowed).
func newPassCode() (string, error) { return pass.NewCode() }

// qrPayload is derived from the visit id with an HMAC, so it can be re-displayed any time
// without storing a secret, and cannot be forged for another visit.
func (s *Service) qrPayload(id uuid.UUID) string { return s.qr.QR(id) }

// parseQR validates a scanned payload and returns the visit id.
func (s *Service) parseQR(payload string) (uuid.UUID, bool) { return s.qr.Parse(payload) }
