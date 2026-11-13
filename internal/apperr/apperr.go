// Package apperr defines error classifications shared across packages:
// temporary errors that are worth retrying, and errors whose message is safe
// to show to end users.
package apperr

import (
	"context"
	"errors"
	"net"
)

type temporary struct{ err error }

func (t *temporary) Error() string { return t.err.Error() }
func (t *temporary) Unwrap() error { return t.err }

// Temporary marks err as transient (rate limits, timeouts, 5xx responses).
func Temporary(err error) error {
	if err == nil {
		return nil
	}
	return &temporary{err}
}

// IsTemporary reports whether err (or anything it wraps) is transient,
// including network timeouts.
func IsTemporary(err error) bool {
	var t *temporary
	if errors.As(err, &t) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return false // the job's own deadline expired; do not retry blindly
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// UserError carries a message that is safe to display to users.
type UserError struct {
	Message string
	Err     error
}

func (u *UserError) Error() string {
	if u.Err != nil {
		return u.Message + ": " + u.Err.Error()
	}
	return u.Message
}

func (u *UserError) Unwrap() error { return u.Err }

// User wraps err with a user-facing message.
func User(msg string, err error) error { return &UserError{Message: msg, Err: err} }

// UserMessage returns the user-facing message for err, or fallback.
func UserMessage(err error, fallback string) string {
	var u *UserError
	if errors.As(err, &u) {
		return u.Message
	}
	return fallback
}

// HTTPStatusTemporary reports whether an HTTP status code from an upstream
// API indicates a transient failure.
func HTTPStatusTemporary(code int) bool {
	return code == 408 || code == 409 || code == 425 || code == 429 || code >= 500
}
