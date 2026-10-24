package visitor

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Nexora48/nexora-api/internal/blob"
	"github.com/Nexora48/nexora-api/internal/platform/authctx"
)

const maxPhotoBytes = 2 << 20

const (
	ringMax      = 3
	ringInterval = 60 * time.Second
)

// sniffPhoto accepts JPEG and PNG only, judged by content, never by the client's header.
func sniffPhoto(b []byte) (ext, ct string, ok bool) {
	switch {
	case len(b) > 3 && bytes.HasPrefix(b, []byte{0xFF, 0xD8, 0xFF}):
		return "jpg", "image/jpeg", true
	case len(b) > 8 && bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return "png", "image/png", true
	}
	return "", "", false
}

// AttachPhoto stores the walk-in's photo. When the photo was required, this is the moment the
// resident is asked (event + push).
func (s *Service) AttachPhoto(ctx context.Context, p authctx.Principal, id uuid.UUID, data []byte) (*Visit, error) {
	if s.blobs == nil {
		return nil, ErrNoBlobs
	}
	if len(data) == 0 || len(data) > maxPhotoBytes {
		return nil, ErrBadPhoto
	}
	ext, ct, ok := sniffPhoto(data)
	if !ok {
		return nil, ErrBadPhoto
	}
	cur, err := s.one(ctx, p.SocietyID, id, nil)
	if err != nil {
		return nil, err
	}
	if cur.Source != "walk_in" {
		return nil, invalidState(cur.Status)
	}
	key := fmt.Sprintf("soc/%s/visits/%s.%s", p.SocietyID, id, ext)
	if err := s.blobs.Put(ctx, key, data, ct); err != nil {
		return nil, err
	}
	if _, err := s.pool.Exec(ctx, `UPDATE visits SET photo_key=$3, awaiting_photo=false, updated_at=now() WHERE id=$1 AND society_id=$2`, id, p.SocietyID, key); err != nil {
		return nil, err
	}
	s.logv(ctx, p, "visit.photo", id, nil)
	v, err := s.one(ctx, p.SocietyID, id, nil)
	if err == nil && cur.AwaitingPhoto && v.Status == StatusWaiting {
		s.emit(ctx, "visit.requested", v)
	}
	return v, err
}

// Photo returns the image to the flat's residents and to the society's guards and admins.
func (s *Service) Photo(ctx context.Context, p authctx.Principal, id uuid.UUID) ([]byte, string, error) {
	if s.blobs == nil {
		return nil, "", ErrNoBlobs
	}
	var key *string
	var unit uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT photo_key, unit_id FROM visits WHERE id=$1 AND society_id=$2`, id, p.SocietyID).Scan(&key, &unit)
	if err != nil {
		return nil, "", ErrNotFound
	}
	switch p.Role {
	case "guard", "society_admin", "committee":
	default:
		var mine *uuid.UUID
		if m, err := s.member(ctx, p); err == nil {
			mine = m.UnitID
		}
		if mine == nil || *mine != unit {
			return nil, "", ErrNotFound
		}
	}
	if key == nil {
		return nil, "", ErrNoPhoto
	}
	b, ct, err := s.blobs.Get(ctx, *key)
	if err == blob.ErrNotFound {
		return nil, "", ErrNoPhoto
	}
	return b, ct, err
}

// GateCancel lets a guard withdraw a request that is still waiting.
func (s *Service) GateCancel(ctx context.Context, p authctx.Principal, id uuid.UUID) (*Visit, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE visits SET status='cancelled', updated_at=now() WHERE id=$1 AND society_id=$2 AND source='walk_in' AND status='waiting'`, id, p.SocietyID)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, s.classify(ctx, p.SocietyID, id, nil)
	}
	s.logv(ctx, p, "visit.gate_cancel", id, nil)
	v, err := s.one(ctx, p.SocietyID, id, nil)
	s.emit(ctx, "visit.cancelled", v)
	return v, err
}

// Ring asks the residents again (at most 3 times, once a minute).
func (s *Service) Ring(ctx context.Context, p authctx.Principal, id uuid.UUID) (*Visit, error) {
	tag, err := s.pool.Exec(ctx, fmt.Sprintf(`
		UPDATE visits SET rerings=rerings+1, last_ring_at=now(), updated_at=now()
		WHERE id=$1 AND society_id=$2 AND source='walk_in' AND status='waiting' AND awaiting_photo=false
		  AND created_at >= now() - interval '%d seconds' AND rerings < %d
		  AND (last_ring_at IS NULL OR last_ring_at < now() - interval '%d seconds')`,
		int(s.walkinWait.Seconds()), ringMax, int(ringInterval.Seconds())), id, p.SocietyID)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		v, err := s.one(ctx, p.SocietyID, id, nil)
		if err != nil {
			return nil, err
		}
		if v.Status == StatusWaiting && !v.AwaitingPhoto {
			return nil, ErrRingLimit
		}
		return nil, invalidState(v.Status)
	}
	v, err := s.one(ctx, p.SocietyID, id, nil)
	s.emit(ctx, "visit.requested", v)
	return v, err
}
