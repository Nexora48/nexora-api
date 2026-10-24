// Package live turns visit changes into realtime events and push notifications.
package live

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nexora48/nexora-api/internal/notify"
	"github.com/Nexora48/nexora-api/internal/realtime"
	"github.com/Nexora48/nexora-api/internal/visitor"
)

type Dispatcher struct {
	hub  *realtime.Hub
	push *notify.Service
	pool *pgxpool.Pool
	wait time.Duration
}

func NewDispatcher(hub *realtime.Hub, push *notify.Service, pool *pgxpool.Pool, walkinWait time.Duration) *Dispatcher {
	if walkinWait <= 0 {
		walkinWait = 10 * time.Minute
	}
	return &Dispatcher{hub: hub, push: push, pool: pool, wait: walkinWait}
}

// VisitEvent implements visitor.Events. The realtime payload is enough to render a card; push carries no visitor data.
func (d *Dispatcher) VisitEvent(ctx context.Context, typ string, v *visitor.Visit) {
	soc, ok := d.society(ctx, v.ID)
	if !ok {
		return
	}
	unit := v.UnitID
	data := map[string]any{
		"visit_id": v.ID, "status": v.Status, "unit_id": v.UnitID, "flat": v.BlockCode + "-" + v.UnitNumber,
		"visitor_name": v.VisitorName, "purpose": v.Purpose, "company": v.Company, "visitor_count": v.VisitorCount,
		"has_photo": v.HasPhoto, "decision_reason": v.DecisionReason, "created_at": v.CreatedAt,
		"expires_at": v.CreatedAt.Add(d.wait), "source": v.Source, "gate_name": v.GateName,
	}
	e := realtime.Event{Type: typ, SocietyID: soc, Unit: &unit, Data: data}
	if typ != "visit.requested" {
		e.Guards = true
	}
	d.hub.Publish(ctx, e)

	if typ == "visit.requested" && d.push != nil {
		d.push.ToUsers(ctx, d.residents(ctx, soc, v.UnitID), notify.Message{
			Title: "Visitor at the gate",
			Body:  "Someone is waiting to enter. Tap to respond.",
			Data:  map[string]string{"type": "visit_request", "visit_id": v.ID.String()},
		})
	}
}

// FeaturesChanged tells every connected app of the society to reload its service switches.
func (d *Dispatcher) FeaturesChanged(ctx context.Context, societyID uuid.UUID) {
	d.hub.Publish(ctx, realtime.Event{Type: "features.updated", SocietyID: societyID, All: true})
}

func (d *Dispatcher) society(ctx context.Context, visitID uuid.UUID) (uuid.UUID, bool) {
	var s uuid.UUID
	if err := d.pool.QueryRow(ctx, `SELECT society_id FROM visits WHERE id=$1`, visitID).Scan(&s); err != nil {
		return uuid.Nil, false
	}
	return s, true
}

func (d *Dispatcher) residents(ctx context.Context, soc, unit uuid.UUID) []uuid.UUID {
	rows, err := d.pool.Query(ctx, `SELECT user_id FROM memberships WHERE society_id=$1 AND unit_id=$2 AND status='active' AND role_key IN ('resident_owner','resident_tenant')`, soc, unit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out
}
