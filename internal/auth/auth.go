// Package auth implements account registration, login, sessions and
// password resets.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"github.com/quantamsage/ai-video-summarizer/internal/model"
	"github.com/quantamsage/ai-video-summarizer/internal/store"
)

const (
	bcryptCost       = 12
	minPasswordLen   = 10
	maxPasswordBytes = 72 // bcrypt limit
	resetTokenTTL    = time.Hour
)

var (
	ErrInvalidCredentials = errors.New("invalid email or password")
	ErrEmailTaken         = errors.New("an account with this email already exists")
	ErrInvalidToken       = errors.New("this link is invalid or has expired")
)

// ValidationError describes invalid user input, keyed by form field.
type ValidationError map[string]string

func (v ValidationError) Error() string {
	parts := make([]string, 0, len(v))
	for k, msg := range v {
		parts = append(parts, k+": "+msg)
	}
	return strings.Join(parts, "; ")
}

// Service provides authentication operations.
type Service struct {
	store      *store.Store
	sessionTTL time.Duration
	dummyHash  []byte
}

func NewService(s *store.Store, sessionTTL time.Duration) *Service {
	// A real hash used to equalize timing when the email does not exist.
	dummy, _ := bcrypt.GenerateFromPassword([]byte("dummy-password-for-timing"), bcryptCost)
	return &Service{store: s, sessionTTL: sessionTTL, dummyHash: dummy}
}

// NormalizeEmail trims and lowercases an email address.
func NormalizeEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

// ValidateEmail checks that email is a plain address.
func ValidateEmail(email string) error {
	if len(email) > 254 {
		return errors.New("email is too long")
	}
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email || !strings.Contains(email, ".") {
		return errors.New("enter a valid email address")
	}
	return nil
}

// ValidatePassword enforces password length rules.
func ValidatePassword(pw string) error {
	if utf8.RuneCountInString(pw) < minPasswordLen {
		return fmt.Errorf("password must be at least %d characters", minPasswordLen)
	}
	if len(pw) > maxPasswordBytes {
		return fmt.Errorf("password must be at most %d bytes", maxPasswordBytes)
	}
	return nil
}

// ValidateDisplayName checks a display name.
func ValidateDisplayName(name string) error {
	n := utf8.RuneCountInString(name)
	if n == 0 {
		return errors.New("name is required")
	}
	if n > 80 {
		return errors.New("name must be at most 80 characters")
	}
	return nil
}

func HashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcryptCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

func checkPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// Register creates a new account.
func (s *Service) Register(ctx context.Context, email, name, password, confirm string) (*model.User, error) {
	email = NormalizeEmail(email)
	name = strings.TrimSpace(name)
	verr := ValidationError{}
	if err := ValidateEmail(email); err != nil {
		verr["email"] = err.Error()
	}
	if err := ValidateDisplayName(name); err != nil {
		verr["name"] = err.Error()
	}
	if err := ValidatePassword(password); err != nil {
		verr["password"] = err.Error()
	} else if password != confirm {
		verr["confirm"] = "passwords do not match"
	}
	if len(verr) > 0 {
		return nil, verr
	}
	hash, err := HashPassword(password)
	if err != nil {
		return nil, err
	}
	u, err := s.store.CreateUser(ctx, email, name, hash)
	if errors.Is(err, store.ErrConflict) {
		return nil, ValidationError{"email": ErrEmailTaken.Error()}
	}
	return u, err
}

// Authenticate verifies credentials.
func (s *Service) Authenticate(ctx context.Context, email, password string) (*model.User, error) {
	u, err := s.store.UserByEmail(ctx, NormalizeEmail(email))
	if errors.Is(err, store.ErrNotFound) {
		bcrypt.CompareHashAndPassword(s.dummyHash, []byte(password))
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, err
	}
	if len(password) > maxPasswordBytes || !checkPassword(u.PasswordHash, password) {
		return nil, ErrInvalidCredentials
	}
	return u, nil
}

// CreateSession issues a new session token for a user. Only the token's
// hash is stored.
func (s *Service) CreateSession(ctx context.Context, userID int64, userAgent string) (token string, expires time.Time, err error) {
	token, err = RandomToken()
	if err != nil {
		return "", time.Time{}, err
	}
	expires = time.Now().Add(s.sessionTTL)
	if len(userAgent) > 255 {
		userAgent = userAgent[:255]
	}
	if err := s.store.CreateSession(ctx, HashToken(token), userID, expires, userAgent); err != nil {
		return "", time.Time{}, err
	}
	return token, expires, nil
}

