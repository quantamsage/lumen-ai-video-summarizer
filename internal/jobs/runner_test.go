package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/quantamsage/ai-video-summarizer/internal/apperr"
	"github.com/quantamsage/ai-video-summarizer/internal/db"
	"github.com/quantamsage/ai-video-summarizer/internal/model"
	"github.com/quantamsage/ai-video-summarizer/internal/store"
)

type fakeHandler struct {
	mu     sync.Mutex
	errs   []error
	calls  int
	failed []error
	block  chan struct{}
}

func (f *fakeHandler) Handle(ctx context.Context, job *model.Job, progress ProgressFunc) error {
	f.mu.Lock()
	f.calls++
	var err error
	if len(f.errs) > 0 {
		err = f.errs[0]
		f.errs = f.errs[1:]
	}
	block := f.block
	f.mu.Unlock()
	progress("working", 50)
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (f *fakeHandler) Failed(ctx context.Context, job *model.Job, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed = append(f.failed, err)
}

func setup(t *testing.T, h Handler) (*Runner, *store.Store, int64) {
	t.Helper()
	conn, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := db.Migrate(context.Background(), conn, nil); err != nil {
		t.Fatal(err)
	}
	s := store.New(conn)
	u, _ := s.CreateUser(context.Background(), "a@b.co", "A", "h")
	v := &model.Video{UserID: u.ID, SourceType: model.SourceUpload, Title: "v"}
	if err := s.CreateVideoWithJob(context.Background(), v, 3); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Store: s, Handler: h, Concurrency: 1, BaseBackoff: time.Millisecond,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return r, s, v.ID
}

func claim(t *testing.T, s *store.Store) *model.Job {
	t.Helper()
	j, err := s.ClaimJob(context.Background())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	return j
}

func TestRunnerRetriesTemporaryThenSucceeds(t *testing.T) {
	h := &fakeHandler{errs: []error{apperr.Temporary(errors.New("429"))}}
	r, s, vid := setup(t, h)
	ctx := context.Background()

	r.RunOne(ctx, claim(t, s))
	j, _ := s.LatestJob(ctx, vid, model.JobProcessVideo)
	if j.Status != model.JobQueued || j.ErrorMessage == "" {
		t.Fatalf("temporary failure should requeue: %+v", j)
	}
	time.Sleep(5 * time.Millisecond)
	r.RunOne(ctx, claim(t, s))
	j, _ = s.LatestJob(ctx, vid, model.JobProcessVideo)
	if j.Status != model.JobCompleted || j.Attempts != 2 {
		t.Fatalf("expected completion on second attempt: %+v", j)
	}
}

func TestRunnerGivesUpAfterMaxAttempts(t *testing.T) {
	tmp := apperr.Temporary(errors.New("503"))
	h := &fakeHandler{errs: []error{tmp, tmp, tmp}}
	r, s, vid := setup(t, h)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		time.Sleep(10 * time.Millisecond)
		r.RunOne(ctx, claim(t, s))
	}
	j, _ := s.LatestJob(ctx, vid, model.JobProcessVideo)
	if j.Status != model.JobFailed || len(h.failed) != 1 {
		t.Fatalf("expected permanent failure after 3 attempts: %+v failed=%d", j, len(h.failed))
	}
}

func TestRunnerPermanentErrorUsesUserMessage(t *testing.T) {
	h := &fakeHandler{errs: []error{apperr.User("No audio track found.", errors.New("ffmpeg: no streams"))}}
	r, s, vid := setup(t, h)
	r.RunOne(context.Background(), claim(t, s))
	j, _ := s.LatestJob(context.Background(), vid, model.JobProcessVideo)
	if j.Status != model.JobFailed || j.ErrorMessage != "No audio track found." {
		t.Fatalf("permanent failure: %+v", j)
	}
}

func TestRunnerTimeout(t *testing.T) {
	h := &fakeHandler{block: make(chan struct{})}
	r, s, vid := setup(t, h)
	r.Timeout = 20 * time.Millisecond
	r.RunOne(context.Background(), claim(t, s))
	j, _ := s.LatestJob(context.Background(), vid, model.JobProcessVideo)
	if j.Status != model.JobFailed || j.ErrorMessage == "" {
		t.Fatalf("timed-out job should fail with a message: %+v", j)
	}
}

func TestRunnerShutdownRequeues(t *testing.T) {
	h := &fakeHandler{block: make(chan struct{})}
	r, s, vid := setup(t, h)
	ctx, cancel := context.WithCancel(context.Background())
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		j, _ := s.LatestJob(context.Background(), vid, model.JobProcessVideo)
		if j.Status == model.JobRunning && j.Progress == 50 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("job never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	r.Wait()
	j, _ := s.LatestJob(context.Background(), vid, model.JobProcessVideo)
	if j.Status != model.JobQueued {
		t.Fatalf("job should be requeued on shutdown: %+v", j)
	}
	if len(h.failed) != 0 {
		t.Fatal("shutdown must not mark the job failed")
	}
}

func TestRunnerRecoversPanics(t *testing.T) {
	r, s, vid := setup(t, panicHandler{})
	r.RunOne(context.Background(), claim(t, s))
	j, _ := s.LatestJob(context.Background(), vid, model.JobProcessVideo)
	if j.Status != model.JobFailed {
		t.Fatalf("panicking job should fail: %+v", j)
	}
}

type panicHandler struct{}

func (panicHandler) Handle(context.Context, *model.Job, ProgressFunc) error { panic("boom") }
func (panicHandler) Failed(context.Context, *model.Job, error)              {}
