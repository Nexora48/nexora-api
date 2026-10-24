// Package pass holds the pieces every gate pass shares: 6-digit codes, HMAC-signed QR payloads that can be
// shown again at any time without storing a secret, and the hashed codes the guard app caches for offline use.
package pass

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"

	"github.com/google/uuid"
)

// NewCode returns a random 6-digit code (leading zeros allowed).
func NewCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// Signer signs QR payloads of one kind of pass. The key is derived from the server secret and a purpose
// string, so a visitor QR can never be replayed as a helper or staff QR.
type Signer struct {
	key    []byte
	prefix string
}

// NewSigner derives the key exactly as the visitor module always did (purpose "visitor-qr"), so existing
// visitor QR codes stay valid.
func NewSigner(secret, purpose, prefix string) *Signer {
	k := hmac.New(sha256.New, []byte(secret))
	k.Write([]byte(purpose))
	return &Signer{key: k.Sum(nil), prefix: prefix}
}

func (s *Signer) sign(id uuid.UUID) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(id.String()))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:12])
}

// QR is the payload shown as a QR code: <prefix><id>.<signature>.
func (s *Signer) QR(id uuid.UUID) string { return s.prefix + id.String() + "." + s.sign(id) }

// Parse validates a scanned payload of this kind and returns the id inside it.
func (s *Signer) Parse(payload string) (uuid.UUID, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(payload), s.prefix)
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

// CodeHash is the offline lookup key: sha256("<society id>:<code>").
func CodeHash(societyID uuid.UUID, code string) string {
	h := sha256.Sum256([]byte(societyID.String() + ":" + strings.TrimSpace(code)))
	return hex.EncodeToString(h[:])
}
