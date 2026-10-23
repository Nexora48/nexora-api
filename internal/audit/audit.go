package audit

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Entry struct {
	ActorID   *uuid.UUID
	ActorType string // user | platform | system
	SocietyID *uuid.UUID
	Action    string
	Entity    string
	EntityID  string
	Meta      map[string]any
	IP        string
}

type Logger struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Logger { return &Logger{pool: pool} }

// Log never fails the caller; audit errors are logged.
func (l *Logger) Log(ctx context.Context, e Entry) {
	if e.ActorType == "" {
		e.ActorType = "user"
	}
	meta, _ := json.Marshal(e.Meta)
	if e.Meta == nil {
		meta = []byte("{}")
	}
	_, err := l.pool.Exec(ctx, `
		INSERT INTO audit_logs (actor_id, actor_type, society_id, action, entity, entity_id, meta, ip)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		e.ActorID, e.ActorType, e.SocietyID, e.Action, e.Entity, e.EntityID, meta, e.IP)
	if err != nil {
		slog.Error("audit log failed", "action", e.Action, "err", err)
	}
}
