package visitor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Nexora48/nexora-api/internal/household"
	"github.com/Nexora48/nexora-api/internal/platform/authctx"
)

// GateDashboard feeds the guard's home screen.
type GateDashboard struct {
	Inside   int `json:"inside"`
	Waiting  int `json:"waiting"`
	Expected int `json:"expected"` // open passes whose window starts within 2 h or already runs
	Today    int `json:"today"`
}

func (s *Service) Dashboard(ctx context.Context, p authctx.Principal) (*GateDashboard, error) {
	e := s.eff()
	var d GateDashboard
	err := s.pool.QueryRow(ctx, fmt.Sprintf(`
		SELECT count(*) FILTER (WHERE v.status='inside'),
		       count(*) FILTER (WHERE %[1]s='waiting'),
		       count(*) FILTER (WHERE %[1]s='invited' AND v.expected_from <= now() + interval '2 hours'),
		       count(*) FILTER (WHERE (v.created_at >= dd.start OR v.entered_at >= dd.start) AND v.status <> 'cancelled')
		FROM visits v
		CROSS JOIN (SELECT date_trunc('day', now() AT TIME ZONE so.timezone) AT TIME ZONE so.timezone AS start FROM societies so WHERE so.id=$1) dd
		WHERE v.society_id=$1`, e), p.SocietyID).Scan(&d.Inside, &d.Waiting, &d.Expected, &d.Today)
	return &d, err
}

func (s *Service) Inside(ctx context.Context, p authctx.Principal) ([]GateVisit, error) {
	rows, _, err := s.many(ctx, []string{"v.society_id=$1", s.eff() + "='inside'"}, []any{p.SocietyID}, "v.entered_at DESC NULLS LAST, v.id", 200, 0)
	if err != nil {
		return nil, err
	}
	return toGate(rows), nil
}

// Vehicles finds visits by vehicle number over the last 7 days.
func (s *Service) Vehicles(ctx context.Context, p authctx.Principal, q string) ([]GateVisit, error) {
	q = household.NormalizeVehicle(q)
	if q == "" {
		return []GateVisit{}, nil
	}
	rows, _, err := s.many(ctx, []string{"v.society_id=$1", "v.vehicle_number LIKE $2", "v.created_at > now() - interval '7 days'", "v.status <> 'cancelled'"},
		[]any{p.SocietyID, "%" + q + "%"}, "v.created_at DESC, v.id", 50, 0)
	if err != nil {
		return nil, err
	}
	return toGate(rows), nil
}

func toGate(rows []Visit) []GateVisit {
	out := make([]GateVisit, len(rows))
	for i, v := range rows {
		out[i] = v.ForGate()
	}
	return out
}

// SyncPass is one open pass in the offline cache. The code is sent hashed so the device never
// stores plaintext codes; the app hashes what the guard types and looks it up.
type SyncPass struct {
	ID            uuid.UUID `json:"id"`
	CodeHash      string    `json:"code_hash"`
	VisitorName   string    `json:"visitor_name"`
	Purpose       string    `json:"purpose"`
	VehicleNumber *string   `json:"vehicle_number,omitempty"`
	VisitorCount  int       `json:"visitor_count"`
	UnitNumber    string    `json:"unit_number"`
	BlockCode     string    `json:"block_code"`
	ExpectedFrom  time.Time `json:"expected_from"`
	ExpectedUntil time.Time `json:"expected_until"`
}

// CodeHash is the offline lookup key: sha256("<society id>:<6-digit code>").
func CodeHash(societyID uuid.UUID, code string) string {
	h := sha256.Sum256([]byte(societyID.String() + ":" + strings.TrimSpace(code)))
	return hex.EncodeToString(h[:])
}

func (s *Service) PassesSync(ctx context.Context, p authctx.Principal) ([]SyncPass, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT v.id, v.pass_code, v.visitor_name, v.purpose, v.vehicle_number, v.visitor_count, un.number, b.code, v.expected_from, v.expected_until
		FROM visits v JOIN units un ON un.id=v.unit_id JOIN blocks b ON b.id=un.block_id
		WHERE v.society_id=$1 AND v.status='invited' AND v.expected_until > now() AND v.expected_from < now() + interval '24 hours'
		ORDER BY v.expected_from LIMIT 2000`, p.SocietyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SyncPass{}
	for rows.Next() {
		var sp SyncPass
		var code string
		if err := rows.Scan(&sp.ID, &code, &sp.VisitorName, &sp.Purpose, &sp.VehicleNumber, &sp.VisitorCount, &sp.UnitNumber, &sp.BlockCode, &sp.ExpectedFrom, &sp.ExpectedUntil); err != nil {
			return nil, err
		}
		sp.CodeHash = CodeHash(p.SocietyID, code)
		out = append(out, sp)
	}
	return out, rows.Err()
}

// GateVisitByID is the guard-safe read of one visit.
func (s *Service) GateVisitByID(ctx context.Context, p authctx.Principal, id uuid.UUID) (*GateVisit, error) {
	v, err := s.one(ctx, p.SocietyID, id, nil)
	if err != nil {
		return nil, err
	}
	g := v.ForGate()
	return &g, nil
}
