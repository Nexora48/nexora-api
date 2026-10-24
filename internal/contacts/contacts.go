// Package contacts holds the society's emergency contacts (security room, manager, ...), managed by the office.
package contacts

import (
	"context"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nexora48/nexora-api/internal/audit"
	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
)

var (
	ErrNotFound = httpx.NewError(http.StatusNotFound, "NOT_FOUND", "contact not found")
	ErrInvalid  = httpx.NewError(http.StatusBadRequest, "VALIDATION", "label 2-60 characters, phone 3-15 digits")
	ErrLimit    = httpx.NewError(http.StatusConflict, "LIMIT", "a society can list up to 20 contacts")
)

type Contact struct {
	ID    uuid.UUID `json:"id"`
	Label string    `json:"label"`
	Phone string    `json:"phone"`
	Hours string    `json:"hours"`
	Sort  int       `json:"sort_order"`
}

type Service struct {
	pool  *pgxpool.Pool
	audit *audit.Logger
}

func NewService(pool *pgxpool.Pool, a *audit.Logger) *Service { return &Service{pool: pool, audit: a} }

func (s *Service) List(ctx context.Context, soc uuid.UUID) ([]Contact, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, label, phone, hours, sort_order FROM society_contacts WHERE society_id=$1 ORDER BY sort_order, lower(label)`, soc)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Contact{}
	for rows.Next() {
		var c Contact
		if err := rows.Scan(&c.ID, &c.Label, &c.Phone, &c.Hours, &c.Sort); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func clean(c Contact) (Contact, error) {
	c.Label, c.Hours = strings.TrimSpace(c.Label), strings.TrimSpace(c.Hours)
	var digits strings.Builder
	for _, r := range c.Phone {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	c.Phone = digits.String()
	if l := len([]rune(c.Label)); l < 2 || l > 60 || len(c.Phone) < 3 || len(c.Phone) > 15 || len([]rune(c.Hours)) > 30 {
		return c, ErrInvalid
	}
	return c, nil
}

func (s *Service) Create(ctx context.Context, p authctx.Principal, in Contact) (*Contact, error) {
	in, err := clean(in)
	if err != nil {
		return nil, err
	}
	var n int
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM society_contacts WHERE society_id=$1`, p.SocietyID).Scan(&n)
	if n >= 20 {
		return nil, ErrLimit
	}
	if err := s.pool.QueryRow(ctx, `INSERT INTO society_contacts (society_id, label, phone, hours, sort_order) VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		p.SocietyID, in.Label, in.Phone, in.Hours, n).Scan(&in.ID); err != nil {
		return nil, err
	}
	in.Sort = n
	s.audit.Log(ctx, audit.Entry{ActorID: &p.UserID, SocietyID: &p.SocietyID, Action: "contact.create", Entity: "contact", EntityID: in.ID.String()})
	return &in, nil
}

func (s *Service) Update(ctx context.Context, p authctx.Principal, id uuid.UUID, in Contact) (*Contact, error) {
	in, err := clean(in)
	if err != nil {
		return nil, err
	}
	err = s.pool.QueryRow(ctx, `UPDATE society_contacts SET label=$3, phone=$4, hours=$5 WHERE id=$1 AND society_id=$2 RETURNING id, sort_order`, id, p.SocietyID, in.Label, in.Phone, in.Hours).Scan(&in.ID, &in.Sort)
	if err != nil {
		return nil, ErrNotFound
	}
	s.audit.Log(ctx, audit.Entry{ActorID: &p.UserID, SocietyID: &p.SocietyID, Action: "contact.update", Entity: "contact", EntityID: id.String()})
	return &in, nil
}

func (s *Service) Delete(ctx context.Context, p authctx.Principal, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM society_contacts WHERE id=$1 AND society_id=$2`, id, p.SocietyID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	s.audit.Log(ctx, audit.Entry{ActorID: &p.UserID, SocietyID: &p.SocietyID, Action: "contact.delete", Entity: "contact", EntityID: id.String()})
	return nil
}

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func principal(r *http.Request) authctx.Principal { p, _ := authctx.From(r.Context()); return p }

type request struct {
	Label string `json:"label" validate:"required,min=2,max=60"`
	Phone string `json:"phone" validate:"required,max=30"`
	Hours string `json:"hours" validate:"max=30"`
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	c, err := h.svc.List(r.Context(), principal(r).SocietyID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, c, nil)
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var req request
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	c, err := h.svc.Create(r.Context(), principal(r), Contact{Label: req.Label, Phone: req.Phone, Hours: req.Hours})
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.Created(w, c)
}

func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, ErrNotFound)
		return
	}
	var req request
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	c, err := h.svc.Update(r.Context(), principal(r), id, Contact{Label: req.Label, Phone: req.Phone, Hours: req.Hours})
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, c, nil)
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, ErrNotFound)
		return
	}
	if err := h.svc.Delete(r.Context(), principal(r), id); err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.NoContent(w)
}
