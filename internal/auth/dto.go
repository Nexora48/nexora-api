package auth

import "github.com/Nexora48/nexora-api/internal/user"

type DeviceInfo struct {
	DeviceID   string `json:"device_id" validate:"required,max=100"`
	Platform   string `json:"platform" validate:"max=20"`
	AppVersion string `json:"app_version" validate:"max=30"`
	FCMToken   string `json:"fcm_token" validate:"max=500"`
}

type OTPRequest struct {
	Phone string `json:"phone" validate:"required"`
}

type OTPVerifyRequest struct {
	Phone  string      `json:"phone" validate:"required"`
	OTP    string      `json:"otp" validate:"required,len=6,numeric"`
	Device *DeviceInfo `json:"device"`
}

type PasswordLoginRequest struct {
	Email    string      `json:"email" validate:"required,email,max=200"`
	Password string      `json:"password" validate:"required,max=200"`
	Device   *DeviceInfo `json:"device"`
}

type SelectSocietyRequest struct {
	SelectionToken string      `json:"selection_token" validate:"required"`
	MembershipID   string      `json:"membership_id" validate:"required,uuid"`
	Device         *DeviceInfo `json:"device"`
}

type ChangePasswordRequest struct {
	CurrentPassword string `json:"current_password" validate:"required,max=200"`
	NewPassword     string `json:"new_password" validate:"required,min=8,max=200"`
}

type RefreshRequest struct {
	RefreshToken string `json:"refresh_token" validate:"required"`
}

type LogoutRequest struct {
	RefreshToken string `json:"refresh_token" validate:"required"`
}

type MembershipDTO struct {
	ID          string  `json:"id"`
	SocietyID   string  `json:"society_id"`
	SocietyName string  `json:"society_name"`
	UnitID      *string `json:"unit_id,omitempty"`
	Role        string  `json:"role"`
}

// LoginResponse is either a full session or a society-selection prompt.
type LoginResponse struct {
	AccessToken        string          `json:"access_token,omitempty"`
	RefreshToken       string          `json:"refresh_token,omitempty"`
	TokenType          string          `json:"token_type,omitempty"`
	ExpiresIn          int             `json:"expires_in,omitempty"`
	Membership         *MembershipDTO  `json:"membership,omitempty"`
	MustChangePassword bool            `json:"must_change_password"`
	SelectionRequired  bool            `json:"selection_required"`
	SelectionToken     string          `json:"selection_token,omitempty"`
	Memberships        []MembershipDTO `json:"memberships,omitempty"`
}

func toMembershipDTO(m user.Membership) MembershipDTO {
	d := MembershipDTO{ID: m.ID.String(), SocietyID: m.SocietyID.String(), SocietyName: m.SocietyName, Role: m.RoleKey}
	if m.UnitID != nil {
		s := m.UnitID.String()
		d.UnitID = &s
	}
	return d
}
