// Package authctx carries the authenticated principal through the request context.
package authctx

import (
	"context"

	"github.com/google/uuid"
)

type Principal struct {
	UserID       uuid.UUID
	SocietyID    uuid.UUID
	MembershipID uuid.UUID
	Role         string
}

type ctxKey struct{}

func With(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

func From(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}
