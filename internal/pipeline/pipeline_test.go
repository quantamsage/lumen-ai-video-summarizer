package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quantamsage/ai-video-summarizer/internal/ai/aitest"
	"github.com/quantamsage/ai-video-summarizer/internal/apperr"
	"github.com/quantamsage/ai-video-summarizer/internal/db"
	"github.com/quantamsage/ai-video-summarizer/internal/jobs"
	"github.com/quantamsage/ai-video-summarizer/internal/media"
	"github.com/quantamsage/ai-video-summarizer/internal/model"
	"github.com/quantamsage/ai-video-summarizer/internal/quiz"
	"github.com/quantamsage/ai-video-summarizer/internal/storage"
	"github.com/quantamsage/ai-video-summarizer/internal/store"
	"github.com/quantamsage/ai-video-summarizer/internal/stt"
	"github.com/quantamsage/ai-video-summarizer/internal/summary"
	"github.com/quantamsage/ai-video-summarizer/internal/urlimport"
)

// mp4Header is the start of a minimal ISO BMFF file.
var mp4Header = append([]byte{0, 0, 0, 0x18}, []byte("ftypmp42isom")...)

type fakeMedia struct {
	meta *media.Metadata
	subs []byte
}

func (f *fakeMedia) Probe(context.Context, string) (*media.Metadata, error) {
	if f.meta == nil {
		return nil, media.ErrToolMissing
	}
	return f.meta, nil
}
func (f *fakeMedia) ExtractSubtitles(context.Context, string, int) ([]byte, error) { return f.subs, nil }
func (f *fakeMedia) ExtractAudio(_ context.Context, _ string, dir string, seg int) ([]media.AudioChunk, error) {
	var out []media.AudioChunk
	for i := 0; i < 2; i++ {
		p := filepath.Join(dir, "chunk_"+string(rune('0'+i))+".mp3")
		os.WriteFile(p, []byte("audio"), 0o600)
		out = append(out, media.AudioChunk{Path: p, Offset: float64(i * seg)})
	}
	return out, nil
}
func (f *fakeMedia) Thumbnail(_ context.Context, _, out string, _ float64) error {
	return os.WriteFile(out, []byte("jpg"), 0o600)
}

type fakeSTT struct {
	mu    sync.Mutex
	calls int
	empty bool
}

func (f *fakeSTT) Name() string { return "fake" }
func (f *fakeSTT) Transcribe(context.Context, string) (*stt.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.empty {
		return &stt.Result{}, nil
	}
	return &stt.Result{Language: "en", Segments: []model.Segment{
		{Start: 0, End: 5, Text: "Goroutines are lightweight threads."},
		{Start: 5, End: 9, Text: "Channels let goroutines communicate."},
	}}, nil
}

type fakeFetch struct{}

func (fakeFetch) OEmbed(context.Context, *urlimport.Source) (*urlimport.Metadata, error) {
	return &urlimport.Metadata{Title: "Remote talk", Author: "Speaker", ThumbnailURL: "https://i.example.com/t.jpg"}, nil
}
func (fakeFetch) Download(_ context.Context, _ string, w io.Writer, _ int64, _ func(int64, int64)) (string, int64, error) {
	n, err := w.Write(append(append([]byte{}, mp4Header...), bytes.Repeat([]byte{1}, 100)...))
	return "video/mp4", int64(n), err
}

const summaryJSON = `{"overview":"About Go concurrency.","detailed_summary":[{"heading":"Basics","content":"Goroutines and channels."}],
"key_takeaways":["Goroutines are cheap"],"concepts":[{"term":"Goroutine","definition":"A lightweight thread","category":"term"}],
"topics":[{"title":"Goroutines","summary":"What they are","start_seconds":0,"end_seconds":5},{"title":"Channels","summary":"How they talk","start_seconds":5,"end_seconds":null}],
"learning_notes":[]}`

const quizJSON = `{"questions":[{"type":"true_false","prompt":"Goroutines are lightweight threads.","options":[{"text":"True","is_correct":true,"explanation":"Stated"},{"text":"False","is_correct":false,"explanation":"No"}],"accepted_answers":[],"explanation":"The video says so.","timestamp_seconds":0,"section_title":"Goroutines"}],"shortfall_reason":"Short video."}`

