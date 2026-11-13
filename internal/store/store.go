// Package store implements all database access using parameterized SQL.
//
// Every query for user-owned data is scoped by user id so authorization is
// enforced at the data layer as well as in the handlers.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNotFound is returned when a row does not exist or is not owned by the
// requesting user.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned when a unique constraint is violated.
var ErrConflict = errors.New("conflict")

// Store wraps the database connection.
type Store struct {
	DB *sql.DB
}

func New(db *sql.DB) *Store { return &Store{DB: db} }

// now returns the current time in UTC, which keeps stored timestamps
// lexically comparable.
func now() time.Time { return time.Now().UTC() }

// InTx runs fn inside a transaction, rolling back on error.
func (s *Store) InTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func nullTimePtr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

func nullFloatPtr(f sql.NullFloat64) *float64 {
	if !f.Valid {
		return nil
	}
	v := f.Float64
	return &v
}

func floatPtrArg(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
