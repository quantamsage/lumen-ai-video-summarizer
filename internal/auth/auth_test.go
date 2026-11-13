package auth

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/quantamsage/ai-video-summarizer/internal/db"
	"github.com/quantamsage/ai-video-summarizer/internal/store"
)

func newService(t *testing.T) *Service {
	t.Helper()
	conn, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := db.Migrate(context.Background(), conn, nil); err != nil {
		t.Fatal(err)
	}
	return NewService(store.New(conn), time.Hour)
}

func TestRegisterValidation(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	cases := []struct {
		name, email, display, pw, confirm, field string
	}{
		{"bad email", "nope", "A", "longpassword1", "longpassword1", "email"},
		{"short password", "a@example.com", "A", "short", "short", "password"},
		{"mismatch", "a@example.com", "A", "longpassword1", "longpassword2", "confirm"},
		{"missing name", "a@example.com", "  ", "longpassword1", "longpassword1", "name"},
		{"display-name address", "A <a@example.com>", "A", "longpassword1", "longpassword1", "email"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := s.Register(ctx, c.email, c.display, c.pw, c.confirm)
			var verr ValidationError
			if !errors.As(err, &verr) || verr[c.field] == "" {
				t.Fatalf("expected validation error on %s, got %v", c.field, err)
			}
		})
	}
}

func TestRegisterLoginSessionFlow(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	u, err := s.Register(ctx, " Alice@Example.com ", "Alice", "correct horse", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if u.Email != "alice@example.com" {
		t.Fatalf("email not normalized: %q", u.Email)
	}
	if u.PasswordHash == "correct horse" {
		t.Fatal("password stored in plaintext")
	}
	if _, err := s.Register(ctx, "alice@example.com", "A2", "correct horse", "correct horse"); err == nil {
		t.Fatal("duplicate registration should fail")
	}
	if _, err := s.Authenticate(ctx, "alice@example.com", "wrong password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected invalid credentials, got %v", err)
	}
	if _, err := s.Authenticate(ctx, "nobody@example.com", "whatever123"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected invalid credentials, got %v", err)
	}
	if _, err := s.Authenticate(ctx, "ALICE@example.com", "correct horse"); err != nil {
		t.Fatalf("login failed: %v", err)
	}

	tok, _, err := s.CreateSession(ctx, u.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := s.SessionUser(ctx, tok)
	if err != nil || got.ID != u.ID {
		t.Fatalf("session user: %v %v", got, err)
	}
	if err := s.Logout(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SessionUser(ctx, tok); err == nil {
		t.Fatal("session should be invalid after logout")
	}
}

func TestPasswordResetFlow(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	u, err := s.Register(ctx, "a@example.com", "A", "original pass", "original pass")
	if err != nil {
		t.Fatal(err)
	}
	sessTok, _, _ := s.CreateSession(ctx, u.ID, "")

	tok, _, err := s.CreatePasswordReset(ctx, "missing@example.com")
	if err != nil || tok != "" {
		t.Fatalf("unknown email should produce no token and no error: %q %v", tok, err)
	}
	tok, _, err = s.CreatePasswordReset(ctx, "a@example.com")
	if err != nil || tok == "" {
		t.Fatalf("reset token: %v", err)
	}
	if err := s.ResetPassword(ctx, tok, "brand new pass", "brand new pass"); err != nil {
		t.Fatal(err)
	}
	if err := s.ResetPassword(ctx, tok, "another pass1", "another pass1"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("token reuse should fail, got %v", err)
	}
	if _, err := s.Authenticate(ctx, "a@example.com", "brand new pass"); err != nil {
		t.Fatalf("login with new password: %v", err)
	}
	if _, _, err := s.SessionUser(ctx, sessTok); err == nil {
		t.Fatal("existing sessions must be revoked after a password reset")
	}
}
