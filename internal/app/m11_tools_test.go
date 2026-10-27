package app_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// cmd/razorpay-check against a stub of Razorpay: the same steps you will run with real test keys.
func TestRazorpayCheckCommand(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a command")
	}
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "rzp_test_abc" || pass != "secret" {
			http.Error(w, `{"error":{"description":"auth"}}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"id":"order_stub123"}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer stub.Close()

	run := func(env ...string) (string, error) {
		cmd := exec.Command("go", "run", "../../cmd/razorpay-check")
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	good := []string{"RAZORPAY_KEY_ID=rzp_test_abc", "RAZORPAY_KEY_SECRET=secret", "RAZORPAY_WEBHOOK_SECRET=whsec", "RAZORPAY_BASE_URL=" + stub.URL}
	out, err := run(good...)
	if err != nil || !strings.Contains(out, "All checks passed") || !strings.Contains(out, "a tampered body is rejected") {
		t.Fatalf("good keys: %v\n%s", err, out)
	}
	// wrong secret: the account check fails and says so
	if out, err := run("RAZORPAY_KEY_ID=rzp_test_abc", "RAZORPAY_KEY_SECRET=wrong", "RAZORPAY_WEBHOOK_SECRET=whsec", "RAZORPAY_BASE_URL="+stub.URL); err == nil || !strings.Contains(out, "FAIL") {
		t.Fatalf("wrong secret must fail: %v\n%s", err, out)
	}
	// live keys are refused before anything is sent
	if out, err := run("RAZORPAY_KEY_ID=rzp_live_abc", "RAZORPAY_KEY_SECRET=secret", "RAZORPAY_WEBHOOK_SECRET=whsec", "RAZORPAY_BASE_URL="+stub.URL); err == nil || !strings.Contains(out, "not a test key") {
		t.Fatalf("live keys must be refused: %v\n%s", err, out)
	}
}
