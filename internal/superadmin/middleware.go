package superadmin

import (
	"context"
	"net"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/Nexora48/nexora-api/internal/platform/httpx"
)

const (
	RoleSuper   = "super_admin"
	RoleSupport = "platform_support"
)

// RolePlatformSupport returns the read-only role key (exported for tests and tooling).
func RolePlatformSupport() string { return RoleSupport }

type Principal struct {
	UserID uuid.UUID
	Role   string
}

type ctxKey struct{}

func principalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

var (
	errNoToken   = httpx.NewError(http.StatusUnauthorized, "UNAUTHORIZED", "missing bearer token")
	errForbidden = httpx.NewError(http.StatusForbidden, "PLATFORM_FORBIDDEN", "your role cannot do this")
	errIP        = httpx.NewError(http.StatusForbidden, "IP_NOT_ALLOWED", "this address is not allowed")
)

// IPAllowlist restricts the console API to the given CIDRs; empty list = allow all.
func IPAllowlist(cidrs []string) func(http.Handler) http.Handler {
	var nets []*net.IPNet
	for _, c := range cidrs {
		if _, n, err := net.ParseCIDR(strings.TrimSpace(c)); err == nil {
			nets = append(nets, n)
		}
	}
	return func(next http.Handler) http.Handler {
		if len(nets) == 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				host = r.RemoteAddr
			}
			ip := net.ParseIP(host)
			for _, n := range nets {
				if ip != nil && n.Contains(ip) {
					next.ServeHTTP(w, r)
					return
				}
			}
			httpx.Fail(w, errIP)
		})
	}
}

// PlatformAuth verifies the platform bearer token and checks the staff user is still active.
func (s *Service) PlatformAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || tok == "" {
			httpx.Fail(w, errNoToken)
			return
		}
		p, err := s.tokens.ParseAccess(tok)
		if err != nil {
			httpx.Fail(w, err)
			return
		}
		u, err := s.store.userByID(r.Context(), p.UserID)
		if err != nil || u.Status != "active" {
			httpx.Fail(w, ErrTokenInvalid)
			return
		}
		p.Role = u.Role // role changes apply immediately
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
	})
}

// RequireSuper limits a route to super_admin staff.
func RequireSuper(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := principalFrom(r.Context())
		if p.Role != RoleSuper {
			httpx.Fail(w, errForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
