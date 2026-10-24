// Package visitor implements visitor pre-approval passes, the gate flow (verify, walk-in,
// check-in/out) and the admin visitor log.
package visitor

import (
	"time"

	"github.com/google/uuid"
)

const (
	StatusInvited   = "invited"
	StatusWaiting   = "waiting"
	StatusApproved  = "approved"
	StatusInside    = "inside"
	StatusCompleted = "completed"
	StatusRejected  = "rejected"
	StatusCancelled = "cancelled"
	StatusExpired   = "expired"
)

// Visit is the full record (resident and admin views). Status is the *effective* status:
// stale invites/requests read as expired even before the sweeper persists it.
type Visit struct {
	ID             uuid.UUID  `json:"id"`
	UnitID         uuid.UUID  `json:"unit_id"`
	UnitNumber     string     `json:"unit_number"`
	BlockCode      string     `json:"block_code"`
	HostName       *string    `json:"host_name,omitempty"`
	VisitorName    string     `json:"visitor_name"`
	VisitorPhone   *string    `json:"visitor_phone,omitempty"`
	Purpose        string     `json:"purpose"`
	Company        *string    `json:"company,omitempty"`
	VehicleNumber  *string    `json:"vehicle_number,omitempty"`
	VisitorCount   int        `json:"visitor_count"`
	Note           *string    `json:"note,omitempty"`
	Source         string     `json:"source"`
	Status         string     `json:"status"`
	PassCode       *string    `json:"pass_code,omitempty"` // only while invited
	QR             *string    `json:"qr,omitempty"`        // only while invited
	ExpectedFrom   time.Time  `json:"expected_from"`
	ExpectedUntil  time.Time  `json:"expected_until"`
	EnteredAt      *time.Time `json:"entered_at,omitempty"`
	ExitedAt       *time.Time `json:"exited_at,omitempty"`
	DecidedAt      *time.Time `json:"decided_at,omitempty"`
	DecisionReason *string    `json:"decision_reason,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

// GateVisit is what a guard may see: no phone number, no private note.
type GateVisit struct {
	ID            uuid.UUID  `json:"id"`
	VisitorName   string     `json:"visitor_name"`
	Purpose       string     `json:"purpose"`
	Company       *string    `json:"company,omitempty"`
	VehicleNumber *string    `json:"vehicle_number,omitempty"`
	VisitorCount  int        `json:"visitor_count"`
	UnitID        uuid.UUID  `json:"unit_id"`
	UnitNumber    string     `json:"unit_number"`
	BlockCode     string     `json:"block_code"`
	HostName      *string    `json:"host_name,omitempty"`
	Source        string     `json:"source"`
	Status        string     `json:"status"`
	ExpectedFrom  time.Time  `json:"expected_from"`
	ExpectedUntil time.Time  `json:"expected_until"`
	EnteredAt     *time.Time `json:"entered_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

func (v Visit) ForGate() GateVisit {
	return GateVisit{ID: v.ID, VisitorName: v.VisitorName, Purpose: v.Purpose, Company: v.Company, VehicleNumber: v.VehicleNumber,
		VisitorCount: v.VisitorCount, UnitID: v.UnitID, UnitNumber: v.UnitNumber, BlockCode: v.BlockCode, HostName: v.HostName,
		Source: v.Source, Status: v.Status, ExpectedFrom: v.ExpectedFrom, ExpectedUntil: v.ExpectedUntil, EnteredAt: v.EnteredAt, CreatedAt: v.CreatedAt}
}

type VerifyResult struct {
	Valid  bool       `json:"valid"`
	Reason string     `json:"reason,omitempty"` // NOT_FOUND, EXPIRED, NOT_YET_VALID, ALREADY_USED, CANCELLED, REJECTED, NOT_APPROVED
	Visit  *GateVisit `json:"visit,omitempty"`
}

type Summary struct {
	Active  int `json:"active"`
	Invited int `json:"invited"`
	Today   int `json:"today"`
}

type Stats struct {
	Today     int `json:"today"`
	InsideNow int `json:"inside_now"`
	Awaiting  int `json:"awaiting"`
}

type GateUnit struct {
	ID        uuid.UUID `json:"id"`
	Number    string    `json:"number"`
	BlockCode string    `json:"block_code"`
	Occupied  bool      `json:"occupied"`
}

type CreateInput struct {
	VisitorName   string
	VisitorPhone  *string
	Purpose       string
	Company       *string
	VehicleNumber *string
	VisitorCount  int
	Note          *string
	ExpectedFrom  *time.Time
	ExpectedUntil *time.Time
}

type WalkInInput struct {
	UnitID        uuid.UUID
	VisitorName   string
	VisitorPhone  *string
	Purpose       string
	Company       *string
	VehicleNumber *string
	VisitorCount  int
}

type ListFilter struct {
	Tab     string // resident: active | history | invited
	Status  string // admin: effective status
	Purpose string
	Query   string
	UnitID  *uuid.UUID
	From    *time.Time
	To      *time.Time
	Limit   int
	Offset  int
}
