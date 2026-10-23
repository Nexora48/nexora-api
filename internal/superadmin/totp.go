package superadmin

import (
	"crypto/rand"
	"crypto/subtle"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

const (
	totpPeriod = 30
	totpIssuer = "Nexora SuperAdmin"
)

var totpOpts = totp.ValidateOpts{Period: totpPeriod, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1}

// NewTOTPSecret returns a fresh base32 secret and its otpauth:// URL for QR codes.
func NewTOTPSecret(accountName string) (secret, url string, err error) {
	k, err := totp.Generate(totp.GenerateOpts{Issuer: totpIssuer, AccountName: accountName})
	if err != nil {
		return "", "", err
	}
	return k.Secret(), k.URL(), nil
}

// MatchTOTP checks code against the previous, current and next 30 s step (clock skew)
// and returns the matched step so the caller can reject replays.
func MatchTOTP(secret, code string, now time.Time) (step int64, ok bool) {
	cur := now.Unix() / totpPeriod
	for _, s := range []int64{cur, cur - 1, cur + 1} {
		want, err := totp.GenerateCodeCustom(secret, time.Unix(s*totpPeriod, 0), totpOpts)
		if err != nil {
			return 0, false
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return s, true
		}
	}
	return 0, false
}

// CurrentTOTP is used by dev tooling and tests to compute the code for a given offset in steps.
func CurrentTOTP(secret string, now time.Time, stepOffset int) (string, error) {
	return totp.GenerateCodeCustom(secret, now.Add(time.Duration(stepOffset*totpPeriod)*time.Second), totpOpts)
}

const recoveryAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

// NewRecoveryCodes returns n one-time recovery codes like "k3m9x-q7wd2".
func NewRecoveryCodes(n int) []string {
	out := make([]string, n)
	for i := range out {
		b := make([]byte, 10)
		_, _ = rand.Read(b)
		code := make([]byte, 0, 11)
		for j, x := range b {
			if j == 5 {
				code = append(code, '-')
			}
			code = append(code, recoveryAlphabet[int(x)%len(recoveryAlphabet)])
		}
		out[i] = string(code)
	}
	return out
}
