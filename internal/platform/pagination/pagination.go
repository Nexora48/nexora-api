package pagination

import (
	"net/http"
	"strconv"

	"github.com/Nexora48/nexora-api/internal/platform/httpx"
)

const (
	defaultSize = 20
	maxSize     = 100
)

type Params struct{ Page, PageSize int }

func (p Params) Offset() int { return (p.Page - 1) * p.PageSize }

// FromRequest reads ?page= and ?page_size= with safe defaults.
func FromRequest(r *http.Request) Params {
	p := Params{Page: 1, PageSize: defaultSize}
	if v, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && v > 0 {
		p.Page = v
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("page_size")); err == nil && v > 0 {
		p.PageSize = min(v, maxSize)
	}
	return p
}

func (p Params) Meta(total int) *httpx.Meta {
	return &httpx.Meta{Page: p.Page, PageSize: p.PageSize, Total: total}
}
