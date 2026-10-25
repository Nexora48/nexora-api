package seed

import (
	"context"
	"fmt"
	"hash/fnv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nexora48/nexora-api/internal/usage"
)

// DemoPlatform puts the demo society on the Standard plan (price copied, services untouched so the demo keeps
// working) and writes 30 days of believable usage so the SuperAdmin dashboards have something to draw.
// Idempotent: it only fills days that have no rows yet.
func DemoPlatform(ctx context.Context, pool *pgxpool.Pool, u *usage.Service) error {
	soc := uuid.MustParse(SocietyID)
	if _, err := pool.Exec(ctx, `INSERT INTO society_subscriptions (society_id, rate_per_unit_paise, flat_fee_paise, status, plan_id)
		SELECT $1, p.rate_per_unit_paise, p.flat_fee_paise, 'active', p.id FROM plans p WHERE p.key='standard'
		ON CONFLICT (society_id) DO UPDATE SET plan_id = COALESCE(society_subscriptions.plan_id, EXCLUDED.plan_id)`, soc); err != nil {
		return fmt.Errorf("demo plan: %w", err)
	}
	if err := u.Backfill(ctx, 1); err != nil { // today's real numbers
		return err
	}
	var have int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM usage_daily WHERE society_id=$1 AND day < current_date - 1`, soc).Scan(&have); err != nil || have > 0 {
		return err
	}
	// deterministic wobble so the charts are not flat
	wob := func(day int, metric string, base, spread int) int {
		h := fnv.New32a()
		fmt.Fprintf(h, "%d/%s", day, metric)
		return base + int(h.Sum32()%uint32(spread+1))
	}
	for d := 1; d <= 29; d++ {
		for _, m := range []struct {
			key          string
			base, spread int
		}{
			{usage.ActiveUsers, 6, 8}, {usage.Logins, 3, 5}, {usage.Visitors, 8, 14}, {usage.Deliveries, 4, 9},
			{usage.ComplaintsNew, 0, 2}, {usage.Notifications, 20, 40}, {usage.Push, 15, 30}, {usage.Bookings, 0, 3},
		} {
			if _, err := pool.Exec(ctx, `INSERT INTO usage_daily (society_id, day, metric, value) VALUES ($1, current_date - $2::int, $3, $4) ON CONFLICT DO NOTHING`,
				soc, d, m.key, wob(d, m.key, m.base, m.spread)); err != nil {
				return err
			}
		}
	}
	return nil
}
