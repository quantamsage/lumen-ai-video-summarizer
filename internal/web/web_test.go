package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/quantamsage/ai-video-summarizer/internal/ai/aitest"
	"github.com/quantamsage/ai-video-summarizer/internal/auth"
	"github.com/quantamsage/ai-video-summarizer/internal/config"
	"github.com/quantamsage/ai-video-summarizer/internal/db"
	"github.com/quantamsage/ai-video-summarizer/internal/library"
	"github.com/quantamsage/ai-video-summarizer/internal/model"
	"github.com/quantamsage/ai-video-summarizer/internal/quiz"
	"github.com/quantamsage/ai-video-summarizer/internal/storage"
	"github.com/quantamsage/ai-video-summarizer/internal/store"
	"github.com/quantamsage/ai-video-summarizer/internal/urlimport"
)

var mp4Header = append([]byte{0, 0, 0, 0x18}, []byte("ftypmp42isom")...)

type nopNotifier struct{ n int }

func (n *nopNotifier) Notify() { n.n++ }

type testApp struct {
	t     *testing.T
	srv   *httptest.Server
	store *store.Store
	files *storage.Local
	ai    *aitest.Mock
}

func newTestApp(t *testing.T) *testApp {
	t.Helper()
	conn, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := db.Migrate(context.Background(), conn, nil); err != nil {
		t.Fatal(err)
	}
	st := store.New(conn)
	files, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		BaseURL: "http://example.test", SessionSecret: []byte(strings.Repeat("s", 32)),
		SessionTTL: time.Hour, MaxUploadBytes: 2048, JobMaxAttempts: 3,
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mock := &aitest.Mock{}
	lib := &library.Service{Store: st, Files: files, Jobs: &nopNotifier{}, MaxAttempts: 3, Log: log,
		CheckHost: func(_ context.Context, src *urlimport.Source) error {
			if strings.Contains(src.URL.Host, "internal") {
				return urlimport.ErrBlockedHost
			}
			return nil
		}}
	s, err := New(Deps{Config: cfg, Log: log, Store: st, Auth: auth.NewService(st, time.Hour),
		Library: lib, Files: files, Grader: &quiz.Grader{AI: mock}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return &testApp{t: t, srv: ts, store: st, files: files, ai: mock}
}

// client is a browser-like client with its own cookie jar.
type client struct {
	app  *testApp
	http *http.Client
	csrf string
}

func (a *testApp) client() *client {
	jar, _ := cookiejar.New(nil)
	return &client{app: a, http: &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

var csrfMeta = regexp.MustCompile(`name="csrf-token" content="([^"]+)"`)

func (c *client) get(path string) (*http.Response, string) {
	c.app.t.Helper()
	resp, err := c.http.Get(c.app.srv.URL + path)
	if err != nil {
		c.app.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if m := csrfMeta.FindSubmatch(b); m != nil {
		c.csrf = string(m[1])
	}
	return resp, string(b)
}

func (c *client) post(path string, form url.Values) (*http.Response, string) {
	c.app.t.Helper()
	if c.csrf == "" {
		c.get("/login")
	}
	if form == nil {
		form = url.Values{}
	}
	if form.Get(csrfField) == "" {
		form.Set(csrfField, c.csrf)
	}
	return c.do(http.MethodPost, path, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()), nil)
}

func (c *client) do(method, path, ct string, body io.Reader, headers map[string]string) (*http.Response, string) {
	c.app.t.Helper()
	req, _ := http.NewRequest(method, c.app.srv.URL+path, body)
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.app.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func (c *client) register(email string) {
	c.app.t.Helper()
	c.get("/register")
	resp, body := c.post("/register", url.Values{"email": {email}, "name": {"Tester"},
		"password": {"a long password"}, "confirm": {"a long password"}})
	if resp.StatusCode != http.StatusSeeOther {
		c.app.t.Fatalf("register: %d %s", resp.StatusCode, body)
	}
	c.get("/dashboard") // refresh CSRF token for the signed-in session
}

func (c *client) userID(email string) int64 {
	u, err := c.app.store.UserByEmail(context.Background(), email)
	if err != nil {
		c.app.t.Fatal(err)
	}
	return u.ID
}

func multipartBody(t *testing.T, fields map[string]string, files map[string][2]string) (string, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, k := range []string{csrfField, "title", "description", "url"} {
		if v, ok := fields[k]; ok {
			w.WriteField(k, v)
		}
	}
	for name, f := range files {
		fw, _ := w.CreateFormFile(name, f[0])
		fw.Write([]byte(f[1]))
	}
	w.Close()
	return w.FormDataContentType(), &buf
}

func (a *testApp) seedVideo(userID int64, title string) *model.Video {
	a.t.Helper()
	key, _ := a.files.NewKey(userID, ".mp4")
	f, _ := a.files.Create(key)
	f.Write(mp4Header)
	f.Close()
	v := &model.Video{UserID: userID, SourceType: model.SourceUpload, Title: title, StorageKey: key, MimeType: "video/mp4"}
	if err := a.store.CreateVideoWithJob(context.Background(), v, 3); err != nil {
		a.t.Fatal(err)
	}
	return v
}

func (a *testApp) seedReady(userID int64) (*model.Video, *model.Quiz) {
	a.t.Helper()
	ctx := context.Background()
	v := a.seedVideo(userID, "Go Concurrency")
	j, _ := a.store.ClaimJob(ctx)
	a.store.CompleteJob(ctx, j.ID)
	a.store.SetVideoStatus(ctx, v.ID, model.VideoReady, "")
	a.store.SetSummaryStatus(ctx, v.ID, model.StageReady)
	a.store.SetQuizStatus(ctx, v.ID, model.StageReady)
	tr := &model.Transcript{VideoID: v.ID, Source: "captions", HasTimestamps: true, FullText: "goroutines are cheap", WordCount: 3}
	a.store.SaveTranscript(ctx, tr, []model.Segment{{Start: 0, End: 4, Text: "goroutines are cheap"}, {Start: 4, End: 9, Text: "channels connect them"}})
	start := 0.0
	a.store.SaveSummary(ctx, &model.Summary{VideoID: v.ID, Overview: "OVERVIEW-TEXT about goroutines.",
		Detailed: "## Basics\n\nDetailed paragraph.", KeyTakeaways: []string{"Takeaway one"},
		Concepts: []model.Concept{{Term: "Goroutine", Definition: "Lightweight thread", Category: "term"}},
		Sections: []model.Section{{Title: "Intro", Content: "Intro content", Start: &start}}})
	ts := 4.0
	q := &model.Quiz{VideoID: v.ID, Title: "Medium quiz", Difficulty: "medium", Scope: "full", RequestedCount: 2,
		Questions: []model.Question{
			{Type: model.SingleChoice, Prompt: "Which keyword starts a goroutine?", Explanation: "go starts goroutines", Timestamp: &ts,
				Options: []model.Option{{Text: "go", IsCorrect: true, Explanation: "right"}, {Text: "run"}, {Text: "spawn"}}},
			{Type: model.ShortAnswer, Prompt: "What connects goroutines?", Explanation: "channels", AcceptedAnswers: []string{"channels"}},
		}}
	if err := a.store.SaveQuiz(ctx, q); err != nil {
		a.t.Fatal(err)
	}
	return v, q
}

// ---------------------------------------------------------------------------

func TestTemplatesParse(t *testing.T) {
	tmpl, err := loadTemplates()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"home", "login", "register", "forgot", "reset", "error", "dashboard",
		"library", "new_video", "video", "quiz", "results", "history", "settings"} {
		if tmpl[name] == nil {
			t.Errorf("template %s missing", name)
		}
	}
}

func TestAnonymousAccessAndSecurityHeaders(t *testing.T) {
	app := newTestApp(t)
	c := app.client()
	resp, body := c.get("/")
	if resp.StatusCode != 200 || !strings.Contains(body, "Get started") {
		t.Fatalf("home: %d", resp.StatusCode)
	}
	for _, h := range []string{"Content-Security-Policy", "X-Frame-Options", "X-Content-Type-Options"} {
		if resp.Header.Get(h) == "" {
			t.Errorf("missing security header %s", h)
		}
	}
	for _, p := range []string{"/dashboard", "/videos", "/videos/1", "/settings", "/quizzes/1", "/attempts/1"} {
		resp, _ := c.get(p)
		if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/login?next=") {
			t.Errorf("%s should redirect to login, got %d %s", p, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	resp, _ = c.get("/does-not-exist")
	if resp.StatusCode != 404 {
		t.Errorf("unknown path: %d", resp.StatusCode)
	}
}

func TestRegisterLoginLogout(t *testing.T) {
	app := newTestApp(t)
	c := app.client()
	c.register("alice@example.com")
	resp, body := c.get("/dashboard")
	if resp.StatusCode != 200 || !strings.Contains(body, "Welcome back, Tester") {
		t.Fatalf("dashboard after register: %d", resp.StatusCode)
	}
	resp, _ = c.post("/logout", nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	resp, _ = c.get("/dashboard")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatal("session should be gone after logout")
	}

	resp, body = c.post("/login", url.Values{"email": {"alice@example.com"}, "password": {"wrong password"}})
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "Incorrect email or password") {
		t.Fatalf("bad login: %d", resp.StatusCode)
	}
	resp, _ = c.post("/login", url.Values{"email": {"alice@example.com"}, "password": {"a long password"}, "next": {"//evil.example/x"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/dashboard" {
		t.Fatalf("open redirect not prevented: %s", resp.Header.Get("Location"))
	}
	resp, _ = c.get("/register")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatal("signed-in users should be redirected away from register")
	}
}

func TestRegisterValidationErrors(t *testing.T) {
	app := newTestApp(t)
	c := app.client()
	c.get("/register")
	resp, body := c.post("/register", url.Values{"email": {"bad"}, "name": {"x"}, "password": {"short"}, "confirm": {"short"}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "valid email") {
		t.Fatalf("expected validation errors: %d", resp.StatusCode)
	}
}

func TestCSRFProtection(t *testing.T) {
	app := newTestApp(t)
	c := app.client()
	c.register("a@example.com")
	resp, _ := c.post("/videos/1/delete", url.Values{csrfField: {"forged"}})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("forged token should be rejected: %d", resp.StatusCode)
	}
	resp, _ = c.do(http.MethodPost, "/logout", "application/x-www-form-urlencoded",
		strings.NewReader(url.Values{csrfField: {c.csrf}}.Encode()), map[string]string{"Origin": "https://evil.example"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin post should be rejected: %d", resp.StatusCode)
	}
	// Multipart upload without any token.
	ct, body := multipartBody(t, map[string]string{"title": "x"}, map[string][2]string{"video": {"a.mp4", string(mp4Header)}})
	resp, _ = c.do(http.MethodPost, "/videos/upload", ct, body, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("upload without CSRF should be rejected: %d", resp.StatusCode)
	}
	entries, _ := filepath.Glob(filepath.Join(app.files.Root, "u*", "*"))
	if len(entries) != 0 {
		t.Fatal("rejected upload must not store files")
	}
}

func TestLoginRateLimit(t *testing.T) {
	app := newTestApp(t)
	c := app.client()
	c.get("/login")
	var last int
	for i := 0; i < 15; i++ {
		resp, _ := c.post("/login", url.Values{"email": {"x@example.com"}, "password": {"nope nope nope"}})
		last = resp.StatusCode
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("expected rate limiting, last status %d", last)
	}
}

func TestVideoUpload(t *testing.T) {
	app := newTestApp(t)
	c := app.client()
	c.register("a@example.com")

	ct, body := multipartBody(t, map[string]string{csrfField: c.csrf, "title": "My lecture"},
		map[string][2]string{"video": {"lecture.mp4", string(mp4Header) + "data"}})
	resp, _ := c.do(http.MethodPost, "/videos/upload", ct, body, nil)
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/videos/") {
		t.Fatalf("upload: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	videos, total, _ := app.store.ListVideos(context.Background(), c.userID("a@example.com"), store.VideoFilter{})
	if total != 1 || videos[0].Title != "My lecture" || videos[0].Status != model.VideoQueued {
		t.Fatalf("video not stored: %+v", videos)
	}
	if j, err := app.store.LatestJob(context.Background(), videos[0].ID, model.JobProcessVideo); err != nil || j.Status != model.JobQueued {
		t.Fatalf("processing job not queued: %v", err)
	}

	// JSON client (XHR) with header token.
	ct, body = multipartBody(t, map[string]string{"title": ""}, map[string][2]string{"video": {"talk_intro.webm", "\x1a\x45\xdf\xa3rest"}})
	resp, raw := c.do(http.MethodPost, "/videos/upload", ct, body, map[string]string{"X-CSRF-Token": c.csrf, "Accept": "application/json"})
	if resp.StatusCode != http.StatusCreated || !strings.Contains(raw, "redirect") {
		t.Fatalf("json upload: %d %s", resp.StatusCode, raw)
	}

	// Oversized upload (limit is 2048 bytes in tests).
	ct, body = multipartBody(t, map[string]string{csrfField: c.csrf}, map[string][2]string{"video": {"big.mp4", string(mp4Header) + strings.Repeat("x", 4096)}})
	resp, raw = c.do(http.MethodPost, "/videos/upload", ct, body, map[string]string{"Accept": "application/json"})
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized upload: %d %s", resp.StatusCode, raw)
	}

	// Wrong type.
	ct, body = multipartBody(t, map[string]string{csrfField: c.csrf}, map[string][2]string{"video": {"notes.pdf", "%PDF-1.4"}})
	resp, raw = c.do(http.MethodPost, "/videos/upload", ct, body, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(raw, "Unsupported file type") {
		t.Fatalf("invalid type: %d", resp.StatusCode)
	}

	// Renamed non-video.
	ct, body = multipartBody(t, map[string]string{csrfField: c.csrf}, map[string][2]string{"video": {"fake.mp4", "<html>hi</html>"}})
	resp, _ = c.do(http.MethodPost, "/videos/upload", ct, body, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("fake video: %d", resp.StatusCode)
	}
	_, total, _ = app.store.ListVideos(context.Background(), c.userID("a@example.com"), store.VideoFilter{})
	if total != 2 {
		t.Fatalf("rejected uploads must not create videos: %d", total)
	}
}

func TestVideoURLSubmission(t *testing.T) {
	app := newTestApp(t)
	c := app.client()
	c.register("a@example.com")

	resp, body := c.post("/videos/url", url.Values{"url": {"https://www.youtube.com/watch?v=dQw4w9WgXcQ"}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "caption file") {
		t.Fatalf("youtube without captions should explain: %d", resp.StatusCode)
	}
	resp, body = c.post("/videos/url", url.Values{"url": {"http://internal.corp/a.mp4"}})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("internal host should be rejected: %d", resp.StatusCode)
	}
	resp, body = c.post("/videos/url", url.Values{"url": {"https://example.com/page.html"}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "upload the video directly") {
		t.Fatalf("unsupported page should explain alternative: %d", resp.StatusCode)
	}
	resp, _ = c.post("/videos/url", url.Values{"url": {"https://cdn.example.com/lectures/week_1.mp4"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("direct link: %d", resp.StatusCode)
	}
	vids, _, _ := app.store.ListVideos(context.Background(), c.userID("a@example.com"), store.VideoFilter{})
	if len(vids) != 1 || vids[0].Title != "week 1" || vids[0].SourceType != model.SourceURL {
		t.Fatalf("url video: %+v", vids)
	}

	// YouTube with a caption file via multipart.
	ct, mb := multipartBody(t, map[string]string{csrfField: c.csrf, "url": "https://youtu.be/dQw4w9WgXcQ"},
		map[string][2]string{"captions": {"c.vtt", "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nhello\n"}})
	resp, _ = c.do(http.MethodPost, "/videos/url", ct, mb, nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("youtube with captions: %d", resp.StatusCode)
	}
}

func TestAuthorizationBetweenUsers(t *testing.T) {
	app := newTestApp(t)
	alice := app.client()
	alice.register("alice@example.com")
	bob := app.client()
	bob.register("bob@example.com")
	v, q := app.seedReady(alice.userID("alice@example.com"))

	resp, _ := alice.get(fmt.Sprintf("/videos/%d", v.ID))
	if resp.StatusCode != 200 {
		t.Fatalf("owner should see video: %d", resp.StatusCode)
	}
	for _, p := range []string{
		fmt.Sprintf("/videos/%d", v.ID), fmt.Sprintf("/videos/%d/status", v.ID),
		fmt.Sprintf("/videos/%d/media", v.ID), fmt.Sprintf("/videos/%d/summary.md", v.ID),
		fmt.Sprintf("/videos/%d/transcript.txt", v.ID), fmt.Sprintf("/quizzes/%d", q.ID),
	} {
		resp, _ := bob.get(p)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("bob GET %s: expected 404, got %d", p, resp.StatusCode)
		}
	}
	resp, _ = bob.post(fmt.Sprintf("/videos/%d/delete", v.ID), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("bob delete: %d", resp.StatusCode)
	}
	resp, _ = bob.post(fmt.Sprintf("/quizzes/%d/submit", q.ID), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("bob submit: %d", resp.StatusCode)
	}
	resp, _ = bob.post(fmt.Sprintf("/videos/%d/quizzes", v.ID), url.Values{"count": {"5"}, "difficulty": {"easy"}, "types": {"true_false"}})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("bob generate quiz: %d", resp.StatusCode)
	}
	if _, err := app.store.VideoForUser(context.Background(), alice.userID("alice@example.com"), v.ID); err != nil {
		t.Fatal("alice's video should still exist")
	}

	// Bob cannot read Alice's attempt.
	alice.get(fmt.Sprintf("/quizzes/%d", q.ID))
	resp, _ = alice.post(fmt.Sprintf("/quizzes/%d/submit", q.ID), url.Values{})
	loc := resp.Header.Get("Location")
	resp, _ = bob.get(loc)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("bob attempt view: %d", resp.StatusCode)
	}
}

func TestSummaryAndTranscriptRetrieval(t *testing.T) {
	app := newTestApp(t)
	c := app.client()
	c.register("a@example.com")
	v, _ := app.seedReady(c.userID("a@example.com"))

	resp, body := c.get(fmt.Sprintf("/videos/%d", v.ID))
	if resp.StatusCode != 200 {
		t.Fatalf("video page: %d", resp.StatusCode)
	}
	for _, want := range []string{"OVERVIEW-TEXT", "Takeaway one", "Goroutine", "Intro", "channels connect them", `data-seek="4"`} {
		if !strings.Contains(body, want) {
			t.Errorf("video page missing %q", want)
		}
	}
	resp, body = c.get(fmt.Sprintf("/videos/%d?tq=channels", v.ID))
	if !strings.Contains(body, "1 matching segment") {
		t.Errorf("transcript search not applied")
	}
	resp, body = c.get(fmt.Sprintf("/videos/%d/summary.md", v.ID))
	if resp.StatusCode != 200 || !strings.Contains(body, "## Key takeaways") || !strings.Contains(resp.Header.Get("Content-Disposition"), "go-concurrency-summary.md") {
		t.Fatalf("summary download: %d %q", resp.StatusCode, resp.Header.Get("Content-Disposition"))
	}
	resp, body = c.get(fmt.Sprintf("/videos/%d/transcript.txt", v.ID))
	if !strings.Contains(body, "[0:04] channels connect them") {
		t.Fatalf("transcript download: %q", body)
	}
	resp, body = c.get(fmt.Sprintf("/videos/%d/status", v.ID))
	var st map[string]any
	if err := json.Unmarshal([]byte(body), &st); err != nil || st["status"] != "ready" {
		t.Fatalf("status json: %s", body)
	}
	resp, _ = c.get(fmt.Sprintf("/videos/%d/media", v.ID))
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "video/mp4" {
		t.Fatalf("media: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	resp, body = c.get("/videos?q=concurrency&status=ready")
	if !strings.Contains(body, "Go Concurrency") {
		t.Fatal("library search failed")
	}
	resp, body = c.get("/dashboard")
	if !strings.Contains(body, "Go Concurrency") {
		t.Fatal("dashboard should list recent video")
	}
}

func TestQuizGenerationRequest(t *testing.T) {
	app := newTestApp(t)
	c := app.client()
	c.register("a@example.com")
	uid := c.userID("a@example.com")
	v, _ := app.seedReady(uid)

	resp, _ := c.post(fmt.Sprintf("/videos/%d/quizzes", v.ID), url.Values{"count": {"5"}, "difficulty": {"hard"},
		"types": {"single_choice", "short_answer"}, "section": {"0"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("generate: %d", resp.StatusCode)
	}
	j, err := app.store.ActiveJob(context.Background(), v.ID, model.JobGenerateQuiz)
	if err != nil || !strings.Contains(j.Payload, `"difficulty":"hard"`) || !strings.Contains(j.Payload, `"section":0`) {
		t.Fatalf("quiz job: %+v %v", j, err)
	}
	// A second request while one is running is rejected with a message.
	resp, _ = c.post(fmt.Sprintf("/videos/%d/quizzes", v.ID), url.Values{"count": {"5"}, "difficulty": {"hard"}, "types": {"true_false"}})
	_, body := c.get(fmt.Sprintf("/videos/%d", v.ID))
	if !strings.Contains(body, "already being generated") {
		t.Fatal("duplicate quiz generation should be explained")
	}
	// Invalid configuration.
	app.store.FailJob(context.Background(), j.ID, "x")
	c.post(fmt.Sprintf("/videos/%d/quizzes", v.ID), url.Values{"count": {"99"}, "difficulty": {"easy"}, "types": {"true_false"}})
	_, body = c.get(fmt.Sprintf("/videos/%d", v.ID))
	if !strings.Contains(body, "between 1 and 30") {
		t.Fatal("invalid count should be reported")
	}
	// Videos without a summary cannot get quizzes.
	pending := app.seedVideo(uid, "Pending")
	c.post(fmt.Sprintf("/videos/%d/quizzes", pending.ID), url.Values{"count": {"5"}, "difficulty": {"easy"}, "types": {"true_false"}})
	_, body = c.get(fmt.Sprintf("/videos/%d", pending.ID))
	if !strings.Contains(body, "finished processing") {
		t.Fatal("quiz request for unprocessed video should be refused")
	}
}

func TestQuizSubmissionAndResults(t *testing.T) {
	app := newTestApp(t)
	c := app.client()
	c.register("a@example.com")
	v, q := app.seedReady(c.userID("a@example.com"))

	resp, body := c.get(fmt.Sprintf("/quizzes/%d", q.ID))
	if resp.StatusCode != 200 || !strings.Contains(body, "Which keyword starts a goroutine?") {
		t.Fatalf("quiz page: %d", resp.StatusCode)
	}
	if strings.Contains(body, "go starts goroutines") {
		t.Fatal("quiz page must not reveal explanations before submission")
	}
	loaded, _ := app.store.QuizForUser(context.Background(), c.userID("a@example.com"), q.ID)
	correct := loaded.Questions[0].Options[0].ID
	form := url.Values{
		fmt.Sprintf("q_%d", loaded.Questions[0].ID):      {fmt.Sprint(correct)},
		fmt.Sprintf("q_%d_text", loaded.Questions[1].ID): {"Channels!"},
		"started_at": {fmt.Sprint(time.Now().Add(-time.Minute).Unix())},
	}
	resp, _ = c.post(fmt.Sprintf("/quizzes/%d/submit", q.ID), form)
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/attempts/") {
		t.Fatalf("submit: %d", resp.StatusCode)
	}
	resp, body = c.get(resp.Header.Get("Location"))
	if resp.StatusCode != 200 {
		t.Fatalf("results: %d", resp.StatusCode)
	}
	for _, want := range []string{"100%", "2 correct", "0 incorrect", "go starts goroutines", fmt.Sprintf("/videos/%d?t=4#player", v.ID)} {
		if !strings.Contains(body, want) {
			t.Errorf("results page missing %q", want)
		}
	}
	if len(app.ai.Calls) != 0 {
		t.Error("exact answers should not call the AI")
	}
	_, body = c.get("/history")
	if !strings.Contains(body, "Go Concurrency") || !strings.Contains(body, "100%") {
		t.Error("history should list the attempt")
	}
}

func TestDeleteVideoRemovesFiles(t *testing.T) {
	app := newTestApp(t)
	c := app.client()
	c.register("a@example.com")
	v, _ := app.seedReady(c.userID("a@example.com"))
	path, _ := app.files.Path(v.StorageKey)
	resp, _ := c.post(fmt.Sprintf("/videos/%d/delete", v.ID), nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	if _, err := app.store.VideoByID(context.Background(), v.ID); err == nil {
		t.Fatal("video row should be gone")
	}
	if matches, _ := filepath.Glob(path); len(matches) != 0 {
		t.Fatal("video file should be removed")
	}
}

func TestSettingsFlows(t *testing.T) {
	app := newTestApp(t)
	c := app.client()
	c.register("a@example.com")
	resp, _ := c.post("/settings/profile", url.Values{"email": {"new@example.com"}, "name": {"Renamed"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("profile: %d", resp.StatusCode)
	}
	resp, body := c.post("/settings/password", url.Values{"current_password": {"wrong"}, "new_password": {"another long pw"}, "confirm": {"another long pw"}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "incorrect") {
		t.Fatalf("wrong current password: %d", resp.StatusCode)
	}
	resp, _ = c.post("/settings/password", url.Values{"current_password": {"a long password"}, "new_password": {"another long pw"}, "confirm": {"another long pw"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("change password: %d", resp.StatusCode)
	}
	resp, _ = c.post("/settings/delete", url.Values{"password": {"another long pw"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete account: %d", resp.StatusCode)
	}
	if _, err := app.store.UserByEmail(context.Background(), "new@example.com"); err == nil {
		t.Fatal("account should be deleted")
	}
}

func TestForgotPasswordWithoutEmailConfigured(t *testing.T) {
	app := newTestApp(t)
	c := app.client()
	_, body := c.get("/forgot-password")
	if !strings.Contains(body, "not configured") {
		t.Fatal("forgot page should explain that email is not configured")
	}
}
