package limits

import (
	"net/http"

	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
)

// MyPlan is GET /society/plan: the society's own plan, limits and usage (read-only).
func (c *Checker) MyPlan(w http.ResponseWriter, r *http.Request) {
	p, ok := authctx.From(r.Context())
	if !ok {
		httpx.Fail(w, httpx.NewError(http.StatusUnauthorized, "UNAUTHORIZED", "sign in first"))
		return
	}
	snap, err := c.Snapshot(r.Context(), p.SocietyID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, snap, nil)
}
