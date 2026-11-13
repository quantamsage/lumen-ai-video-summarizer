package web

import (
	"bytes"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"math"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/quantamsage/ai-video-summarizer/internal/model"
)

//go:embed templates static
var assets embed.FS

// Page is the data passed to every template.
type Page struct {
	Title     string
	Nav       string
	User      *model.User
	CSRFToken string
	Flash     *Flash
	Form      url.Values
	Errors    map[string]string
	Data      any
}

// Flash is a one-time message shown after a redirect.
type Flash struct {
	Kind    string `json:"k"` // success, error, info
	Message string `json:"m"`
}

func loadTemplates() (map[string]*template.Template, error) {
	shared := []string{"templates/layout.html", "templates/partials.html"}
	pages, err := fs.Glob(assets, "templates/pages/*.html")
	if err != nil {
		return nil, err
	}
	out := map[string]*template.Template{}
	for _, p := range pages {
		name := strings.TrimSuffix(path.Base(p), ".html")
		t, err := template.New(path.Base(p)).Funcs(templateFuncs).ParseFS(assets, append([]string{p}, shared...)...)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", p, err)
		}
		out[name] = t
	}
	return out, nil
}

var templateFuncs = template.FuncMap{
	"clock":       formatClock,
	"duration":    formatDuration,
	"date":        func(t time.Time) string { return t.Local().Format("Jan 2, 2006") },
	"datetime":    func(t time.Time) string { return t.Local().Format("Jan 2, 2006 15:04") },
	"ago":         timeAgo,
	"bytes":       formatBytes,
	"pct":         func(f float64) string { return fmt.Sprintf("%.0f%%", math.Round(f)) },
	"statusLabel": statusLabel,
	"add":         func(a, b int) int { return a + b },
	"initial": func(s string) string {
		for _, r := range strings.TrimSpace(s) {
			return strings.ToUpper(string(r))
		}
		return "?"
	},
	"deref":      func(f *float64) float64 { return *f },
	"paragraphs": paragraphs,
	"typeLabel":  typeLabel,
	"title":      func(s string) string { return strings.ToUpper(s[:1]) + s[1:] },
	"hasType": func(types []string, t string) bool {
		for _, x := range types {
			if x == t {
				return true
			}
		}
		return false
	},
	"containsID": func(ids []int64, id int64) bool {
		for _, x := range ids {
			if x == id {
				return true
			}
		}
		return false
	},
	"scoreClass": func(score float64) string {
		switch {
		case score >= 80:
			return "good"
		case score >= 50:
			return "ok"
		default:
			return "poor"
		}
	},
}

func formatClock(sec float64) string {
	if sec < 0 {
		sec = 0
	}
	s := int(sec)
	h, m, ss := s/3600, (s%3600)/60, s%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, ss)
	}
	return fmt.Sprintf("%d:%02d", m, ss)
}

func formatDuration(sec float64) string {
	if sec <= 0 {
		return "—"
	}
	return formatClock(sec)
}

func timeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute") + " ago"
	case d < 24*time.Hour:
		return plural(int(d.Hours()), "hour") + " ago"
	case d < 30*24*time.Hour:
		return plural(int(d.Hours()/24), "day") + " ago"
	default:
		return t.Local().Format("Jan 2, 2006")
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func statusLabel(s string) string {
	switch s {
	case "queued":
		return "Queued"
	case "processing":
		return "Processing"
	case "ready":
		return "Ready"
	case "failed":
		return "Failed"
	case "pending":
		return "Pending"
	case "skipped":
		return "Skipped"
	}
	return s
}

func typeLabel(t string) string {
	switch t {
	case model.SingleChoice:
		return "Single choice"
	case model.MultipleAnswer:
		return "Multiple answer"
	case model.TrueFalse:
		return "True / false"
	case model.ShortAnswer:
		return "Short answer"
	}
	return t
}

// paragraphs splits text on blank lines for rendering.
func paragraphs(s string) []string {
	var out []string
	for _, p := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n\n") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// newPage builds the common template data for a request, consuming any
// pending flash message.
func (s *Server) newPage(w http.ResponseWriter, r *http.Request, title, nav string) *Page {
	return &Page{
		Title:     title,
		Nav:       nav,
		User:      userFrom(r),
		CSRFToken: csrfTokenFrom(r),
		Flash:     s.popFlash(w, r),
		Form:      url.Values{},
		Errors:    map[string]string{},
	}
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name string, p *Page) {
	t, ok := s.templates[name]
	if !ok {
		s.serverError(w, r, fmt.Errorf("template %q not found", name))
		return
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, p); err != nil {
		s.serverError(w, r, fmt.Errorf("render %s: %w", name, err))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

// renderError shows a friendly error page (or JSON for API callers).
func (s *Server) renderError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	if wantsJSON(r) {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	p := &Page{Title: http.StatusText(status), User: userFrom(r), CSRFToken: csrfTokenFrom(r),
		Data: map[string]any{"Status": status, "Message": msg}}
	t, ok := s.templates["error"]
	if !ok {
		http.Error(w, msg, status)
		return
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, p); err != nil {
		s.log.Error("render error page", "err", err)
		http.Error(w, msg, status)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

// serverError logs the real error and shows a generic message.
func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("internal error", "method", r.Method, "path", r.URL.Path, "err", err)
	s.renderError(w, r, http.StatusInternalServerError, "Something went wrong on our side. Please try again.")
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	s.renderError(w, r, http.StatusNotFound, "We couldn't find what you were looking for.")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

const flashCookie = "flash"

func (s *Server) setFlash(w http.ResponseWriter, kind, msg string) {
	b, _ := json.Marshal(Flash{Kind: kind, Message: msg})
	http.SetCookie(w, &http.Cookie{
		Name: flashCookie, Value: base64.RawURLEncoding.EncodeToString(b), Path: "/",
		MaxAge: 60, HttpOnly: true, Secure: s.cfg.SecureCookies, SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) popFlash(w http.ResponseWriter, r *http.Request) *Flash {
	c, err := r.Cookie(flashCookie)
	if err != nil {
		return nil
	}
	s.clearCookie(w, flashCookie)
	b, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return nil
	}
	var f Flash
	if json.Unmarshal(b, &f) != nil || f.Message == "" {
		return nil
	}
	switch f.Kind {
	case "success", "error", "info":
	default:
		f.Kind = "info"
	}
	return &f
}

// redirectWithFlash sets a flash message and redirects with 303.
func (s *Server) redirectWithFlash(w http.ResponseWriter, r *http.Request, to, kind, msg string) {
	s.setFlash(w, kind, msg)
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// safeNext returns a local redirect target, defaulting to the dashboard.
// It rejects absolute and protocol-relative URLs to prevent open redirects.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/dashboard"
	}
	if u, err := url.Parse(next); err != nil || u.Host != "" || u.Scheme != "" {
		return "/dashboard"
	}
	return next
}
