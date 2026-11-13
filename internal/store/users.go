package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/quantamsage/ai-video-summarizer/internal/model"
)

const userCols = `id, email, display_name, password_hash, created_at, updated_at`

func scanUser(row interface{ Scan(...any) error }) (*model.User, error) {
	u := &model.User{}
	err := row.Scan(&u.ID, &u.Email, &u.DisplayName, &u.PasswordHash, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, notFound(err)
	}
	return u, nil
}

// CreateUser inserts a user, returning ErrConflict if the email is taken.
func (s *Store) CreateUser(ctx context.Context, email, name, hash string) (*model.User, error) {
	t := now()
	res, err := s.DB.ExecContext(ctx,
		`INSERT INTO users(email, display_name, password_hash, created_at, updated_at) VALUES (?,?,?,?,?)`,
		email, name, hash, t, t)
	if isUniqueViolation(err) {
		return nil, ErrConflict
	}
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.UserByID(ctx, id)
}

func (s *Store) UserByID(ctx context.Context, id int64) (*model.User, error) {
	return scanUser(s.DB.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

func (s *Store) UserByEmail(ctx context.Context, email string) (*model.User, error) {
	return scanUser(s.DB.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE email = ?`, email))
}

func (s *Store) UpdateUserProfile(ctx context.Context, id int64, email, name string) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE users SET email = ?, display_name = ?, updated_at = ? WHERE id = ?`, email, name, now(), id)
	if isUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

func (s *Store) UpdateUserPassword(ctx context.Context, id int64, hash string) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`, hash, now(), id)
	return err
}

func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	return err
}

// Sessions ------------------------------------------------------------------

func (s *Store) CreateSession(ctx context.Context, tokenHash string, userID int64, expires time.Time, userAgent string) error {
	t := now()
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO sessions(token_hash, user_id, created_at, expires_at, last_seen_at, user_agent) VALUES (?,?,?,?,?,?)`,
		tokenHash, userID, t, expires.UTC(), t, userAgent)
	return err
}

// SessionUser returns the user for a valid, unexpired session.
func (s *Store) SessionUser(ctx context.Context, tokenHash string) (*model.User, time.Time, error) {
	var expires time.Time
	u := &model.User{}
	err := s.DB.QueryRowContext(ctx, `
		SELECT u.id, u.email, u.display_name, u.password_hash, u.created_at, u.updated_at, s.expires_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ? AND s.expires_at > ?`, tokenHash, now()).
		Scan(&u.ID, &u.Email, &u.DisplayName, &u.PasswordHash, &u.CreatedAt, &u.UpdatedAt, &expires)
	if err != nil {
		return nil, time.Time{}, notFound(err)
	}
	return u, expires, nil
}

// TouchSession extends a session's expiry (sliding expiration).
func (s *Store) TouchSession(ctx context.Context, tokenHash string, expires time.Time) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE sessions SET last_seen_at = ?, expires_at = ? WHERE token_hash = ?`, now(), expires.UTC(), tokenHash)
	return err
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}

// DeleteUserSessions removes all sessions for a user, optionally keeping one.
func (s *Store) DeleteUserSessions(ctx context.Context, userID int64, keepHash string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND token_hash != ?`, userID, keepHash)
	return err
}

func (s *Store) DeleteExpiredSessions(ctx context.Context) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, now())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if _, err := s.DB.ExecContext(ctx, `DELETE FROM password_resets WHERE expires_at <= ?`, now()); err != nil {
		return n, err
	}
	return n, nil
}

// Password resets -----------------------------------------------------------

func (s *Store) CreatePasswordReset(ctx context.Context, tokenHash string, userID int64, expires time.Time) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO password_resets(token_hash, user_id, created_at, expires_at) VALUES (?,?,?,?)`,
		tokenHash, userID, now(), expires.UTC())
	return err
}

// ConsumePasswordReset atomically marks a valid reset token as used and
// returns its user id.
func (s *Store) ConsumePasswordReset(ctx context.Context, tokenHash string) (int64, error) {
	var userID int64
	err := s.InTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `
			SELECT user_id FROM password_resets
			WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?`, tokenHash, now()).Scan(&userID)
		if err != nil {
			return notFound(err)
		}
		_, err = tx.ExecContext(ctx, `UPDATE password_resets SET used_at = ? WHERE token_hash = ?`, now(), tokenHash)
		return err
	})
	return userID, err
}

// PasswordResetValid reports whether a token exists and is still usable.
func (s *Store) PasswordResetValid(ctx context.Context, tokenHash string) bool {
	var one int
	err := s.DB.QueryRowContext(ctx, `
		SELECT 1 FROM password_resets WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?`,
		tokenHash, now()).Scan(&one)
	return err == nil
}
