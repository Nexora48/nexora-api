package society

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Nexora48/nexora-api/internal/audit"
	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
	"github.com/Nexora48/nexora-api/internal/platform/pagination"
)

type Handler struct {
	repo  *Repository
	audit *audit.Logger
}

func NewHandler(repo *Repository, a *audit.Logger) *Handler { return &Handler{repo: repo, audit: a} }

var (
	errBlockExists   = httpx.NewError(http.StatusConflict, "BLOCK_EXISTS", "a block with this code already exists")
	errBlockNotFound = httpx.NewError(http.StatusNotFound, "NOT_FOUND", "block not found")
	errBlockNotEmpty = httpx.NewError(http.StatusConflict, "BLOCK_NOT_EMPTY", "this block has residents or resident history and cannot be deleted")
	errUnitExists    = httpx.NewError(http.StatusConflict, "UNIT_EXISTS", "a flat with this number already exists in the block")
	errUnitNotFound  = httpx.NewError(http.StatusNotFound, "NOT_FOUND", "flat not found")
	errUnitOccupied  = httpx.NewError(http.StatusConflict, "UNIT_OCCUPIED", "this flat has residents or resident history and cannot be deleted")
)

func mapErr(err error) error {
	switch {
	case errors.Is(err, ErrBlockExists):
		return errBlockExists
	case errors.Is(err, ErrBlockNotFound):
		return errBlockNotFound
	case errors.Is(err, ErrBlockNotEmpty):
		return errBlockNotEmpty
	case errors.Is(err, ErrUnitExists):
		return errUnitExists
	case errors.Is(err, ErrUnitNotFound):
		return errUnitNotFound
	case errors.Is(err, ErrUnitOccupied):
		return errUnitOccupied
	}
	return err
}

func (h *Handler) log(r *http.Request, action, entity, id string, meta map[string]any) {
	p, _ := authctx.From(r.Context())
	h.audit.Log(r.Context(), audit.Entry{ActorID: &p.UserID, SocietyID: &p.SocietyID, Action: action, Entity: entity, EntityID: id, Meta: meta, IP: r.RemoteAddr})
}

func idParam(r *http.Request, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		return uuid.Nil, httpx.NewError(http.StatusNotFound, "NOT_FOUND", "not found")
	}
	return id, nil
}

type blockRequest struct {
	Name string `json:"name" validate:"required,max=60"`
	Code string `json:"code" validate:"required,max=10,alphanum"`
}

type bulkRequest struct {
	Floors        int    `json:"floors" validate:"required,min=1,max=60"`
	UnitsPerFloor int    `json:"units_per_floor" validate:"required,min=1,max=50"`
	StartFloor    *int   `json:"start_floor" validate:"omitempty,min=0,max=100"`
	UnitType      string `json:"unit_type" validate:"required,oneof=1BHK 2BHK 3BHK 4BHK Villa Shop Studio"`
	NumberFormat  string `json:"number_format" validate:"max=40"`
	// UnitMix sets the first flats of every floor to other types, in order; the rest of the floor uses UnitType.
	UnitMix []mixItem `json:"unit_mix" validate:"omitempty,max=7,dive"`
}

type mixItem struct {
	UnitType string `json:"unit_type" validate:"required,oneof=1BHK 2BHK 3BHK 4BHK Villa Shop Studio"`
	Count    int    `json:"count" validate:"required,min=1,max=50"`
}

type unitRequest struct {
	BlockID  string `json:"block_id" validate:"required,uuid"`
	Floor    int    `json:"floor" validate:"min=0,max=100"`
	Number   string `json:"number" validate:"required,max=20"`
	UnitType string `json:"unit_type" validate:"required,oneof=1BHK 2BHK 3BHK 4BHK Villa Shop Studio"`
	AreaSqft *int   `json:"area_sqft" validate:"omitempty,min=1,max=100000"`
}

// ── Blocks ───────────────────────────────────────────────────────────────────

func (h *Handler) ListBlocks(w http.ResponseWriter, r *http.Request) {
	p, _ := authctx.From(r.Context())
	bs, err := h.repo.ListBlocks(r.Context(), p.SocietyID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, bs, nil)
}

func (h *Handler) CreateBlock(w http.ResponseWriter, r *http.Request) {
	p, _ := authctx.From(r.Context())
	var req blockRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	b, err := h.repo.CreateBlock(r.Context(), p.SocietyID, strings.TrimSpace(req.Name), strings.ToUpper(req.Code))
	if err != nil {
		httpx.Fail(w, mapErr(err))
		return
	}
	h.log(r, "block.create", "block", b.ID.String(), map[string]any{"code": b.Code})
	httpx.Created(w, b)
}

func (h *Handler) UpdateBlock(w http.ResponseWriter, r *http.Request) {
	p, _ := authctx.From(r.Context())
	id, err := idParam(r, "id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	var req blockRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	if err := h.repo.UpdateBlock(r.Context(), p.SocietyID, id, strings.TrimSpace(req.Name), strings.ToUpper(req.Code)); err != nil {
		httpx.Fail(w, mapErr(err))
		return
	}
	h.log(r, "block.update", "block", id.String(), nil)
	httpx.NoContent(w)
}

func (h *Handler) DeleteBlock(w http.ResponseWriter, r *http.Request) {
	p, _ := authctx.From(r.Context())
	id, err := idParam(r, "id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if err := h.repo.DeleteBlock(r.Context(), p.SocietyID, id); err != nil {
		httpx.Fail(w, mapErr(err))
		return
	}
	h.log(r, "block.delete", "block", id.String(), nil)
	httpx.NoContent(w)
}

