package seed

import (
	"context"
	"fmt"
	"strings"
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
	if err := demoM6(ctx, pool); err != nil {
		return err
	}
	if err := demoM7(ctx, pool); err != nil {
		return err
	}
	if err := demoM8(ctx, pool); err != nil {
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
		var args []any
		if strings.Contains(q, "$1") {
			args = []any{SocietyID}
		}
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			return fmt.Errorf("demo guard: %w", err)
		}
	}
	return nil
}

// demoM6 adds emergency contacts, a delivery rule for A101, a resolved SOS and a few notifications.
func demoM6(ctx context.Context, pool *pgxpool.Pool) error {
	for _, q := range []string{
		`INSERT INTO society_contacts (society_id, label, phone, hours, sort_order)
		   SELECT $1, v.l, v.p, v.h, v.o FROM (VALUES ('Security Control Room','02212345678','24/7',0), ('Society Manager','02287654321','9AM-6PM',1), ('Ambulance (Sunrise Clinic)','02255501234','24/7',2)) AS v(l,p,h,o)
		   WHERE NOT EXISTS (SELECT 1 FROM society_contacts WHERE society_id=$1)`,
		`INSERT INTO delivery_rules (society_id, unit_id, scope, mode, companies, updated_by)
		   SELECT $1, u.id, 'delivery', 'auto_approve', ARRAY['Swiggy','Zomato'], $2 FROM units u WHERE u.society_id=$1 AND u.number='A101'
		   ON CONFLICT (unit_id, scope) DO NOTHING`,
		`INSERT INTO sos_events (society_id, unit_id, raised_by_membership_id, kind, note, status, raised_at, last_alert_at, acknowledged_at, resolved_at, resolution_note)
		   SELECT $1, u.id, $2, 'medical', 'Demo: resolved alert', 'resolved', now() - interval '2 days', now() - interval '2 days', now() - interval '2 days' + interval '1 minute', now() - interval '2 days' + interval '9 minutes', 'Demo outcome: ambulance called'
		   FROM units u WHERE u.society_id=$1 AND u.number='A101' AND NOT EXISTS (SELECT 1 FROM sos_events WHERE society_id=$1)`,
		`INSERT INTO notifications (society_id, user_id, type, title, body, data, priority, read_at, created_at)
		   SELECT $1, $2, v.t, v.ti, v.b, '{}'::jsonb, 'normal', v.r, now() - v.age FROM (VALUES
		     ('visit.arrived','Arrival at the gate','A delivery was let in by your rule.', NULL::timestamptz, interval '3 hours'),
		     ('visit.request','Visitor at the gate','Someone is waiting to enter. Tap to respond.', now(), interval '1 day'),
		     ('system','Welcome to Nexora Living','Your society is set up. Pre-approve visitors and manage deliveries from the app.', now(), interval '5 days')) AS v(t,ti,b,r,age)
		   WHERE NOT EXISTS (SELECT 1 FROM notifications WHERE user_id=$2)`,
	} {
		args := []any{SocietyID}
		switch {
		case strings.Contains(q, "delivery_rules"), strings.Contains(q, "sos_events"):
			args = append(args, ResidentMem)
		case strings.Contains(q, "INTO notifications"):
			args = append(args, ResidentID)
		}
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			return fmt.Errorf("demo m6: %w (%.60s)", err, strings.Join(strings.Fields(q), " "))
		}
	}
	return nil
}

