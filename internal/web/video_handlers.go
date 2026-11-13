package web

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/quantamsage/ai-video-summarizer/internal/apperr"
	"github.com/quantamsage/ai-video-summarizer/internal/library"
	"github.com/quantamsage/ai-video-summarizer/internal/model"
	"github.com/quantamsage/ai-video-summarizer/internal/pipeline"
	"github.com/quantamsage/ai-video-summarizer/internal/store"
	"github.com/quantamsage/ai-video-summarizer/internal/urlimport"
)

const (
	libraryPageSize    = 12
	transcriptPageSize = 60
)

// Dashboard -----------------------------------------------------------------

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	ctx := r.Context()
	stats, err := s.store.DashboardStats(ctx, u.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	recent, _, err := s.store.ListVideos(ctx, u.ID, store.VideoFilter{Limit: 6})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	attempts, err := s.store.AttemptHistory(ctx, u.ID, 0, 5)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p := s.newPage(w, r, "Dashboard", "dashboard")
	p.Data = map[string]any{"Stats": stats, "Recent": recent, "Attempts": attempts}
	s.render(w, r, http.StatusOK, "dashboard", p)
}

// Library -------------------------------------------------------------------

var validStatuses = map[string]bool{"": true, model.VideoQueued: true, model.VideoProcessing: true,
	model.VideoReady: true, model.VideoFailed: true}

func (s *Server) library(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	q := r.URL.Query()
	status := q.Get("status")
	if !validStatuses[status] {
		status = ""
	}
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	f := store.VideoFilter{
		Query:  limitLen(q.Get("q"), 200),
		Status: status,
		Oldest: q.Get("sort") == "oldest",
		Limit:  libraryPageSize,
		Offset: (page - 1) * libraryPageSize,
	}
	videos, total, err := s.store.ListVideos(r.Context(), u.ID, f)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	pages := (total + libraryPageSize - 1) / libraryPageSize
	p := s.newPage(w, r, "Video library", "library")
	p.Form = url.Values{"q": {f.Query}, "status": {status}, "sort": {q.Get("sort")}}
	p.Data = map[string]any{
		"Videos": videos, "Total": total, "Page": page, "Pages": pages,
		"PrevURL": libraryURL(p.Form, page-1), "NextURL": libraryURL(p.Form, page+1),
		"Filtered": f.Query != "" || status != "",
	}
	s.render(w, r, http.StatusOK, "library", p)
}

func libraryURL(form url.Values, page int) string {
	v := url.Values{}
	for _, k := range []string{"q", "status", "sort"} {
		if form.Get(k) != "" {
			v.Set(k, form.Get(k))
		}
	}
	if page > 1 {
		v.Set("page", strconv.Itoa(page))
	}
	if len(v) == 0 {
		return "/videos"
	}
	return "/videos?" + v.Encode()
}

// Adding videos -------------------------------------------------------------

func (s *Server) newVideoPage(w http.ResponseWriter, r *http.Request) {
	p := s.newPage(w, r, "Add a video", "new")
	p.Data = s.uploadPageData()
	if r.URL.Query().Get("mode") == "url" {
		p.Form.Set("mode", "url")
	}
	s.render(w, r, http.StatusOK, "new_video", p)
}

func (s *Server) uploadPageData() map[string]any {
	exts := make([]string, 0, len(pipeline.VideoExtensions))
	for e := range pipeline.VideoExtensions {
		exts = append(exts, e)
	}
	return map[string]any{
		"MaxUploadMB": s.cfg.MaxUploadBytes >> 20,
		"MaxBytes":    s.cfg.MaxUploadBytes,
		"Accept":      strings.Join(sortedStrings(exts), ","),
	}
}

// multipartForm streams a multipart request part by part. Files are saved
// as they arrive so large videos are never buffered in memory. The CSRF
// token must arrive (as a header or the first form field) before any file.
type multipartForm struct {
	fields     url.Values
	videoKey   string
	videoSize  int64
	videoMime  string
	videoName  string
	captionKey string
}

