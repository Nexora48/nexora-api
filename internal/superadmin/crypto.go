// Package superadmin is the internal Nexora console backend: platform staff auth (password + TOTP),
// society onboarding, per-society service switches, suspension and audit queries.
package superadmin

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
)

// Cipher encrypts small secrets (TOTP seeds) at rest with AES-256-GCM.
type Cipher struct{ aead cipher.AEAD }

func NewCipher(key string) (*Cipher, error) {
	sum := sha256.Sum256([]byte(key))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}

func (c *Cipher) Encrypt(plain string) (string, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(c.aead.Seal(nonce, nonce, []byte(plain), nil)), nil
}

func (c *Cipher) Decrypt(enc string) (string, error) {
	raw, err := base64.RawStdEncoding.DecodeString(enc)
	if err != nil {
		return "", err
	}
	if len(raw) < c.aead.NonceSize() {
		return "", errors.New("ciphertext too short")
	}
	nonce, ct := raw[:c.aead.NonceSize()], raw[c.aead.NonceSize():]
	out, err := c.aead.Open(nil, nonce, ct, nil)
	return string(out), err
}
