package gate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
	"github.com/Nexora48/nexora-api/internal/visitor"
)

type WalkInPayload struct {
	UnitID        string  `json:"unit_id" validate:"required,uuid"`
	VisitorName   string  `json:"visitor_name" validate:"required,min=2,max=80"`
	VisitorPhone  *string `json:"visitor_phone" validate:"omitempty,max=20"`
	Purpose       string  `json:"purpose" validate:"required,oneof=guest delivery cab service"`
	Company       *string `json:"company" validate:"omitempty,max=60"`
	VehicleNumber *string `json:"vehicle_number" validate:"omitempty,max=20"`
	VisitorCount  int     `json:"visitor_count" validate:"omitempty,min=1,max=20"`
}

type BatchEvent struct {
	ClientEventID string         `json:"client_event_id" validate:"required,min=8,max=64"`
	Type          string         `json:"type" validate:"required,oneof=check_in check_out walk_in"`
	VisitID       string         `json:"visit_id" validate:"omitempty,uuid"`
	WalkIn        *WalkInPayload `json:"walk_in" validate:"omitempty"`
}

type BatchRequest struct {
	Events []BatchEvent `json:"events" validate:"required,min=1,max=50,dive"`
}

type BatchError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type BatchResult struct {
	ClientEventID string      `json:"client_event_id"`
	Status        int         `json:"status"`
	Replayed      bool        `json:"replayed,omitempty"`
	Data          any         `json:"data,omitempty"`
	Error         *BatchError `json:"error,omitempty"`
}

// Batch replays actions queued offline, in order. Each client_event_id is applied at most once:
// a replay returns the stored outcome, so a retry after a lost response never double-enters anyone.
func (s *Service) Batch(ctx context.Context, p authctx.Principal, vs *visitor.Service, req BatchRequest) []BatchResult {
	out := make([]BatchResult, 0, len(req.Events))
	for _, ev := range req.Events {
		out = append(out, s.one(ctx, p, vs, ev))
	}
	return out
}

func (s *Service) one(ctx context.Context, p authctx.Principal, vs *visitor.Service, ev BatchEvent) BatchResult {
	res := BatchResult{ClientEventID: ev.ClientEventID}
	var stored []byte
	var status int
	err := s.pool.QueryRow(ctx, `SELECT http_status, response FROM gate_events WHERE society_id=$1 AND client_event_id=$2`, p.SocietyID, ev.ClientEventID).Scan(&status, &stored)
	if err == nil {
		res.Status, res.Replayed = status, true
		var wrap struct {
			Data  json.RawMessage `json:"data"`
			Error *BatchError     `json:"error"`
		}
		_ = json.Unmarshal(stored, &wrap)
		if len(wrap.Data) > 0 && string(wrap.Data) != "null" {
			res.Data = wrap.Data
		}
		res.Error = wrap.Error
		return res
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		res.Status, res.Error = http.StatusInternalServerError, &BatchError{"INTERNAL", "internal server error"}
		return res
	}

	var data any
	var runErr error
	vid, perr := uuid.Parse(ev.VisitID)
	switch ev.Type {
	case "check_in", "check_out":
		if perr != nil {
			runErr = httpx.NewError(http.StatusBadRequest, "VALIDATION", "visit_id is required")
			break
		}
		var v *visitor.Visit
		if ev.Type == "check_in" {
			v, runErr = vs.CheckIn(ctx, p, vid)
		} else {
			v, runErr = vs.CheckOut(ctx, p, vid)
		}
		if runErr == nil {
			data = v.ForGate()
		}
	case "walk_in":
		if ev.WalkIn == nil {
			runErr = httpx.NewError(http.StatusBadRequest, "VALIDATION", "walk_in is required")
			break
		}
		w := ev.WalkIn
		var v *visitor.Visit
		v, runErr = vs.WalkIn(ctx, p, visitor.WalkInInput{UnitID: uuid.MustParse(w.UnitID), VisitorName: w.VisitorName, VisitorPhone: w.VisitorPhone,
			Purpose: w.Purpose, Company: w.Company, VehicleNumber: w.VehicleNumber, VisitorCount: w.VisitorCount})
		if runErr == nil {
			data = v.ForGate()
		}
	}

	wrap := map[string]any{}
	if runErr != nil {
		var ae *httpx.AppError
		if errors.As(runErr, &ae) {
			res.Status, res.Error = ae.Status, &BatchError{ae.Code, ae.Message}
		} else {
			res.Status, res.Error = http.StatusInternalServerError, &BatchError{"INTERNAL", "internal server error"}
			return res // unexpected failures are retried, not remembered
		}
		wrap["error"] = res.Error
	} else {
		res.Status, res.Data = http.StatusOK, data
		wrap["data"] = data
	}
	raw, _ := json.Marshal(wrap)
	_, _ = s.pool.Exec(ctx, `INSERT INTO gate_events (society_id, client_event_id, kind, http_status, response) VALUES ($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`,
		p.SocietyID, ev.ClientEventID, ev.Type, res.Status, raw)
	return res
}