func (s *Server) readUploadForm(w http.ResponseWriter, r *http.Request, allowVideo bool) (*multipartForm, error) {
	u := userFrom(r)
	// Allow some overhead beyond the video for captions and form fields.
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes+pipeline.MaxCaptionBytes+(1<<20))
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, apperr.User("The upload could not be read. Please try again.", err)
	}
	form := &multipartForm{fields: url.Values{}}
	csrfOK := s.verifyCSRF(r, r.Header.Get(csrfHeader))
	cleanup := func() {
		s.files.Remove(form.videoKey)
		s.files.Remove(form.captionKey)
	}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			cleanup()
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				return nil, apperr.User(fmt.Sprintf("The upload is larger than the %d MB limit.", s.cfg.MaxUploadBytes>>20), pipeline.ErrUploadTooLarge)
			}
			return nil, apperr.User("The upload was interrupted before it finished. Please try again.", err)
		}
		name := part.FormName()
		if part.FileName() == "" {
			val, err := io.ReadAll(io.LimitReader(part, 64<<10))
			if err != nil {
				cleanup()
				return nil, apperr.User("The upload was interrupted before it finished. Please try again.", err)
			}
			if name == csrfField {
				csrfOK = csrfOK || s.verifyCSRF(r, string(val))
			}
			form.fields.Set(name, string(val))
			continue
		}
		if !csrfOK {
			cleanup()
			return nil, errCSRF
		}
		switch name {
		case "video":
			if !allowVideo || form.videoKey != "" {
				cleanup()
				return nil, apperr.User("Unexpected file in upload.", nil)
			}
			form.videoName = filepath.Base(part.FileName())
			form.videoKey, form.videoSize, form.videoMime, err = pipeline.SaveVideoUpload(s.files, u.ID, form.videoName, part, s.cfg.MaxUploadBytes)
		case "captions":
			if form.captionKey != "" {
				cleanup()
				return nil, apperr.User("Only one caption file can be attached.", nil)
			}
			form.captionKey, err = pipeline.SaveCaptionUpload(s.files, u.ID, part.FileName(), part)
		default:
			_, err = io.Copy(io.Discard, part)
		}
		if err != nil {
			cleanup()
			return nil, err
		}
	}
	if !csrfOK {
		cleanup()
		return nil, errCSRF
	}
	return form, nil
}

var errCSRF = errors.New("csrf token missing or invalid")

