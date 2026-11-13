package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/quantamsage/ai-video-summarizer/internal/model"
)

const jobCols = `id, video_id, kind, payload, status, stage, progress, attempts, max_attempts,
	run_after, error_message, created_at, started_at, finished_at`

func scanJob(row interface{ Scan(...any) error }) (*model.Job, error) {
	j := &model.Job{}
	var started, finished sql.NullTime
	err := row.Scan(&j.ID, &j.VideoID, &j.Kind, &j.Payload, &j.Status, &j.Stage, &j.Progress,
		&j.Attempts, &j.MaxAttempts, &j.RunAfter, &j.ErrorMessage, &j.CreatedAt, &started, &finished)
	if err != nil {
		return nil, notFound(err)
	}
	j.StartedAt = nullTimePtr(started)
	j.FinishedAt = nullTimePtr(finished)
	return j, nil
}

func insertJob(ctx context.Context, tx *sql.Tx, videoID int64, kind, payload string, maxAttempts int) (int64, error) {
	t := now()
	res, err := tx.ExecContext(ctx, `
		INSERT INTO processing_jobs(video_id, kind, payload, max_attempts, run_after, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?)`, videoID, kind, payload, maxAttempts, t, t, t)
	if isUniqueViolation(err) {
		return 0, ErrConflict
	}
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// EnqueueJob creates a job. It returns ErrConflict if an identical job for
// the video is already queued or running.
func (s *Store) EnqueueJob(ctx context.Context, videoID int64, kind, payload string, maxAttempts int) (int64, error) {
	var id int64
	err := s.InTx(ctx, func(tx *sql.Tx) error {
		var err error
		id, err = insertJob(ctx, tx, videoID, kind, payload, maxAttempts)
		return err
	})
	return id, err
}

// ClaimJob atomically takes the oldest runnable queued job, marking it as
// running. It returns ErrNotFound when there is nothing to do.
func (s *Store) ClaimJob(ctx context.Context) (*model.Job, error) {
	var job *model.Job
	err := s.InTx(ctx, func(tx *sql.Tx) error {
		t := now()
		j, err := scanJob(tx.QueryRowContext(ctx, `
			SELECT `+jobCols+` FROM processing_jobs
			WHERE status = 'queued' AND run_after <= ?
			ORDER BY run_after, id LIMIT 1`, t))
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE processing_jobs
			SET status = 'running', attempts = attempts + 1, started_at = ?, updated_at = ?, error_message = ''
			WHERE id = ? AND status = 'queued'`, t, t, j.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrNotFound
		}
		j.Status = model.JobRunning
		j.Attempts++
		j.StartedAt = &t
		job = j
		return nil
	})
	return job, err
}

// UpdateJobProgress records the current stage and percentage.
func (s *Store) UpdateJobProgress(ctx context.Context, jobID int64, stage string, progress int) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE processing_jobs SET stage = ?, progress = ?, updated_at = ? WHERE id = ?`,
		stage, progress, now(), jobID)
	return err
}

func (s *Store) CompleteJob(ctx context.Context, jobID int64) error {
	t := now()
	_, err := s.DB.ExecContext(ctx, `
		UPDATE processing_jobs SET status = 'completed', progress = 100, stage = 'done',
			finished_at = ?, updated_at = ?, error_message = '' WHERE id = ?`, t, t, jobID)
	return err
}

// FailJob marks a job as permanently failed.
func (s *Store) FailJob(ctx context.Context, jobID int64, msg string) error {
	t := now()
	_, err := s.DB.ExecContext(ctx, `
		UPDATE processing_jobs SET status = 'failed', error_message = ?, finished_at = ?, updated_at = ?
		WHERE id = ?`, msg, t, t, jobID)
	return err
}

// RetryJob requeues a job to run after a delay.
func (s *Store) RetryJob(ctx context.Context, jobID int64, msg string, delay time.Duration) error {
	t := now()
	_, err := s.DB.ExecContext(ctx, `
		UPDATE processing_jobs SET status = 'queued', error_message = ?, run_after = ?, updated_at = ?
		WHERE id = ?`, msg, t.Add(delay), t, jobID)
	return err
}

// RequeueStaleJobs moves jobs left in the running state (for example after a
// crash) back to the queue. Called once at startup.
func (s *Store) RequeueStaleJobs(ctx context.Context) (int64, error) {
	t := now()
	res, err := s.DB.ExecContext(ctx, `
		UPDATE processing_jobs SET status = 'queued', run_after = ?, updated_at = ?,
			error_message = 'requeued after server restart'
		WHERE status = 'running'`, t, t)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// LatestJob returns the most recent job of a kind for a video.
func (s *Store) LatestJob(ctx context.Context, videoID int64, kind string) (*model.Job, error) {
	return scanJob(s.DB.QueryRowContext(ctx, `
		SELECT `+jobCols+` FROM processing_jobs WHERE video_id = ? AND kind = ?
		ORDER BY id DESC LIMIT 1`, videoID, kind))
}

// ActiveJob returns a queued or running job of a kind for a video.
func (s *Store) ActiveJob(ctx context.Context, videoID int64, kind string) (*model.Job, error) {
	return scanJob(s.DB.QueryRowContext(ctx, `
		SELECT `+jobCols+` FROM processing_jobs
		WHERE video_id = ? AND kind = ? AND status IN ('queued','running')
		ORDER BY id DESC LIMIT 1`, videoID, kind))
}

func (s *Store) JobByID(ctx context.Context, id int64) (*model.Job, error) {
	return scanJob(s.DB.QueryRowContext(ctx, `SELECT `+jobCols+` FROM processing_jobs WHERE id = ?`, id))
}
