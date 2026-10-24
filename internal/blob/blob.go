// Package blob stores uploaded files (visitor photos) behind a small interface.
// Reads always go through authenticated API endpoints; keys are never public URLs.
package blob

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var ErrNotFound = errors.New("blob: not found")

type Store interface {
	Put(ctx context.Context, key string, data []byte, contentType string) error
	Get(ctx context.Context, key string) ([]byte, string, error)
	Delete(ctx context.Context, key string) error
}

// Disk keeps files under a root directory (dev and single-node deployments).
type Disk struct{ root string }

func NewDisk(root string) (*Disk, error) {
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, err
	}
	return &Disk{root: root}, nil
}

func (d *Disk) path(key string) (string, error) {
	clean := filepath.Clean("/" + key)
	if strings.Contains(key, "..") || clean == "/" {
		return "", errors.New("blob: invalid key")
	}
	return filepath.Join(d.root, clean), nil
}

func (d *Disk) Put(_ context.Context, key string, data []byte, _ string) error {
	p, err := d.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o640)
}

func (d *Disk) Get(_ context.Context, key string) ([]byte, string, error) {
	p, err := d.path(key)
	if err != nil {
		return nil, "", err
	}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, "", err
	}
	ct := "image/jpeg"
	if strings.HasSuffix(key, ".png") {
		ct = "image/png"
	}
	return b, ct, nil
}

func (d *Disk) Delete(_ context.Context, key string) error {
	p, err := d.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