func (s *Server) uploadError(w http.ResponseWriter, r *http.Request, mode string, form url.Values, err error) {
	if errors.Is(err, errCSRF) {
		s.renderError(w, r, http.StatusForbidden, "Your session form expired. Please refresh the page and try again.")
		return
	}
	msg := apperr.UserMessage(err, "")
	if msg == "" {
		s.serverError(w, r, err)
		return
	}
	status := http.StatusUnprocessableEntity
	if errors.Is(err, pipeline.ErrUploadTooLarge) {
		status = http.StatusRequestEntityTooLarge
	}
	if wantsJSON(r) {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	p := s.newPage(w, r, "Add a video", "new")
	p.Data = s.uploadPageData()
	if form != nil {
		p.Form = form
	}
	p.Form.Set("mode", mode)
	p.Errors["form"] = msg
	s.render(w, r, status, "new_video", p)
}

func (s *Server) uploadVideo(w http.ResponseWriter, r *http.Request) {
	form, err := s.readUploadForm(w, r, true)
	if err != nil {
		s.uploadError(w, r, "upload", nil, err)
		return
	}
	if form.videoKey == "" {
		s.files.Remove(form.captionKey)
		s.uploadError(w, r, "upload", form.fields, apperr.User("Choose a video file to upload.", nil))
		return
	}
	title, desc := strings.TrimSpace(form.fields.Get("title")), strings.TrimSpace(form.fields.Get("description"))
	if errs := library.ValidateDetails(title, desc); len(errs) > 0 {
		s.files.Remove(form.videoKey)
		s.files.Remove(form.captionKey)
		s.uploadError(w, r, "upload", form.fields, apperr.User(firstValue(errs), nil))
		return
	}
	v, err := s.lib.AddUpload(r.Context(), userFrom(r).ID, library.NewVideo{
		Title: title, Description: desc, OriginalFilename: form.videoName,
		StorageKey: form.videoKey, SizeBytes: form.videoSize, MimeType: form.videoMime, CaptionKey: form.captionKey,
	})
	if err != nil {
		s.files.Remove(form.videoKey)
		s.files.Remove(form.captionKey)
		s.serverError(w, r, err)
		return
	}
	s.finishAdd(w, r, v)
}

func (s *Server) submitURL(w http.ResponseWriter, r *http.Request) {
	var fields url.Values
	var captionKey string
	if isMultipart(r) {
		form, err := s.readUploadForm(w, r, false)
		if err != nil {
			s.uploadError(w, r, "url", nil, err)
			return
		}
		fields, captionKey = form.fields, form.captionKey
	} else {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		if err := r.ParseForm(); err != nil {
			s.uploadError(w, r, "url", nil, apperr.User("The form could not be read.", err))
			return
		}
		fields = r.PostForm
	}
	title, desc := strings.TrimSpace(fields.Get("title")), strings.TrimSpace(fields.Get("description"))
	if errs := library.ValidateDetails(title, desc); len(errs) > 0 {
		s.files.Remove(captionKey)
		s.uploadError(w, r, "url", fields, apperr.User(firstValue(errs), nil))
		return
	}
	v, err := s.lib.AddURL(r.Context(), userFrom(r).ID, library.NewVideo{
		Title: title, Description: desc, SourceURL: fields.Get("url"), CaptionKey: captionKey,
	})
	if err != nil {
		s.files.Remove(captionKey)
		s.uploadError(w, r, "url", fields, err)
		return
	}
	s.finishAdd(w, r, v)
}

func (s *Server) finishAdd(w http.ResponseWriter, r *http.Request, v *model.Video) {
	to := fmt.Sprintf("/videos/%d", v.ID)
	s.setFlash(w, "success", "Video added. Processing has started — you can leave this page and come back later.")
	if wantsJSON(r) {
		writeJSON(w, http.StatusCreated, map[string]any{"id": v.ID, "redirect": to})
		return
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// Video page ----------------------------------------------------------------

func (s *Server) loadVideo(w http.ResponseWriter, r *http.Request) (*model.Video, bool) {
	id, err := pathID(r)
	if err != nil {
		s.notFound(w, r)
		return nil, false
	}
	v, err := s.store.VideoForUser(r.Context(), userFrom(r).ID, id)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return nil, false
	}
	if err != nil {
		s.serverError(w, r, err)
		return nil, false
	}
	return v, true
}

// videoView is the data for the video detail page.
type videoView struct {
	Video          *model.Video
	Job            *model.Job
	QuizJob        *model.Job
	Transcript     *model.Transcript
	Segments       []model.Segment
	SegTotal       int
	SegPage        int
	SegPages       int
	SegQuery       string
	SegPrevURL     string
	SegNextURL     string
	Summary        *model.Summary
	Quizzes        []*model.Quiz
	EmbedURL       string
	EmbedBase      string
	EmbedKind      string
	MediaURL       string
	StartAt        int
	QuestionTypes  []string
	Difficulties   []string
	DetailedBlocks []detailBlock
	SummaryText    string
}

type detailBlock struct {
	Heading    string
	Paragraphs []string
}

func (s *Server) videoPage(w http.ResponseWriter, r *http.Request) {
	v, ok := s.loadVideo(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	view := &videoView{Video: v, QuestionTypes: model.QuestionTypes, Difficulties: model.Difficulties}
	q := r.URL.Query()
	if t, err := strconv.Atoi(q.Get("t")); err == nil && t > 0 {
		view.StartAt = t
	}
	if j, err := s.store.LatestJob(ctx, v.ID, model.JobProcessVideo); err == nil {
		view.Job = j
	}
	if j, err := s.store.LatestJob(ctx, v.ID, model.JobGenerateQuiz); err == nil {
		view.QuizJob = j
	}
	if v.Playable() {
		view.MediaURL = fmt.Sprintf("/videos/%d/media", v.ID)
		if view.StartAt > 0 {
			view.MediaURL += fmt.Sprintf("#t=%d", view.StartAt)
		}
	} else if v.SourceURL != "" {
		if src, err := urlimport.Classify(v.SourceURL); err == nil && src.NeedsCaptions() {
			view.EmbedKind = string(src.Kind)
			view.EmbedBase = urlimport.EmbedURL(v.SourceURL, 0)
			view.EmbedURL = urlimport.EmbedURL(v.SourceURL, view.StartAt)
		}
	}

	tr, err := s.store.TranscriptForVideo(ctx, v.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.serverError(w, r, err)
		return
	}
	if tr != nil {
		view.Transcript = tr
		view.SegQuery = limitLen(strings.TrimSpace(q.Get("tq")), 200)
		page, _ := strconv.Atoi(q.Get("tpage"))
		if page < 1 {
			page = 1
		}
		segs, total, err := s.store.SegmentsPage(ctx, tr.ID, view.SegQuery, (page-1)*transcriptPageSize, transcriptPageSize)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		view.Segments, view.SegTotal, view.SegPage = segs, total, page
		view.SegPages = (total + transcriptPageSize - 1) / transcriptPageSize
		view.SegPrevURL = transcriptURL(v.ID, view.SegQuery, page-1)
		view.SegNextURL = transcriptURL(v.ID, view.SegQuery, page+1)
	}

	sum, err := s.store.SummaryForVideo(ctx, v.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.serverError(w, r, err)
		return
	}
	if sum != nil {
		view.Summary = sum
		view.DetailedBlocks = parseDetailed(sum.Detailed)
		view.SummaryText = summaryMarkdown(v, sum)
	}
	if view.Quizzes, err = s.store.QuizzesForVideo(ctx, v.ID); err != nil {
		s.serverError(w, r, err)
		return
	}

	p := s.newPage(w, r, v.Title, "library")
	p.Data = view
	s.render(w, r, http.StatusOK, "video", p)
}

func transcriptURL(videoID int64, query string, page int) string {
	v := url.Values{}
	if query != "" {
		v.Set("tq", query)
	}
	if page > 1 {
		v.Set("tpage", strconv.Itoa(page))
	}
	u := fmt.Sprintf("/videos/%d", videoID)
	if len(v) > 0 {
		u += "?" + v.Encode()
	}
	return u + "#transcript"
}

// parseDetailed splits the stored detailed summary ("## Heading" lines
// followed by paragraphs) into blocks for rendering.
func parseDetailed(s string) []detailBlock {
	var blocks []detailBlock
	cur := detailBlock{}
	for _, para := range paragraphs(s) {
		if strings.HasPrefix(para, "## ") {
			if cur.Heading != "" || len(cur.Paragraphs) > 0 {
				blocks = append(blocks, cur)
			}
			cur = detailBlock{Heading: strings.TrimPrefix(para, "## ")}
			continue
		}
		cur.Paragraphs = append(cur.Paragraphs, para)
	}
	if cur.Heading != "" || len(cur.Paragraphs) > 0 {
		blocks = append(blocks, cur)
	}
	return blocks
}

// summaryMarkdown renders the full summary as a Markdown document for
// copying and downloading.
func summaryMarkdown(v *model.Video, sum *model.Summary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n## Overview\n\n%s\n\n", v.Title, sum.Overview)
	if sum.Detailed != "" {
		fmt.Fprintf(&b, "%s\n\n", sum.Detailed)
	}
	if len(sum.KeyTakeaways) > 0 {
		b.WriteString("## Key takeaways\n\n")
		for _, k := range sum.KeyTakeaways {
			fmt.Fprintf(&b, "- %s\n", k)
		}
		b.WriteString("\n")
	}
	if len(sum.Concepts) > 0 {
		b.WriteString("## Important concepts\n\n")
		for _, c := range sum.Concepts {
			fmt.Fprintf(&b, "- **%s** (%s): %s\n", c.Term, c.Category, c.Definition)
		}
		b.WriteString("\n")
	}
	if len(sum.Sections) > 0 {
		b.WriteString("## Topic breakdown\n\n")
		for _, sec := range sum.Sections {
			ts := ""
			if sec.Start != nil {
				ts = "[" + formatClock(*sec.Start) + "] "
			}
			fmt.Fprintf(&b, "- %s**%s** — %s\n", ts, sec.Title, sec.Content)
		}
		b.WriteString("\n")
	}
	if len(sum.LearningNotes) > 0 {
		b.WriteString("## Additional learning notes\n\n")
		for _, n := range sum.LearningNotes {
			label := ""
			if n.IsExternal {
				label = " _(additional context, not from the video)_"
			}
			fmt.Fprintf(&b, "- %s%s\n", n.Note, label)
		}
	}
	return b.String()
}

func (s *Server) videoStatus(w http.ResponseWriter, r *http.Request) {
	v, ok := s.loadVideo(w, r)
	if !ok {
		return
	}
	resp := map[string]any{
		"status": v.Status, "summary_status": v.SummaryStatus, "quiz_status": v.QuizStatus,
		"error": v.ErrorMessage, "stage": "", "progress": 0,
	}
	if j, err := s.store.LatestJob(r.Context(), v.ID, model.JobProcessVideo); err == nil {
		resp["stage"], resp["progress"] = j.Stage, j.Progress
		if j.Status == model.JobQueued && j.ErrorMessage != "" {
			resp["stage"] = j.ErrorMessage
		}
	}
	if j, err := s.store.ActiveJob(r.Context(), v.ID, model.JobGenerateQuiz); err == nil {
		resp["quiz_stage"], resp["quiz_progress"] = j.Stage, j.Progress
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) videoMedia(w http.ResponseWriter, r *http.Request) {
	v, ok := s.loadVideo(w, r)
	if !ok {
		return
	}
	s.serveStored(w, r, v.StorageKey, v.MimeType)
}

func (s *Server) videoThumbnail(w http.ResponseWriter, r *http.Request) {
	v, ok := s.loadVideo(w, r)
	if !ok {
		return
	}
	s.serveStored(w, r, v.ThumbnailKey, "image/jpeg")
}

// serveStored streams a stored file with range support.
func (s *Server) serveStored(w http.ResponseWriter, r *http.Request, key, contentType string) {
	if key == "" {
		s.notFound(w, r)
		return
	}
	path, err := s.files.Path(key)
	if err != nil {
		s.notFound(w, r)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		s.notFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("Content-Disposition", "inline")
	http.ServeContent(w, r, "", st.ModTime(), f)
}

func (s *Server) downloadSummary(w http.ResponseWriter, r *http.Request) {
	v, ok := s.loadVideo(w, r)
	if !ok {
		return
	}
	sum, err := s.store.SummaryForVideo(r.Context(), v.ID)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-summary.md"`, slug(v.Title)))
	io.WriteString(w, summaryMarkdown(v, sum))
}

func (s *Server) downloadTranscript(w http.ResponseWriter, r *http.Request) {
	v, ok := s.loadVideo(w, r)
	if !ok {
		return
	}
	tr, err := s.store.TranscriptForVideo(r.Context(), v.ID)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	segs, err := s.store.Segments(r.Context(), tr.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-transcript.txt"`, slug(v.Title)))
	for _, sg := range segs {
		if tr.HasTimestamps {
			fmt.Fprintf(w, "[%s] %s\n", formatClock(sg.Start), sg.Text)
		} else {
			fmt.Fprintln(w, sg.Text)
		}
	}
}

func (s *Server) deleteVideo(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.notFound(w, r)
		return
	}
	err = s.lib.Delete(r.Context(), userFrom(r).ID, id)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.redirectWithFlash(w, r, "/videos", "success", "The video and its learning materials were deleted.")
}

func (s *Server) reprocessVideo(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.notFound(w, r)
		return
	}
	to := fmt.Sprintf("/videos/%d", id)
	err = s.lib.Reprocess(r.Context(), userFrom(r).ID, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.notFound(w, r)
	case err != nil && apperr.UserMessage(err, "") != "":
		s.redirectWithFlash(w, r, to, "error", apperr.UserMessage(err, ""))
	case err != nil:
		s.serverError(w, r, err)
	default:
		s.redirectWithFlash(w, r, to, "success", "Processing restarted.")
	}
}

func (s *Server) generateQuiz(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.notFound(w, r)
		return
	}
	to := fmt.Sprintf("/videos/%d#quizzes", id)
	count, _ := strconv.Atoi(r.PostFormValue("count"))
	section := -1
	if sv := r.PostFormValue("section"); sv != "" && sv != "full" {
		if n, err := strconv.Atoi(sv); err == nil && n >= 0 {
			section = n
		}
	}
	cfg := model.QuizConfig{
		Count:      count,
		Difficulty: r.PostFormValue("difficulty"),
		Types:      r.PostForm["types"],
		Section:    section,
	}
	err = s.lib.RequestQuiz(r.Context(), userFrom(r).ID, id, cfg)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.notFound(w, r)
	case err != nil && apperr.UserMessage(err, "") != "":
		s.redirectWithFlash(w, r, to, "error", apperr.UserMessage(err, ""))
	case err != nil:
		s.serverError(w, r, err)
	default:
		s.redirectWithFlash(w, r, to, "success", "Generating your quiz. It will appear here in a minute or two.")
	}
}

// helpers -----------------------------------------------------------------------

func limitLen(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

func firstValue(m map[string]string) string {
	for _, v := range m {
		return v
	}
	return ""
}

func sortedStrings(s []string) []string {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
	return s
}

func slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= 60 {
			break
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "video"
	}
	return out
}
