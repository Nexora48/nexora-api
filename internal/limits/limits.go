// Package limits enforces the limits of the plan a society is on. A society without a plan
// (or on the "legacy" plan) is unlimited. Hard limits stop creation with PLAN_LIMIT; soft
// limits are only reported so the apps can warn.
package limits

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Nexora48/nexora-api/internal/platform/httpx"
	"github.com/Nexora48/nexora-api/internal/platform/metrics"
)

const (
	MaxUnits         = "max_units"
	MaxResidents     = "max_residents"
	MaxGuards        = "max_guards"
	MaxAdmins        = "max_admins"
	SMSPerMonth      = "sms_per_month"
	StorageMB        = "storage_mb"
	VisitorsPerMonth = "visitors_per_month"
)

// Keys lists every limit with its kind, in display order.
var Keys = []struct {
	Key  string
	Name string
	Hard bool
}{
	{MaxUnits, "Flats", true},
	{MaxResidents, "Residents", true},
	{MaxGuards, "Guards", true},
	{MaxAdmins, "Office admins", true},
	{SMSPerMonth, "SMS this month", true},
	{StorageMB, "Storage (MB)", false},
	{VisitorsPerMonth, "Visitors this month", false},
}

// Valid reports whether k is a known limit key.
func Valid(k string) bool {
	for _, x := range Keys {
		if x.Key == k {
			return true
		}
	}
	return false
}

func isHard(k string) bool {
	for _, x := range Keys {
		if x.Key == k {
			return x.Hard
		}
	}
	return false
}

func name(k string) string {
	for _, x := range Keys {
		if x.Key == k {
			return x.Name
		}
	}
	return k
}

type Checker struct {
	pool *pgxpool.Pool
	rdb  *redis.Client
}

func New(pool *pgxpool.Pool) *Checker { return &Checker{pool: pool} }

// SetRedis lets Guard serialise creations per society and limit across API instances.
func (c *Checker) SetRedis(r *redis.Client) *Checker { c.rdb = r; return c }

var errBusy = httpx.NewError(http.StatusServiceUnavailable, "BUSY", "another change to this society is in progress, please try again")

// Guard takes a short lock for (society, limit) so that the "is there room?" check and the creation that follows
// cannot interleave with another creation: twenty simultaneous requests at a limit of five create exactly five.
// Call the returned function when the creation is done (or has failed). Without Redis, or on a nil Checker, it does nothing.
func (c *Checker) Guard(ctx context.Context, society uuid.UUID, key string) (func(), error) {
	if c == nil || c.rdb == nil || !isHard(key) {
		return func() {}, nil
	}
	lock := "limit-lock:" + society.String() + ":" + key
	token := uuid.NewString()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ok, err := c.rdb.SetNX(ctx, lock, token, 10*time.Second).Result()
		if err != nil {
			return func() {}, nil // Redis unavailable: fall back to the unguarded check rather than refuse creations
		}
		if ok {
			return func() {
				// release only our own lock
				_ = unlock.Run(context.WithoutCancel(ctx), c.rdb, []string{lock}, token).Err()
			}, nil
		}
		if time.Now().After(deadline) {
			return func() {}, errBusy
		}
		select {
		case <-ctx.Done():
			return func() {}, ctx.Err()
		case <-time.After(15 * time.Millisecond):
		}
	}
}

var unlock = redis.NewScript(`if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end return 0`)

// Err is the PLAN_LIMIT error.
func Err(key string, used, max int64) *httpx.AppError {
	metrics.PlanLimit.Inc()
	return httpx.NewError(http.StatusUnprocessableEntity, "PLAN_LIMIT",
		fmt.Sprintf("Plan limit reached: %d of %d (%s). Contact Nexora to upgrade your plan.", used, max, name(key)))
}

// Limits returns the society's limit map (empty = unlimited) and the plan name.
func (c *Checker) Limits(ctx context.Context, society uuid.UUID) (map[string]int64, string, string, error) {
	var raw []byte
	var planName, planKey string
	err := c.pool.QueryRow(ctx, `SELECT p.limits, p.name, p.key FROM society_subscriptions s JOIN plans p ON p.id=s.plan_id WHERE s.society_id=$1`, society).Scan(&raw, &planName, &planKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return map[string]int64{}, "", "", nil
	}
	if err != nil {
		return nil, "", "", err
	}
	m := map[string]int64{}
	var f map[string]float64
	if json.Unmarshal(raw, &f) == nil {
		for k, v := range f {
			m[k] = int64(v)
		}
	}
	return m, planName, planKey, nil
}

