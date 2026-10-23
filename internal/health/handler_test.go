package health

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLive(t *testing.T) {
	rec := httptest.NewRecorder()
	New(nil).Live(rec, httptest.NewRequest("GET", "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestReadyDown(t *testing.T) {
	h := New(map[string]Pinger{"db": func(context.Context) error { return errors.New("x") }})
	rec := httptest.NewRecorder()
	h.Ready(rec, httptest.NewRequest("GET", "/ready", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d", rec.Code)
	}
}