// demoM7 adds notices, complaints in every status with timelines, and an open poll.
func demoM7(ctx context.Context, pool *pgxpool.Pool) error {
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM notices WHERE society_id=$1)`, SocietyID).Scan(&exists); err != nil || exists {
		return err
	}
	var blockB *string
	_ = pool.QueryRow(ctx, `SELECT id::text FROM blocks WHERE society_id=$1 AND code='B'`, SocietyID).Scan(&blockB)

	notices := []struct {
		title, body, category, priority, status string
		pinned, onlyB                           bool
		age                                     string
	}{
		{"Annual Society Meeting 2026", "The AGM is on Sunday at 10 AM in the community hall. Agenda: budget, security contract, lift renovation. Please attend.", "event", "important", "published", true, false, "2 days"},
		{"Water Supply Interruption — Block B", "Water supply to Block B will be off on Sunday 10 AM – 2 PM for tank cleaning. Please store water in advance.", "maintenance", "urgent", "published", false, true, "5 hours"},
		{"New Gate Entry Security Protocol", "From Monday every visitor is verified by OTP or QR at the gate. Pre-approve guests from the app to avoid waiting.", "notice", "important", "published", true, false, "6 days"},
		{"Diwali celebration plan", "Community Diwali evening on 8 November. Volunteers please contact the office.", "event", "normal", "scheduled", false, false, "0"},
		{"Terrace garden proposal (draft)", "Draft proposal for a rooftop garden, to be shared after committee review.", "general", "normal", "draft", false, false, "0"},
	}
	for _, n := range notices {
		var id string
		publishedAt := "NULL"
		publishAt := "NULL"
		switch n.status {
		case "published":
			publishedAt = "now() - interval '" + n.age + "'"
		case "scheduled":
			publishAt = "now() + interval '3 days'"
		}
		q := `INSERT INTO notices (society_id, title, body, category, priority, status, pinned, all_blocks, publish_at, published_at, created_by)
		      VALUES ($1,$2,$3,$4,$5,$6,$7,$8,` + publishAt + `,` + publishedAt + `,$9) RETURNING id::text`
		if err := pool.QueryRow(ctx, q, SocietyID, n.title, n.body, n.category, n.priority, n.status, n.pinned, !n.onlyB, AdminUserID).Scan(&id); err != nil {
			return err
		}
		if n.onlyB && blockB != nil {
			if _, err := pool.Exec(ctx, `INSERT INTO notice_blocks (notice_id, block_id) VALUES ($1,$2)`, id, *blockB); err != nil {
				return err
			}
		}
	}

	// complaints from flat A101 (the demo resident), one per status
	type cmp struct {
		title, desc, category, priority, status string
		age                                     string
		events                                  [][3]string // type, body, who (office|resident)
	}
	list := []cmp{
		{"Water leakage in bathroom", "Water drips from the overhead pipe onto the floor and tiles.", "plumbing", "high", "in_progress", "2 days", [][3]string{{"raised", "", "resident"}, {"assigned", "Assigned to Anita Sharma (vendor: Aqua Plumbers), target in 2 days", "office"}, {"status", "in_progress: Plumber visited, part ordered", "office"}}},
		{"Lift not working in Block A", "The lift has been stuck on the ground floor since morning.", "lift", "urgent", "assigned", "1 day", [][3]string{{"raised", "", "resident"}, {"assigned", "Assigned to Anita Sharma", "office"}}},
		{"Corridor light flickering", "The third-floor corridor light flickers at night.", "electrical", "medium", "open", "3 hours", [][3]string{{"raised", "", "resident"}}},
		{"Garbage not collected", "Garbage was not collected from the floor for two days.", "cleaning", "medium", "resolved", "3 days", [][3]string{{"raised", "", "resident"}, {"status", "resolved: Housekeeping schedule corrected", "office"}}},
		{"Noise from the flat above", "Loud music after 11 PM on weekdays.", "noise", "low", "closed", "12 days", [][3]string{{"raised", "", "resident"}, {"status", "resolved: Spoke to the neighbour", "office"}, {"auto_closed", "Closed automatically after a week without a reply", "office"}}},
	}
	for i, c := range list {
		var id string
		resolved := "NULL"
		if c.status == "resolved" {
			resolved = "now() - interval '1 day'"
		} else if c.status == "closed" {
			resolved = "now() - interval '9 days'"
		}
		q := `INSERT INTO complaints (society_id, seq, unit_id, raised_by_membership_id, title, description, category, priority, status, resolved_at, created_at, updated_at, assigned_membership_id)
		      SELECT $1, $2, u.id, $3, $4, $5, $6, $7, $8, ` + resolved + `, now() - interval '` + c.age + `', now(), CASE WHEN $8 IN ('assigned','in_progress') THEN $9::uuid END
		      FROM units u WHERE u.society_id=$1 AND u.number='A101' RETURNING id::text`
		if err := pool.QueryRow(ctx, q, SocietyID, i+1, ResidentMem, c.title, c.desc, c.category, c.priority, c.status, AdminMemID).Scan(&id); err != nil {
			return err
		}
		for _, e := range c.events {
			actor := ResidentMem
			if e[2] == "office" {
				actor = AdminMemID
			}
			if _, err := pool.Exec(ctx, `INSERT INTO complaint_events (complaint_id, society_id, actor_membership_id, type, body) VALUES ($1,$2,$3,$4,$5)`, id, SocietyID, actor, e[0], e[1]); err != nil {
				return err
			}
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO complaint_counters (society_id, last) VALUES ($1,$2) ON CONFLICT (society_id) DO UPDATE SET last=EXCLUDED.last`, SocietyID, len(list)); err != nil {
		return err
	}

	// an open poll with a few votes
	var pid string
	if err := pool.QueryRow(ctx, `INSERT INTO polls (society_id, question, multi, anonymous, closes_at, created_by) VALUES ($1,'Should we repaint the lobby this year?',false,true, now() + interval '5 days', $2) RETURNING id::text`, SocietyID, AdminUserID).Scan(&pid); err != nil {
		return err
	}
	for i, label := range []string{"Yes, repaint now", "Next year", "No, keep it as it is"} {
		if _, err := pool.Exec(ctx, `INSERT INTO poll_options (poll_id, label, position) VALUES ($1,$2,$3)`, pid, label, i); err != nil {
			return err
		}
	}
	for i, number := range []string{"A102", "A103", "B101"} {
		if _, err := pool.Exec(ctx, `INSERT INTO poll_votes (poll_id, unit_id, option_id, user_id)
			SELECT $1, u.id, (SELECT id FROM poll_options WHERE poll_id=$1 AND position=$2), $3 FROM units u WHERE u.society_id=$4 AND u.number=$5`,
			pid, []int{0, 0, 1}[i], ResidentID, SocietyID, number); err != nil {
			return err
		}
	}
	return nil
}

