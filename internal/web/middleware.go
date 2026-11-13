package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"time"

	"github.com/quantamsage/ai-video-summarizer/internal/auth"
	"github.com/quantamsage/ai-video-summarizer/internal/model"
)

const (
	sessionCookie = "session"
	csrfCookie    = "csrf"
	csrfField     = "csrf_token"
	csrfHeader    = "X-CSRF-Token"
)

type ctxKey int

const (
	ctxUser ctxKey = iota
	ctxSessionToken
	ctxCSRF
)

func userFrom(r *http.Request) *model.User {
	u, _ := r.Context().Value(ctxUser).(*model.User)
	return u
}

func sessionTokenFrom(r *http.Request) string {
	t, _ := r.Context().Value(ctxSessionToken).(string)
	return t
}

func csrfTokenFrom(r *http.Request) string {
	t, _ := r.Context().Value(ctxCSRF).(string)
	return t
}

// statusRecorder captures the response status for access logs.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// logAndRecover logs each request and converts panics into 500 responses.
func (s *Server) logAndRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		defer func() {
			if p := recover(); p != nil {
				if p == http.ErrAbortHandler {
					panic(p)
				}
				s.log.Error("panic serving request", "path", r.URL.Path, "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
				if rec.status == 0 {
					s.renderError(rec, r, http.StatusInternalServerError, "Something went wrong on our side. Please try again.")
				}
			}
			level := slog.LevelInfo
			if strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/healthz" {
				level = slog.LevelDebug
			}
			attrs := []any{"method", r.Method, "path", r.URL.Path, "status", rec.status,
				"bytes", rec.bytes, "duration_ms", time.Since(start).Milliseconds(), "ip", clientIP(r)}
			if u := userFrom(r); u != nil {
				attrs = append(attrs, "user_id", u.ID)
			}
			s.log.Log(r.Context(), level, "http request", attrs...)
		}()
		next.ServeHTTP(rec, r)
	})
}

// securityHeaders sets conservative browser security headers.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	csp := strings.Join([]string{
		"default-src 'self'",
		"img-src 'self' data: https:",
		"media-src 'self' blob:",
		"style-src 'self'",
		"script-src 'self'",
		"connect-src 'self'",
		"frame-src https://www.youtube-nocookie.com https://player.vimeo.com",
		"frame-ancestors 'none'",
		"form-action 'self'",
		"base-uri 'self'",
		"object-src 'none'",
	}, "; ")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if s.cfg.SecureCookies {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// loadSession attaches the signed-in user, if any, to the request context.
func (s *Server) loadSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil || c.Value == "" {
			next.ServeHTTP(w, r)
			return
		}
		u, expires, err := s.auth.SessionUser(r.Context(), c.Value)
		if err != nil {
			s.clearCookie(w, sessionCookie)
			next.ServeHTTP(w, r)
			return
		}
		if time.Until(expires) > s.cfg.SessionTTL-time.Minute {
			// Session was just extended; refresh the cookie lifetime too.
			s.setSessionCookie(w, c.Value, expires)
		}
		ctx := context.WithValue(r.Context(), ctxUser, u)
		ctx = context.WithValue(ctx, ctxSessionToken, c.Value)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// csrf implements signed double-submit CSRF protection. A random value is
// kept in an HttpOnly cookie; forms and JavaScript send HMAC(secret, value).
// Requests with an Origin header from another site are rejected outright.
func (s *Server) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var seed string
		if c, err := r.Cookie(csrfCookie); err == nil && len(c.Value) >= 32 {
			seed = c.Value
		} else {
			seed, err = auth.RandomToken()
			if err != nil {
				s.serverError(w, r, err)
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name: csrfCookie, Value: seed, Path: "/", HttpOnly: true,
				Secure: s.cfg.SecureCookies, SameSite: http.SameSiteLaxMode,
			})
		}
		token := s.csrfToken(seed)
		r = r.WithContext(context.WithValue(r.Context(), ctxCSRF, token))

		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		if !s.sameOrigin(r) {
			s.renderError(w, r, http.StatusForbidden, "Cross-site request rejected.")
			return
		}
		sent := r.Header.Get(csrfHeader)
		if sent == "" && !isMultipart(r) {
			sent = r.PostFormValue(csrfField)
		}
		if sent == "" && isMultipart(r) {
			// Streaming upload handlers verify the token themselves with
			// verifyCSRF once they read the form field.
			next.ServeHTTP(w, r)
			return
		}
		if !s.verifyCSRF(r, sent) {
			s.renderError(w, r, http.StatusForbidden, "Your session form expired. Please go back, refresh the page and try again.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) csrfToken(seed string) string {
	mac := hmac.New(sha256.New, s.cfg.SessionSecret)
	mac.Write([]byte("csrf:" + seed))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Server) verifyCSRF(r *http.Request, sent string) bool {
	want := csrfTokenFrom(r)
	return want != "" && sent != "" && hmac.Equal([]byte(sent), []byte(want))
}

func (s *Server) sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

func isMultipart(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data")
}

// requireUser redirects anonymous visitors to the login page.
func (s *Server) requireUser(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if userFrom(r) == nil {
			if wantsJSON(r) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
				return
			}
			next := r.URL.RequestURI()
			http.Redirect(w, r, "/login?next="+url.QueryEscape(next), http.StatusSeeOther)
			return
		}
		h(w, r)
	})
}

// requireGuest redirects signed-in users away from login/register pages.
func (s *Server) requireGuest(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if userFrom(r) != nil {
			http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
			return
		}
		h(w, r)
	})
}

// limit applies a named rate limit keyed by client IP to unsafe requests.
func (s *Server) limit(bucket string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			key := bucket + "|" + clientIP(r)
			if u := userFrom(r); u != nil {
				key = fmt.Sprintf("%s|u%d", bucket, u.ID)
			}
			if ok, retry := s.limiter.Allow(bucket, key); !ok {
				w.Header().Set("Retry-After", fmt.Sprint(int(retry.Seconds())+1))
				s.log.Warn("rate limit exceeded", "bucket", bucket, "ip", clientIP(r))
				s.renderError(w, r, http.StatusTooManyRequests, "Too many attempts. Please wait a moment and try again.")
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

// clientIP returns the remote IP. Forwarded headers are deliberately not
// trusted; deploy behind a proxy that sets RemoteAddr correctly if needed.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", Expires: expires,
		MaxAge: int(time.Until(expires).Seconds()), HttpOnly: true,
		Secure: s.cfg.SecureCookies, SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
		Secure: s.cfg.SecureCookies, SameSite: http.SameSiteLaxMode,
	})
}

func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

var errBadID = errors.New("invalid id")
