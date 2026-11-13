package storage

import (
	"errors"
	"strings"
	"testing"
)

func TestKeysAndTraversal(t *testing.T) {
	l, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key, err := l.NewKey(7, ".MP4")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, "u7/") || !strings.HasSuffix(key, ".mp4") {
		t.Fatalf("key format: %s", key)
	}
	f, err := l.Create(key)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := l.Create(key); err == nil {
		t.Fatal("create must not overwrite an existing file")
	}
	if err := l.Remove(key); err != nil {
		t.Fatal(err)
	}
	if err := l.Remove(key); err != nil {
		t.Fatal("removing a missing file should not error")
	}

	for _, bad := range []string{"../etc/passwd", "u1/../../x", "u1/abc.mp4", "/abs/path", `u1\..\x`, "u1/" + strings.Repeat("a", 32) + "/x"} {
		if _, err := l.Path(bad); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Path(%q) should be rejected", bad)
		}
	}
	if _, err := l.NewKey(1, ".exe/../x"); err == nil {
		t.Error("extension with path separators must be rejected")
	}
}
