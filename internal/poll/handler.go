package poll

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func principal(r *http.Request) authctx.Principal { p, _ := authctx.From(r.Context()); return p }

func pid(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return uuid.Nil, ErrNotFound
	}
	return id, nil
}

func (h *Handler) one(fn func(r *http.Request, id uuid.UUID) (*Poll, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := pid(r)
		if err != nil {
			httpx.Fail(w, err)
			return
		}
		p, err := fn(r, id)
		if err != nil {
			httpx.Fail(w, err)
			return
		}
		httpx.OK(w, p, nil)
	}
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	l, err := h.svc.List(r.Context(), principal(r))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if l == nil {
		l = []*Poll{}
	}
	httpx.OK(w, l, nil)
}

func (h *Handler) Get() http.HandlerFunc {
	return h.one(func(r *http.Request, id uuid.UUID) (*Poll, error) { return h.svc.Get(r.Context(), principal(r), id) })
}

type voteRequest struct {
	OptionIDs []uuid.UUID `json:"option_ids" validate:"required,min=1,max=8"`
}

func (h *Handler) Vote(w http.ResponseWriter, r *http.Request) {
	var req voteRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	h.one(func(r *http.Request, id uuid.UUID) (*Poll, error) {
		return h.svc.Vote(r.Context(), principal(r), id, req.OptionIDs)
	})(w, r)
}

type pollRequest struct {
	Question  string      `json:"question" validate:"required,max=400"`
	Options   []string    `json:"options" validate:"required,max=20,dive,max=200"`
	Multi     bool        `json:"multi"`
	Anonymous *bool       `json:"anonymous"`
	AllBlocks *bool       `json:"all_blocks"`
	BlockIDs  []uuid.UUID `json:"block_ids" validate:"max=50"`
	ClosesAt  *time.Time  `json:"closes_at"`
}

func (q pollRequest) input() Input {
	anon, all := true, true
	if q.Anonymous != nil {
		anon = *q.Anonymous
	}
	if q.AllBlocks != nil {
		all = *q.AllBlocks
	}
	return Input{Question: q.Question, Options: q.Options, Multi: q.Multi, Anonymous: anon, AllBlocks: all, BlockIDs: q.BlockIDs, ClosesAt: q.ClosesAt}
}

func (h *Handler) AdminList(w http.ResponseWriter, r *http.Request) {
	l, err := h.svc.AdminList(r.Context(), principal(r))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if l == nil {
		l = []*Poll{}
	}
	httpx.OK(w, l, nil)
}

func (h *Handler) AdminGet() http.HandlerFunc {
	return h.one(func(r *http.Request, id uuid.UUID) (*Poll, error) {
		return h.svc.AdminGet(r.Context(), principal(r), id)
	})
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var req pollRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	p, err := h.svc.Create(r.Context(), principal(r), req.input())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.Created(w, p)
}

func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	var req pollRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	h.one(func(r *http.Request, id uuid.UUID) (*Poll, error) {
		return h.svc.Update(r.Context(), principal(r), id, req.input())
	})(w, r)
}

func (h *Handler) Close() http.HandlerFunc {
	return h.one(func(r *http.Request, id uuid.UUID) (*Poll, error) { return h.svc.Close(r.Context(), principal(r), id) })
}

func (h *Handler) Archive(w http.ResponseWriter, r *http.Request) {
	id, err := pid(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if err := h.svc.Archive(r.Context(), principal(r), id); err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.NoContent(w)
}

func (h *Handler) Export(w http.ResponseWriter, r *http.Request) {
	id, err := pid(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="poll-results.csv"`)
	if err := h.svc.Export(r.Context(), principal(r), id, w); err != nil {
		httpx.Fail(w, err)
	}
}
