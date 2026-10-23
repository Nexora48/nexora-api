package resident

import (
	"time"

	"github.com/google/uuid"
)

// Roles managed through the resident APIs.
const (
	RoleOwner  = "resident_owner"
	RoleTenant = "resident_tenant"
	RoleFamily = "family_member"
)

// typeToRole maps the API/UI "type" to a role key.
var typeToRole = map[string]string{"owner": RoleOwner, "tenant": RoleTenant, "family": RoleFamily}

func roleToType(role string) string {
	switch role {
	case RoleOwner:
		return "owner"
	case RoleTenant:
		return "tenant"
	}
	return "family"
}

type Resident struct {
	MembershipID uuid.UUID  `json:"membership_id"`
	UserID       uuid.UUID  `json:"user_id"`
	FullName     string     `json:"full_name"`
	Phone        string     `json:"phone"`
	Email        *string    `json:"email,omitempty"`
	UnitID       *uuid.UUID `json:"unit_id,omitempty"`
	UnitNumber   *string    `json:"unit_number,omitempty"`
	BlockCode    *string    `json:"block_code,omitempty"`
	Type         string     `json:"type"`   // owner | tenant | family
	Status       string     `json:"status"` // active | pending | suspended | inactive
	MoveInDate   *string    `json:"move_in_date,omitempty"`
	JoinedAt     time.Time  `json:"joined_at"`
}

type EmergencyContact struct {
	Name     string `json:"name"`
	Phone    string `json:"phone"`
	Relation string `json:"relation"`
}

type FamilyMember struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Relation    string    `json:"relation"`
	Phone       *string   `json:"phone,omitempty"`
	DateOfBirth *string   `json:"date_of_birth,omitempty"`
	HasLogin    bool      `json:"has_login"`
}

type Vehicle struct {
	ID          uuid.UUID `json:"id"`
	VehicleType string    `json:"vehicle_type"`
	Number      string    `json:"number"`
	MakeModel   string    `json:"make_model"`
	Color       string    `json:"color"`
	StickerNo   *string   `json:"sticker_no,omitempty"`
}

type Detail struct {
	Resident
	AgreementEndDate  *string           `json:"agreement_end_date,omitempty"`
	DateOfBirth       *string           `json:"date_of_birth,omitempty"`
	GovIDType         *string           `json:"gov_id_type,omitempty"`
	GovIDLast4        *string           `json:"gov_id_last4,omitempty"`
	AppAccess         bool              `json:"app_access"`
	CanInviteVisitors bool              `json:"can_invite_visitors"`
	CanPayMaintenance bool              `json:"can_pay_maintenance"`
	SOSEnabled        bool              `json:"sos_enabled"`
	UserShared        bool              `json:"user_shared"` // name/email read-only when true
	LastLoginAt       *time.Time        `json:"last_login_at,omitempty"`
	EmergencyContact  *EmergencyContact `json:"emergency_contact,omitempty"`
	Family            []FamilyMember    `json:"family"`
	Vehicles          []Vehicle         `json:"vehicles"`
}

type CreateInput struct {
	FullName          string
	Phone             string
	Email             *string
	UnitID            uuid.UUID
	Type              string
	Relation          string
	MoveInDate        *string
	AgreementEndDate  *string
	DateOfBirth       *string
	GovIDType         *string
	GovIDNumber       *string
	Emergency         *EmergencyContact
	AppAccess         *bool
	CanInviteVisitors *bool
	CanPayMaintenance *bool
	SOSEnabled        *bool
}

type UpdateInput struct {
	FullName          *string
	Email             *string
	DateOfBirth       *string
	MoveInDate        *string
	AgreementEndDate  *string
	Emergency         *EmergencyContact
	AppAccess         *bool
	CanInviteVisitors *bool
	CanPayMaintenance *bool
	SOSEnabled        *bool
}

type ListFilter struct {
	Type   string
	Status string
	UnitID *uuid.UUID
	Query  string
	Limit  int
	Offset int
}
