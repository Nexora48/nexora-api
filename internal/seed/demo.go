package seed

import (
	"context"
	"fmt"
	"time"

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
	if err := demoGuard(ctx, pool); err != nil {
		return err
	}
	return demoVisits(ctx, pool)
}

// demoVisits adds a spread of visitors in every status (only when the society has none yet).
func demoVisits(ctx context.Context, pool *pgxpool.Pool) error {
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM visits WHERE society_id=$1`, SocietyID).Scan(&n); err != nil || n > 0 {
		return err
	}
	now := time.Now()
	h := func(d time.Duration) time.Time { return now.Add(d) }
	type v struct {
		unit, name, purpose, company, vehicle, status, source, code, reason string
		host                                                                bool
		from, until                                                         time.Time
		entered, exited, created                                            *time.Time
	}
	t := func(d time.Duration) *time.Time { x := now.Add(d); return &x }
	rows := []v{
		{"A101", "Priya Sharma", "guest", "", "MH12AB1234", "inside", "pre_approved", "", "", true, h(-time.Hour), h(3 * time.Hour), t(-35 * time.Minute), nil, t(-90 * time.Minute)},
		{"A101", "Zomato Delivery", "delivery", "Zomato", "", "waiting", "walk_in", "", "", false, h(0), h(2 * time.Hour), nil, nil, t(-time.Minute)},
		{"A101", "Sneha N", "guest", "", "", "invited", "pre_approved", "482913", "", true, h(30 * time.Minute), h(270 * time.Minute), nil, nil, t(-10 * time.Minute)},
		{"A101", "Ramesh Electrician", "service", "", "", "completed", "pre_approved", "", "", true, h(-27 * time.Hour), h(-23 * time.Hour), t(-26 * time.Hour), t(-25 * time.Hour), t(-28 * time.Hour)},
		{"A101", "Amazon Courier", "delivery", "Amazon", "", "rejected", "walk_in", "", "Not expecting a parcel", false, h(-5 * time.Hour), h(-3 * time.Hour), nil, nil, t(-5 * time.Hour)},
		{"A101", "Uber cab", "cab", "Uber", "MH02CD5678", "cancelled", "pre_approved", "", "", true, h(-8 * time.Hour), h(-4 * time.Hour), nil, nil, t(-9 * time.Hour)},
		{"A101", "Old friend", "guest", "", "", "invited", "pre_approved", "111222", "", true, h(-30 * time.Hour), h(-26 * time.Hour), nil, nil, t(-31 * time.Hour)},
		{"A104", "Manoj Kini", "guest", "", "", "inside", "walk_in", "", "", false, h(-2 * time.Hour), h(0), t(-100 * time.Minute), nil, t(-2 * time.Hour)},
		{"B101", "Anil - Swiggy", "delivery", "Swiggy", "", "completed", "walk_in", "", "", false, h(-4 * time.Hour), h(-2 * time.Hour), t(-230 * time.Minute), t(-220 * time.Minute), t(-4 * time.Hour)},
		{"B102", "Jaya Cleaners", "service", "", "", "completed", "walk_in", "", "", false, h(-6 * time.Hour), h(-4 * time.Hour), t(-350 * time.Minute), t(-300 * time.Minute), t(-6 * time.Hour)},
	}
	for _, r := range rows {
		var host any
		if r.host {
			host = ResidentMem
		}
		var code, company, vehicle, reason any
		for dst, val := range map[*any]string{&code: r.code, &company: r.company, &vehicle: r.vehicle, &reason: r.reason} {
			if val != "" {
				*dst = val
			}
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO visits (society_id, unit_id, host_membership_id, visitor_name, purpose, company, vehicle_number, source, status, pass_code,
			                    expected_from, expected_until, entered_at, exited_at, decision_reason, created_at)
			SELECT $1, un.id, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16 FROM units un WHERE un.society_id=$1 AND un.number=$2`,
			SocietyID, r.unit, host, r.name, r.purpose, company, vehicle, r.source, r.status, code, r.from, r.until, r.entered, r.exited, reason, r.created); err != nil {
			return err
		}
	}
	return nil
}

// GuardPhone is the demo guard (dev OTP 123456) with Main Gate and an open shift.
const GuardPhone = "7777777777"

func demoGuard(ctx context.Context, pool *pgxpool.Pool) error {
	for _, q := range []string{
		`INSERT INTO gates (society_id, name) VALUES ($1,'Main Gate') ON CONFLICT DO NOTHING`,
		`INSERT INTO gates (society_id, name) VALUES ($1,'Back Gate') ON CONFLICT DO NOTHING`,
		`INSERT INTO users (full_name, phone) VALUES ('Ramesh Singh','` + GuardPhone + `') ON CONFLICT (phone) DO NOTHING`,
		`INSERT INTO memberships (user_id, society_id, role_key)
		   SELECT u.id, $1, 'guard' FROM users u WHERE u.phone='` + GuardPhone + `'
		   AND NOT EXISTS (SELECT 1 FROM memberships m WHERE m.user_id=u.id AND m.society_id=$1)`,
	} {
		if _, err := pool.Exec(ctx, q, SocietyID); err != nil {
			return err
		}
	}
	return nil
}
