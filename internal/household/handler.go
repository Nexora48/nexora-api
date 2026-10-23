package household

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
	"github.com/Nexora48/nexora-api/internal/resident"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

type profileRequest struct {
	FullName *string `json:"full_name" validate:"omitempty,min=1,max=120"`
	Email    *string `json:"email" validate:"omitempty,email,max=200"`
}

type emergencyRequest struct {
	Name     string `json:"name" validate:"required,max=100"`
	Phone    string `json:"phone" validate:"required,max=20"`
	Relation string `json:"relation" validate:"max=40"`
}

type familyRequest struct {
	Name        string  `json:"name" validate:"required,max=120"`
	Relation    string  `json:"relation" validate:"required,max=40"`
	Phone       *string `json:"phone" validate:"omitempty,max=20"`
	DateOfBirth *string `json:"date_of_birth"`
}

type vehicleRequest struct {
	VehicleType string  `json:"vehicle_type" validate:"required,oneof=car bike other"`
	Number      string  `json:"number" validate:"required,max=20"`
	MakeModel   string  `json:"make_model" validate:"max=60"`
	Color       string  `json:"color" validate:"max=30"`
	StickerNo   *string `json:"sticker_no" validate:"omitempty,max=30"`
}

func id(r *http.Request) (uuid.UUID, error) {
	v, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return uuid.Nil, ErrNotFound
	}
	return v, nil
}

func (h *Handler) UpdateProfile(w http.ResponseWriter, r *http.Request) {
	p, _ := authctx.From(r.Context())
	var req profileRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	if err := h.svc.UpdateProfile(r.Context(), p, req.FullName, req.Email); err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.NoContent(w)
}

func (h *Handler) GetEmergency(w http.ResponseWriter, r *http.Request) {
	p, _ := authctx.From(r.Context())
	e, err := h.svc.GetEmergency(r.Context(), p.UserID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, e, nil) // null when not set
}

func (h *Handler) SetEmergency(w http.ResponseWriter, r *http.Request) {
	p, _ := authctx.From(r.Context())
	var req emergencyRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	phone := resident.NormalizePhone(req.Phone)
	if !resident.ValidPhone(phone) {
		httpx.Fail(w, ErrInvalidPhone)
		return
	}
	if err := h.svc.SetEmergency(r.Context(), p.UserID, resident.EmergencyContact{Name: strings.TrimSpace(req.Name), Phone: phone, Relation: strings.TrimSpace(req.Relation)}); err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.NoContent(w)
}

func (h *Handler) Unit(w http.ResponseWriter, r *http.Request) {
	p, _ := authctx.From(r.Context())
	v, err := h.svc.Unit(r.Context(), p)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

func (h *Handler) ListFamily(w http.ResponseWriter, r *http.Request) {
	p, _ := authctx.From(r.Context())
	out, err := h.svc.ListFamily(r.Context(), p)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, out, nil)
}

func (h *Handler) AddFamily(w http.ResponseWriter, r *http.Request) {
	p, _ := authctx.From(r.Context())
	var req familyRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	f, err := h.svc.AddFamily(r.Context(), p, FamilyInput{Name: req.Name, Relation: req.Relation, Phone: req.Phone, DateOfBirth: req.DateOfBirth})
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.Created(w, f)
}

func (h *Handler) UpdateFamily(w http.ResponseWriter, r *http.Request) {
	p, _ := authctx.From(r.Context())
	fid, err := id(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	var req familyRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	if err := h.svc.UpdateFamily(r.Context(), p, fid, FamilyInput{Name: req.Name, Relation: req.Relation, DateOfBirth: req.DateOfBirth}); err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.NoContent(w)
}

func (h *Handler) DeleteFamily(w http.ResponseWriter, r *http.Request) {
	p, _ := authctx.From(r.Context())
	fid, err := id(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if err := h.svc.DeleteFamily(r.Context(), p, fid); err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.NoContent(w)
}

func (h *Handler) ListVehicles(w http.ResponseWriter, r *http.Request) {
	p, _ := authctx.From(r.Context())
	out, err := h.svc.ListVehicles(r.Context(), p)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, out, nil)
}

func (r vehicleRequest) input() VehicleInput {
	return VehicleInput{VehicleType: r.VehicleType, Number: r.Number, MakeModel: r.MakeModel, Color: r.Color, StickerNo: r.StickerNo}
}

func (h *Handler) AddVehicle(w http.ResponseWriter, r *http.Request) {
	p, _ := authctx.From(r.Context())
	var req vehicleRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	v, err := h.svc.AddVehicle(r.Context(), p, req.input())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.Created(w, v)
}

func (h *Handler) UpdateVehicle(w http.ResponseWriter, r *http.Request) {
	p, _ := authctx.From(r.Context())
	vid, err := id(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	var req vehicleRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	if err := h.svc.UpdateVehicle(r.Context(), p, vid, req.input()); err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.NoContent(w)
}

func (h *Handler) DeleteVehicle(w http.ResponseWriter, r *http.Request) {
	p, _ := authctx.From(r.Context())
	vid, err := id(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if err := h.svc.DeleteVehicle(r.Context(), p, vid); err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.NoContent(w)
}
