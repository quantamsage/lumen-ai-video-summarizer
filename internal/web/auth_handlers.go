package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/quantamsage/ai-video-summarizer/internal/auth"
)

func (s *Server) registerPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "register", s.newPage(w, r, "Create account", ""))
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	p := s.newPage(w, r, "Create account", "")
	p.Form = r.PostForm
	u, err := s.auth.Register(r.Context(), r.PostFormValue("email"), r.PostFormValue("name"),
		r.PostFormValue("password"), r.PostFormValue("confirm"))
	var verr auth.ValidationError
	if errors.As(err, &verr) {
		p.Errors = verr
		s.render(w, r, http.StatusUnprocessableEntity, "register", p)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.log.Info("user registered", "user_id", u.ID)
	if err := s.startSession(w, r, u.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.redirectWithFlash(w, r, "/dashboard", "success", "Welcome! Add your first video to get started.")
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	p := s.newPage(w, r, "Sign in", "")
	p.Form.Set("next", r.URL.Query().Get("next"))
	s.render(w, r, http.StatusOK, "login", p)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	u, err := s.auth.Authenticate(r.Context(), r.PostFormValue("email"), r.PostFormValue("password"))
	if errors.Is(err, auth.ErrInvalidCredentials) {
		s.log.Warn("failed login", "ip", clientIP(r))
		p := s.newPage(w, r, "Sign in", "")
		p.Form = url.Values{"email": {r.PostFormValue("email")}, "next": {r.PostFormValue("next")}}
		p.Errors["form"] = "Incorrect email or password."
		s.render(w, r, http.StatusUnauthorized, "login", p)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.startSession(w, r, u.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.log.Info("user logged in", "user_id", u.ID)
	http.Redirect(w, r, safeNext(r.PostFormValue("next")), http.StatusSeeOther)
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, userID int64) error {
	// Discard any existing session cookie to prevent session fixation.
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		s.auth.Logout(r.Context(), c.Value)
	}
	token, expires, err := s.auth.CreateSession(r.Context(), userID, r.UserAgent())
	if err != nil {
		return err
	}
	s.setSessionCookie(w, token, expires)
	return nil
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if err := s.auth.Logout(r.Context(), sessionTokenFrom(r)); err != nil {
		s.log.Error("logout", "err", err)
	}
	s.clearCookie(w, sessionCookie)
	s.redirectWithFlash(w, r, "/login", "info", "You have been signed out.")
}

func (s *Server) forgotPage(w http.ResponseWriter, r *http.Request) {
	p := s.newPage(w, r, "Reset password", "")
	p.Data = map[string]any{"EmailEnabled": s.mailer != nil}
	s.render(w, r, http.StatusOK, "forgot", p)
}

func (s *Server) forgot(w http.ResponseWriter, r *http.Request) {
	if s.mailer == nil {
		s.redirectWithFlash(w, r, "/forgot-password", "error",
			"Password reset emails are not configured on this server. Please contact the administrator.")
		return
	}
	email := auth.NormalizeEmail(r.PostFormValue("email"))
	token, u, err := s.auth.CreatePasswordReset(r.Context(), email)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if u != nil {
		link := s.cfg.BaseURL + "/reset-password?token=" + url.QueryEscape(token)
		body := fmt.Sprintf("Hi %s,\n\nSomeone requested a password reset for your account. "+
			"If it was you, open this link within one hour to choose a new password:\n\n%s\n\n"+
			"If you did not request this, you can ignore this email.\n", u.DisplayName, link)
		if err := s.mailer.Send(r.Context(), u.Email, "Reset your password", body); err != nil {
			s.log.Error("send password reset email", "user_id", u.ID, "err", err)
		} else {
			s.log.Info("password reset email sent", "user_id", u.ID)
		}
	}
	// Same response whether or not the account exists.
	s.redirectWithFlash(w, r, "/login", "info",
		"If an account exists for that email, we've sent a link to reset the password.")
}

func (s *Server) resetPage(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	p := s.newPage(w, r, "Choose a new password", "")
	p.Form.Set("token", token)
	p.Data = map[string]any{"Valid": s.auth.ResetTokenValid(r.Context(), token)}
	s.render(w, r, http.StatusOK, "reset", p)
}

func (s *Server) reset(w http.ResponseWriter, r *http.Request) {
	token := r.PostFormValue("token")
	err := s.auth.ResetPassword(r.Context(), token, r.PostFormValue("password"), r.PostFormValue("confirm"))
	var verr auth.ValidationError
	switch {
	case errors.As(err, &verr):
		p := s.newPage(w, r, "Choose a new password", "")
		p.Form.Set("token", token)
		p.Errors = verr
		p.Data = map[string]any{"Valid": true}
		s.render(w, r, http.StatusUnprocessableEntity, "reset", p)
	case errors.Is(err, auth.ErrInvalidToken):
		s.redirectWithFlash(w, r, "/forgot-password", "error", "That reset link is invalid or has expired. Request a new one.")
	case err != nil:
		s.serverError(w, r, err)
	default:
		s.clearCookie(w, sessionCookie)
		s.redirectWithFlash(w, r, "/login", "success", "Your password has been changed. Please sign in.")
	}
}

// Settings ------------------------------------------------------------------

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	p := s.newPage(w, r, "Settings", "settings")
	u := userFrom(r)
	p.Form.Set("email", u.Email)
	p.Form.Set("name", u.DisplayName)
	s.render(w, r, http.StatusOK, "settings", p)
}

func (s *Server) updateProfile(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	err := s.auth.UpdateProfile(r.Context(), u, r.PostFormValue("email"), r.PostFormValue("name"))
	var verr auth.ValidationError
	if errors.As(err, &verr) {
		p := s.newPage(w, r, "Settings", "settings")
		p.Form = r.PostForm
		p.Errors = verr
		s.render(w, r, http.StatusUnprocessableEntity, "settings", p)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.redirectWithFlash(w, r, "/settings", "success", "Profile updated.")
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	err := s.auth.ChangePassword(r.Context(), u, sessionTokenFrom(r), r.PostFormValue("current_password"),
		r.PostFormValue("new_password"), r.PostFormValue("confirm"))
	var verr auth.ValidationError
	if errors.As(err, &verr) {
		p := s.newPage(w, r, "Settings", "settings")
		p.Form.Set("email", u.Email)
		p.Form.Set("name", u.DisplayName)
		p.Errors = verr
		s.render(w, r, http.StatusUnprocessableEntity, "settings", p)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.log.Info("password changed", "user_id", u.ID)
	s.redirectWithFlash(w, r, "/settings", "success", "Password changed. Other devices have been signed out.")
}

func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	if _, err := s.auth.Authenticate(r.Context(), u.Email, r.PostFormValue("password")); err != nil {
		p := s.newPage(w, r, "Settings", "settings")
		p.Form.Set("email", u.Email)
		p.Form.Set("name", u.DisplayName)
		p.Errors["delete_password"] = "Password is incorrect."
		s.render(w, r, http.StatusUnprocessableEntity, "settings", p)
		return
	}
	if err := s.lib.DeleteAccount(r.Context(), u.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.clearCookie(w, sessionCookie)
	s.setFlash(w, "info", "Your account and all of its data have been deleted.")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
