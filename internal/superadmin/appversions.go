package superadmin

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Nexora48/nexora-api/internal/platform/httpx"
)

// App versions: the minimum and latest build of each app. The public /app-config answers
// "none | available | required" so an old app can show a nudge or a blocking screen.

type AppVersion struct {
	App           string    `json:"app"`
	MinVersion    string    `json:"min_version"`
	LatestVersion string    `json:"latest_version"`
	Message       string    `json:"message"`
	StoreURL      string    `json:"store_url"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type AppConfig struct {
	Update   string `json:"update"` // none | available | required
	Latest   string `json:"latest"`
	Message  string `json:"message"`
	StoreURL string `json:"store_url"`
}

var versionRe = regexp.MustCompile(`^\d{1,4}(\.\d{1,4}){0,2}([+-][0-9A-Za-z.\-+]*)?$`)

func validApp(a string) bool { return a == "living" || a == "admin" || a == "get" }

// CompareVersions compares dotted numbers ("1.2.3"); build suffixes (+7, -rc1) are ignored. It returns -1, 0 or 1.
func CompareVersions(a, b string) int {
	pa, pb := versionParts(a), versionParts(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func versionParts(v string) [3]int {
	if i := strings.IndexAny(v, "+-"); i >= 0 {
		v = v[:i]
	}
	var out [3]int
	for i, p := range strings.Split(strings.TrimSpace(v), ".") {
		if i > 2 {
			break
		}
		out[i], _ = strconv.Atoi(p)
	}
	return out
}

func (s *Service) ListAppVersions(ctx context.Context) ([]AppVersion, error) {
	rows, err := s.store.pool.Query(ctx, `SELECT app, min_version, latest_version, message, store_url, updated_at FROM app_versions ORDER BY app`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AppVersion{}
	for rows.Next() {
		var a AppVersion
		if err := rows.Scan(&a.App, &a.MinVersion, &a.LatestVersion, &a.Message, &a.StoreURL, &a.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Service) SetAppVersion(ctx context.Context, actor uuid.UUID, in AppVersion) (*AppVersion, error) {
	in.Message, in.StoreURL = strings.TrimSpace(in.Message), strings.TrimSpace(in.StoreURL)
	if !validApp(in.App) {
		return nil, httpx.NewError(http.StatusBadRequest, "VALIDATION", "app must be living, admin or get")
	}
	if !versionRe.MatchString(in.MinVersion) || !versionRe.MatchString(in.LatestVersion) {
		return nil, httpx.NewError(http.StatusBadRequest, "VALIDATION", "versions look like 1.4.0")
	}
	if CompareVersions(in.MinVersion, in.LatestVersion) > 0 {
		return nil, httpx.NewError(http.StatusBadRequest, "VALIDATION", "the minimum version cannot be above the latest version")
	}
	if len(in.Message) > 300 || (in.StoreURL != "" && !strings.HasPrefix(in.StoreURL, "https://")) || len(in.StoreURL) > 300 {
		return nil, httpx.NewError(http.StatusBadRequest, "VALIDATION", "message up to 300 characters; store link must start with https://")
	}
	var before AppVersion
	_ = s.store.pool.QueryRow(ctx, `SELECT app, min_version, latest_version, message, store_url, updated_at FROM app_versions WHERE app=$1`, in.App).
		Scan(&before.App, &before.MinVersion, &before.LatestVersion, &before.Message, &before.StoreURL, &before.UpdatedAt)
	if _, err := s.store.pool.Exec(ctx, `UPDATE app_versions SET min_version=$2, latest_version=$3, message=$4, store_url=$5, updated_at=now() WHERE app=$1`,
		in.App, in.MinVersion, in.LatestVersion, in.Message, in.StoreURL); err != nil {
		return nil, err
	}
	s.platformLog(ctx, actor, nil, "app_version.set", "app", in.App, map[string]any{
		"before": map[string]any{"min": before.MinVersion, "latest": before.LatestVersion},
		"after":  map[string]any{"min": in.MinVersion, "latest": in.LatestVersion}, "message": in.Message})
	var out AppVersion
	err := s.store.pool.QueryRow(ctx, `SELECT app, min_version, latest_version, message, store_url, updated_at FROM app_versions WHERE app=$1`, in.App).
		Scan(&out.App, &out.MinVersion, &out.LatestVersion, &out.Message, &out.StoreURL, &out.UpdatedAt)
	return &out, err
}

// AppConfigFor answers the public check. Unknown apps and unreadable versions never block anyone.
func AppConfigFor(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, app, version string) AppConfig {
	none := AppConfig{Update: "none"}
	if !validApp(app) || !versionRe.MatchString(version) {
		return none
	}
	var a AppVersion
	err := q.QueryRow(ctx, `SELECT min_version, latest_version, message, store_url FROM app_versions WHERE app=$1`, app).Scan(&a.MinVersion, &a.LatestVersion, &a.Message, &a.StoreURL)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return none
		}
		return none
	}
	out := AppConfig{Update: "none", Latest: a.LatestVersion, Message: a.Message, StoreURL: a.StoreURL}
	switch {
	case CompareVersions(version, a.MinVersion) < 0:
		out.Update = "required"
	case CompareVersions(version, a.LatestVersion) < 0:
		out.Update = "available"
	}
	return out
}
