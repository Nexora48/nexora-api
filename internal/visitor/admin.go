package visitor

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Nexora48/nexora-api/internal/platform/authctx"
)

func (s *Service) adminWhere(societyID uuid.UUID, f ListFilter) ([]string, []any) {
	args := []any{societyID}
	where := []string{"v.society_id=$1"}
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, strings.ReplaceAll(cond, "?", fmt.Sprintf("$%d", len(args))))
	}
	if f.Status != "" {
		add(s.eff()+" = ?", f.Status)
	}
	if f.Purpose != "" {
		add("v.purpose = ?", f.Purpose)
	}
	if f.UnitID != nil {
		add("v.unit_id = ?", *f.UnitID)
	}
	if f.From != nil {
		add("v.created_at >= ?", *f.From)
	}
	if f.To != nil {
		add("v.created_at < ?", *f.To)
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		args = append(args, "%"+q+"%")
		n := fmt.Sprintf("$%d", len(args))
		where = append(where, "(v.visitor_name ILIKE "+n+" OR v.visitor_phone ILIKE "+n+" OR un.number ILIKE "+n+" OR v.vehicle_number ILIKE "+n+" OR hu.full_name ILIKE "+n+")")
	}
	return where, args
}

// AdminList is the society-wide visitor log.
func (s *Service) AdminList(ctx context.Context, p authctx.Principal, f ListFilter) ([]Visit, int, error) {
	where, args := s.adminWhere(p.SocietyID, f)
	return s.many(ctx, where, args, "v.created_at DESC, v.id", f.Limit, f.Offset)
}

// AdminStats feeds the KPI strip (today uses the society's timezone).
func (s *Service) AdminStats(ctx context.Context, p authctx.Principal) (*Stats, error) {
	e := s.eff()
	var st Stats
	err := s.pool.QueryRow(ctx, fmt.Sprintf(`
		SELECT count(*) FILTER (WHERE (v.created_at >= d.start OR v.entered_at >= d.start) AND v.status <> 'cancelled'),
		       count(*) FILTER (WHERE v.status = 'inside'),
		       count(*) FILTER (WHERE %s IN ('waiting','invited'))
		FROM visits v
		CROSS JOIN (SELECT date_trunc('day', now() AT TIME ZONE so.timezone) AT TIME ZONE so.timezone AS start FROM societies so WHERE so.id=$1) d
		WHERE v.society_id=$1`, e), p.SocietyID).Scan(&st.Today, &st.InsideNow, &st.Awaiting)
	return &st, err
}

const maxExportRows = 10000

// csvSafe stops spreadsheet formula injection: cells that start with = + - @ (or tab/CR) get a leading quote.
func csvSafe(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func ts(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// AdminExport streams the filtered log as CSV (capped at maxExportRows).
func (s *Service) AdminExport(ctx context.Context, p authctx.Principal, f ListFilter, w io.Writer) (int, error) {
	where, args := s.adminWhere(p.SocietyID, f)
	rows, _, err := s.many(ctx, where, args, "v.created_at DESC, v.id", maxExportRows, 0)
	if err != nil {
		return 0, err
	}
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"Visitor", "Phone", "Purpose", "Company", "Vehicle", "People", "Flat", "Resident", "Status", "Source", "Expected from", "Expected until", "Entered", "Exited", "Created"})
	for _, v := range rows {
		_ = cw.Write([]string{
			csvSafe(v.VisitorName), csvSafe(deref(v.VisitorPhone)), v.Purpose, csvSafe(deref(v.Company)), csvSafe(deref(v.VehicleNumber)),
			fmt.Sprint(v.VisitorCount), v.UnitNumber, csvSafe(deref(v.HostName)), v.Status, v.Source,
			ts(&v.ExpectedFrom), ts(&v.ExpectedUntil), ts(v.EnteredAt), ts(v.ExitedAt), ts(&v.CreatedAt),
		})
	}
	cw.Flush()
	s.logv(ctx, p, "visit.export", uuid.Nil, map[string]any{"rows": len(rows)})
	return len(rows), cw.Error()
}
