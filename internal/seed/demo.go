package seed

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Demo adds a realistic society layout and people for local development:
// blocks A–C (3 floors × 4 flats), ~10 residents, a family member and vehicles.
// Idempotent; run after Dev.
func Demo(ctx context.Context, pool *pgxpool.Pool) error {
	for i, code := range []string{"A", "B", "C"} {
		if _, err := pool.Exec(ctx, `INSERT INTO blocks (society_id, name, code, sort_order) VALUES ($1,$2,$3,$4) ON CONFLICT (society_id, code) DO NOTHING`,
			SocietyID, "Block "+code, code, i); err != nil {
			return err
		}
		for floor := 1; floor <= 3; floor++ {
			for n := 1; n <= 4; n++ {
				typ := "2BHK"
				if n > 2 {
					typ = "3BHK"
				}
				if _, err := pool.Exec(ctx, `
					INSERT INTO units (society_id, block_id, floor, number, unit_type)
					SELECT $1, b.id, $3, $4, $5 FROM blocks b WHERE b.society_id=$1 AND b.code=$2
					ON CONFLICT (society_id, block_id, number) DO NOTHING`,
					SocietyID, code, floor, fmt.Sprintf("%s%d%02d", code, floor, n), typ); err != nil {
					return err
				}
			}
		}
	}

	// Rajesh (seeded in Dev) owns A101.
	if _, err := pool.Exec(ctx, `
		UPDATE memberships SET unit_id=(SELECT id FROM units WHERE society_id=$1 AND number='A101'), move_in_date='2021-03-12'
		WHERE id=$2 AND unit_id IS NULL`, SocietyID, ResidentMem); err != nil {
		return err
	}

	people := []struct{ name, phone, email, unit, role string }{
		{"Prabhu Nayak", "9000000001", "prabhu.nayak@example.com", "A102", "resident_owner"},
		{"Divya Rao", "9000000002", "divya.rao@example.com", "A102", "resident_tenant"},
		{"Sandeep Kamath", "9000000003", "sandeep.kamath@example.com", "A103", "resident_tenant"},
		{"Meghana Jain", "9000000004", "", "A104", "resident_owner"},
		{"Arif Husain", "9000000005", "", "B101", "resident_owner"},
		{"Girish Hegde", "9000000006", "girish.hegde@example.com", "B102", "resident_owner"},
		{"Rekha Bhat", "9000000007", "", "B201", "resident_owner"},
		{"Mohd Dar", "9000000008", "", "B202", "resident_tenant"},
		{"Sudhir Rao", "9000000009", "", "C101", "resident_owner"},
	}
	for _, p := range people {
		var email any
		if p.email != "" {
			email = p.email
		}
		if _, err := pool.Exec(ctx, `INSERT INTO users (full_name, phone, email) VALUES ($1,$2,$3) ON CONFLICT (phone) DO NOTHING`, p.name, p.phone, email); err != nil {
			return err
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO memberships (user_id, society_id, unit_id, role_key, move_in_date)
			SELECT u.id, $2, un.id, $4, '2023-06-01'
			FROM users u, units un
			WHERE u.phone=$1 AND un.society_id=$2 AND un.number=$3
			  AND NOT EXISTS (SELECT 1 FROM memberships m WHERE m.user_id=u.id AND m.society_id=$2)`,
			p.phone, SocietyID, p.unit, p.role); err != nil {
			return err
		}
	}

	// a family member (no login) and two vehicles for Rajesh
	if _, err := pool.Exec(ctx, `
		INSERT INTO family_members (society_id, unit_id, head_membership_id, name, relation)
		SELECT $1, m.unit_id, m.id, 'Sunita Kumar', 'Spouse' FROM memberships m
		WHERE m.id=$2 AND m.unit_id IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM family_members f WHERE f.head_membership_id=m.id)`, SocietyID, ResidentMem); err != nil {
		return err
	}
	for _, v := range []struct{ typ, num, model, color string }{
		{"car", "MH02AB1234", "Hyundai Creta", "White"},
		{"bike", "MH02XY9876", "Honda Activa", "Black"},
	} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO vehicles (society_id, unit_id, owner_user_id, vehicle_type, number, make_model, color)
			SELECT $1, m.unit_id, m.user_id, $3, $4, $5, $6 FROM memberships m WHERE m.id=$2 AND m.unit_id IS NOT NULL
			ON CONFLICT (society_id, number) DO NOTHING`, SocietyID, ResidentMem, v.typ, v.num, v.model, v.color); err != nil {
			return err
		}
	}
	return nil
}
