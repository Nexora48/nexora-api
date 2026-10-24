package notify

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestFCM(t *testing.T, sendStatus int, sendBody string) (*FCM, *[]string) {
	t.Helper()
	var seen []string
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") == "" || r.Form.Get("assertion") == "" {
			http.Error(w, "bad", 400)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at-123", "expires_in": 3600})
	})
	mux.HandleFunc("/send", func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 4096)
		n, _ := r.Body.Read(b)
		seen = append(seen, r.Header.Get("Authorization")+"|"+string(b[:n]))
		w.WriteHeader(sendStatus)
		_, _ = w.Write([]byte(sendBody))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	sa, _ := json.Marshal(map[string]string{"project_id": "p1", "client_email": "svc@p1.iam", "private_key": string(pemKey), "token_uri": srv.URL + "/token"})
	file := filepath.Join(t.TempDir(), "sa.json")
	_ = os.WriteFile(file, sa, 0o600)
	f, err := NewFCM("", file)
	if err != nil {
		t.Fatal(err)
	}
	f.sendURL = srv.URL + "/send"
	return f, &seen
}

func TestFCMSends(t *testing.T) {
	f, seen := newTestFCM(t, 200, `{}`)
	err := f.Send(context.Background(), "device-token", Message{Title: "Visitor at the gate", Body: "Tap", Data: map[string]string{"visit_id": "v1"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(*seen) != 1 || !strings.HasPrefix((*seen)[0], "Bearer at-123|") || !strings.Contains((*seen)[0], `"token":"device-token"`) || !strings.Contains((*seen)[0], `"visit_id":"v1"`) {
		t.Fatalf("request: %v", *seen)
	}
	// the access token is reused
	_ = f.Send(context.Background(), "t2", Message{Title: "x"})
	if len(*seen) != 2 {
		t.Fatalf("second send: %v", *seen)
	}
}

func TestFCMUnregistered(t *testing.T) {
	f, _ := newTestFCM(t, 404, `{"error":{"status":"NOT_FOUND","details":[{"errorCode":"UNREGISTERED"}]}}`)
	if err := f.Send(context.Background(), "gone", Message{Title: "x"}); err != ErrUnregistered {
		t.Fatalf("want ErrUnregistered, got %v", err)
	}
	f2, _ := newTestFCM(t, 500, `boom`)
	if err := f2.Send(context.Background(), "t", Message{Title: "x"}); err == nil || err == ErrUnregistered {
		t.Fatalf("server error must be a plain error, got %v", err)
	}
}

func TestFCMBadCredentials(t *testing.T) {
	file := filepath.Join(t.TempDir(), "sa.json")
	_ = os.WriteFile(file, []byte(`{"project_id":"p","private_key":"nope"}`), 0o600)
	if _, err := NewFCM("", file); err == nil {
		t.Fatal("invalid key must be rejected")
	}
	if _, err := NewFCM("", "/does/not/exist.json"); err == nil {
		t.Fatal("missing file must be rejected")
	}
}