func (h *Handler) BulkUnits(w http.ResponseWriter, r *http.Request) {
	p, _ := authctx.From(r.Context())
	id, err := idParam(r, "id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	var req bulkRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	b, err := h.repo.GetBlock(r.Context(), p.SocietyID, id)
	if err != nil {
		httpx.Fail(w, mapErr(err))
		return
	}
	dry := r.URL.Query().Get("dry_run") == "true"
	start := 1 // default: first floor; pass start_floor=0 to include a ground floor
	if req.StartFloor != nil {
		start = *req.StartFloor
	}
	mix := make([]MixItem, len(req.UnitMix))
	total := 0
	for i, m := range req.UnitMix {
		mix[i] = MixItem{UnitType: m.UnitType, Count: m.Count}
		total += m.Count
	}
	if total > req.UnitsPerFloor {
		httpx.Fail(w, httpx.NewError(http.StatusBadRequest, "VALIDATION", fmt.Sprintf("the mix has %d flats but each floor only has %d", total, req.UnitsPerFloor)))
		return
	}
	res, err := h.repo.BulkCreateUnits(r.Context(), p.SocietyID, b, BulkSpec{
		Floors: req.Floors, UnitsPerFloor: req.UnitsPerFloor, StartFloor: start,
		UnitType: req.UnitType, NumberFormat: req.NumberFormat, Mix: mix}, dry)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate") {
			httpx.Fail(w, httpx.NewError(http.StatusBadRequest, "VALIDATION", err.Error()))
			return
		}
		httpx.Fail(w, err)
		return
	}
	if !dry {
		h.log(r, "unit.bulk_create", "block", id.String(), map[string]any{"created": res.Created, "skipped": len(res.Skipped)})
		httpx.Created(w, res)
		return
	}
	httpx.OK(w, res, nil)
}

// ── Units ────────────────────────────────────────────────────────────────────

func (h *Handler) ListUnits(w http.ResponseWriter, r *http.Request) {
	p, _ := authctx.From(r.Context())
	q := r.URL.Query()
	pg := pagination.FromRequest(r)
	f := UnitFilter{Occupancy: q.Get("occupancy"), Query: strings.TrimSpace(q.Get("q")), Limit: pg.PageSize, Offset: pg.Offset()}
	if v := q.Get("block_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			httpx.Fail(w, httpx.NewError(http.StatusBadRequest, "VALIDATION", "invalid block_id"))
			return
		}
		f.BlockID = &id
	}
	if v := q.Get("floor"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			httpx.Fail(w, httpx.NewError(http.StatusBadRequest, "VALIDATION", "invalid floor"))
			return
		}
		f.Floor = &n
	}
	units, total, err := h.repo.ListUnits(r.Context(), p.SocietyID, f)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, units, pg.Meta(total))
}

func (h *Handler) GetUnit(w http.ResponseWriter, r *http.Request) {
	p, _ := authctx.From(r.Context())
	id, err := idParam(r, "id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	u, err := h.repo.GetUnit(r.Context(), p.SocietyID, id)
	if err != nil {
		httpx.Fail(w, mapErr(err))
		return
	}
	httpx.OK(w, u, nil)
}

func (h *Handler) CreateUnit(w http.ResponseWriter, r *http.Request) {
	p, _ := authctx.From(r.Context())
	var req unitRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	u, err := h.repo.CreateUnit(r.Context(), p.SocietyID, uuid.MustParse(req.BlockID), req.Floor, strings.ToUpper(strings.TrimSpace(req.Number)), req.UnitType, req.AreaSqft)
	if err != nil {
		httpx.Fail(w, mapErr(err))
		return
	}
	h.log(r, "unit.create", "unit", u.ID.String(), map[string]any{"number": u.Number})
	httpx.Created(w, u)
}

func (h *Handler) UpdateUnit(w http.ResponseWriter, r *http.Request) {
	p, _ := authctx.From(r.Context())
	id, err := idParam(r, "id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	var req unitRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	if err := h.repo.UpdateUnit(r.Context(), p.SocietyID, id, req.Floor, strings.ToUpper(strings.TrimSpace(req.Number)), req.UnitType, req.AreaSqft); err != nil {
		httpx.Fail(w, mapErr(err))
		return
	}
	h.log(r, "unit.update", "unit", id.String(), nil)
	httpx.NoContent(w)
}

func (h *Handler) DeleteUnit(w http.ResponseWriter, r *http.Request) {
	p, _ := authctx.From(r.Context())
	id, err := idParam(r, "id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if err := h.repo.DeleteUnit(r.Context(), p.SocietyID, id); err != nil {
		httpx.Fail(w, mapErr(err))
		return
	}
	h.log(r, "unit.delete", "unit", id.String(), nil)
	httpx.NoContent(w)
}

// ── Overview & stats ─────────────────────────────────────────────────────────

func (h *Handler) Apartments(w http.ResponseWriter, r *http.Request) {
	p, _ := authctx.From(r.Context())
	v, err := h.repo.Overview(r.Context(), p.SocietyID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string]any{"blocks": v}, nil)
}

func (h *Handler) Stats(w http.ResponseWriter, r *http.Request) {
	p, _ := authctx.From(r.Context())
	s, err := h.repo.Stats(r.Context(), p.SocietyID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, s, nil)
}
