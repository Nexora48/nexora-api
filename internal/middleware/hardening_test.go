package middleware

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBodyCeilingRejectsHugeBodies(t *testing.T) {
	h := BodyCeiling(1024)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			http.Error(w, "too big", http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	small := httptest.NewRecorder()
	h.ServeHTTP(small, httptest.NewRequest("POST", "/x", strings.NewReader("ok")))
	if small.Code != http.StatusNoContent {
		t.Fatalf("small body: %d", small.Code)
	}
	big := httptest.NewRecorder()
	h.ServeHTTP(big, httptest.NewRequest("POST", "/x", bytes.NewReader(make([]byte, 4096))))
	if big.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("big body: %d", big.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	api := httptest.NewRecorder()
	h.ServeHTTP(api, httptest.NewRequest("GET", "/api/v1/me", nil))
	for k, v := range map[string]string{"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer", "Cache-Control": "no-store"} {
		if api.Header().Get(k) != v {
			t.Errorf("%s = %q", k, api.Header().Get(k))
		}
	}
	page := httptest.NewRecorder()
	h.ServeHTTP(page, httptest.NewRequest("GET", "/pay/1", nil))
	if page.Header().Get("Cache-Control") != "" {
		t.Error("non-API paths must not get no-store")
	}
}

func TestLoggerFlagsSlowAndFailingRequests(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	h := Logger(log, 10*time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/slow":
			time.Sleep(30 * time.Millisecond)
		case "/boom":
			w.WriteHeader(500)
		}
	}))
	for _, p := range []string{"/fast", "/slow", "/boom"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", p, nil))
	}
	out := buf.String()
	if !strings.Contains(out, `msg="slow request"`) || !strings.Contains(out, "path=/slow") {
		t.Errorf("slow request not flagged:\n%s", out)
	}
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "path=/boom") {
		t.Errorf("5xx not logged as error:\n%s", out)
	}
	if strings.Count(out, "level=INFO") != 1 {
		t.Errorf("the fast request should be INFO only:\n%s", out)
	}
}
