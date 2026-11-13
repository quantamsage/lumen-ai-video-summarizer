// Package web implements the HTTP interface: routing, middleware, handlers
// and server-rendered templates.
package web

import (
	"context"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/quantamsage/ai-video-summarizer/internal/auth"
	"github.com/quantamsage/ai-video-summarizer/internal/config"
	"github.com/quantamsage/ai-video-summarizer/internal/library"
	"github.com/quantamsage/ai-video-summarizer/internal/quiz"
	"github.com/quantamsage/ai-video-summarizer/internal/storage"
	"github.com/quantamsage/ai-video-summarizer/internal/store"
)

// Deps are the server's collaborators.
type Deps struct {
	Config  *config.Config
	Log     *slog.Logger
	Store   *store.Store
	Auth    *auth.Service
	Library *library.Service
	Files   *storage.Local
	Grader  *quiz.Grader
	Mailer  auth.Mailer // nil when email is not configured
}

// Server handles HTTP requests.
type Server struct {
	cfg       *config.Config
	log       *slog.Logger
	store     *store.Store
	auth      *auth.Service
	lib       *library.Service
	files     *storage.Local
	grader    *quiz.Grader
	mailer    auth.Mailer
	templates map[string]*template.Template
	limiter   *rateLimiter
}

func New(d Deps) (*Server, error) {
	tmpl, err := loadTemplates()
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg: d.Config, log: d.Log, store: d.Store, auth: d.Auth, lib: d.Library,
		files: d.Files, grader: d.Grader, mailer: d.Mailer, templates: tmpl,
		limiter: newRateLimiter(defaultRateRules()),
	}, nil
}

// StartMaintenance periodically removes expired sessions and idle rate
// limiter buckets until ctx is cancelled.
func (s *Server) StartMaintenance(ctx context.Context) {
	go func() {
		t := time.NewTicker(15 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.limiter.Sweep()
				if n, err := s.store.DeleteExpiredSessions(ctx); err != nil {
					s.log.Error("delete expired sessions", "err", err)
				} else if n > 0 {
					s.log.Info("deleted expired sessions", "count", n)
				}
			}
		}
	}()
}

// Handler returns the root HTTP handler with all middleware applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	user := s.requireUser
	guest := s.requireGuest

	static, _ := fs.Sub(assets, "static")
	fileServer := http.StripPrefix("/static/", http.FileServer(http.FS(static)))
	mux.Handle("GET /static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		fileServer.ServeHTTP(w, r)
	}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.store.DB.PingContext(r.Context()); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok"))
	})

	mux.HandleFunc("GET /{$}", s.home)

	// Authentication
	mux.Handle("GET /register", guest(s.registerPage))
	mux.Handle("POST /register", s.limit("register", guest(s.register)))
	mux.Handle("GET /login", guest(s.loginPage))
	mux.Handle("POST /login", s.limit("login", guest(s.login)))
	mux.Handle("POST /logout", user(s.logout))
	mux.Handle("GET /forgot-password", guest(s.forgotPage))
	mux.Handle("POST /forgot-password", s.limit("reset", guest(s.forgot)))
	mux.HandleFunc("GET /reset-password", s.resetPage)
	mux.Handle("POST /reset-password", s.limit("reset", http.HandlerFunc(s.reset)))

	// Dashboard and library
	mux.Handle("GET /dashboard", user(s.dashboard))
	mux.Handle("GET /videos", user(s.library))
	mux.Handle("GET /videos/new", user(s.newVideoPage))
	mux.Handle("POST /videos/upload", s.limit("upload", user(s.uploadVideo)))
	mux.Handle("POST /videos/url", s.limit("upload", user(s.submitURL)))
	mux.Handle("GET /videos/{id}", user(s.videoPage))
	mux.Handle("GET /videos/{id}/status", user(s.videoStatus))
	mux.Handle("GET /videos/{id}/media", user(s.videoMedia))
	mux.Handle("GET /videos/{id}/thumbnail", user(s.videoThumbnail))
	mux.Handle("GET /videos/{id}/summary.md", user(s.downloadSummary))
	mux.Handle("GET /videos/{id}/transcript.txt", user(s.downloadTranscript))
	mux.Handle("POST /videos/{id}/delete", user(s.deleteVideo))
	mux.Handle("POST /videos/{id}/reprocess", s.limit("generate", user(s.reprocessVideo)))
	mux.Handle("POST /videos/{id}/quizzes", s.limit("generate", user(s.generateQuiz)))

	// Quizzes
	mux.Handle("GET /quizzes/{id}", user(s.quizPage))
	mux.Handle("POST /quizzes/{id}/submit", s.limit("submit", user(s.submitQuiz)))
	mux.Handle("GET /attempts/{id}", user(s.attemptPage))
	mux.Handle("GET /history", user(s.historyPage))

	// Settings
	mux.Handle("GET /settings", user(s.settingsPage))
	mux.Handle("POST /settings/profile", s.limit("account", user(s.updateProfile)))
	mux.Handle("POST /settings/password", s.limit("login", user(s.changePassword)))
	mux.Handle("POST /settings/delete", s.limit("login", user(s.deleteAccount)))

	notFound := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// mux.Handler is only used to detect unmatched paths; ServeHTTP must
		// do the dispatch so that path values ({id}) are populated.
		if _, pattern := mux.Handler(r); pattern == "" {
			s.notFound(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})

	var h http.Handler = notFound
	h = s.csrf(h)
	h = s.loadSession(h)
	h = s.securityHeaders(h)
	h = s.logAndRecover(h)
	return h
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	if userFrom(r) != nil {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "home", s.newPage(w, r, "Learn from any video", ""))
}

// pathID parses the {id} path value.
func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, errBadID
	}
	return id, nil
}
