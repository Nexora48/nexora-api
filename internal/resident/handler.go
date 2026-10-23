package resident

import (
	"context"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
	"github.com/Nexora48/nexora-api/internal/platform/pagination"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

type emergencyDTO struct {
	Name     string `json:"name" validate:"max=100"`
	Phone    string `json:"phone" validate:"max=20"`
	Relation string `json:"relation" validate:"max=40"`
}

func (e *emergencyDTO) model() *EmergencyContact {
	if e == nil {
		return nil
	}
	return &EmergencyContact{Name: strings.TrimSpace(e.Name), Phone: NormalizePhone(e.Phone), Relation: strings.TrimSpace(e.Relation)}
}

type createRequest struct {
	FullName          string        `json:"full_name" validate:"required,max=120"`
	Phone             string        `json:"phone" validate:"required,max=20"`
	Email             *string       `json:"email" validate:"omitempty,email,max=200"`
	UnitID            string        `json:"unit_id" validate:"required,uuid"`
	Type              string        `json:"type" validate:"required,oneof=owner tenant family"`
	Relation          string        `json:"relation" validate:"max=40"`
	MoveInDate        *string       `json:"move_in_date"`
	AgreementEndDate  *string       `json:"agreement_end_date"`
	DateOfBirth       *string       `json:"date_of_birth"`
	GovIDType         *string       `json:"gov_id_type" validate:"omitempty,oneof=aadhaar pan passport driving_license voter_id"`
	GovIDNumber       *string       `json:"gov_id_number" validate:"omitempty,max=30"`
	EmergencyContact  *emergencyDTO `json:"emergency_contact"`
	AppAccess         *bool         `json:"app_access"`
	CanInviteVisitors *bool         `json:"can_invite_visitors"`
	CanPayMaintenance *bool         `json:"can_pay_maintenance"`
	SOSEnabled        *bool         `json:"sos_enabled"`
}

type updateRequest struct {
	FullName          *string       `json:"full_name" validate:"omitempty,min=1,max=120"`
	Email             *string       `json:"email" validate:"omitempty,email,max=200"`
	DateOfBirth       *string       `json:"date_of_birth"`
	MoveInDate        *string       `json:"move_in_date"`
	AgreementEndDate  *string       `json:"agreement_end_date"`
	EmergencyContact  *emergencyDTO `json:"emergency_contact"`
	AppAccess         *bool         `json:"app_access"`
	CanInviteVisitors *bool         `json:"can_invite_visitors"`
	CanPayMaintenance *bool         `json:"can_pay_maintenance"`
	SOSEnabled        *bool         `json:"sos_enabled"`
}

func idParam(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return uuid.Nil, ErrNotFound
	}
	return id, nil
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	p, _ := authctx.From(r.Context())
	q := r.URL.Query()
	pg := pagination.FromRequest(r)
	f := ListFilter{Type: q.Get("type"), Status: q.Get("status"), Query: strings.TrimSpace(q.Get("q")), Limit: pg.PageSize, Offset: pg.Offset()}
	if v := q.Get("unit_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			httpx.Fail(w, httpx.NewError(http.StatusBadRequest, "VALIDATION", "invalid unit_id"))
			return
		}
		f.UnitID = &id
	}
	res, total, err := h.svc.List(r.Context(), p.SocietyID, f)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, res, pg.Meta(total))
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	p, _ := authctx.From(r.Context())
	id, err := idParam(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	d, err := h.svc.Get(r.Context(), p.SocietyID, id)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, d, nil)
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	p, _ := authctx.From(r.Context())
	var req createRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	d, err := h.svc.Create(r.Context(), p.SocietyID, p.UserID, CreateInput{
		FullName: req.FullName, Phone: req.Phone, Email: req.Email, UnitID: uuid.MustParse(req.UnitID), Type: req.Type,
		Relation: req.Relation, MoveInDate: req.MoveInDate, AgreementEndDate: req.AgreementEndDate, DateOfBirth: req.DateOfBirth,
		GovIDType: req.GovIDType, GovIDNumber: req.GovIDNumber, Emergency: req.EmergencyContact.model(),
		AppAccess: req.AppAccess, CanInviteVisitors: req.CanInviteVisitors, CanPayMaintenance: req.CanPayMaintenance, SOSEnabled: req.SOSEnabled,
	})
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.Created(w, d)
}

func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	p, _ := authctx.From(r.Context())
	id, err := idParam(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	var req updateRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	d, err := h.svc.Update(r.Context(), p.SocietyID, p.UserID, id, UpdateInput{
		FullName: req.FullName, Email: req.Email, DateOfBirth: req.DateOfBirth, MoveInDate: req.MoveInDate,
		AgreementEndDate: req.AgreementEndDate, Emergency: req.EmergencyContact.model(), AppAccess: req.AppAccess,
		CanInviteVisitors: req.CanInviteVisitors, CanPayMaintenance: req.CanPayMaintenance, SOSEnabled: req.SOSEnabled,
	})
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, d, nil)
}

func (h *Handler) act(fn func(ctx context.Context, society, actor, id uuid.UUID) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, _ := authctx.From(r.Context())
		id, err := idParam(r)
		if err != nil {
			httpx.Fail(w, err)
			return
		}
		if err := fn(r.Context(), p.SocietyID, p.UserID, id); err != nil {
			httpx.Fail(w, err)
			return
		}
		httpx.NoContent(w)
	}
}

func (h *Handler) Suspend() http.HandlerFunc { return h.act(h.svc.Suspend) }
func (h *Handler) Restore() http.HandlerFunc { return h.act(h.svc.Restore) }
func (h *Handler) MoveOut() http.HandlerFunc { return h.act(h.svc.MoveOut) }

func (h *Handler) Import(w http.ResponseWriter, r *http.Request) {
	p, _ := authctx.From(r.Context())
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	if err := r.ParseMultipartForm(2 << 20); err != nil {
		httpx.Fail(w, httpx.NewError(http.StatusBadRequest, "IMPORT_INVALID", "upload a CSV file (max 2 MB) in the 'file' field"))
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		httpx.Fail(w, httpx.NewError(http.StatusBadRequest, "IMPORT_INVALID", "missing 'file' field"))
		return
	}
	defer f.Close()
	res, err := h.svc.Import(r.Context(), p.SocietyID, p.UserID, f, r.URL.Query().Get("dry_run") == "true")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, res, nil)
}

func (h *Handler) ImportTemplate(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", `attachment; filename="residents_template.csv"`)
	_, _ = w.Write([]byte(ImportTemplate))
}
