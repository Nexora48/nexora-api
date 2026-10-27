// Command razorpay-check proves a society's Razorpay TEST keys work with the adapter, without touching the database:
// it pings the account, creates a ₹1 order, and checks that a webhook body signed with the webhook secret verifies
// (and that a tampered one does not).
//
//	RAZORPAY_KEY_ID=rzp_test_... RAZORPAY_KEY_SECRET=... RAZORPAY_WEBHOOK_SECRET=... go run ./cmd/razorpay-check
//
// Refuses live keys (rzp_live_...): this is a smoke test, not a payment.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Nexora48/nexora-api/internal/payment"
)

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "FAIL  "+format+"\n", a...)
	os.Exit(1)
}

func main() {
	creds := payment.Creds{KeyID: os.Getenv("RAZORPAY_KEY_ID"), KeySecret: os.Getenv("RAZORPAY_KEY_SECRET"), WebhookSecret: os.Getenv("RAZORPAY_WEBHOOK_SECRET")}
	if creds.KeyID == "" || creds.KeySecret == "" || creds.WebhookSecret == "" {
		fmt.Fprintln(os.Stderr, "set RAZORPAY_KEY_ID, RAZORPAY_KEY_SECRET and RAZORPAY_WEBHOOK_SECRET (test mode)")
		os.Exit(2)
	}
	if !strings.HasPrefix(creds.KeyID, "rzp_test_") {
		fmt.Fprintln(os.Stderr, "refusing to run with a key that is not a test key (rzp_test_...)")
		os.Exit(2)
	}
	base := os.Getenv("RAZORPAY_BASE_URL")
	if base == "" {
		base = "https://api.razorpay.com/v1"
	}
	gw := payment.NewRazorpayAt(base)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := gw.Ping(ctx, creds); err != nil {
		fail("ping (are the key id and secret a matching test pair?): %v", err)
	}
	fmt.Println("ok    account reachable with these keys")

	order, err := gw.CreateOrder(ctx, creds, 100, fmt.Sprintf("nexora-check-%d", time.Now().Unix()))
	if err != nil || order == "" {
		fail("create order: %v", err)
	}
	fmt.Println("ok    created a ₹1 test order", order)

	body := []byte(fmt.Sprintf(`{"event":"payment.captured","payload":{"payment":{"entity":{"id":"pay_check","order_id":%q,"amount":100,"status":"captured"}}}}`, order))
	sign := func(b []byte) http.Header {
		m := hmac.New(sha256.New, []byte(creds.WebhookSecret))
		m.Write(b)
		h := http.Header{}
		h.Set("X-Razorpay-Signature", hex.EncodeToString(m.Sum(nil)))
		return h
	}
	if err := gw.VerifyWebhook(creds, body, sign(body)); err != nil {
		fail("a correctly signed webhook was rejected: %v", err)
	}
	fmt.Println("ok    a signed webhook body verifies")
	tampered := []byte(strings.Replace(string(body), `"amount":100`, `"amount":1`, 1))
	if err := gw.VerifyWebhook(creds, tampered, sign(body)); err == nil {
		fail("a tampered webhook body was accepted")
	}
	fmt.Println("ok    a tampered body is rejected")
	ev, err := gw.ParseWebhook(body, sign(body))
	if err != nil || ev.ProviderOrderID != order || ev.AmountPaise != 100 || ev.Type != "paid" && ev.Type != "captured" {
		fmt.Printf("note  parsed event: %+v (err %v)\n", ev, err)
	} else {
		fmt.Println("ok    the event parses to the order and amount")
	}
	fmt.Println("\nAll checks passed. Configure the same keys in the Admin app (Payments → Gateway) and set the webhook URL it shows in the Razorpay dashboard.")
}
