package app_test

import (
	"context"
	"strings"
	"testing"
)

const pinPath = "/api/v1/me/pin"

func TestPINRulesAndLockout(t *testing.T) {
	c := newCommunity(t)
	dev := "dev-pin-1"

	// no token, no PIN
	if r := call(t, "GET", pinPath+"?device_id="+dev, "", nil); r.Status != 401 {
		t.Fatalf("anonymous: %d", r.Status)
	}
	if r := call(t, "GET", pinPath+"?device_id="+dev, c.rA, nil); r.Status != 200 || r.data()["has_pin"] != false {
		t.Fatalf("status before: %d %v", r.Status, r.Body)
	}
	if r := call(t, "POST", pinPath+"/verify", c.rA, map[string]any{"device_id": dev, "pin": "2580"}); r.Status != 404 || r.errCode() != "PIN_NOT_SET" {
		t.Fatalf("verify without a PIN: %d %v", r.Status, r.Body)
	}

	// weak or malformed PINs are refused
	for _, bad := range []string{"", "123", "1234567", "1111", "1234", "9876", "12a4", "      ", "123456"} {
		if r := call(t, "PUT", pinPath, c.rA, map[string]any{"device_id": dev, "pin": bad}); r.Status != 400 {
			t.Fatalf("PIN %q should be refused: %d %v", bad, r.Status, r.Body)
		}
	}
	if r := call(t, "PUT", pinPath, c.rA, map[string]any{"pin": "2580"}); r.Status != 400 {
		t.Fatalf("missing device: %d", r.Status)
	}
	if r := call(t, "PUT", pinPath, c.rA, map[string]any{"device_id": dev, "pin": "2580"}); r.Status != 204 {
		t.Fatalf("set: %d %v", r.Status, r.Body)
	}
	if r := call(t, "GET", pinPath+"?device_id="+dev, c.rA, nil); r.data()["has_pin"] != true {
		t.Fatalf("status after: %v", r.Body)
	}

	// right PIN works any number of times; the hash is not the PIN
	for i := 0; i < 3; i++ {
		if r := call(t, "POST", pinPath+"/verify", c.rA, map[string]any{"device_id": dev, "pin": "2580"}); r.Status != 204 {
			t.Fatalf("verify ok #%d: %d %v", i, r.Status, r.Body)
		}
	}
	var hash string
	if err := pool.QueryRow(context.Background(), `SELECT pin_hash FROM user_pins WHERE device_id=$1`, dev).Scan(&hash); err != nil || hash == "2580" || len(hash) < 40 {
		t.Fatalf("stored hash: %q %v", hash, err)
	}

	// another device and another person have their own PIN state
	if r := call(t, "POST", pinPath+"/verify", c.rA, map[string]any{"device_id": "other-device", "pin": "2580"}); r.Status != 404 {
		t.Fatalf("other device: %d", r.Status)
	}
	if r := call(t, "POST", pinPath+"/verify", c.rB, map[string]any{"device_id": dev, "pin": "2580"}); r.Status != 404 {
		t.Fatalf("other user on the same device id: %d", r.Status)
	}

	// wrong tries count down, a right one resets the counter
	for i, left := range []string{"4", "3"} {
		r := call(t, "POST", pinPath+"/verify", c.rA, map[string]any{"device_id": dev, "pin": "1357"})
		if r.Status != 403 || r.errCode() != "PIN_INVALID" || !contains(r.Body, left+" tries left") {
			t.Fatalf("wrong #%d: %d %v", i, r.Status, r.Body)
		}
	}
	if r := call(t, "POST", pinPath+"/verify", c.rA, map[string]any{"device_id": dev, "pin": "2580"}); r.Status != 204 {
		t.Fatalf("right after wrong: %d", r.Status)
	}
	r := call(t, "POST", pinPath+"/verify", c.rA, map[string]any{"device_id": dev, "pin": "1357"})
	if r.Status != 403 || !contains(r.Body, "4 tries left") {
		t.Fatalf("counter was not reset: %d %v", r.Status, r.Body)
	}

	// five wrong in a row: PIN removed and the device's sessions revoked
	for i := 0; i < 3; i++ {
		call(t, "POST", pinPath+"/verify", c.rA, map[string]any{"device_id": dev, "pin": "1357"})
	}
	if r := call(t, "POST", pinPath+"/verify", c.rA, map[string]any{"device_id": dev, "pin": "1357"}); r.Status != 423 || r.errCode() != "PIN_LOCKED" {
		t.Fatalf("lockout: %d %v", r.Status, r.Body)
	}
	if r := call(t, "GET", pinPath+"?device_id="+dev, c.rA, nil); r.data()["has_pin"] != false {
		t.Fatalf("PIN should be gone after lockout: %v", r.Body)
	}

	// set again, then forget (sign-out)
	call(t, "PUT", pinPath, c.rA, map[string]any{"device_id": dev, "pin": "2580"})
	if r := call(t, "PUT", pinPath, c.rA, map[string]any{"device_id": dev, "pin": "7413"}); r.Status != 204 {
		t.Fatalf("replace: %d", r.Status)
	}
	if r := call(t, "POST", pinPath+"/verify", c.rA, map[string]any{"device_id": dev, "pin": "2580"}); r.Status != 403 {
		t.Fatalf("old PIN must stop working: %d", r.Status)
	}
	if r := call(t, "DELETE", pinPath+"?device_id="+dev, c.rA, nil); r.Status != 204 {
		t.Fatalf("forget: %d", r.Status)
	}
	if r := call(t, "POST", pinPath+"/verify", c.rA, map[string]any{"device_id": dev, "pin": "7413"}); r.Status != 404 {
		t.Fatalf("verify after forget: %d", r.Status)
	}
}

func contains(body map[string]any, sub string) bool {
	e, _ := body["error"].(map[string]any)
	m, _ := e["message"].(string)
	return strings.Contains(m, sub)
}
