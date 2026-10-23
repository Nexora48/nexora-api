package middleware

import (
	"net/http"
	"strings"

	"github.com/Nexora48/nexora-api/internal/auth"
	"github.com/Nexora48/nexora-api/internal/feature"
	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
	"github.com/Nexora48/nexora-api/internal/rbac"
	"github.com/Nexora48/nexora-api/internal/society"
)

// Auth verifies the Bearer access token and stores the principal in the context.
func Auth(tm *auth.TokenManager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := r.Header.Get("Authorization")
			tok, ok := strings.CutPrefix(h, "Bearer ")
			if !ok || tok == "" {
				httpx.Fail(w, httpx.NewError(http.StatusUnauthorized, "UNAUTHORIZED", "missing bearer token"))
				return
			}
			p, err := tm.ParseAccess(tok)
			if err != nil {
				httpx.Fail(w, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(authctx.With(r.Context(), p)))
		})
	}
}

// Tenant rejects requests for suspended or unknown societies.
// The society always comes from the token, never from the request.
func Tenant(societies *society.Repository) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := authctx.From(r.Context())
			if !ok {
				httpx.Fail(w, httpx.NewError(http.StatusUnauthorized, "UNAUTHORIZED", "not authenticated"))
				return
			}
			s, err := societies.Get(r.Context(), p.SocietyID)
			if err != nil {
				httpx.Fail(w, httpx.NewError(http.StatusUnauthorized, "UNAUTHORIZED", "society not found"))
				return
			}
			if !s.IsUsable() {
				httpx.Fail(w, auth.ErrSocietySuspended)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequirePermission checks the principal's role grants perm.
func RequirePermission(rb *rbac.Service, perm string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, _ := authctx.From(r.Context())
			ok, err := rb.Has(r.Context(), p.Role, perm)
			if err != nil {
				httpx.Fail(w, err)
				return
			}
			if !ok {
				httpx.Fail(w, httpx.NewError(http.StatusForbidden, "FORBIDDEN", "you do not have permission to do this"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireFeature blocks routes of a product feature that is disabled for the society.
func RequireFeature(f *feature.Service, key string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, _ := authctx.From(r.Context())
			on, err := f.Enabled(r.Context(), p.SocietyID, key)
			if err != nil {
				httpx.Fail(w, err)
				return
			}
			if !on {
				httpx.Fail(w, httpx.NewError(http.StatusForbidden, "FEATURE_DISABLED", "this service is not enabled for your society"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequirePasswordChanged blocks accounts that still use a temporary password.
func RequirePasswordChanged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := authctx.From(r.Context()); ok && p.MustChangePassword {
			httpx.Fail(w, httpx.NewError(http.StatusForbidden, "PASSWORD_CHANGE_REQUIRED", "set a new password to continue"))
			return
		}
		next.ServeHTTP(w, r)
	})
}
