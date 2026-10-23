package resident

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Nexora48/nexora-api/internal/audit"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
)

func revokeTokens(ctx context.Context, tx pgx.Tx, membershipID uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at=now() WHERE membership_id=$1 AND revoked_at IS NULL`, membershipID)
	return err
}

// transition moves a resident between statuses. Suspend/move-out revoke sessions.
func (s *Service) transition(ctx context.Context, societyID, actorID, membershipID uuid.UUID, action string) error {
	var from []string
	var to string
	switch action {
	case "suspend":
		from, to = []string{"active"}, "suspended"
	case "restore":
		from, to = []string{"suspended"}, "active"
	case "move-out":
		from, to = []string{"active", "suspended"}, "inactive"
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var status string
	err = tx.QueryRow(ctx, `SELECT status FROM memberships WHERE id=$1 AND society_id=$2 AND role_key IN `+residentRoles+` FOR UPDATE`,
		membershipID, societyID).Scan(&status)
	if err == pgx.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	okFrom := false
	for _, f := range from {
		okFrom = okFrom || f == status
	}
	if !okFrom {
		return ErrInvalidState
	}
	ended := "NULL"
	if to == "inactive" {
		ended = "now()"
	}
	if _, err := tx.Exec(ctx, `UPDATE memberships SET status=$2, ended_at=`+ended+` WHERE id=$1`, membershipID, to); err != nil {
		return err
	}
	if to != "active" {
		if err := revokeTokens(ctx, tx, membershipID); err != nil {
			return err
		}
	}
	if action == "move-out" {
		// the household of a leaving owner/tenant loses access too
		rows, err := tx.Query(ctx, `SELECT login_membership_id FROM family_members WHERE head_membership_id=$1 AND login_membership_id IS NOT NULL`, membershipID)
		if err != nil {
			return err
		}
		var logins []uuid.UUID
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			logins = append(logins, id)
		}
		rows.Close()
		for _, id := range logins {
			if _, err := tx.Exec(ctx, `UPDATE memberships SET status='inactive', ended_at=now() WHERE id=$1`, id); err != nil {
				return err
			}
			if err := revokeTokens(ctx, tx, id); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.audit.Log(ctx, audit.Entry{ActorID: &actorID, SocietyID: &societyID, Action: "resident." + strings.ReplaceAll(action, "-", "_"),
		Entity: "membership", EntityID: membershipID.String()})
	return nil
}

func (s *Service) Suspend(ctx context.Context, soc, actor, id uuid.UUID) error {
	return s.transition(ctx, soc, actor, id, "suspend")
}
func (s *Service) Restore(ctx context.Context, soc, actor, id uuid.UUID) error {
	return s.transition(ctx, soc, actor, id, "restore")
}
func (s *Service) MoveOut(ctx context.Context, soc, actor, id uuid.UUID) error {
	return s.transition(ctx, soc, actor, id, "move-out")
}

// ── CSV import ───────────────────────────────────────────────────────────────

const (
	maxImportRows  = 1000
	ImportTemplate = "full_name,phone,email,unit,role,move_in_date\nAsha Verma,9876543210,asha@example.com,A101,owner,2024-04-01\nRavi Iyer,9123456780,,A102,tenant,\n"
)

type ImportError struct {
	Row     int    `json:"row"`
	Message string `json:"message"`
}

type ImportResult struct {
	Total   int           `json:"total"`
	Valid   int           `json:"valid"`
	Created int           `json:"created"`
	DryRun  bool          `json:"dry_run"`
	Errors  []ImportError `json:"errors"`
}

type importRow struct {
	line int
	in   CreateInput
}

// Import validates every CSV row; with dryRun nothing is written, otherwise valid rows are created.
func (s *Service) Import(ctx context.Context, societyID, actorID uuid.UUID, r io.Reader, dryRun bool) (*ImportResult, error) {
	cr := csv.NewReader(r)
	cr.TrimLeadingSpace = true
	records, err := cr.ReadAll()
	if err != nil {
		return nil, httpx.NewError(http.StatusBadRequest, "IMPORT_INVALID", "could not read CSV: "+err.Error())
	}
	if len(records) == 0 {
		return nil, httpx.NewError(http.StatusBadRequest, "IMPORT_INVALID", "the file is empty")
	}
	col := map[string]int{}
	for i, h := range records[0] {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}
	for _, need := range []string{"full_name", "phone", "unit", "role"} {
		if _, ok := col[need]; !ok {
			return nil, httpx.NewError(http.StatusBadRequest, "IMPORT_INVALID", "missing column: "+need)
		}
	}
	data := records[1:]
	if len(data) > maxImportRows {
		return nil, httpx.NewError(http.StatusBadRequest, "IMPORT_INVALID", fmt.Sprintf("too many rows (max %d)", maxImportRows))
	}
	get := func(rec []string, name string) string {
		if i, ok := col[name]; ok && i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}

	// preload society state once
	units := map[string]uuid.UUID{}
	ur, err := s.pool.Query(ctx, `SELECT upper(number), id FROM units WHERE society_id=$1`, societyID)
	if err != nil {
		return nil, err
	}
	for ur.Next() {
		var n string
		var id uuid.UUID
		if err := ur.Scan(&n, &id); err != nil {
			ur.Close()
			return nil, err
		}
		units[n] = id
	}
	ur.Close()
	taken := map[string]bool{} // unitID|role
	tr, err := s.pool.Query(ctx, `SELECT unit_id, role_key FROM memberships WHERE society_id=$1 AND status IN ('active','suspended') AND unit_id IS NOT NULL AND role_key IN ('resident_owner','resident_tenant')`, societyID)
	if err != nil {
		return nil, err
	}
	for tr.Next() {
		var id uuid.UUID
		var role string
		if err := tr.Scan(&id, &role); err != nil {
			tr.Close()
			return nil, err
		}
		taken[id.String()+"|"+role] = true
	}
	tr.Close()
	phones := map[string]bool{}
	pr, err := s.pool.Query(ctx, `SELECT u.phone FROM memberships m JOIN users u ON u.id=m.user_id WHERE m.society_id=$1 AND m.status IN ('active','suspended') AND m.role_key IN `+residentRoles, societyID)
	if err != nil {
		return nil, err
	}
	for pr.Next() {
		var p string
		if err := pr.Scan(&p); err != nil {
			pr.Close()
			return nil, err
		}
		phones[p] = true
	}
	pr.Close()

	res := &ImportResult{Total: len(data), DryRun: dryRun, Errors: []ImportError{}}
	var valid []importRow
	for i, rec := range data {
		line := i + 2 // header is line 1
		bad := func(msg string) { res.Errors = append(res.Errors, ImportError{Row: line, Message: msg}) }
		name := get(rec, "full_name")
		phone := NormalizePhone(get(rec, "phone"))
		unitNo := strings.ToUpper(get(rec, "unit"))
		roleStr := strings.ToLower(get(rec, "role"))
		moveIn := get(rec, "move_in_date")
		email := get(rec, "email")
		switch {
		case name == "":
			bad("full_name is required")
			continue
		case !ValidPhone(phone):
			bad("invalid phone number")
			continue
		case roleStr != "owner" && roleStr != "tenant":
			bad("role must be owner or tenant")
			continue
		}
		unitID, ok := units[unitNo]
		if !ok {
			bad("unknown unit " + unitNo)
			continue
		}
		if moveIn != "" && !validDate(&moveIn) {
			bad("move_in_date must be YYYY-MM-DD")
			continue
		}
		if phones[phone] {
			bad("phone is already a resident (or repeated in the file)")
			continue
		}
		key := unitID.String() + "|" + typeToRole[roleStr]
		if taken[key] {
			bad("flat " + unitNo + " already has an active " + roleStr)
			continue
		}
		phones[phone], taken[key] = true, true
		in := CreateInput{FullName: name, Phone: phone, UnitID: unitID, Type: roleStr}
		if email != "" {
			in.Email = &email
		}
		if moveIn != "" {
			in.MoveInDate = &moveIn
		}
		valid = append(valid, importRow{line, in})
	}
	res.Valid = len(valid)
	if dryRun {
		return res, nil
	}
	for _, v := range valid {
		if _, err := s.Create(ctx, societyID, actorID, v.in); err != nil {
			var ae *httpx.AppError
			msg := err.Error()
			if asApp(err, &ae) {
				msg = ae.Message
			}
			res.Errors = append(res.Errors, ImportError{Row: v.line, Message: msg})
			continue
		}
		res.Created++
	}
	s.audit.Log(ctx, audit.Entry{ActorID: &actorID, SocietyID: &societyID, Action: "resident.import",
		Meta: map[string]any{"total": res.Total, "created": res.Created, "errors": len(res.Errors)}})
	return res, nil
}

func asApp(err error, target **httpx.AppError) bool { return errors.As(err, target) }
