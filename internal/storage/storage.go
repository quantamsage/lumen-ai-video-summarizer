// Package storage keeps uploaded media on the local filesystem.
//
// Files are addressed by server-generated keys of the form
// "u<user id>/<random>.<ext>". Keys never contain user-supplied names and
// are validated before use, which prevents path traversal.
package storage

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var keyPattern = regexp.MustCompile(`^u[0-9]+/[a-f0-9]{32}(\.[a-z0-9]{1,5})?$`)

// ErrInvalidKey is returned for keys that do not match the expected format.
var ErrInvalidKey = errors.New("invalid storage key")

// Local stores files under Root.
type Local struct {
	Root string
}

func NewLocal(root string) (*Local, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(abs, "tmp"), 0o750); err != nil {
		return nil, fmt.Errorf("create upload directory: %w", err)
	}
	return &Local{Root: abs}, nil
}

// NewKey generates a fresh random key for a user's file with extension ext
// (including the dot, e.g. ".mp4").
func (l *Local) NewKey(userID int64, ext string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	ext = strings.ToLower(ext)
	key := fmt.Sprintf("u%d/%s%s", userID, hex.EncodeToString(b), ext)
	if !keyPattern.MatchString(key) {
		return "", ErrInvalidKey
	}
	return key, nil
}

// Path resolves a key to an absolute path inside Root.
func (l *Local) Path(key string) (string, error) {
	if !keyPattern.MatchString(key) {
		return "", ErrInvalidKey
	}
	p := filepath.Join(l.Root, filepath.FromSlash(key))
	rel, err := filepath.Rel(l.Root, p)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", ErrInvalidKey
	}
	return p, nil
}

// Create opens a new file for key, creating parent directories.
func (l *Local) Create(key string) (*os.File, error) {
	p, err := l.Path(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return nil, err
	}
	return os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
}

// Remove deletes the file for key. Missing files are not an error.
func (l *Local) Remove(key string) error {
	if key == "" {
		return nil
	}
	p, err := l.Path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// TempDir creates a scratch directory for processing work.
func (l *Local) TempDir(prefix string) (string, error) {
	return os.MkdirTemp(filepath.Join(l.Root, "tmp"), prefix)
}

// CleanTemp removes leftover scratch directories, e.g. after a crash.
func (l *Local) CleanTemp() error {
	dir := filepath.Join(l.Root, "tmp")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