// Used counts what a limit measures, live from the tables.
func (c *Checker) Used(ctx context.Context, society uuid.UUID, key string) (int64, error) {
	var n int64
	var q string
	switch key {
	case MaxUnits:
		q = `SELECT count(*) FROM units WHERE society_id=$1`
	case MaxResidents:
		q = `SELECT count(*) FROM memberships WHERE society_id=$1 AND status IN ('active','suspended') AND role_key IN ('resident_owner','resident_tenant','family_member')`
	case MaxGuards:
		q = `SELECT count(*) FROM memberships WHERE society_id=$1 AND status IN ('active','suspended') AND role_key='guard'`
	case MaxAdmins:
		q = `SELECT count(*) FROM memberships WHERE society_id=$1 AND status IN ('active','suspended') AND role_key='society_admin'`
	case SMSPerMonth:
		q = `SELECT count(*) FROM notification_outbox o WHERE o.channel='sms' AND o.created_at >= date_trunc('month', now())
			AND EXISTS (SELECT 1 FROM memberships m WHERE m.user_id=o.user_id AND m.society_id=$1)`
	case StorageMB:
		q = `SELECT COALESCE(sum(size_bytes),0)/1048576 FROM attachments WHERE society_id=$1`
	case VisitorsPerMonth:
		q = `SELECT count(*) FROM visits WHERE society_id=$1 AND created_at >= date_trunc('month', now())`
	default:
		return 0, fmt.Errorf("unknown limit %q", key)
	}
	err := c.pool.QueryRow(ctx, q, society).Scan(&n)
	return n, err
}

// Room is how many more of key can be created: -1 means unlimited.
func (c *Checker) Room(ctx context.Context, society uuid.UUID, key string) (int64, error) {
	if c == nil {
		return -1, nil
	}
	m, _, _, err := c.Limits(ctx, society)
	if err != nil {
		return 0, err
	}
	max, ok := m[key]
	if !ok {
		return -1, nil
	}
	used, err := c.Used(ctx, society, key)
	if err != nil {
		return 0, err
	}
	if used >= max {
		return 0, nil
	}
	return max - used, nil
}

// Check refuses with PLAN_LIMIT when adding `add` more of a hard-limited key would pass the maximum.
// Soft keys and societies without that limit always pass. A nil Checker allows everything.
func (c *Checker) Check(ctx context.Context, society uuid.UUID, key string, add int) error {
	if c == nil || !isHard(key) {
		return nil
	}
	m, _, _, err := c.Limits(ctx, society)
	if err != nil {
		return err
	}
	max, ok := m[key]
	if !ok {
		return nil
	}
	used, err := c.Used(ctx, society, key)
	if err != nil {
		return err
	}
	if used+int64(add) > max {
		return Err(key, used, max)
	}
	return nil
}

// AllowSMS says whether one more SMS may be queued. Critical messages (OTP, SOS) always pass.
func (c *Checker) AllowSMS(ctx context.Context, society uuid.UUID, critical bool) (bool, error) {
	if critical || c == nil {
		return true, nil
	}
	return c.Check(ctx, society, SMSPerMonth, 1) == nil, nil
}

type Usage struct {
	Key       string `json:"key"`
	Name      string `json:"name"`
	Used      int64  `json:"used"`
	Max       *int64 `json:"max"` // null = unlimited
	Hard      bool   `json:"hard"`
	Exceeded  bool   `json:"exceeded"`
	AtWarning bool   `json:"at_warning"` // 90 % or more
}

type Snapshot struct {
	PlanKey  string  `json:"plan_key"`
	PlanName string  `json:"plan_name"`
	Usage    []Usage `json:"usage"`
}

// Snapshot is used by the society's own plan card and the SuperAdmin usage tab.
func (c *Checker) Snapshot(ctx context.Context, society uuid.UUID) (*Snapshot, error) {
	m, pn, pk, err := c.Limits(ctx, society)
	if err != nil {
		return nil, err
	}
	out := &Snapshot{PlanKey: pk, PlanName: pn, Usage: []Usage{}}
	if pn == "" {
		out.PlanName = "No plan"
	}
	for _, k := range Keys {
		used, err := c.Used(ctx, society, k.Key)
		if err != nil {
			return nil, err
		}
		u := Usage{Key: k.Key, Name: k.Name, Used: used, Hard: k.Hard}
		if max, ok := m[k.Key]; ok {
			mm := max
			u.Max = &mm
			u.Exceeded = used > max
			u.AtWarning = max > 0 && used*100 >= max*90
		}
		out.Usage = append(out.Usage, u)
	}
	return out, nil
}
