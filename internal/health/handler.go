package health

import (
	"context"
	"net/http"
	"time"

	"github.com/Nexora48/nexora-api/internal/platform/httpx"
)

// Pinger is satisfied by pgxpool.Pool and redis.Client wrappers.
type Pinger func(ctx context.Context) error

type Handler struct {
	checks map[string]Pinger
}

func New(checks map[string]Pinger) *Handler { return &Handler{checks: checks} }

// Live reports process liveness only.
func (h *Handler) Live(w http.ResponseWriter, r *http.Request) {
	httpx.OK(w, map[string]string{"status": "ok", "service": "nexora-api"}, nil)
}

// Ready checks dependencies.
func (h *Handler) Ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	result := map[string]string{}
	status := http.StatusOK
	for name, ping := range h.checks {
		if err := ping(ctx); err != nil {
			result[name] = "down"
			status = http.StatusServiceUnavailable
		} else {
			result[name] = "up"
		}
	}
	httpx.JSON(w, status, map[string]any{"data": result})
}
