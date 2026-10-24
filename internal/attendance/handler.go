package attendance

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Nexora48/nexora-api/internal/attachment"
	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
)

type Handler struct {
	svc   *Service
	files *attachment.Service
}

func NewHandler(svc *Service, files *attachment.Service) *Handler {
	return &Handler{svc: svc, files: files}
}

func principal(r *http.Request) authctx.Principal { p, _ := authctx.From(r.Context()); return p }

func pid(r *http.Request, name string, notFound error) (uuid.UUID, error) {
	v, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		return uuid.Nil, notFound
	}
	return v, nil
}

func ok(w http.ResponseWriter) { httpx.OK(w, map[string]bool{"ok": true}, nil) }

// ── gate ─────────────────────────────────────────────────────────────────────

func (h *Handler) Resolve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		QR   string `json:"qr"`
		Code string `json:"code"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	sub, err := h.svc.Resolve(r.Context(), principal(r), req.QR, req.Code)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, sub, nil)
}

type markRequest struct {
	SubjectType string     `json:"subject_type"`
	SubjectID   string     `json:"subject_id"`
	GateID      *string    `json:"gate_id"`
	At          *time.Time `json:"at"`
}

func (h *Handler) mark(in bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req markRequest
		if err := httpx.Decode(w, r, &req); err != nil {
			httpx.Fail(w, err)
			return
		}
		rec, err := h.svc.Mark(r.Context(), principal(r), in, req.SubjectType, req.SubjectID, req.GateID, req.At)
		if err != nil {
			httpx.Fail(w, err)
			return
		}
		httpx.OK(w, rec, nil)
	}
}

func (h *Handler) CheckIn() http.HandlerFunc  { return h.mark(true) }
func (h *Handler) CheckOut() http.HandlerFunc { return h.mark(false) }

// Mark validates the ids and runs a check-in or check-out (shared with the offline batch).
func (s *Service) Mark(ctx context.Context, p authctx.Principal, in bool, typ, subject string, gate *string, at *time.Time) (*Record, error) {
	id, err := uuid.Parse(subject)
	if err != nil {
		return nil, invalid("subject_id must be a UUID")
	}
	var gid *uuid.UUID
	if gate != nil && *gate != "" {
		g, err := uuid.Parse(*gate)
		if err != nil {
			return nil, invalid("gate_id must be a UUID")
		}
		gid = &g
	}
	if in {
		return s.CheckIn(ctx, p, typ, id, gid, at)
	}
	return s.CheckOut(ctx, p, typ, id, at)
}

func (h *Handler) Inside(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.Inside(r.Context(), principal(r))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, list, nil)
}

func (h *Handler) GatePhoto(w http.ResponseWriter, r *http.Request) {
	id, err := pid(r, "id", ErrNotFound)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	b, f, err := h.svc.Photo(r.Context(), principal(r), chi.URLParam(r, "type"), id)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	attachment.Serve(w, f, b)
}

// ── office ───────────────────────────────────────────────────────────────────

func (h *Handler) Sheet(w http.ResponseWriter, r *http.Request) {
	sh, err := h.svc.Sheet(r.Context(), principal(r), r.URL.Query().Get("month"), r.URL.Query().Get("type"))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, sh, nil)
}

func (h *Handler) Export(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="attendance.csv"`)
	if err := h.svc.ExportSheet(r.Context(), principal(r), r.URL.Query().Get("month"), r.URL.Query().Get("type"), w); err != nil {
		httpx.Fail(w, err)
	}
}

func (h *Handler) Log(w http.ResponseWriter, r *http.Request) {
	id, err := pid(r, "id", ErrNotFound)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	typ := chi.URLParam(r, "type")
	if typ != "helper" && typ != "staff" {
		httpx.Fail(w, ErrNotFound)
		return
	}
	list, err := h.svc.Log(r.Context(), principal(r), typ, id, r.URL.Query().Get("month"))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, list, nil)
}

func (h *Handler) Manual(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SubjectType string    `json:"subject_type"`
		SubjectID   string    `json:"subject_id"`
		In          time.Time `json:"in_at"`
		Out         time.Time `json:"out_at"`
		Note        string    `json:"note"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	id, err := uuid.Parse(req.SubjectID)
	if err != nil {
		httpx.Fail(w, invalid("subject_id must be a UUID"))
		return
	}
	rec, err := h.svc.ManualMark(r.Context(), principal(r), req.SubjectType, id, req.In, req.Out, req.Note)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.Created(w, rec)
}

// ── staff registry ───────────────────────────────────────────────────────────

type staffRequest struct {
	Name      string `json:"full_name"`
	Phone     string `json:"phone"`
	Role      string `json:"role"`
	ShiftFrom string `json:"shift_from"`
	ShiftTo   string `json:"shift_to"`
	WorkDays  []int  `json:"work_days"`
	Active    *bool  `json:"active"`
}

func (q staffRequest) input() StaffInput {
	return StaffInput{Name: q.Name, Phone: q.Phone, Role: q.Role, ShiftFrom: q.ShiftFrom, ShiftTo: q.ShiftTo, WorkDays: q.WorkDays, Active: q.Active}
}

func (h *Handler) ListStaff(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.ListStaff(r.Context(), principal(r), r.URL.Query().Get("all") == "1")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, list, nil)
}

func (h *Handler) GetStaff(w http.ResponseWriter, r *http.Request) {
	id, err := pid(r, "id", ErrStaffNotFound)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	st, err := h.svc.GetStaff(r.Context(), principal(r), id)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, st, nil)
}

func (h *Handler) CreateStaff(w http.ResponseWriter, r *http.Request) {
	var req staffRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	st, err := h.svc.CreateStaff(r.Context(), principal(r), req.input())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.Created(w, st)
}

func (h *Handler) UpdateStaff(w http.ResponseWriter, r *http.Request) {
	id, err := pid(r, "id", ErrStaffNotFound)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	var req staffRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	st, err := h.svc.UpdateStaff(r.Context(), principal(r), id, req.input())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, st, nil)
}

func (h *Handler) staffPass(regen bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := pid(r, "id", ErrStaffNotFound)
		if err != nil {
			httpx.Fail(w, err)
			return
		}
		v, err := h.svc.StaffPass(r.Context(), principal(r), id, regen)
		if err != nil {
			httpx.Fail(w, err)
			return
		}
		httpx.OK(w, v, nil)
	}
}

func (h *Handler) StaffPass() http.HandlerFunc    { return h.staffPass(false) }
func (h *Handler) StaffNewCode() http.HandlerFunc { return h.staffPass(true) }

func (h *Handler) StaffPhoto(w http.ResponseWriter, r *http.Request) {
	id, err := pid(r, "id", ErrStaffNotFound)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	name, data, err := h.files.ReadUpload(w, r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if err := h.svc.SetStaffPhoto(r.Context(), principal(r), id, name, data); err != nil {
		httpx.Fail(w, err)
		return
	}
	ok(w)
}