// SessionUser resolves a session token to its user, extending the session
// when more than an hour has passed since it was last extended.
func (s *Service) SessionUser(ctx context.Context, token string) (*model.User, time.Time, error) {
	if token == "" {
		return nil, time.Time{}, store.ErrNotFound
	}
	h := HashToken(token)
	u, expires, err := s.store.SessionUser(ctx, h)
	if err != nil {
		return nil, time.Time{}, err
	}
	newExpiry := time.Now().Add(s.sessionTTL)
	if newExpiry.Sub(expires) > time.Hour {
		if err := s.store.TouchSession(ctx, h, newExpiry); err != nil {
			return nil, time.Time{}, err
		}
		expires = newExpiry
	}
	return u, expires, nil
}

func (s *Service) Logout(ctx context.Context, token string) error {
	return s.store.DeleteSession(ctx, HashToken(token))
}

// ChangePassword updates a password after verifying the current one, and
// signs out all other sessions.
func (s *Service) ChangePassword(ctx context.Context, u *model.User, currentToken, current, next, confirm string) error {
	if !checkPassword(u.PasswordHash, current) {
		return ValidationError{"current_password": "current password is incorrect"}
	}
	if err := ValidatePassword(next); err != nil {
		return ValidationError{"new_password": err.Error()}
	}
	if next != confirm {
		return ValidationError{"confirm": "passwords do not match"}
	}
	hash, err := HashPassword(next)
	if err != nil {
		return err
	}
	if err := s.store.UpdateUserPassword(ctx, u.ID, hash); err != nil {
		return err
	}
	return s.store.DeleteUserSessions(ctx, u.ID, HashToken(currentToken))
}

// UpdateProfile changes a user's display name and email.
func (s *Service) UpdateProfile(ctx context.Context, u *model.User, email, name string) error {
	email = NormalizeEmail(email)
	name = strings.TrimSpace(name)
	verr := ValidationError{}
	if err := ValidateEmail(email); err != nil {
		verr["email"] = err.Error()
	}
	if err := ValidateDisplayName(name); err != nil {
		verr["name"] = err.Error()
	}
	if len(verr) > 0 {
		return verr
	}
	err := s.store.UpdateUserProfile(ctx, u.ID, email, name)
	if errors.Is(err, store.ErrConflict) {
		return ValidationError{"email": ErrEmailTaken.Error()}
	}
	return err
}

// CreatePasswordReset issues a one-time reset token for the account with
// the given email. It returns ("", nil, nil) when no such account exists so
// callers can respond identically either way.
func (s *Service) CreatePasswordReset(ctx context.Context, email string) (string, *model.User, error) {
	u, err := s.store.UserByEmail(ctx, NormalizeEmail(email))
	if errors.Is(err, store.ErrNotFound) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	token, err := RandomToken()
	if err != nil {
		return "", nil, err
	}
	if err := s.store.CreatePasswordReset(ctx, HashToken(token), u.ID, time.Now().Add(resetTokenTTL)); err != nil {
		return "", nil, err
	}
	return token, u, nil
}

// ResetTokenValid reports whether a reset token can still be used.
func (s *Service) ResetTokenValid(ctx context.Context, token string) bool {
	return token != "" && s.store.PasswordResetValid(ctx, HashToken(token))
}

// ResetPassword sets a new password using a reset token and signs out all
// existing sessions.
func (s *Service) ResetPassword(ctx context.Context, token, password, confirm string) error {
	if err := ValidatePassword(password); err != nil {
		return ValidationError{"password": err.Error()}
	}
	if password != confirm {
		return ValidationError{"confirm": "passwords do not match"}
	}
	userID, err := s.store.ConsumePasswordReset(ctx, HashToken(token))
	if errors.Is(err, store.ErrNotFound) {
		return ErrInvalidToken
	}
	if err != nil {
		return err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	if err := s.store.UpdateUserPassword(ctx, userID, hash); err != nil {
		return err
	}
	return s.store.DeleteUserSessions(ctx, userID, "")
}

// RandomToken returns 32 bytes of randomness, base64url encoded.
func RandomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken returns the hex SHA-256 of a token for storage.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