// demoM8 adds four amenities with bookings for A101, daily help for A101/A102 and society staff with today's attendance.
func demoM8(ctx context.Context, pool *pgxpool.Pool) error {
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM amenities WHERE society_id=$1)`, SocietyID).Scan(&exists); err != nil || exists {
		return err
	}
	type am struct {
		name, desc          string
		capacity, slot      int
		fee, deposit        float64
		approval            bool
		opens, closes       string
		maxUp, maxWeek, cut int
	}
	list := []am{
		{"Clubhouse", "Air-conditioned hall for birthdays and meetings, seats 60.", 1, 120, 1500, 2000, true, "09:00", "22:00", 2, 3, 24},
		{"Gym", "Fully equipped gym. Hourly slots, up to 6 people at a time.", 6, 60, 0, 0, false, "05:00", "22:00", 3, 7, 1},
		{"Swimming Pool", "Pool with lifeguard. Closed on Mondays for cleaning.", 10, 60, 0, 0, false, "06:00", "20:00", 3, 5, 1},
		{"Tennis Court", "Floodlit court, one booking per slot.", 1, 60, 100, 0, false, "06:00", "21:00", 3, 5, 2},
	}
	ids := map[string]string{}
	for _, a := range list {
		var id string
		if err := pool.QueryRow(ctx, `INSERT INTO amenities (society_id, name, description, capacity, slot_minutes, min_notice_minutes, max_days_ahead, max_upcoming_per_flat, max_per_week_per_flat,
			cancel_cutoff_hours, fee_amount, deposit_amount, requires_approval) VALUES ($1,$2,$3,$4,$5,30,14,$6,$7,$8,$9,$10,$11) RETURNING id::text`,
			SocietyID, a.name, a.desc, a.capacity, a.slot, a.maxUp, a.maxWeek, a.cut, a.fee, a.deposit, a.approval).Scan(&id); err != nil {
			return fmt.Errorf("demo m8 amenity: %w", err)
		}
		ids[a.name] = id
		for d := 0; d < 7; d++ {
			if a.name == "Swimming Pool" && d == 1 {
				continue
			}
			if _, err := pool.Exec(ctx, `INSERT INTO amenity_hours (amenity_id, weekday, opens, closes) VALUES ($1,$2,$3::time,$4::time)`, id, d, a.opens, a.closes); err != nil {
				return err
			}
		}
	}
	// bookings for the demo flat: tomorrow's gym, the day after tomorrow's tennis, a clubhouse request, and some history
	type bk struct {
		amenity, status string
		dayOffset, hour int
		minutes         int
		fee, deposit    float64
		charge          string
	}
	for _, b := range []bk{
		{"Gym", "confirmed", 1, 7, 60, 0, 0, "none"},
		{"Tennis Court", "confirmed", 2, 18, 60, 100, 0, "pending"},
		{"Clubhouse", "pending", 5, 17, 120, 1500, 2000, "pending"},
		{"Gym", "completed", -2, 7, 60, 0, 0, "none"},
		{"Swimming Pool", "cancelled", -1, 17, 60, 0, 0, "none"},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO amenity_bookings (society_id, amenity_id, unit_id, booked_by_membership_id, slot_start, slot_end, status, people, fee_amount, deposit_amount, charge_status, created_at)
			SELECT $1, $2::uuid, u.id, $3, s.st, s.st + make_interval(mins => $7::int), $4, 2, $8, $9, $10, now() - interval '3 days'
			FROM units u, societies so,
			LATERAL (SELECT ((date_trunc('day', now() AT TIME ZONE so.timezone) + make_interval(days => $5::int, hours => $6::int)) AT TIME ZONE so.timezone) AS st) s
			WHERE u.society_id=$1 AND u.number='A101' AND so.id=$1`,
			SocietyID, ids[b.amenity], ResidentMem, b.status, b.dayOffset, b.hour, b.minutes, b.fee, b.deposit, b.charge); err != nil {
			return fmt.Errorf("demo m8 booking: %w", err)
		}
	}

	// daily help: Sunita works for A101 and A102, Mohan for A101 only (not verified yet)
	type hp struct {
		name, phone, kind, code, status, last4 string
		flats                                  []string
	}
	for _, h := range []hp{
		{"Sunita Devi", "9800000001", "maid", "482913", "verified", "4821", []string{"A101", "A102"}},
		{"Mohan Lal", "9800000002", "driver", "620417", "pending", "7305", []string{"A101"}},
		{"Kavita Rao", "9800000003", "cook", "135790", "verified", "1190", []string{"A102"}},
	} {
		var id string
		if err := pool.QueryRow(ctx, `INSERT INTO helpers (society_id, full_name, phone, kind, id_last4, status, pass_code, created_by) VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id::text`,
			SocietyID, h.name, h.phone, h.kind, h.last4, h.status, h.code, ResidentMem).Scan(&id); err != nil {
			return fmt.Errorf("demo m8 helper: %w", err)
		}
		for _, f := range h.flats {
			if _, err := pool.Exec(ctx, `INSERT INTO helper_flats (helper_id, unit_id, days, window_from, window_to)
				SELECT $1::uuid, u.id, '{0,1,2,3,4,5,6}', '06:00', '12:00' FROM units u WHERE u.society_id=$2 AND u.number=$3`, id, SocietyID, f); err != nil {
				return err
			}
		}
		if h.status == "verified" {
			if _, err := pool.Exec(ctx, `INSERT INTO helper_ratings (helper_id, unit_id, rating, comment) SELECT $1::uuid, u.id, 5, 'Always on time' FROM units u WHERE u.society_id=$2 AND u.number=$3`, id, SocietyID, h.flats[0]); err != nil {
				return err
			}
		}
	}

	// society staff and attendance: Sunita is inside now, Ramesh left an hour ago, the electrician has not come
	for _, st := range []struct{ name, phone, role, code, from, to string }{
		{"Ramesh Kumar", "9810000001", "gardener", "246801", "06:00", "14:00"},
		{"Lakshmi Bai", "9810000002", "housekeeping", "369258", "08:00", "16:00"},
		{"Anil Verma", "9810000003", "electrician", "159753", "10:00", "18:00"},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO society_staff (society_id, full_name, phone, role, pass_code, shift_from, shift_to) VALUES ($1,$2,$3,$4,$5,$6::time,$7::time)`,
			SocietyID, st.name, st.phone, st.role, st.code, st.from, st.to); err != nil {
			return fmt.Errorf("demo m8 staff: %w", err)
		}
	}
	for _, a := range []struct {
		table, phone string
		inAgo, out   string
		late         bool
	}{
		{"helpers", "9800000001", "2 hours", "", false},
		{"society_staff", "9810000001", "7 hours", "1 hour", false},
		{"society_staff", "9810000002", "6 hours", "", true},
	} {
		typ := "helper"
		if a.table == "society_staff" {
			typ = "staff"
		}
		out := "NULL"
		if a.out != "" {
			out = "now() - interval '" + a.out + "'"
		}
		if _, err := pool.Exec(ctx, `INSERT INTO attendance_logs (society_id, subject_type, subject_id, in_at, out_at, late)
			SELECT $1, '`+typ+`', t.id, now() - interval '`+a.inAgo+`', `+out+`, $3 FROM `+a.table+` t WHERE t.society_id=$1 AND t.phone=$2`, SocietyID, a.phone, a.late); err != nil {
			return fmt.Errorf("demo m8 attendance: %w", err)
		}
	}
	return nil
}
