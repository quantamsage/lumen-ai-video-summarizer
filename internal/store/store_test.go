package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/quantamsage/ai-video-summarizer/internal/db"
	"github.com/quantamsage/ai-video-summarizer/internal/model"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	conn, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := db.Migrate(context.Background(), conn, nil); err != nil {
		t.Fatal(err)
	}
	return New(conn)
}

func mustUser(t *testing.T, s *Store, email string) *model.User {
	t.Helper()
	u, err := s.CreateUser(context.Background(), email, "Test", "hash")
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func mustVideo(t *testing.T, s *Store, userID int64, title string) *model.Video {
	t.Helper()
	v := &model.Video{UserID: userID, SourceType: model.SourceUpload, Title: title, StorageKey: "k/" + title}
	if err := s.CreateVideoWithJob(context.Background(), v, 3); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestUsersAndSessions(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := mustUser(t, s, "a@example.com")
	if _, err := s.CreateUser(ctx, "A@EXAMPLE.com", "dup", "h"); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict for case-insensitive duplicate email, got %v", err)
	}
	if err := s.CreateSession(ctx, "tok", u.ID, time.Now().Add(time.Hour), "ua"); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.SessionUser(ctx, "tok")
	if err != nil || got.ID != u.ID {
		t.Fatalf("session lookup: %v %v", got, err)
	}
	if err := s.CreateSession(ctx, "old", u.ID, time.Now().Add(-time.Minute), "ua"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SessionUser(ctx, "old"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired session should not resolve, got %v", err)
	}
	if n, err := s.DeleteExpiredSessions(ctx); err != nil || n != 1 {
		t.Fatalf("delete expired: n=%d err=%v", n, err)
	}
}

func TestPasswordResetSingleUse(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := mustUser(t, s, "a@example.com")
	if err := s.CreatePasswordReset(ctx, "r", u.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	id, err := s.ConsumePasswordReset(ctx, "r")
	if err != nil || id != u.ID {
		t.Fatalf("consume: %d %v", id, err)
	}
	if _, err := s.ConsumePasswordReset(ctx, "r"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second use should fail, got %v", err)
	}
}

func TestVideoOwnershipAndListing(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	alice := mustUser(t, s, "alice@example.com")
	bob := mustUser(t, s, "bob@example.com")
	v1 := mustVideo(t, s, alice.ID, "Go concurrency")
	mustVideo(t, s, alice.ID, "SQL indexes 100%")
	mustVideo(t, s, bob.ID, "Bob's video")

	if _, err := s.VideoForUser(ctx, bob.ID, v1.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob must not see alice's video, got %v", err)
	}
	if _, err := s.DeleteVideo(ctx, bob.ID, v1.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob must not delete alice's video, got %v", err)
	}

	list, total, err := s.ListVideos(ctx, alice.ID, VideoFilter{})
	if err != nil || total != 2 || len(list) != 2 {
		t.Fatalf("list: total=%d len=%d err=%v", total, len(list), err)
	}
	list, total, _ = s.ListVideos(ctx, alice.ID, VideoFilter{Query: "100%"})
	if total != 1 || list[0].Title != "SQL indexes 100%" {
		t.Fatalf("search with LIKE wildcard: %+v", list)
	}
	list, _, _ = s.ListVideos(ctx, alice.ID, VideoFilter{Query: "%"})
	if len(list) != 1 {
		t.Fatalf("literal %% should match only one title, got %d", len(list))
	}
	if err := s.SetVideoStatus(ctx, v1.ID, model.VideoReady, ""); err != nil {
		t.Fatal(err)
	}
	_, total, _ = s.ListVideos(ctx, alice.ID, VideoFilter{Status: model.VideoReady})
	if total != 1 {
		t.Fatalf("status filter: %d", total)
	}
}

func TestJobLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := mustUser(t, s, "a@example.com")
	v := mustVideo(t, s, u.ID, "v")

	if _, err := s.EnqueueJob(ctx, v.ID, model.JobProcessVideo, "{}", 3); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate active job should conflict, got %v", err)
	}

	j, err := s.ClaimJob(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != model.JobRunning || j.Attempts != 1 {
		t.Fatalf("claimed job: %+v", j)
	}
	if _, err := s.ClaimJob(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no second job should be claimable, got %v", err)
	}

	if err := s.RetryJob(ctx, j.ID, "temporary", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimJob(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("job delayed by run_after must not be claimed, got %v", err)
	}
	if err := s.RetryJob(ctx, j.ID, "temporary", 0); err != nil {
		t.Fatal(err)
	}
	j, err = s.ClaimJob(ctx)
	if err != nil || j.Attempts != 2 {
		t.Fatalf("reclaim: %+v %v", j, err)
	}
	if err := s.UpdateJobProgress(ctx, j.ID, "transcribing", 40); err != nil {
		t.Fatal(err)
	}
	// Simulate crash and restart.
	if n, err := s.RequeueStaleJobs(ctx); err != nil || n != 1 {
		t.Fatalf("requeue: %d %v", n, err)
	}
	j, _ = s.ClaimJob(ctx)
	if err := s.CompleteJob(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := s.LatestJob(ctx, v.ID, model.JobProcessVideo)
	if got.Status != model.JobCompleted || got.Progress != 100 || got.FinishedAt == nil {
		t.Fatalf("completed job: %+v", got)
	}
}

func TestLearningMaterialPersistenceAndCascade(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := mustUser(t, s, "a@example.com")
	other := mustUser(t, s, "b@example.com")
	v := mustVideo(t, s, u.ID, "v")

	tr := &model.Transcript{VideoID: v.ID, Source: "captions", HasTimestamps: true, FullText: "hello world", WordCount: 2}
	segs := []model.Segment{{Start: 0, End: 2, Text: "hello"}, {Start: 2, End: 4, Text: "world"}}
	if err := s.SaveTranscript(ctx, tr, segs); err != nil {
		t.Fatal(err)
	}
	page, total, err := s.SegmentsPage(ctx, tr.ID, "wor", 0, 10)
	if err != nil || total != 1 || page[0].Text != "world" {
		t.Fatalf("segment search: %+v %d %v", page, total, err)
	}

	start := 0.0
	sum := &model.Summary{
		VideoID: v.ID, Overview: "o", Detailed: "d",
		KeyTakeaways:  []string{"k1"},
		LearningNotes: []model.LearningNote{{Note: "n", IsExternal: true}},
		Sections:      []model.Section{{Title: "Intro", Content: "c", Start: &start}},
		Concepts:      []model.Concept{{Term: "t", Definition: "def", Category: "term"}},
	}
	if err := s.SaveSummary(ctx, sum); err != nil {
		t.Fatal(err)
	}
	gotSum, err := s.SummaryForVideo(ctx, v.ID)
	if err != nil || len(gotSum.Sections) != 1 || *gotSum.Sections[0].Start != 0 || !gotSum.LearningNotes[0].IsExternal {
		t.Fatalf("summary roundtrip: %+v %v", gotSum, err)
	}

	quiz := &model.Quiz{VideoID: v.ID, Title: "Q", Difficulty: "easy", Scope: "full", RequestedCount: 2,
		Questions: []model.Question{
			{Type: model.TrueFalse, Prompt: "p", Explanation: "e", Options: []model.Option{
				{Text: "True", IsCorrect: true}, {Text: "False"}}},
			{Type: model.ShortAnswer, Prompt: "p2", Explanation: "e2", AcceptedAnswers: []string{"goroutine"}},
		}}
	if err := s.SaveQuiz(ctx, quiz); err != nil {
		t.Fatal(err)
	}
	if _, err := s.QuizForUser(ctx, other.ID, quiz.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other user must not load quiz, got %v", err)
	}
	loaded, err := s.QuizForUser(ctx, u.ID, quiz.ID)
	if err != nil || len(loaded.Questions) != 2 || len(loaded.Questions[0].Options) != 2 ||
		loaded.Questions[1].AcceptedAnswers[0] != "goroutine" {
		t.Fatalf("quiz roundtrip: %+v %v", loaded, err)
	}

	att := &model.Attempt{QuizID: quiz.ID, UserID: u.ID, Score: 50, CorrectCount: 1, TotalCount: 2,
		Answers: []model.Answer{
			{QuestionID: loaded.Questions[0].ID, SelectedOptions: []int64{loaded.Questions[0].Options[0].ID}, IsCorrect: true, GradingMethod: "choice"},
			{QuestionID: loaded.Questions[1].ID, TextAnswer: "thread", GradingMethod: "exact"},
		}}
	if err := s.SaveAttempt(ctx, att); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AttemptForUser(ctx, other.ID, att.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other user must not load attempt, got %v", err)
	}
	hist, err := s.AttemptHistory(ctx, u.ID, 0, 10)
	if err != nil || len(hist) != 1 || hist[0].VideoTitle != "v" {
		t.Fatalf("history: %+v %v", hist, err)
	}
	st, err := s.DashboardStats(ctx, u.ID)
	if err != nil || st.TotalVideos != 1 || st.Summaries != 1 || st.QuizzesTaken != 1 || st.AverageScore != 50 {
		t.Fatalf("stats: %+v %v", st, err)
	}

	if _, err := s.DeleteVideo(ctx, u.ID, v.ID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"transcripts", "transcript_segments", "summaries", "summary_sections",
		"concepts", "quizzes", "quiz_questions", "quiz_options", "quiz_attempts", "quiz_answers", "processing_jobs"} {
		var n int
		if err := s.DB.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s not cleaned up after video delete: %d rows", table, n)
		}
	}
}
