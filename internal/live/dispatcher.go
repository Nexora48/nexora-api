// Package live turns visit changes into realtime events and notifications.
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
	note *notify.Service
	pool *pgxpool.Pool
	wait time.Duration
}

func NewDispatcher(hub *realtime.Hub, note *notify.Service, pool *pgxpool.Pool, walkinWait time.Duration) *Dispatcher {
	if walkinWait <= 0 {
		walkinWait = 10 * time.Minute
	}
	return &Dispatcher{hub: hub, note: note, pool: pool, wait: walkinWait}
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
		"auto_approved": v.DecidedByRule, "handover": v.Handover,
	}
	e := realtime.Event{Type: typ, SocietyID: soc, Unit: &unit, Data: data}
	if typ != "visit.requested" {
		e.Guards = true
	}
	d.hub.Publish(ctx, e)
	if d.note == nil {
		return
	}

	switch typ {
	case "visit.requested":
		d.note.Send(ctx, notify.Notification{
			SocietyID: soc, Users: d.note.UnitResidents(ctx, soc, v.UnitID), Type: "visit.request",
			Title: "Visitor at the gate", Body: "Someone is waiting to enter. Tap to respond.",
			Data: map[string]any{"type": "visit_request", "visit_id": v.ID}, Priority: notify.PriorityNormal,
		})
	case "visit.arrived":
		body := "A visitor was let in by your rule."
		if v.Handover != nil && *v.Handover == "leave_at_gate" {
			body = "A delivery was left at the gate."
		} else if v.Purpose == "delivery" || v.Purpose == "cab" {
			body = "A " + v.Purpose + " was let in by your rule."
		}
		d.note.Send(ctx, notify.Notification{
			SocietyID: soc, Users: d.note.UnitResidents(ctx, soc, v.UnitID), Type: "visit.arrived",
			Title: "Arrival at the gate", Body: body,
			Data: map[string]any{"type": "visit_arrived", "visit_id": v.ID}, Priority: notify.PriorityNormal,
		})
	case "visit.decided":
		word := "approved"
		if v.Status == visitor.StatusRejected {
			word = "rejected"
		}
		d.note.Send(ctx, notify.Notification{
			SocietyID: soc, Users: d.note.UnitMembers(ctx, soc, v.UnitID), Type: "visit.decided",
			Title: "Visitor request " + word, Body: "A visitor request for your flat was " + word + ".",
			Data: map[string]any{"type": "visit_decided", "visit_id": v.ID}, Priority: notify.PrioritySilent,
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
