// Package pipeline implements the video processing pipeline executed by
// background jobs:
//
//  1. validate input and (for URL imports) fetch metadata or the media file
//  2. extract metadata and a thumbnail with ffprobe/ffmpeg
//  3. use existing captions (user-supplied or embedded text subtitles)
//  4. otherwise extract audio and transcribe it with the speech-to-text provider
//  5. clean, normalize and store the transcript
//  6. generate the summary and concepts (chunked for long transcripts)
//  7. generate a default quiz
//
// The pipeline depends on the ai.Provider and stt.Provider interfaces only,
// so it is independent of any particular AI vendor.
package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/quantamsage/ai-video-summarizer/internal/apperr"
	"github.com/quantamsage/ai-video-summarizer/internal/jobs"
	"github.com/quantamsage/ai-video-summarizer/internal/media"
	"github.com/quantamsage/ai-video-summarizer/internal/model"
	"github.com/quantamsage/ai-video-summarizer/internal/quiz"
	"github.com/quantamsage/ai-video-summarizer/internal/storage"
	"github.com/quantamsage/ai-video-summarizer/internal/store"
	"github.com/quantamsage/ai-video-summarizer/internal/stt"
	"github.com/quantamsage/ai-video-summarizer/internal/summary"
	"github.com/quantamsage/ai-video-summarizer/internal/transcript"
	"github.com/quantamsage/ai-video-summarizer/internal/urlimport"
)

// MediaTools is the subset of media.Tools used by the pipeline, so tests can
// substitute a fake.
type MediaTools interface {
	Probe(ctx context.Context, path string) (*media.Metadata, error)
	ExtractSubtitles(ctx context.Context, path string, streamIndex int) ([]byte, error)
	ExtractAudio(ctx context.Context, path, dir string, segmentSeconds int) ([]media.AudioChunk, error)
	Thumbnail(ctx context.Context, path, outPath string, at float64) error
}

// Fetcher is the subset of urlimport.Client used by the pipeline.
type Fetcher interface {
	OEmbed(ctx context.Context, src *urlimport.Source) (*urlimport.Metadata, error)
	Download(ctx context.Context, rawURL string, w io.Writer, maxBytes int64, progress func(int64, int64)) (string, int64, error)
}

// DefaultQuiz is generated automatically after summarization.
var DefaultQuiz = model.QuizConfig{
	Count:      8,
	Difficulty: "medium",
	Types:      []string{model.SingleChoice, model.MultipleAnswer, model.TrueFalse, model.ShortAnswer},
	Section:    -1,
}

// AudioSegmentSeconds is the length of audio chunks sent to speech-to-text.
const AudioSegmentSeconds = 600

// Processor executes processing jobs.
type Processor struct {
	Store          *store.Store
	Files          *storage.Local
	Media          MediaTools
	STT            stt.Provider
	Summarizer     *summary.Generator
	Quizzer        *quiz.Generator
	Fetch          Fetcher
	MaxUploadBytes int64
	Log            *slog.Logger
	// CheckHost verifies a URL's host resolves only to public addresses.
	// Defaults to urlimport.CheckHost.
	CheckHost func(ctx context.Context, u *url.URL) error
}

var _ jobs.Handler = (*Processor)(nil)

// Handle dispatches a job by kind.
func (p *Processor) Handle(ctx context.Context, job *model.Job, progress jobs.ProgressFunc) error {
	switch job.Kind {
	case model.JobProcessVideo:
		return p.processVideo(ctx, job, progress)
	case model.JobGenerateQuiz:
		return p.generateQuiz(ctx, job, progress)
	default:
		return fmt.Errorf("unknown job kind %q", job.Kind)
	}
}

// Failed records a permanent failure on the video.
func (p *Processor) Failed(ctx context.Context, job *model.Job, err error) {
	switch job.Kind {
	case model.JobProcessVideo:
		msg := apperr.UserMessage(err, "Processing failed. Please try again later.")
		if errors.Is(err, transcript.ErrEmpty) {
			msg = "No speech could be found in this video, so there is nothing to summarize."
		}
		if serr := p.Store.SetVideoStatus(ctx, job.VideoID, model.VideoFailed, msg); serr != nil {
			p.Log.Error("set video failed", "video_id", job.VideoID, "err", serr)
		}
		v, verr := p.Store.VideoByID(ctx, job.VideoID)
		if verr == nil {
			if v.SummaryStatus == model.StageProcessing || v.SummaryStatus == model.StagePending {
				p.Store.SetSummaryStatus(ctx, job.VideoID, model.StageFailed)
			}
			if v.QuizStatus == model.StageProcessing || v.QuizStatus == model.StagePending {
				p.Store.SetQuizStatus(ctx, job.VideoID, model.StageFailed)
			}
		}
	case model.JobGenerateQuiz:
		if serr := p.Store.SetQuizStatus(ctx, job.VideoID, model.StageFailed); serr != nil {
			p.Log.Error("set quiz failed", "video_id", job.VideoID, "err", serr)
		}
	}
}

