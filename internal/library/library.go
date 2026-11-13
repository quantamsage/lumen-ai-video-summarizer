// Package library contains the business logic for managing a user's video
// library: adding videos, deleting them with their learning materials,
// reprocessing failures and requesting quizzes.
package library

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/quantamsage/ai-video-summarizer/internal/apperr"
	"github.com/quantamsage/ai-video-summarizer/internal/model"
	"github.com/quantamsage/ai-video-summarizer/internal/pipeline"
	"github.com/quantamsage/ai-video-summarizer/internal/quiz"
	"github.com/quantamsage/ai-video-summarizer/internal/storage"
	"github.com/quantamsage/ai-video-summarizer/internal/store"
	"github.com/quantamsage/ai-video-summarizer/internal/urlimport"
)

const (
	MaxTitleLen       = 200
	MaxDescriptionLen = 5000
)

// Notifier wakes the background workers.
type Notifier interface{ Notify() }

// Service implements library operations.
type Service struct {
	Store       *store.Store
	Files       *storage.Local
	Jobs        Notifier
	MaxAttempts int
	Log         *slog.Logger
	// CheckHost verifies URL hosts; defaults to urlimport.CheckHost.
	CheckHost func(ctx context.Context, src *urlimport.Source) error
}

// NewVideo holds the details of a video being added.
type NewVideo struct {
	Title            string
	Description      string
	OriginalFilename string
	StorageKey       string
	SizeBytes        int64
	MimeType         string
	CaptionKey       string
	SourceURL        string
}

// ValidateDetails checks a title and description.
func ValidateDetails(title, description string) map[string]string {
	errs := map[string]string{}
	if utf8.RuneCountInString(title) > MaxTitleLen {
		errs["title"] = "Title must be at most 200 characters."
	}
	if utf8.RuneCountInString(description) > MaxDescriptionLen {
		errs["description"] = "Description must be at most 5000 characters."
	}
	return errs
}

// AddUpload records an uploaded video (already stored) and queues it.
func (s *Service) AddUpload(ctx context.Context, userID int64, nv NewVideo) (*model.Video, error) {
	title := strings.TrimSpace(nv.Title)
	if title == "" {
		title = titleFromFilename(nv.OriginalFilename)
	}
	v := &model.Video{
		UserID: userID, SourceType: model.SourceUpload, Title: title,
		Description: strings.TrimSpace(nv.Description), OriginalFilename: limit(nv.OriginalFilename, 255),
		StorageKey: nv.StorageKey, CaptionKey: nv.CaptionKey, SizeBytes: nv.SizeBytes, MimeType: nv.MimeType,
	}
	if err := s.Store.CreateVideoWithJob(ctx, v, s.MaxAttempts); err != nil {
		return nil, err
	}
	s.Log.Info("video added", "video_id", v.ID, "user_id", userID, "source", "upload", "bytes", v.SizeBytes)
	s.Jobs.Notify()
	return v, nil
}

// AddURL validates a URL and queues it for import. Platforms that do not
// permit third-party transcript access require a caption file.
func (s *Service) AddURL(ctx context.Context, userID int64, nv NewVideo) (*model.Video, error) {
	src, err := urlimport.Classify(nv.SourceURL)
	if err != nil {
		return nil, err
	}
	check := s.CheckHost
	if check == nil {
		check = func(ctx context.Context, src *urlimport.Source) error { return urlimport.CheckHost(ctx, src.URL) }
	}
	if err := check(ctx, src); err != nil {
		if apperr.UserMessage(err, "") == "" {
			err = apperr.User("That URL can't be used: it points to a private address or its host could not be verified.", err)
		}
		return nil, err
	}
	if src.NeedsCaptions() && nv.CaptionKey == "" {
		name := "YouTube"
		if src.Kind == urlimport.KindVimeo {
			name = "Vimeo"
		}
		return nil, apperr.User(name+" does not let third-party apps download its videos or transcripts. "+
			"Attach the video's caption file (.vtt or .srt — video owners can download it from their dashboard), "+
			"or upload the video file instead.", nil)
	}
	title := strings.TrimSpace(nv.Title)
	v := &model.Video{
		UserID: userID, SourceType: model.SourceURL, SourceURL: src.URL.String(), Title: title,
		Description: strings.TrimSpace(nv.Description), CaptionKey: nv.CaptionKey,
	}
	if v.Title == "" {
		v.Title = defaultURLTitle(src)
	}
	if err := s.Store.CreateVideoWithJob(ctx, v, s.MaxAttempts); err != nil {
		return nil, err
	}
	s.Log.Info("video added", "video_id", v.ID, "user_id", userID, "source", "url", "kind", src.Kind)
	s.Jobs.Notify()
	return v, nil
}

