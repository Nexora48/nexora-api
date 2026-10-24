package helper

import (
	"net/http"

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

func id(r *http.Request) (uuid.UUID, error) {
	v, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return uuid.Nil, ErrNotFound
	}
	return v, nil
}

type scheduleReq struct {
	Days        []int  `json:"days"`
	WindowFrom  string `json:"window_from"`
	WindowTo    string `json:"window_to"`
	NotifyEntry *bool  `json:"notify_entry"`
	NotifyExit  *bool  `json:"notify_exit"`
}

func (q scheduleReq) schedule() Schedule {
	return Schedule{Days: q.Days, WindowFrom: q.WindowFrom, WindowTo: q.WindowTo, NotifyEntry: q.NotifyEntry, NotifyExit: q.NotifyExit}
}

// with decodes nothing: it resolves {id} and runs fn, writing the result.
func with[T any](fn func(r *http.Request, id uuid.UUID) (T, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hid, err := id(r)
		if err != nil {
			httpx.Fail(w, err)
			return
		}
		v, err := fn(r, hid)
		if err != nil {
			httpx.Fail(w, err)
			return
		}
		httpx.OK(w, v, nil)
	}
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.Mine(r.Context(), principal(r))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, items, nil)
}

func (h *Handler) Get() http.HandlerFunc {
	return with(func(r *http.Request, id uuid.UUID) (*Helper, error) {
		return h.svc.MineByID(r.Context(), principal(r), id)
	})
}

type registerReq struct {
	Name    string `json:"full_name"`
	Phone   string `json:"phone"`
	Kind    string `json:"kind"`
	IDLast4 string `json:"id_last4"`
	scheduleReq
}

func (h *Handler) Register(w http.ResponseWriter, r *http.Request) {
	var req registerReq
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	v, err := h.svc.Register(r.Context(), principal(r), RegisterInput{Name: req.Name, Phone: req.Phone, Kind: req.Kind, IDLast4: req.IDLast4, Schedule: req.schedule()})
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.Created(w, v)
}

func (h *Handler) UpdateLink(w http.ResponseWriter, r *http.Request) {
	hid, err := id(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	var req scheduleReq
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	v, err := h.svc.UpdateLink(r.Context(), principal(r), hid, req.schedule())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

func (h *Handler) Unlink() http.HandlerFunc {
	return with(func(r *http.Request, id uuid.UUID) (map[string]bool, error) {
		return map[string]bool{"ok": true}, h.svc.Unlink(r.Context(), principal(r), id)
	})
}

func (h *Handler) Pass() http.HandlerFunc {
	return with(func(r *http.Request, id uuid.UUID) (*Pass, error) { return h.svc.Pass(r.Context(), principal(r), id) })
}

func (h *Handler) RegenerateCode() http.HandlerFunc {
	return with(func(r *http.Request, id uuid.UUID) (*Pass, error) {
		return h.svc.RegenerateCode(r.Context(), principal(r), id)
	})
}

func (h *Handler) Rate(w http.ResponseWriter, r *http.Request) {
	hid, err := id(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	var req struct {
		Rating  int    `json:"rating"`
		Comment string `json:"comment"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	v, err := h.svc.Rate(r.Context(), principal(r), hid, req.Rating, req.Comment)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, v, nil)
}

func (h *Handler) upload(office bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hid, err := id(r)
		if err != nil {
			httpx.Fail(w, err)
			return
		}
		name, data, err := h.files.ReadUpload(w, r)
		if err != nil {
			httpx.Fail(w, err)
			return
		}
		if err := h.svc.SetPhoto(r.Context(), principal(r), hid, office, name, data); err != nil {
			httpx.Fail(w, err)
			return
		}
		httpx.OK(w, map[string]bool{"ok": true}, nil)
	}
}

func (h *Handler) UploadPhoto() http.HandlerFunc      { return h.upload(false) }
func (h *Handler) AdminUploadPhoto() http.HandlerFunc { return h.upload(true) }

func (h *Handler) photo(office bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hid, err := id(r)
		if err != nil {
			httpx.Fail(w, err)
			return
		}
		b, f, err := h.svc.Photo(r.Context(), principal(r), hid, office)
		if err != nil {
			httpx.Fail(w, err)
			return
		}
		attachment.Serve(w, f, b)
	}
}

func (h *Handler) Photo() http.HandlerFunc      { return h.photo(false) }
func (h *Handler) AdminPhoto() http.HandlerFunc { return h.photo(true) }

// ── office ───────────────────────────────────────────────────────────────────

func (h *Handler) AdminList(w http.ResponseWriter, r *http.Request) {
	pg := pagination.FromRequest(r)
	q := r.URL.Query()
	f := Filter{Status: q.Get("status"), Kind: q.Get("kind"), Query: q.Get("q"), Limit: pg.PageSize, Offset: pg.Offset()}
	switch f.Status {
	case "", "pending", "verified", "suspended", "blacklisted":
	default:
		httpx.Fail(w, invalid("invalid status"))
		return
	}
	if f.Kind != "" && !kinds[f.Kind] {
		httpx.Fail(w, invalid("invalid kind"))
		return
	}
	items, total, err := h.svc.AdminList(r.Context(), principal(r), f)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, items, pg.Meta(total))
}

func (h *Handler) AdminGet() http.HandlerFunc {
	return with(func(r *http.Request, id uuid.UUID) (*Helper, error) {
		return h.svc.AdminGet(r.Context(), principal(r), id)
	})
}

func (h *Handler) Decide(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hid, err := id(r)
		if err != nil {
			httpx.Fail(w, err)
			return
		}
		var req struct {
			Reason string `json:"reason"`
		}
		if r.ContentLength != 0 {
			if err := httpx.Decode(w, r, &req); err != nil {
				httpx.Fail(w, err)
				return
			}
		}
		v, err := h.svc.Decide(r.Context(), principal(r), hid, action, req.Reason)
		if err != nil {
			httpx.Fail(w, err)
			return
		}
		httpx.OK(w, v, nil)
	}
}
