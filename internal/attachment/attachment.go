// Package attachment stores the files of notices and complaints: JPEG, PNG and PDF, content-sniffed,
// size- and count-limited, and served only through authorised routes of their owner.
package attachment

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nexora48/nexora-api/internal/blob"
	"github.com/Nexora48/nexora-api/internal/platform/httpx"
)

const (
	OwnerNotice         = "notice"
	OwnerComplaint      = "complaint"
	OwnerComplaintEvent = "complaint_event"
	OwnerAmenity        = "amenity"
	OwnerHelper         = "helper"
	OwnerStaff          = "staff"
)

var (
	ErrBadFile  = httpx.NewError(http.StatusBadRequest, "BAD_FILE", "send a JPEG, PNG or PDF file up to the size limit")
	ErrTooMany  = httpx.NewError(http.StatusConflict, "TOO_MANY_FILES", "this item already has the maximum number of files")
	ErrNoBlobs  = httpx.NewError(http.StatusServiceUnavailable, "FILES_UNAVAILABLE", "file storage is not configured")
	ErrNotFound = httpx.NewError(http.StatusNotFound, "NOT_FOUND", "file not found")
)

type Service struct {
	pool    *pgxpool.Pool
	blobs   blob.Store
	maxSize int
}

func NewService(pool *pgxpool.Pool, blobs blob.Store, maxMB int) *Service {
	if maxMB <= 0 {
		maxMB = 5
	}
	return &Service{pool: pool, blobs: blobs, maxSize: maxMB << 20}
}

// MaxBytes is the upload limit (handlers use it to cap request bodies).
func (s *Service) MaxBytes() int { return s.maxSize }

// File is the public shape of an attachment.
type File struct {
	ID          uuid.UUID `json:"id"`
	Filename    string    `json:"filename"`
	ContentType string    `json:"content_type"`
	Size        int       `json:"size"`
}

func sniff(b []byte) (ext, ct string, ok bool) {
	switch {
	case len(b) > 3 && bytes.HasPrefix(b, []byte{0xFF, 0xD8, 0xFF}):
		return "jpg", "image/jpeg", true
	case len(b) > 8 && bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return "png", "image/png", true
	case len(b) > 5 && bytes.HasPrefix(b, []byte("%PDF-")):
		return "pdf", "application/pdf", true
	}
	return "", "", false
}

func cleanName(n, ext string) string {
	n = strings.TrimSpace(path.Base(strings.ReplaceAll(n, "\\", "/")))
	if n == "" || n == "." || n == "/" {
		return "file." + ext
	}
	r := []rune(n)
	if len(r) > 80 {
		n = string(r[:80])
	}
	return n
}

// Save validates and stores one file for an owner; max limits how many files the owner may hold.
func (s *Service) Save(ctx context.Context, society uuid.UUID, ownerType string, ownerID, by uuid.UUID, filename string, data []byte, max int) (*File, error) {
	if s.blobs == nil {
		return nil, ErrNoBlobs
	}
	if len(data) == 0 || len(data) > s.maxSize {
		return nil, ErrBadFile
	}
	ext, ct, ok := sniff(data)
	if !ok {
		return nil, ErrBadFile
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM attachments WHERE owner_type=$1 AND owner_id=$2 AND society_id=$3`, ownerType, ownerID, society).Scan(&n); err != nil {
		return nil, err
	}
	if n >= max {
		return nil, ErrTooMany
	}
	id := uuid.New()
	key := fmt.Sprintf("soc/%s/%s/%s/%s.%s", society, ownerType, ownerID, id, ext)
	if err := s.blobs.Put(ctx, key, data, ct); err != nil {
		return nil, err
	}
	name := cleanName(filename, ext)
	if _, err := s.pool.Exec(ctx, `INSERT INTO attachments (id, society_id, owner_type, owner_id, blob_key, content_type, size_bytes, filename, created_by) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		id, society, ownerType, ownerID, key, ct, len(data), name, by); err != nil {
		_ = s.blobs.Delete(ctx, key)
		return nil, err
	}
	return &File{ID: id, Filename: name, ContentType: ct, Size: len(data)}, nil
}

// List returns the files of one owner.
func (s *Service) List(ctx context.Context, society uuid.UUID, ownerType string, ownerID uuid.UUID) ([]File, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, filename, content_type, size_bytes FROM attachments WHERE society_id=$1 AND owner_type=$2 AND owner_id=$3 ORDER BY created_at, id`, society, ownerType, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []File{}
	for rows.Next() {
		var f File
		if err := rows.Scan(&f.ID, &f.Filename, &f.ContentType, &f.Size); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ListMany groups the files of several owners (avoids one query per item).
func (s *Service) ListMany(ctx context.Context, society uuid.UUID, ownerType string, ids []uuid.UUID) (map[uuid.UUID][]File, error) {
	out := map[uuid.UUID][]File{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT owner_id, id, filename, content_type, size_bytes FROM attachments WHERE society_id=$1 AND owner_type=$2 AND owner_id = ANY($3) ORDER BY created_at, id`, society, ownerType, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var owner uuid.UUID
		var f File
		if err := rows.Scan(&owner, &f.ID, &f.Filename, &f.ContentType, &f.Size); err != nil {
			return nil, err
		}
		out[owner] = append(out[owner], f)
	}
	return out, rows.Err()
}

// Open returns the bytes of a file that belongs to the given owner (the caller has already authorised the owner).
func (s *Service) Open(ctx context.Context, society uuid.UUID, ownerType string, ownerID, id uuid.UUID) ([]byte, *File, error) {
	if s.blobs == nil {
		return nil, nil, ErrNoBlobs
	}
	var key string
	var f File
	err := s.pool.QueryRow(ctx, `SELECT blob_key, id, filename, content_type, size_bytes FROM attachments WHERE id=$1 AND society_id=$2 AND owner_type=$3 AND owner_id=$4`, id, society, ownerType, ownerID).
		Scan(&key, &f.ID, &f.Filename, &f.ContentType, &f.Size)
	if err != nil {
		return nil, nil, ErrNotFound
	}
	b, _, err := s.blobs.Get(ctx, key)
	if errors.Is(err, blob.ErrNotFound) {
		return nil, nil, ErrNotFound
	}
	return b, &f, err
}

// ReadUpload reads the multipart field `file`, bounded by the size limit.
func (s *Service) ReadUpload(w http.ResponseWriter, r *http.Request) (string, []byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, int64(s.maxSize)+64<<10)
	f, hdr, err := r.FormFile("file")
	if err != nil {
		return "", nil, ErrBadFile
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, int64(s.maxSize)+1))
	if err != nil || len(b) > s.maxSize {
		return "", nil, ErrBadFile
	}
	return hdr.Filename, b, nil
}

// Serve writes a file with safe headers (never sniffed, downloads for PDFs).
func Serve(w http.ResponseWriter, f *File, data []byte) {
	w.Header().Set("Content-Type", f.ContentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	disp := "inline"
	if f.ContentType == "application/pdf" {
		disp = "attachment"
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`%s; filename=%q`, disp, f.Filename))
	_, _ = w.Write(data)
}