func (p *Processor) processVideo(ctx context.Context, job *model.Job, progress jobs.ProgressFunc) error {
	v, err := p.Store.VideoByID(ctx, job.VideoID)
	if err != nil {
		return err
	}
	log := p.Log.With("video_id", v.ID, "job_id", job.ID)
	if err := p.Store.SetVideoStatus(ctx, v.ID, model.VideoProcessing, ""); err != nil {
		return err
	}

	// On retries, reuse work already completed by earlier attempts.
	tr, err := p.Store.TranscriptForVideo(ctx, v.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	var segs []model.Segment
	if tr != nil {
		if segs, err = p.Store.Segments(ctx, tr.ID); err != nil {
			return err
		}
		log.Info("reusing stored transcript", "segments", len(segs))
	} else {
		progress("Preparing video", 2)
		if err := p.prepareSource(ctx, v, progress); err != nil {
			return err
		}
		progress("Extracting transcript", 20)
		segs, tr, err = p.buildTranscript(ctx, v, progress)
		if err != nil {
			return err
		}
		if err := p.Store.SaveTranscript(ctx, tr, segs); err != nil {
			return err
		}
		log.Info("transcript stored", "source", tr.Source, "words", tr.WordCount, "segments", len(segs))
	}

	// Summary
	sum, err := p.Store.SummaryForVideo(ctx, v.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if sum == nil {
		if err := p.Store.SetSummaryStatus(ctx, v.ID, model.StageProcessing); err != nil {
			return err
		}
		sum, err = p.Summarizer.Generate(ctx, summary.Input{
			Title: v.Title, Description: v.Description, Duration: v.DurationSeconds, Segments: segs,
		}, func(f float64, stage string) { progress(stage, 50+int(f*30)) })
		if err != nil {
			return fmt.Errorf("generate summary: %w", err)
		}
		sum.VideoID = v.ID
		if err := p.Store.SaveSummary(ctx, sum); err != nil {
			return err
		}
		log.Info("summary stored", "sections", len(sum.Sections), "concepts", len(sum.Concepts))
	}
	if err := p.Store.SetSummaryStatus(ctx, v.ID, model.StageReady); err != nil {
		return err
	}

	// Default quiz. Failure here does not fail the video: the user can
	// generate quizzes manually, and the reason is shown on the video page.
	progress("Generating quiz", 85)
	if err := p.Store.SetQuizStatus(ctx, v.ID, model.StageProcessing); err != nil {
		return err
	}
	q, qerr := p.Quizzer.Generate(ctx, DefaultQuiz, quiz.Material{
		Title: v.Title, Duration: v.DurationSeconds, Segments: segs, Summary: sum,
	})
	switch {
	case qerr == nil:
		q.VideoID = v.ID
		if err := p.Store.SaveQuiz(ctx, q); err != nil {
			return err
		}
		if err := p.Store.SetQuizStatus(ctx, v.ID, model.StageReady); err != nil {
			return err
		}
	case apperr.IsTemporary(qerr):
		return fmt.Errorf("generate quiz: %w", qerr)
	case ctx.Err() != nil:
		return ctx.Err()
	default:
		log.Warn("default quiz generation failed", "err", qerr)
		if err := p.Store.SetQuizStatus(ctx, v.ID, model.StageFailed); err != nil {
			return err
		}
	}

	return p.Store.SetVideoStatus(ctx, v.ID, model.VideoReady, "")
}

// prepareSource ensures local media (if any) is present and metadata is
// filled in.
func (p *Processor) prepareSource(ctx context.Context, v *model.Video, progress jobs.ProgressFunc) error {
	if v.SourceType == model.SourceURL {
		src, err := urlimport.Classify(v.SourceURL)
		if err != nil {
			return err
		}
		check := p.CheckHost
		if check == nil {
			check = urlimport.CheckHost
		}
		if err := check(ctx, src.URL); err != nil {
			return err
		}
		switch src.Kind {
		case urlimport.KindYouTube, urlimport.KindVimeo:
			meta, err := p.Fetch.OEmbed(ctx, src)
			if err != nil {
				return err
			}
			if v.Title == "" || v.Title == v.SourceURL {
				v.Title = meta.Title
			}
			if v.Description == "" && meta.Author != "" {
				v.Description = "By " + meta.Author
			}
			v.ThumbnailURL = meta.ThumbnailURL
			if err := p.Store.UpdateVideoMetadata(ctx, v); err != nil {
				return err
			}
			if v.CaptionKey == "" {
				return apperr.User(fmt.Sprintf("%s does not allow third-party apps to download its videos or transcripts. "+
					"Add the video again with its caption file (.vtt or .srt), or upload the video file directly.",
					titleCase(string(src.Kind))), nil)
			}
			return nil
		case urlimport.KindDirect:
			if v.StorageKey == "" {
				if err := p.download(ctx, v, src, progress); err != nil {
					return err
				}
			}
		}
	}
	if v.StorageKey == "" {
		return nil
	}
	return p.probe(ctx, v)
}

func (p *Processor) download(ctx context.Context, v *model.Video, src *urlimport.Source, progress jobs.ProgressFunc) error {
	ext := strings.ToLower(filepath.Ext(src.URL.Path))
	key, err := p.Files.NewKey(v.UserID, ext)
	if err != nil {
		return err
	}
	f, err := p.Files.Create(key)
	if err != nil {
		return err
	}
	ct, n, err := p.Fetch.Download(ctx, src.URL.String(), f, p.MaxUploadBytes, func(done, total int64) {
		if total > 0 {
			progress("Downloading video", 2+int(float64(done)/float64(total)*15))
		}
	})
	cerr := f.Close()
	if err == nil {
		err = cerr
	}
	if err != nil {
		p.Files.Remove(key)
		return err
	}
	if err := checkMagic(p.Files, key); err != nil {
		p.Files.Remove(key)
		return err
	}
	if ct == "" || !strings.HasPrefix(ct, "video/") {
		ct = MimeForExt(ext)
	}
	// Persist the file so retries do not download again.
	if err := p.Store.SetVideoFile(ctx, v.ID, key, n, ct); err != nil {
		p.Files.Remove(key)
		return err
	}
	v.StorageKey, v.SizeBytes, v.MimeType = key, n, ct
	return nil
}

func (p *Processor) probe(ctx context.Context, v *model.Video) error {
	path, err := p.Files.Path(v.StorageKey)
	if err != nil {
		return err
	}
	meta, err := p.Media.Probe(ctx, path)
	if errors.Is(err, media.ErrToolMissing) {
		if v.CaptionKey != "" {
			p.Log.Warn("ffprobe unavailable; continuing with captions only", "video_id", v.ID)
			return nil
		}
		return apperr.User("Video processing tools (FFmpeg) are not installed on the server.", err)
	}
	if err != nil {
		return apperr.User("The file could not be read as a video. It may be corrupted or in an unsupported format.", err)
	}
	v.DurationSeconds = meta.Duration
	v.Width, v.Height = meta.Width, meta.Height
	if v.Description == "" && meta.Comment != "" {
		v.Description = truncate(meta.Comment, 2000)
	}
	if meta.HasVideo && v.ThumbnailKey == "" {
		thumbKey, err := p.Files.NewKey(v.UserID, ".jpg")
		if err == nil {
			thumbPath, _ := p.Files.Path(thumbKey)
			at := 1.0
			if meta.Duration > 10 {
				at = meta.Duration * 0.1
			}
			if terr := p.Media.Thumbnail(ctx, path, thumbPath, at); terr == nil {
				v.ThumbnailKey = thumbKey
			} else {
				p.Log.Warn("thumbnail generation failed", "video_id", v.ID, "err", terr)
				os.Remove(thumbPath)
			}
		}
	}
	return p.Store.UpdateVideoMetadata(ctx, v)
}

// buildTranscript obtains transcript segments: user captions first, then
// embedded subtitles, then speech-to-text.
func (p *Processor) buildTranscript(ctx context.Context, v *model.Video, progress jobs.ProgressFunc) ([]model.Segment, *model.Transcript, error) {
	tr := &model.Transcript{VideoID: v.ID}
	var segs []model.Segment
	var err error

	switch {
	case v.CaptionKey != "":
		segs, err = p.captionSegments(v.CaptionKey)
		if err != nil {
			return nil, nil, err
		}
		tr.Source = "captions"
	case v.StorageKey != "":
		path, perr := p.Files.Path(v.StorageKey)
		if perr != nil {
			return nil, nil, perr
		}
		segs, err = p.embeddedSubtitles(ctx, path)
		if err != nil {
			return nil, nil, err
		}
		if segs != nil {
			tr.Source = "captions"
		} else {
			segs, tr.Language, err = p.speechToText(ctx, path, progress)
			if err != nil {
				return nil, nil, err
			}
			tr.Source = "speech_to_text"
		}
	default:
		return nil, nil, apperr.User("No video file or captions are available for this video.", nil)
	}

	segs, err = transcript.Normalize(segs)
	if err != nil {
		return nil, nil, err
	}
	tr.HasTimestamps = transcript.HasTiming(segs)
	tr.FullText = transcript.FullText(segs)
	tr.WordCount = transcript.WordCount(tr.FullText)
	return segs, tr, nil
}

func (p *Processor) captionSegments(key string) ([]model.Segment, error) {
	path, err := p.Files.Path(key)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	segs, err := transcript.ParseCaptions(data)
	if err != nil {
		return nil, apperr.User("The caption file could not be parsed. Use a WebVTT (.vtt) or SubRip (.srt) file.", err)
	}
	return segs, nil
}

// embeddedSubtitles returns nil, nil when the file has no usable text
// subtitle track.
func (p *Processor) embeddedSubtitles(ctx context.Context, path string) ([]model.Segment, error) {
	meta, err := p.Media.Probe(ctx, path)
	if err != nil {
		if errors.Is(err, media.ErrToolMissing) {
			return nil, apperr.User("Video processing tools (FFmpeg) are not installed on the server.", err)
		}
		return nil, err
	}
	stream, ok := meta.PreferredSubtitle()
	if !ok {
		if !meta.HasAudio {
			return nil, apperr.User("This video has no audio track and no subtitles, so no transcript can be produced.", nil)
		}
		return nil, nil
	}
	data, err := p.Media.ExtractSubtitles(ctx, path, stream.Index)
	if err != nil {
		p.Log.Warn("embedded subtitle extraction failed; falling back to speech-to-text", "err", err)
		return nil, nil
	}
	segs, err := transcript.ParseCaptions(data)
	if err != nil {
		return nil, nil
	}
	if _, nerr := transcript.Normalize(segs); nerr != nil {
		return nil, nil
	}
	return segs, nil
}

func (p *Processor) speechToText(ctx context.Context, path string, progress jobs.ProgressFunc) ([]model.Segment, string, error) {
	if _, disabled := p.STT.(stt.Disabled); disabled {
		return nil, "", apperr.User("This video has no captions and speech-to-text is not configured on the server. "+
			"Upload a caption file with the video, or ask the administrator to configure STT_PROVIDER.", stt.ErrNotConfigured)
	}
	dir, err := p.Files.TempDir("audio-")
	if err != nil {
		return nil, "", err
	}
	defer os.RemoveAll(dir)

	progress("Extracting audio", 22)
	chunks, err := p.Media.ExtractAudio(ctx, path, dir, AudioSegmentSeconds)
	if err != nil {
		if errors.Is(err, media.ErrToolMissing) {
			return nil, "", apperr.User("Video processing tools (FFmpeg) are not installed on the server.", err)
		}
		return nil, "", apperr.User("Audio could not be extracted from this video.", err)
	}
	var segs []model.Segment
	lang := ""
	for i, c := range chunks {
		progress(fmt.Sprintf("Transcribing audio (%d of %d)", i+1, len(chunks)), 25+int(float64(i)/float64(len(chunks))*23))
		res, err := p.STT.Transcribe(ctx, c.Path)
		if err != nil {
			return nil, "", err
		}
		if lang == "" {
			lang = res.Language
		}
		for _, s := range res.Segments {
			s.Start += c.Offset
			s.End += c.Offset
			segs = append(segs, s)
		}
	}
	return segs, lang, nil
}

// QuizJobPayload is stored on generate_quiz jobs.
type QuizJobPayload struct {
	Config model.QuizConfig `json:"config"`
}

func (p *Processor) generateQuiz(ctx context.Context, job *model.Job, progress jobs.ProgressFunc) error {
	var payload QuizJobPayload
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		return fmt.Errorf("decode quiz job payload: %w", err)
	}
	v, err := p.Store.VideoByID(ctx, job.VideoID)
	if err != nil {
		return err
	}
	tr, err := p.Store.TranscriptForVideo(ctx, v.ID)
	if err != nil {
		return apperr.User("This video has no transcript yet.", err)
	}
	segs, err := p.Store.Segments(ctx, tr.ID)
	if err != nil {
		return err
	}
	sum, err := p.Store.SummaryForVideo(ctx, v.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	m := quiz.Material{Title: v.Title, Duration: v.DurationSeconds, Segments: segs, Summary: sum}
	if payload.Config.Section >= 0 {
		if sum == nil || payload.Config.Section >= len(sum.Sections) {
			return apperr.User("The selected section no longer exists.", nil)
		}
		sec := sum.Sections[payload.Config.Section]
		m.Section = &sec
	}
	progress("Generating quiz", 20)
	q, err := p.Quizzer.Generate(ctx, payload.Config, m)
	if errors.Is(err, quiz.ErrNoQuestions) {
		return apperr.User(err.Error(), err)
	}
	if err != nil {
		return err
	}
	progress("Saving quiz", 95)
	q.VideoID = v.ID
	if err := p.Store.SaveQuiz(ctx, q); err != nil {
		return err
	}
	return p.Store.SetQuizStatus(ctx, v.ID, model.StageReady)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func titleCase(s string) string {
	switch s {
	case "youtube":
		return "YouTube"
	case "vimeo":
		return "Vimeo"
	}
	return s
}
