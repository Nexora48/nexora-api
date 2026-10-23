package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"regexp"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Nexora48/nexora-api/internal/notification/sms"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
)

const (
	devOTP         = "123456"
	otpMaxAttempts = 5
	otpCooldown    = 60 * time.Second
	otpMaxPerHour  = 5
	otpRateWindow  = time.Hour
)

var phoneRe = regexp.MustCompile(`^[6-9]\d{9}$`)

var (
	ErrInvalidPhone = httpx.NewError(http.StatusBadRequest, "INVALID_PHONE", "enter a valid 10-digit mobile number")
	ErrOTPCooldown  = httpx.NewError(http.StatusTooManyRequests, "OTP_COOLDOWN", "please wait before requesting another OTP")
	ErrOTPRateLimit = httpx.NewError(http.StatusTooManyRequests, "OTP_RATE_LIMITED", "too many OTP requests, try again later")
	ErrOTPInvalid   = httpx.NewError(http.StatusUnauthorized, "OTP_INVALID", "incorrect OTP")
	ErrOTPExpired   = httpx.NewError(http.StatusUnauthorized, "OTP_EXPIRED", "OTP expired, request a new one")
)

// OTPService issues and verifies one-time codes. Codes are stored only as HMACs.
type OTPService struct {
	rdb    *redis.Client
	sender sms.Sender
	secret []byte
	ttl    time.Duration
	fixed  bool // dev: always 123456
}

func NewOTPService(rdb *redis.Client, sender sms.Sender, secret string, ttl time.Duration, fixedDev bool) *OTPService {
	return &OTPService{rdb: rdb, sender: sender, secret: []byte(secret), ttl: ttl, fixed: fixedDev}
}

func ValidPhone(p string) bool { return phoneRe.MatchString(p) }

func (o *OTPService) mac(phone, code string) string {
	h := hmac.New(sha256.New, o.secret)
	h.Write([]byte(phone + ":" + code))
	return hex.EncodeToString(h.Sum(nil))
}

func (o *OTPService) generate() (string, error) {
	if o.fixed {
		return devOTP, nil
	}
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

func key(kind, phone string) string { return "otp:" + kind + ":" + phone }

// Request creates and sends a code, enforcing cooldown and hourly limits.
func (o *OTPService) Request(ctx context.Context, phone string) error {
	if !ValidPhone(phone) {
		return ErrInvalidPhone
	}
	ok, err := o.rdb.SetNX(ctx, key("cd", phone), 1, otpCooldown).Result()
	if err != nil {
		return err
	}
	if !ok {
		return ErrOTPCooldown
	}
	n, err := o.rdb.Incr(ctx, key("rl", phone)).Result()
	if err != nil {
		return err
	}
	if n == 1 {
		o.rdb.Expire(ctx, key("rl", phone), otpRateWindow)
	}
	if n > otpMaxPerHour {
		return ErrOTPRateLimit
	}
	code, err := o.generate()
	if err != nil {
		return err
	}
	pipe := o.rdb.TxPipeline()
	pipe.Set(ctx, key("code", phone), o.mac(phone, code), o.ttl)
	pipe.Del(ctx, key("att", phone))
	if _, err := pipe.Exec(ctx); err != nil {
		return err
	}
	return o.sender.Send(ctx, phone, fmt.Sprintf("%s is your Nexora verification code. Valid for %d minutes.", code, int(o.ttl.Minutes())))
}

// Verify checks a code; after too many wrong attempts the code is burned.
func (o *OTPService) Verify(ctx context.Context, phone, code string) error {
	if !ValidPhone(phone) {
		return ErrInvalidPhone
	}
	want, err := o.rdb.Get(ctx, key("code", phone)).Result()
	if errors.Is(err, redis.Nil) {
		return ErrOTPExpired
	}
	if err != nil {
		return err
	}
	attempts, err := o.rdb.Incr(ctx, key("att", phone)).Result()
	if err != nil {
		return err
	}
	if attempts == 1 {
		o.rdb.Expire(ctx, key("att", phone), o.ttl)
	}
	if attempts > otpMaxAttempts {
		o.rdb.Del(ctx, key("code", phone))
		return ErrOTPExpired
	}
	if !hmac.Equal([]byte(want), []byte(o.mac(phone, code))) {
		return ErrOTPInvalid
	}
	o.rdb.Del(ctx, key("code", phone), key("att", phone))
	return nil
}
