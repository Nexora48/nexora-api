// Package errtrack sends panics and server errors to Sentry when SENTRY_DSN is set. It is off otherwise.
// Events never carry personal data: no headers, cookies, bodies, query strings or user fields; only the
// route, request id and society id.
package errtrack

import (
	"fmt"
	"net/http"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/Nexora48/nexora-api/internal/platform/authctx"
)

var enabled bool

// Init returns a flush function to call on shutdown. An empty DSN leaves tracking off.
func Init(dsn, env, release string) (flush func(), err error) {
	if dsn == "" {
		return func() {}, nil
	}
	err = sentry.Init(sentry.ClientOptions{Dsn: dsn, Environment: env, Release: release, SendDefaultPII: false,
		AttachStacktrace: true, BeforeSend: scrub})
	if err != nil {
		return func() {}, err
	}
	enabled = true
	return func() { sentry.Flush(2 * time.Second) }, nil
}

// Enabled reports whether events are being sent.
func Enabled() bool { return enabled }

// scrub removes everything that could hold personal data.
func scrub(e *sentry.Event, _ *sentry.EventHint) *sentry.Event {
	e.User = sentry.User{}
	if e.Request != nil {
		e.Request = &sentry.Request{Method: e.Request.Method, URL: stripQuery(e.Request.URL)}
	}
	e.Contexts = nil
	return e
}

func stripQuery(u string) string {
	for i := 0; i < len(u); i++ {
		if u[i] == '?' {
			return u[:i]
		}
	}
	return u
}

// Panic reports a recovered panic.
func Panic(r *http.Request, rec any) {
	if !enabled {
		return
	}
	hub := sentry.CurrentHub().Clone()
	hub.WithScope(func(sc *sentry.Scope) {
		tag(sc, r)
		hub.CaptureException(fmt.Errorf("panic: %v", rec))
	})
}

// ServerError reports a 5xx the handler returned without panicking.
func ServerError(r *http.Request, status int) {
	if !enabled {
		return
	}
	hub := sentry.CurrentHub().Clone()
	hub.WithScope(func(sc *sentry.Scope) {
		tag(sc, r)
		sc.SetLevel(sentry.LevelError)
		hub.CaptureMessage(fmt.Sprintf("%d %s %s", status, r.Method, route(r)))
	})
}

func route(r *http.Request) string {
	if rc := chi.RouteContext(r.Context()); rc != nil && rc.RoutePattern() != "" {
		return rc.RoutePattern()
	}
	return "unmatched"
}

func tag(sc *sentry.Scope, r *http.Request) {
	sc.SetTag("route", route(r))
	sc.SetTag("method", r.Method)
	sc.SetTag("request_id", chimw.GetReqID(r.Context()))
	if p, ok := authctx.From(r.Context()); ok {
		sc.SetTag("society_id", p.SocietyID.String())
	}
}