func defaultURLTitle(src *urlimport.Source) string {
	if src.Kind == urlimport.KindDirect {
		parts := strings.Split(src.URL.Path, "/")
		return titleFromFilename(parts[len(parts)-1])
	}
	// Replaced by the oEmbed title during processing.
	return src.URL.String()
}

func titleFromFilename(name string) string {
	name = strings.TrimSpace(name)
	if i := strings.LastIndex(name, "."); i > 0 {
		name = name[:i]
	}
	name = strings.NewReplacer("_", " ", "-", " ").Replace(name)
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return "Untitled video"
	}
	return limit(name, MaxTitleLen)
}

func limit(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// Delete removes a video, all associated learning materials (via cascading
// foreign keys) and its stored files.
func (s *Service) Delete(ctx context.Context, userID, videoID int64) error {
	v, err := s.Store.DeleteVideo(ctx, userID, videoID)
	if err != nil {
		return err
	}
	for _, key := range []string{v.StorageKey, v.CaptionKey, v.ThumbnailKey} {
		if err := s.Files.Remove(key); err != nil {
			s.Log.Error("remove video file", "video_id", videoID, "err", err)
		}
	}
	s.Log.Info("video deleted", "video_id", videoID, "user_id", userID)
	return nil
}

// Reprocess queues a failed video for processing again.
func (s *Service) Reprocess(ctx context.Context, userID, videoID int64) error {
	v, err := s.Store.VideoForUser(ctx, userID, videoID)
	if err != nil {
		return err
	}
	if v.Status != model.VideoFailed {
		return apperr.User("Only failed videos can be reprocessed.", nil)
	}
	if _, err := s.Store.EnqueueJob(ctx, v.ID, model.JobProcessVideo, "{}", s.MaxAttempts); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return apperr.User("This video is already being processed.", err)
		}
		return err
	}
	if err := s.Store.SetVideoStatus(ctx, v.ID, model.VideoQueued, ""); err != nil {
		return err
	}
	if v.SummaryStatus == model.StageFailed {
		s.Store.SetSummaryStatus(ctx, v.ID, model.StagePending)
	}
	if v.QuizStatus == model.StageFailed {
		s.Store.SetQuizStatus(ctx, v.ID, model.StagePending)
	}
	s.Jobs.Notify()
	return nil
}

// RequestQuiz validates a quiz configuration and queues generation.
func (s *Service) RequestQuiz(ctx context.Context, userID, videoID int64, cfg model.QuizConfig) error {
	v, err := s.Store.VideoForUser(ctx, userID, videoID)
	if err != nil {
		return err
	}
	if v.SummaryStatus != model.StageReady {
		return apperr.User("Quizzes can be generated once the video has finished processing.", nil)
	}
	if err := quiz.ValidateConfig(&cfg); err != nil {
		return apperr.User(capitalize(err.Error())+".", err)
	}
	if cfg.Section >= 0 {
		sum, err := s.Store.SummaryForVideo(ctx, v.ID)
		if err != nil {
			return err
		}
		if cfg.Section >= len(sum.Sections) {
			return apperr.User("Choose a valid section.", nil)
		}
	}
	payload, err := json.Marshal(pipeline.QuizJobPayload{Config: cfg})
	if err != nil {
		return err
	}
	if _, err := s.Store.EnqueueJob(ctx, v.ID, model.JobGenerateQuiz, string(payload), s.MaxAttempts); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return apperr.User("A quiz is already being generated for this video. Please wait for it to finish.", err)
		}
		return err
	}
	if err := s.Store.SetQuizStatus(ctx, v.ID, model.StageProcessing); err != nil {
		return err
	}
	s.Log.Info("quiz requested", "video_id", v.ID, "count", cfg.Count, "difficulty", cfg.Difficulty, "section", cfg.Section)
	s.Jobs.Notify()
	return nil
}

// DeleteAccount removes a user, all their data and stored files.
func (s *Service) DeleteAccount(ctx context.Context, userID int64) error {
	keys, err := s.Store.UserStorageKeys(ctx, userID)
	if err != nil {
		return err
	}
	if err := s.Store.DeleteUser(ctx, userID); err != nil {
		return err
	}
	for _, k := range keys {
		if err := s.Files.Remove(k); err != nil {
			s.Log.Error("remove file during account deletion", "user_id", userID, "err", err)
		}
	}
	s.Log.Info("account deleted", "user_id", userID)
	return nil
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
