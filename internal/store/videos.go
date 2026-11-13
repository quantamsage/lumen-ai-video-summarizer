package store

import (
	"context"
	"database/sql"
	"strings"

	"github.com/quantamsage/ai-video-summarizer/internal/model"
)

const videoCols = `id, user_id, source_type, source_url, title, description, original_filename,
	storage_key, caption_key, thumbnail_key, thumbnail_url, mime_type, size_bytes, duration_seconds,
	width, height, status, summary_status, quiz_status, error_message, created_at, updated_at`

func scanVideo(row interface{ Scan(...any) error }) (*model.Video, error) {
	v := &model.Video{}
	err := row.Scan(&v.ID, &v.UserID, &v.SourceType, &v.SourceURL, &v.Title, &v.Description,
		&v.OriginalFilename, &v.StorageKey, &v.CaptionKey, &v.ThumbnailKey, &v.ThumbnailURL,
		&v.MimeType, &v.SizeBytes, &v.DurationSeconds, &v.Width, &v.Height, &v.Status,
		&v.SummaryStatus, &v.QuizStatus, &v.ErrorMessage, &v.CreatedAt, &v.UpdatedAt)
	if err != nil {
		return nil, notFound(err)
	}
	return v, nil
}

// CreateVideoWithJob inserts a video together with its initial processing
// job in one transaction.
func (s *Store) CreateVideoWithJob(ctx context.Context, v *model.Video, maxAttempts int) error {
	return s.InTx(ctx, func(tx *sql.Tx) error {
		t := now()
		res, err := tx.ExecContext(ctx, `
			INSERT INTO videos(user_id, source_type, source_url, title, description, original_filename,
				storage_key, caption_key, thumbnail_url, mime_type, size_bytes, status, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			v.UserID, v.SourceType, v.SourceURL, v.Title, v.Description, v.OriginalFilename,
			v.StorageKey, v.CaptionKey, v.ThumbnailURL, v.MimeType, v.SizeBytes, model.VideoQueued, t, t)
		if err != nil {
			return err
		}
		v.ID, _ = res.LastInsertId()
		v.Status = model.VideoQueued
		v.SummaryStatus = model.StagePending
		v.QuizStatus = model.StagePending
		v.CreatedAt, v.UpdatedAt = t, t
		_, err = insertJob(ctx, tx, v.ID, model.JobProcessVideo, "{}", maxAttempts)
		return err
	})
}

// VideoForUser returns a video only if it belongs to userID.
func (s *Store) VideoForUser(ctx context.Context, userID, videoID int64) (*model.Video, error) {
	return scanVideo(s.DB.QueryRowContext(ctx,
		`SELECT `+videoCols+` FROM videos WHERE id = ? AND user_id = ?`, videoID, userID))
}

// VideoByID returns a video without an ownership check. It is only for
// background workers.
func (s *Store) VideoByID(ctx context.Context, videoID int64) (*model.Video, error) {
	return scanVideo(s.DB.QueryRowContext(ctx, `SELECT `+videoCols+` FROM videos WHERE id = ?`, videoID))
}

// VideoFilter controls library listing.
type VideoFilter struct {
	Query  string
	Status string // "" for all
	Oldest bool   // sort ascending by date added
	Limit  int
	Offset int
}

// ListVideos returns a user's videos matching the filter and the total count.
func (s *Store) ListVideos(ctx context.Context, userID int64, f VideoFilter) ([]*model.Video, int, error) {
	where := []string{"user_id = ?"}
	args := []any{userID}
	if q := strings.TrimSpace(f.Query); q != "" {
		like := "%" + escapeLike(q) + "%"
		where = append(where, `(title LIKE ? ESCAPE '\' OR description LIKE ? ESCAPE '\')`)
		args = append(args, like, like)
	}
	if f.Status != "" {
		where = append(where, "status = ?")
		args = append(args, f.Status)
	}
	cond := strings.Join(where, " AND ")

	var total int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM videos WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	order := "created_at DESC, id DESC"
	if f.Oldest {
		order = "created_at ASC, id ASC"
	}
	limit := f.Limit
	if limit <= 0 || limit > 100 {
		limit = 24
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT `+videoCols+` FROM videos WHERE `+cond+` ORDER BY `+order+` LIMIT ? OFFSET ?`,
		append(args, limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*model.Video
	for rows.Next() {
		v, err := scanVideo(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, v)
	}
	return out, total, rows.Err()
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// UpdateVideoMetadata stores metadata extracted during processing.
func (s *Store) UpdateVideoMetadata(ctx context.Context, v *model.Video) error {
	_, err := s.DB.ExecContext(ctx, `
		UPDATE videos SET title = ?, description = ?, duration_seconds = ?, width = ?, height = ?,
			mime_type = ?, size_bytes = ?, thumbnail_key = ?, thumbnail_url = ?, caption_key = ?, updated_at = ?
		WHERE id = ?`,
		v.Title, v.Description, v.DurationSeconds, v.Width, v.Height, v.MimeType, v.SizeBytes,
		v.ThumbnailKey, v.ThumbnailURL, v.CaptionKey, now(), v.ID)
	return err
}

// SetVideoStatus updates the pipeline status and error message.
func (s *Store) SetVideoStatus(ctx context.Context, videoID int64, status, errMsg string) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE videos SET status = ?, error_message = ?, updated_at = ? WHERE id = ?`,
		status, errMsg, now(), videoID)
	return err
}

func (s *Store) SetSummaryStatus(ctx context.Context, videoID int64, status string) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE videos SET summary_status = ?, updated_at = ? WHERE id = ?`, status, now(), videoID)
	return err
}

func (s *Store) SetQuizStatus(ctx context.Context, videoID int64, status string) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE videos SET quiz_status = ?, updated_at = ? WHERE id = ?`, status, now(), videoID)
	return err
}

// DeleteVideo removes a user's video and, through cascading foreign keys,
// all of its jobs, transcripts, summaries, quizzes and attempts. It returns
// the deleted row so the caller can remove stored files.
func (s *Store) DeleteVideo(ctx context.Context, userID, videoID int64) (*model.Video, error) {
	var v *model.Video
	err := s.InTx(ctx, func(tx *sql.Tx) error {
		var err error
		v, err = scanVideo(tx.QueryRowContext(ctx,
			`SELECT `+videoCols+` FROM videos WHERE id = ? AND user_id = ?`, videoID, userID))
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM videos WHERE id = ? AND user_id = ?`, videoID, userID)
		return err
	})
	return v, err
}

// UserStorageKeys returns all stored file keys belonging to a user, used when
// deleting an account.
func (s *Store) UserStorageKeys(ctx context.Context, userID int64) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT storage_key, caption_key, thumbnail_key FROM videos WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var a, b, c string
		if err := rows.Scan(&a, &b, &c); err != nil {
			return nil, err
		}
		for _, k := range []string{a, b, c} {
			if k != "" {
				keys = append(keys, k)
			}
		}
	}
	return keys, rows.Err()
}
