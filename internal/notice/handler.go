package notice

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Nexora48/nexora-api/internal/attachment"
	"github.com/Nexora48/nexora-api/internal/platform/authctx"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
	"github.com/Nexora48/nexora-api/internal/platform/pagination"
)

type Handler struct {
	svc   *Service
	files *attachment.Service
}

func NewHandler(svc *Service, files *attachment.Service) *Handler {
	return &Handler{svc: svc, files: files}
}

func principal(r *http.Request) authctx.Principal { p, _ := authctx.From(r.Context()); return p }

func id(r *http.Request, name string) (uuid.UUID, error) {
	v, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		return uuid.Nil, ErrNotFound
	}
	return v, nil
}

// ── residents / guards ───────────────────────────────────────────────────────

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	pg := pagination.FromRequest(r)
	items, total, err := h.svc.List(r.Context(), principal(r), r.URL.Query().Get("unread") == "1", pg.PageSize, pg.Offset())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if items == nil {
		items = []*Notice{}
	}
	httpx.OK(w, items, pg.Meta(total))
}

func (h *Handler) UnreadCount(w http.ResponseWriter, r *http.Request) {
	n, err := h.svc.UnreadCount(r.Context(), principal(r))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string]int{"unread": n}, nil)
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	nid, err := id(r, "id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	n, err := h.svc.Get(r.Context(), principal(r), nid)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, n, nil)
}

func (h *Handler) Attachment(w http.ResponseWriter, r *http.Request) {
	nid, err := id(r, "id")
	aid, err2 := id(r, "aid")
	if err != nil || err2 != nil {
		httpx.Fail(w, ErrNotFound)
		return
	}
	b, f, err := h.svc.Attachment(r.Context(), principal(r), nid, aid)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	attachment.Serve(w, f, b)
}

// ── office ───────────────────────────────────────────────────────────────────

type noticeRequest struct {
	Title     string      `json:"title" validate:"required,max=200"`
	Body      string      `json:"body" validate:"required,max=6000"`
	Category  string      `json:"category" validate:"required"`
	Priority  string      `json:"priority" validate:"omitempty"`
	Pinned    bool        `json:"pinned"`
	AllBlocks *bool       `json:"all_blocks"`
	BlockIDs  []uuid.UUID `json:"block_ids" validate:"max=50"`
	Status    string      `json:"status" validate:"omitempty"`
	PublishAt *time.Time  `json:"publish_at"`
	ExpiresAt *time.Time  `json:"expires_at"`
}

func (q noticeRequest) input() Input {
	all := true
	if q.AllBlocks != nil {
		all = *q.AllBlocks
	}
	return Input{Title: q.Title, Body: q.Body, Category: q.Category, Priority: q.Priority, Pinned: q.Pinned, AllBlocks: all, BlockIDs: q.BlockIDs,
		Status: q.Status, PublishAt: q.PublishAt, ExpiresAt: q.ExpiresAt}
}

func (h *Handler) AdminList(w http.ResponseWriter, r *http.Request) {
	pg := pagination.FromRequest(r)
	status := r.URL.Query().Get("status")
	switch status {
	case "", "draft", "scheduled", "published", "archived":
	default:
		httpx.Fail(w, errValidName("invalid status"))
		return
	}
	items, total, err := h.svc.AdminList(r.Context(), principal(r), status, pg.PageSize, pg.Offset())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if items == nil {
		items = []*Notice{}
	}
	httpx.OK(w, items, pg.Meta(total))
}

func (h *Handler) AdminGet(w http.ResponseWriter, r *http.Request) {
	nid, err := id(r, "id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	n, err := h.svc.AdminGet(r.Context(), principal(r), nid)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, n, nil)
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var req noticeRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	n, err := h.svc.Create(r.Context(), principal(r), req.input())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.Created(w, n)
}

func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	nid, err := id(r, "id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	var req noticeRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	n, err := h.svc.Update(r.Context(), principal(r), nid, req.input())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, n, nil)
}

func (h *Handler) Publish(w http.ResponseWriter, r *http.Request) {
	nid, err := id(r, "id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	n, err := h.svc.Publish(r.Context(), principal(r), nid)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, n, nil)
}

func (h *Handler) Archive(w http.ResponseWriter, r *http.Request) {
	nid, err := id(r, "id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if err := h.svc.Archive(r.Context(), principal(r), nid); err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.NoContent(w)
}

func (h *Handler) Upload(w http.ResponseWriter, r *http.Request) {
	nid, err := id(r, "id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	name, data, err := h.files.ReadUpload(w, r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	f, err := h.svc.AddAttachment(r.Context(), principal(r), nid, name, data)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.Created(w, f)
}

func (h *Handler) RemoveFile(w http.ResponseWriter, r *http.Request) {
	nid, err := id(r, "id")
	aid, err2 := id(r, "aid")
	if err != nil || err2 != nil {
		httpx.Fail(w, ErrNotFound)
		return
	}
	if err := h.svc.RemoveAttachment(r.Context(), principal(r), nid, aid); err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.NoContent(w)
}

func (h *Handler) AdminFile(w http.ResponseWriter, r *http.Request) {
	nid, err := id(r, "id")
	aid, err2 := id(r, "aid")
	if err != nil || err2 != nil {
		httpx.Fail(w, ErrNotFound)
		return
	}
	b, f, err := h.svc.AdminAttachment(r.Context(), principal(r), nid, aid)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	attachment.Serve(w, f, b)
}

func (h *Handler) Stats(w http.ResponseWriter, r *http.Request) {
	nid, err := id(r, "id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	st, err := h.svc.Stats(r.Context(), principal(r), nid)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, st, nil)
}
