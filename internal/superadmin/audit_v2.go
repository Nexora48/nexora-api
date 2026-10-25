package superadmin

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Nexora48/nexora-api/internal/platform/heartbeat"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
	"github.com/Nexora48/nexora-api/internal/platform/pagination"
)

var ErrAuditNotFound = httpx.NewError(http.StatusNotFound, "NOT_FOUND", "audit entry not found")

const auditExportCap = 50000

// csvSafe stops spreadsheet formulas: a cell starting with = + - @ is prefixed with a quote.
func csvSafe(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}

// auditFilterFrom reads the filters shared by the list and the export.
func auditFilterFrom(r *http.Request) (AuditFilter, error) {
	q := r.URL.Query()
	f := AuditFilter{Action: q.Get("action"), Entity: q.Get("entity"), Text: strings.TrimSpace(q.Get("q")), ActorType: q.Get("actor_type"), From: parseTime(q.Get("from")), To: parseTime(q.Get("to"))}
	if f.ActorType != "" && f.ActorType != "user" && f.ActorType != "platform" && f.ActorType != "system" {
		return f, httpx.NewError(http.StatusBadRequest, "VALIDATION", "actor_type must be user, platform or system")
	}
	if f.To != nil && len(q.Get("to")) == 10 { // a bare date means "through that day"
		t := f.To.AddDate(0, 0, 1)
		f.To = &t
	}
	if v := q.Get("society_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			return f, httpx.NewError(http.StatusBadRequest, "VALIDATION", "invalid society_id")
		}
		f.SocietyID = &id
	}
	if v := q.Get("actor"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			return f, httpx.NewError(http.StatusBadRequest, "VALIDATION", "invalid actor")
		}
		f.ActorID = &id
	}
	return f, nil
}

func (h *Handler) AuditV2(w http.ResponseWriter, r *http.Request) {
	pg := pagination.FromRequest(r)
	f, err := auditFilterFrom(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	f.Limit, f.Offset = pg.PageSize, pg.Offset()
	rows, total, err := h.svc.ListAudit(r.Context(), f)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, rows, pg.Meta(total))
}

func (h *Handler) AuditGet(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		httpx.Fail(w, ErrAuditNotFound)
		return
	}
	v, err := h.svc.GetAudit(r.Context(), id)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

// AuditExport streams the filtered log as CSV (at most 50,000 rows); the export itself is audited.
func (h *Handler) AuditExport(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	f, err := auditFilterFrom(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	cond, args := f.where()
	rows, err := h.svc.store.pool.Query(r.Context(), auditSelect+auditFrom+` WHERE `+cond+fmt.Sprintf(` ORDER BY a.created_at DESC, a.id DESC LIMIT %d`, auditExportCap), args...)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	defer rows.Close()
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="audit-`+time.Now().UTC().Format("20060102-150405")+`.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"id", "time_utc", "actor_type", "actor", "actor_email", "society", "action", "entity", "entity_id", "ip", "meta"})
	n := 0
	for rows.Next() {
		a, err := scanAudit(rows)
		if err != nil {
			slog.Error("audit export row", "err", err)
			break
		}
		str := func(v *string) string {
			if v == nil {
				return ""
			}
			return *v
		}
		_ = cw.Write([]string{strconv.FormatInt(a.ID, 10), a.CreatedAt.UTC().Format(time.RFC3339), a.ActorType, csvSafe(str(a.ActorName)), csvSafe(str(a.ActorEmail)),
			csvSafe(str(a.SocietyName)), csvSafe(a.Action), csvSafe(a.Entity), csvSafe(a.EntityID), csvSafe(a.IP), csvSafe(string(a.Meta))})
		n++
	}
	cw.Flush()
	h.svc.platformLog(r.Context(), p.UserID, f.SocietyID, "audit.export", "audit", "", map[string]any{"rows": n, "action": f.Action, "q": f.Text, "actor_type": f.ActorType})
}

// ── retention ────────────────────────────────────────────────────────────────

type AuditSettings struct {
	RetentionDays int `json:"retention_days"`
}

func (s *Service) AuditSettings(ctx context.Context) (*AuditSettings, error) {
	var a AuditSettings
	if err := s.store.pool.QueryRow(ctx, `SELECT retention_days FROM audit_settings`).Scan(&a.RetentionDays); err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *Service) SetAuditSettings(ctx context.Context, actor uuid.UUID, in AuditSettings) (*AuditSettings, error) {
	if in.RetentionDays < 30 || in.RetentionDays > 3650 {
		return nil, httpx.NewError(http.StatusBadRequest, "VALIDATION", "retention must be 30 to 3650 days")
	}
	before, err := s.AuditSettings(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := s.store.pool.Exec(ctx, `UPDATE audit_settings SET retention_days=$1`, in.RetentionDays); err != nil {
		return nil, err
	}
	s.platformLog(ctx, actor, nil, "audit.retention", "audit", "", map[string]any{"before": before.RetentionDays, "after": in.RetentionDays})
	return s.AuditSettings(ctx)
}

// PurgeAudit deletes rows past retention in batches (platform rows are kept twice as long) and
// leaves one summary row. It returns how many rows were removed.
func (s *Service) PurgeAudit(ctx context.Context, now time.Time) (int64, error) {
	set, err := s.AuditSettings(ctx)
	if err != nil {
		return 0, err
	}
	var total int64
	for {
		tag, err := s.store.pool.Exec(ctx, `DELETE FROM audit_logs WHERE id IN (
			SELECT id FROM audit_logs WHERE created_at < CASE WHEN actor_type='platform' THEN $1::timestamptz - make_interval(days => $2*2) ELSE $1::timestamptz - make_interval(days => $2) END
			AND action <> 'audit.purge' ORDER BY id LIMIT 1000)`, now, set.RetentionDays)
		if err != nil {
			return total, err
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < 1000 {
			break
		}
	}
	if total > 0 {
		meta, _ := json.Marshal(map[string]any{"removed": total, "retention_days": set.RetentionDays})
		_, _ = s.store.pool.Exec(ctx, `INSERT INTO audit_logs (actor_type, action, entity, meta) VALUES ('system','audit.purge','audit',$1)`, meta)
	}
	return total, nil
}

// RunRetention purges once a day.
func (s *Service) RunRetention(ctx context.Context) {
	t := time.NewTicker(6 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := s.PurgeAudit(ctx, time.Now()); err == nil {
				heartbeat.Beat("audit-retention")
			}
		}
	}
}
