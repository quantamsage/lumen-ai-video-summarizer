// Package jobs runs background processing jobs from the database queue
// with a fixed pool of workers.
package jobs

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/quantamsage/ai-video-summarizer/internal/apperr"
	"github.com/quantamsage/ai-video-summarizer/internal/model"
	"github.com/quantamsage/ai-video-summarizer/internal/store"
)

// ProgressFunc reports the current stage and percentage of a running job.
type ProgressFunc func(stage string, percent int)

// Handler executes jobs.
type Handler interface {
	// Handle runs the job. Returning an error marked with apperr.Temporary
	// schedules a retry (up to the job's max attempts).
	Handle(ctx context.Context, job *model.Job, progress ProgressFunc) error
	// Failed is called once a job has failed permanently.
	Failed(ctx context.Context, job *model.Job, err error)
}

// Runner polls the job queue and executes jobs concurrently.
type Runner struct {
	Store        *store.Store
	Handler      Handler
	Concurrency  int
	Timeout      time.Duration
	PollInterval time.Duration
	BaseBackoff  time.Duration
	Log          *slog.Logger

	wake chan struct{}
	wg   sync.WaitGroup
}

// Start launches the workers. They stop when ctx is cancelled; call Wait to
// block until in-flight jobs have been released.
func (r *Runner) Start(ctx context.Context) error {
	if r.Concurrency < 1 {
		r.Concurrency = 1
	}
	if r.PollInterval == 0 {
		r.PollInterval = 3 * time.Second
	}
	if r.BaseBackoff == 0 {
		r.BaseBackoff = 30 * time.Second
	}
	r.wake = make(chan struct{}, r.Concurrency)
	n, err := r.Store.RequeueStaleJobs(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		r.Log.Warn("requeued jobs interrupted by a previous shutdown", "count", n)
	}
	for i := 0; i < r.Concurrency; i++ {
		r.wg.Add(1)
		go r.worker(ctx, i)
	}
	r.Log.Info("job workers started", "concurrency", r.Concurrency, "timeout", r.Timeout)
	return nil
}

// Wait blocks until all workers have exited.
func (r *Runner) Wait() { r.wg.Wait() }

// Notify wakes an idle worker so new jobs start without waiting for the
// next poll.
func (r *Runner) Notify() {
	if r.wake == nil {
		return
	}
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *Runner) worker(ctx context.Context, id int) {
	defer r.wg.Done()
	ticker := time.NewTicker(r.PollInterval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		job, err := r.Store.ClaimJob(ctx)
		switch {
		case errors.Is(err, store.ErrNotFound):
			select {
			case <-ctx.Done():
				return
			case <-r.wake:
			case <-ticker.C:
			}
			continue
		case err != nil:
			if ctx.Err() == nil {
				r.Log.Error("claim job", "worker", id, "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(r.PollInterval):
			}
			continue
		}
		r.run(ctx, job)
	}
}

// RunOne executes a single claimed job synchronously. Exposed for tests.
func (r *Runner) RunOne(ctx context.Context, job *model.Job) { r.run(ctx, job) }

func (r *Runner) run(ctx context.Context, job *model.Job) {
	log := r.Log.With("job_id", job.ID, "video_id", job.VideoID, "kind", job.Kind, "attempt", job.Attempts)
	log.Info("job started")
	start := time.Now()

	jobCtx := ctx
	var cancel context.CancelFunc
	if r.Timeout > 0 {
		jobCtx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}

	// Progress writes use a background context so a cancelled job can
	// still record its final state.
	bg := context.WithoutCancel(ctx)
	progress := func(stage string, pct int) {
		pct = int(math.Max(0, math.Min(99, float64(pct))))
		if err := r.Store.UpdateJobProgress(bg, job.ID, stage, pct); err != nil {
			log.Warn("update job progress", "err", err)
		}
	}

	err := r.safeHandle(jobCtx, job, progress)
	elapsed := time.Since(start).Round(time.Millisecond)
	switch {
	case err == nil:
		if err := r.Store.CompleteJob(bg, job.ID); err != nil {
			log.Error("mark job complete", "err", err)
		}
		log.Info("job completed", "duration", elapsed)

	case ctx.Err() != nil:
		// Server shutting down: put the job back without consuming an attempt's
		// worth of backoff. It will resume after restart.
		if err := r.Store.RetryJob(bg, job.ID, "interrupted by server shutdown", 0); err != nil {
			log.Error("requeue interrupted job", "err", err)
		}
		log.Warn("job interrupted by shutdown", "duration", elapsed)

	case errors.Is(err, context.DeadlineExceeded) || errors.Is(jobCtx.Err(), context.DeadlineExceeded):
		err = apperr.User("Processing took longer than the configured time limit and was stopped.", err)
		r.fail(bg, log, job, err)

	case apperr.IsTemporary(err) && job.Attempts < job.MaxAttempts:
		delay := r.BaseBackoff * time.Duration(1<<(job.Attempts-1))
		msg := "A temporary problem occurred; retrying automatically."
		if rerr := r.Store.RetryJob(bg, job.ID, msg, delay); rerr != nil {
			log.Error("schedule retry", "err", rerr)
		}
		log.Warn("job failed temporarily; retry scheduled", "err", err, "retry_in", delay)

	default:
		r.fail(bg, log, job, err)
	}
}

func (r *Runner) fail(ctx context.Context, log *slog.Logger, job *model.Job, err error) {
	msg := apperr.UserMessage(err, "Processing failed. Please try again later.")
	if ferr := r.Store.FailJob(ctx, job.ID, msg); ferr != nil {
		log.Error("mark job failed", "err", ferr)
	}
	r.Handler.Failed(ctx, job, err)
	log.Error("job failed", "err", err)
}

func (r *Runner) safeHandle(ctx context.Context, job *model.Job, progress ProgressFunc) (err error) {
	defer func() {
		if p := recover(); p != nil {
			r.Log.Error("panic in job handler", "job_id", job.ID, "panic", p)
			err = errors.New("internal error while processing")
		}
	}()
	return r.Handler.Handle(ctx, job, progress)
}
