package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/quantamsage/ai-video-summarizer/internal/model"
	"github.com/quantamsage/ai-video-summarizer/internal/quiz"
	"github.com/quantamsage/ai-video-summarizer/internal/store"
)

func (s *Server) loadQuiz(w http.ResponseWriter, r *http.Request) (*model.Quiz, *model.Video, bool) {
	id, err := pathID(r)
	if err != nil {
		s.notFound(w, r)
		return nil, nil, false
	}
	u := userFrom(r)
	q, err := s.store.QuizForUser(r.Context(), u.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return nil, nil, false
	}
	if err != nil {
		s.serverError(w, r, err)
		return nil, nil, false
	}
	v, err := s.store.VideoForUser(r.Context(), u.ID, q.VideoID)
	if err != nil {
		s.serverError(w, r, err)
		return nil, nil, false
	}
	return q, v, true
}

func (s *Server) quizPage(w http.ResponseWriter, r *http.Request) {
	q, v, ok := s.loadQuiz(w, r)
	if !ok {
		return
	}
	history, err := s.store.AttemptHistory(r.Context(), userFrom(r).ID, q.ID, 10)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p := s.newPage(w, r, q.Title, "library")
	p.Data = map[string]any{"Quiz": q, "Video": v, "StartedAt": time.Now().Unix(), "History": history}
	s.render(w, r, http.StatusOK, "quiz", p)
}

func (s *Server) submitQuiz(w http.ResponseWriter, r *http.Request) {
	q, _, ok := s.loadQuiz(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "The quiz submission could not be read.")
		return
	}
	subs := map[int64]quiz.Submission{}
	for _, qq := range q.Questions {
		key := fmt.Sprintf("q_%d", qq.ID)
		var sub quiz.Submission
		if qq.Type == model.ShortAnswer {
			sub.Text = r.PostForm.Get(key + "_text")
		} else {
			for _, raw := range r.PostForm[key] {
				if id, err := strconv.ParseInt(raw, 10, 64); err == nil {
					sub.OptionIDs = append(sub.OptionIDs, id)
				}
			}
		}
		subs[qq.ID] = sub
	}
	attempt := s.grader.Grade(r.Context(), q, subs)
	attempt.UserID = userFrom(r).ID
	if ts, err := strconv.ParseInt(r.PostForm.Get("started_at"), 10, 64); err == nil {
		started := time.Unix(ts, 0)
		if time.Since(started) < 24*time.Hour && started.Before(time.Now().Add(time.Minute)) {
			attempt.StartedAt = started
		}
	}
	if err := s.store.SaveAttempt(r.Context(), attempt); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.log.Info("quiz submitted", "quiz_id", q.ID, "user_id", attempt.UserID, "score", attempt.Score)
	http.Redirect(w, r, fmt.Sprintf("/attempts/%d", attempt.ID), http.StatusSeeOther)
}

// resultRow pairs a question with the learner's graded answer.
type resultRow struct {
	Number   int
	Question model.Question
	Answer   model.Answer
	Answered bool
}

func (s *Server) attemptPage(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.notFound(w, r)
		return
	}
	u := userFrom(r)
	a, err := s.store.AttemptForUser(r.Context(), u.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	q, err := s.store.QuizForUser(r.Context(), u.ID, a.QuizID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	v, err := s.store.VideoForUser(r.Context(), u.ID, q.VideoID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	answers := map[int64]model.Answer{}
	for _, ans := range a.Answers {
		answers[ans.QuestionID] = ans
	}
	rows := make([]resultRow, len(q.Questions))
	for i, qq := range q.Questions {
		ans, ok := answers[qq.ID]
		rows[i] = resultRow{Number: i + 1, Question: qq, Answer: ans,
			Answered: ok && (len(ans.SelectedOptions) > 0 || ans.TextAnswer != "")}
	}
	var duration time.Duration
	if a.CompletedAt != nil {
		duration = a.CompletedAt.Sub(a.StartedAt).Round(time.Second)
	}
	p := s.newPage(w, r, "Quiz results", "history")
	p.Data = map[string]any{"Attempt": a, "Quiz": q, "Video": v, "Rows": rows,
		"Incorrect": a.TotalCount - a.CorrectCount, "Duration": duration}
	s.render(w, r, http.StatusOK, "results", p)
}

func (s *Server) historyPage(w http.ResponseWriter, r *http.Request) {
	attempts, err := s.store.AttemptHistory(r.Context(), userFrom(r).ID, 0, 200)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p := s.newPage(w, r, "Learning history", "history")
	p.Data = map[string]any{"Attempts": attempts}
	s.render(w, r, http.StatusOK, "history", p)
}