const verifyJSON = `{"results":[{"index":0,"valid":true,"reason":""}]}`

type env struct {
	store  *store.Store
	files  *storage.Local
	runner *jobs.Runner
	proc   *Processor
	ai     *aitest.Mock
	stt    *fakeSTT
	media  *fakeMedia
	user   *model.User
}

func newEnv(t *testing.T) *env {
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
	files, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mock := (&aitest.Mock{}).
		On("Review these quiz questions", verifyJSON).
		On("Write 8 questions", quizJSON).
		On("Write 3 questions", quizJSON).
		On("FULL TRANSCRIPT", summaryJSON)
	e := &env{store: s, files: files, ai: mock, stt: &fakeSTT{},
		media: &fakeMedia{meta: &media.Metadata{Duration: 9, Width: 640, Height: 360, HasVideo: true, HasAudio: true}}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e.proc = &Processor{
		Store: s, Files: files, Media: e.media, STT: e.stt,
		Summarizer: &summary.Generator{AI: mock}, Quizzer: &quiz.Generator{AI: mock},
		Fetch: fakeFetch{}, MaxUploadBytes: 1 << 20, Log: log,
		CheckHost: func(context.Context, *url.URL) error { return nil },
	}
	e.runner = &jobs.Runner{Store: s, Handler: e.proc, BaseBackoff: time.Millisecond, Log: log}
	e.user, _ = s.CreateUser(context.Background(), "u@example.com", "U", "h")
	return e
}

func (e *env) uploadVideo(t *testing.T, captions string) *model.Video {
	t.Helper()
	key, size, mime, err := SaveVideoUpload(e.files, e.user.ID, "talk.mp4", bytes.NewReader(append(mp4Header, 1, 2, 3)), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	v := &model.Video{UserID: e.user.ID, SourceType: model.SourceUpload, Title: "Talk", StorageKey: key, SizeBytes: size, MimeType: mime}
	if captions != "" {
		ck, err := SaveCaptionUpload(e.files, e.user.ID, "talk.vtt", strings.NewReader(captions))
		if err != nil {
			t.Fatal(err)
		}
		v.CaptionKey = ck
	}
	if err := e.store.CreateVideoWithJob(context.Background(), v, 3); err != nil {
		t.Fatal(err)
	}
	return v
}

// drain runs queued jobs until none remain claimable.
func (e *env) drain(t *testing.T) {
	t.Helper()
	for i := 0; i < 10; i++ {
		time.Sleep(3 * time.Millisecond) // let retry backoff elapse
		j, err := e.store.ClaimJob(context.Background())
		if errors.Is(err, store.ErrNotFound) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		e.runner.RunOne(context.Background(), j)
	}
}

func (e *env) video(t *testing.T, id int64) *model.Video {
	v, err := e.store.VideoByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestPipelineWithCaptions(t *testing.T) {
	e := newEnv(t)
	v := e.uploadVideo(t, "WEBVTT\n\n00:00:00.000 --> 00:00:04.000\nGoroutines are lightweight threads.\n\n00:00:04.000 --> 00:00:09.000\nChannels connect them.\n")
	e.drain(t)

	got := e.video(t, v.ID)
	if got.Status != model.VideoReady || got.SummaryStatus != model.StageReady || got.QuizStatus != model.StageReady {
		t.Fatalf("statuses: %s/%s/%s err=%q", got.Status, got.SummaryStatus, got.QuizStatus, got.ErrorMessage)
	}
	if got.DurationSeconds != 9 || got.ThumbnailKey == "" {
		t.Fatalf("metadata not stored: %+v", got)
	}
	tr, err := e.store.TranscriptForVideo(context.Background(), v.ID)
	if err != nil || tr.Source != "captions" || !tr.HasTimestamps {
		t.Fatalf("transcript: %+v %v", tr, err)
	}
	if e.stt.calls != 0 {
		t.Fatal("speech-to-text must not run when captions exist")
	}
	quizzes, _ := e.store.QuizzesForVideo(context.Background(), v.ID)
	if len(quizzes) != 1 || quizzes[0].ShortfallReason == "" {
		t.Fatalf("default quiz: %+v", quizzes)
	}
	j, _ := e.store.LatestJob(context.Background(), v.ID, model.JobProcessVideo)
	if j.Status != model.JobCompleted || j.Progress != 100 {
		t.Fatalf("job: %+v", j)
	}
}

func TestPipelineSpeechToText(t *testing.T) {
	e := newEnv(t)
	v := e.uploadVideo(t, "")
	e.drain(t)
	got := e.video(t, v.ID)
	if got.Status != model.VideoReady {
		t.Fatalf("status %s: %s", got.Status, got.ErrorMessage)
	}
	tr, _ := e.store.TranscriptForVideo(context.Background(), v.ID)
	if tr.Source != "speech_to_text" || tr.Language != "en" {
		t.Fatalf("transcript: %+v", tr)
	}
	segs, _ := e.store.Segments(context.Background(), tr.ID)
	// Second audio chunk segments must be offset by the chunk start.
	if last := segs[len(segs)-1]; last.Start < AudioSegmentSeconds {
		t.Fatalf("chunk offsets not applied: %+v", last)
	}
	// Temp audio directory cleaned up.
	entries, _ := os.ReadDir(filepath.Join(e.files.Root, "tmp"))
	if len(entries) != 0 {
		t.Fatalf("temporary audio not removed: %d entries", len(entries))
	}
}

func TestPipelineEmbeddedSubtitles(t *testing.T) {
	e := newEnv(t)
	e.media.meta.SubtitleStreams = []media.SubtitleStream{{Index: 2, Codec: "subrip", Language: "eng"}}
	e.media.subs = []byte("WEBVTT\n\n00:00:01.000 --> 00:00:03.000\nEmbedded caption text\n")
	v := e.uploadVideo(t, "")
	e.drain(t)
	tr, err := e.store.TranscriptForVideo(context.Background(), v.ID)
	if err != nil || tr.Source != "captions" || e.stt.calls != 0 {
		t.Fatalf("embedded subtitles should be used: %+v %v stt=%d", tr, err, e.stt.calls)
	}
}

func TestPipelineSTTDisabled(t *testing.T) {
	e := newEnv(t)
	e.proc.STT = stt.Disabled{}
	v := e.uploadVideo(t, "")
	e.drain(t)
	got := e.video(t, v.ID)
	if got.Status != model.VideoFailed || !strings.Contains(got.ErrorMessage, "speech-to-text is not configured") {
		t.Fatalf("expected clear failure: %s %q", got.Status, got.ErrorMessage)
	}
	if got.SummaryStatus != model.StageFailed {
		t.Fatalf("summary status should be failed: %s", got.SummaryStatus)
	}
}

func TestPipelineEmptyTranscript(t *testing.T) {
	e := newEnv(t)
	e.stt.empty = true
	v := e.uploadVideo(t, "")
	e.drain(t)
	got := e.video(t, v.ID)
	if got.Status != model.VideoFailed || !strings.Contains(got.ErrorMessage, "No speech") {
		t.Fatalf("empty transcript should fail clearly: %s %q", got.Status, got.ErrorMessage)
	}
}

func TestPipelineMalformedAI(t *testing.T) {
	e := newEnv(t)
	e.ai.Rules = nil
	e.ai.On("FULL TRANSCRIPT", `{"overview": 42}`)
	v := e.uploadVideo(t, "")
	e.drain(t)
	got := e.video(t, v.ID)
	if got.Status != model.VideoFailed || got.SummaryStatus != model.StageFailed {
		t.Fatalf("malformed AI output should fail the video: %s/%s", got.Status, got.SummaryStatus)
	}
	if strings.Contains(got.ErrorMessage, "42") || strings.Contains(got.ErrorMessage, "json") {
		t.Fatalf("internal error details leaked to user: %q", got.ErrorMessage)
	}
}

func TestPipelineRetryReusesTranscript(t *testing.T) {
	e := newEnv(t)
	e.ai.Rules = nil
	e.ai.OnceErr("FULL TRANSCRIPT", apperr.Temporary(errors.New("rate limited")))
	e.ai.On("Review these quiz questions", verifyJSON).On("Write 8 questions", quizJSON).On("FULL TRANSCRIPT", summaryJSON)
	v := e.uploadVideo(t, "")
	e.drain(t)
	got := e.video(t, v.ID)
	if got.Status != model.VideoReady {
		t.Fatalf("expected success after retry: %s %q", got.Status, got.ErrorMessage)
	}
	if e.stt.calls != 2 { // two audio chunks, transcribed once
		t.Fatalf("transcript should be reused on retry; STT calls = %d", e.stt.calls)
	}
	j, _ := e.store.LatestJob(context.Background(), v.ID, model.JobProcessVideo)
	if j.Attempts != 2 {
		t.Fatalf("expected 2 attempts, got %d", j.Attempts)
	}
}

func TestPipelineYouTubeRequiresCaptions(t *testing.T) {
	e := newEnv(t)
	v := &model.Video{UserID: e.user.ID, SourceType: model.SourceURL, SourceURL: "https://www.youtube.com/watch?v=dQw4w9WgXcQ", Title: ""}
	if err := e.store.CreateVideoWithJob(context.Background(), v, 3); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	got := e.video(t, v.ID)
	if got.Status != model.VideoFailed || !strings.Contains(got.ErrorMessage, "caption file") {
		t.Fatalf("expected explanation: %q", got.ErrorMessage)
	}
	if got.Title != "Remote talk" || got.ThumbnailURL == "" {
		t.Fatalf("oEmbed metadata should still be stored: %+v", got)
	}
}

func TestPipelineDirectURLDownload(t *testing.T) {
	e := newEnv(t)
	v := &model.Video{UserID: e.user.ID, SourceType: model.SourceURL, SourceURL: "https://cdn.example.com/talk.mp4", Title: "Direct"}
	if err := e.store.CreateVideoWithJob(context.Background(), v, 3); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	got := e.video(t, v.ID)
	if got.Status != model.VideoReady || got.StorageKey == "" || got.MimeType != "video/mp4" {
		t.Fatalf("direct download: %+v", got)
	}
}

func TestGenerateQuizJobForSection(t *testing.T) {
	e := newEnv(t)
	v := e.uploadVideo(t, "")
	e.drain(t)
	payload, _ := json.Marshal(QuizJobPayload{Config: model.QuizConfig{Count: 3, Difficulty: "easy",
		Types: []string{model.TrueFalse}, Section: 1}})
	if _, err := e.store.EnqueueJob(context.Background(), v.ID, model.JobGenerateQuiz, string(payload), 3); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	quizzes, _ := e.store.QuizzesForVideo(context.Background(), v.ID)
	if len(quizzes) != 2 || quizzes[0].Scope != "1" {
		t.Fatalf("section quiz not created: %+v", quizzes[0])
	}

	// Invalid section fails with a clear message.
	payload, _ = json.Marshal(QuizJobPayload{Config: model.QuizConfig{Count: 3, Difficulty: "easy",
		Types: []string{model.TrueFalse}, Section: 99}})
	e.store.EnqueueJob(context.Background(), v.ID, model.JobGenerateQuiz, string(payload), 3)
	e.drain(t)
	j, _ := e.store.LatestJob(context.Background(), v.ID, model.JobGenerateQuiz)
	if j.Status != model.JobFailed || !strings.Contains(j.ErrorMessage, "section") {
		t.Fatalf("expected failure for bad section: %+v", j)
	}
}

func TestUploadValidation(t *testing.T) {
	files, _ := storage.NewLocal(t.TempDir())
	if _, _, _, err := SaveVideoUpload(files, 1, "evil.exe", strings.NewReader("MZ"), 100); err == nil {
		t.Error("disallowed extension accepted")
	}
	if _, _, _, err := SaveVideoUpload(files, 1, "fake.mp4", strings.NewReader("<html>not a video</html>"), 100); err == nil {
		t.Error("non-video content accepted")
	}
	_, _, _, err := SaveVideoUpload(files, 1, "big.mp4", bytes.NewReader(append(mp4Header, make([]byte, 200)...)), 100)
	if !errors.Is(err, ErrUploadTooLarge) {
		t.Errorf("oversized upload: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(files.Root, "u1"))
	if len(entries) != 0 {
		t.Errorf("rejected uploads must not leave files behind: %d", len(entries))
	}
	if _, err := SaveCaptionUpload(files, 1, "c.vtt", strings.NewReader("not captions")); err == nil {
		t.Error("invalid captions accepted")
	}
	if _, err := SaveCaptionUpload(files, 1, "c.txt", strings.NewReader("WEBVTT")); err == nil {
		t.Error("caption with wrong extension accepted")
	}
}
