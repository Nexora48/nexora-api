package user

import (
	"time"

	"github.com/google/uuid"
)

type User struct {
	ID           uuid.UUID
	FullName     string
	Phone        string
	Email        *string
	PasswordHash *string
	Status       string
}

// Membership links a user to a society (and optionally a unit) with a role.
type Membership struct {
	ID            uuid.UUID
	UserID        uuid.UUID
	SocietyID     uuid.UUID
	UnitID        *uuid.UUID
	RoleKey       string
	Status        string
	SocietyName   string
	SocietyStatus string
	AppAccess     bool
	CreatedAt     time.Time
}
