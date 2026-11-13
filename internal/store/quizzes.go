package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/quantamsage/ai-video-summarizer/internal/model"
)

// SaveQuiz stores a quiz with its questions and options.
func (s *Store) SaveQuiz(ctx context.Context, q *model.Quiz) error {
	return s.InTx(ctx, func(tx *sql.Tx) error {
		t := now()
		res, err := tx.ExecContext(ctx, `
			INSERT INTO quizzes(video_id, title, difficulty, scope, requested_count, shortfall_reason, created_at)
			VALUES (?,?,?,?,?,?,?)`,
			q.VideoID, q.Title, q.Difficulty, q.Scope, q.RequestedCount, q.ShortfallReason, t)
		if err != nil {
			return err
		}
		q.ID, _ = res.LastInsertId()
		q.CreatedAt = t
		for i := range q.Questions {
			qq := &q.Questions[i]
			accepted, err := json.Marshal(nonNil(qq.AcceptedAnswers))
			if err != nil {
				return err
			}
			res, err := tx.ExecContext(ctx, `
				INSERT INTO quiz_questions(quiz_id, position, type, prompt, explanation, accepted_answers,
					timestamp_seconds, section_title)
				VALUES (?,?,?,?,?,?,?,?)`,
				q.ID, i, qq.Type, qq.Prompt, qq.Explanation, string(accepted), floatPtrArg(qq.Timestamp), qq.SectionTitle)
			if err != nil {
				return err
			}
			qq.ID, _ = res.LastInsertId()
			qq.Position = i
			for j := range qq.Options {
				o := &qq.Options[j]
				res, err := tx.ExecContext(ctx, `
					INSERT INTO quiz_options(question_id, position, text, is_correct, explanation)
					VALUES (?,?,?,?,?)`, qq.ID, j, o.Text, boolInt(o.IsCorrect), o.Explanation)
				if err != nil {
					return err
				}
				o.ID, _ = res.LastInsertId()
				o.Position = j
			}
		}
		q.QuestionCount = len(q.Questions)
		return nil
	})
}

// QuizzesForVideo lists quizzes for a video (without questions), newest first.
func (s *Store) QuizzesForVideo(ctx context.Context, videoID int64) ([]*model.Quiz, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT q.id, q.video_id, q.title, q.difficulty, q.scope, q.requested_count, q.shortfall_reason, q.created_at,
			(SELECT COUNT(*) FROM quiz_questions qq WHERE qq.quiz_id = q.id)
		FROM quizzes q WHERE q.video_id = ? ORDER BY q.created_at DESC, q.id DESC`, videoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Quiz
	for rows.Next() {
		q := &model.Quiz{}
		if err := rows.Scan(&q.ID, &q.VideoID, &q.Title, &q.Difficulty, &q.Scope, &q.RequestedCount,
			&q.ShortfallReason, &q.CreatedAt, &q.QuestionCount); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// QuizForUser loads a quiz with all questions and options, only if the
// quiz's video belongs to userID.
func (s *Store) QuizForUser(ctx context.Context, userID, quizID int64) (*model.Quiz, error) {
	q := &model.Quiz{}
	err := s.DB.QueryRowContext(ctx, `
		SELECT q.id, q.video_id, q.title, q.difficulty, q.scope, q.requested_count, q.shortfall_reason, q.created_at
		FROM quizzes q JOIN videos v ON v.id = q.video_id
		WHERE q.id = ? AND v.user_id = ?`, quizID, userID).
		Scan(&q.ID, &q.VideoID, &q.Title, &q.Difficulty, &q.Scope, &q.RequestedCount, &q.ShortfallReason, &q.CreatedAt)
	if err != nil {
		return nil, notFound(err)
	}

	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, position, type, prompt, explanation, accepted_answers, timestamp_seconds, section_title
		FROM quiz_questions WHERE quiz_id = ? ORDER BY position`, q.ID)
	if err != nil {
		return nil, err
	}
	index := map[int64]int{}
	for rows.Next() {
		var qq model.Question
		var accepted string
		var ts sql.NullFloat64
		if err := rows.Scan(&qq.ID, &qq.Position, &qq.Type, &qq.Prompt, &qq.Explanation, &accepted, &ts, &qq.SectionTitle); err != nil {
			rows.Close()
			return nil, err
		}
		if err := json.Unmarshal([]byte(accepted), &qq.AcceptedAnswers); err != nil {
			rows.Close()
			return nil, err
		}
		qq.Timestamp = nullFloatPtr(ts)
		index[qq.ID] = len(q.Questions)
		q.Questions = append(q.Questions, qq)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = s.DB.QueryContext(ctx, `
		SELECT o.id, o.question_id, o.position, o.text, o.is_correct, o.explanation
		FROM quiz_options o JOIN quiz_questions qq ON qq.id = o.question_id
		WHERE qq.quiz_id = ? ORDER BY qq.position, o.position`, q.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var o model.Option
		var qid int64
		var correct int
		if err := rows.Scan(&o.ID, &qid, &o.Position, &o.Text, &correct, &o.Explanation); err != nil {
			return nil, err
		}
		o.IsCorrect = correct == 1
		if i, ok := index[qid]; ok {
			q.Questions[i].Options = append(q.Questions[i].Options, o)
		}
	}
	q.QuestionCount = len(q.Questions)
	return q, rows.Err()
}

// SaveAttempt stores a completed quiz attempt and its answers.
func (s *Store) SaveAttempt(ctx context.Context, a *model.Attempt) error {
	return s.InTx(ctx, func(tx *sql.Tx) error {
		t := now()
		if a.StartedAt.IsZero() {
			a.StartedAt = t
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO quiz_attempts(quiz_id, user_id, score, correct_count, total_count, started_at, completed_at)
			VALUES (?,?,?,?,?,?,?)`,
			a.QuizID, a.UserID, a.Score, a.CorrectCount, a.TotalCount, a.StartedAt.UTC(), t)
		if err != nil {
			return err
		}
		a.ID, _ = res.LastInsertId()
		a.CompletedAt = &t
		for _, ans := range a.Answers {
			sel := ans.SelectedOptions
			if sel == nil {
				sel = []int64{}
			}
			selJSON, err := json.Marshal(sel)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO quiz_answers(attempt_id, question_id, selected_options, text_answer, is_correct,
					grading_method, feedback)
				VALUES (?,?,?,?,?,?,?)`,
				a.ID, ans.QuestionID, string(selJSON), ans.TextAnswer, boolInt(ans.IsCorrect),
				ans.GradingMethod, ans.Feedback); err != nil {
				return err
			}
		}
		return nil
	})
}

// AttemptForUser loads an attempt with its answers if it belongs to userID.
func (s *Store) AttemptForUser(ctx context.Context, userID, attemptID int64) (*model.Attempt, error) {
	a := &model.Attempt{}
	var completed sql.NullTime
	err := s.DB.QueryRowContext(ctx, `
		SELECT id, quiz_id, user_id, score, correct_count, total_count, started_at, completed_at
		FROM quiz_attempts WHERE id = ? AND user_id = ?`, attemptID, userID).
		Scan(&a.ID, &a.QuizID, &a.UserID, &a.Score, &a.CorrectCount, &a.TotalCount, &a.StartedAt, &completed)
	if err != nil {
		return nil, notFound(err)
	}
	a.CompletedAt = nullTimePtr(completed)
	rows, err := s.DB.QueryContext(ctx, `
		SELECT question_id, selected_options, text_answer, is_correct, grading_method, feedback
		FROM quiz_answers WHERE attempt_id = ?`, a.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ans model.Answer
		var sel string
		var correct int
		if err := rows.Scan(&ans.QuestionID, &sel, &ans.TextAnswer, &correct, &ans.GradingMethod, &ans.Feedback); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(sel), &ans.SelectedOptions); err != nil {
			return nil, err
		}
		ans.IsCorrect = correct == 1
		a.Answers = append(a.Answers, ans)
	}
	return a, rows.Err()
}

// AttemptSummary is a row in attempt history lists.
type AttemptSummary struct {
	ID           int64
	QuizID       int64
	QuizTitle    string
	VideoID      int64
	VideoTitle   string
	Difficulty   string
	Score        float64
	CorrectCount int
	TotalCount   int
	CompletedAt  time.Time
}

// AttemptHistory returns a user's completed attempts, newest first. When
// quizID is non-zero only attempts for that quiz are returned.
func (s *Store) AttemptHistory(ctx context.Context, userID, quizID int64, limit int) ([]AttemptSummary, error) {
	cond := "a.user_id = ? AND a.completed_at IS NOT NULL"
	args := []any{userID}
	if quizID != 0 {
		cond += " AND a.quiz_id = ?"
		args = append(args, quizID)
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT a.id, q.id, q.title, v.id, v.title, q.difficulty, a.score, a.correct_count, a.total_count, a.completed_at
		FROM quiz_attempts a
		JOIN quizzes q ON q.id = a.quiz_id
		JOIN videos v ON v.id = q.video_id AND v.user_id = a.user_id
		WHERE `+cond+` ORDER BY a.completed_at DESC, a.id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AttemptSummary
	for rows.Next() {
		var r AttemptSummary
		if err := rows.Scan(&r.ID, &r.QuizID, &r.QuizTitle, &r.VideoID, &r.VideoTitle, &r.Difficulty,
			&r.Score, &r.CorrectCount, &r.TotalCount, &r.CompletedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DashboardStats aggregates a user's learning activity.
type DashboardStats struct {
	TotalVideos     int
	ProcessedVideos int
	ProcessingCount int
	Summaries       int
	QuizzesTaken    int
	AverageScore    float64
}

func (s *Store) DashboardStats(ctx context.Context, userID int64) (DashboardStats, error) {
	var st DashboardStats
	err := s.DB.QueryRowContext(ctx, `
		SELECT COUNT(*),
			COALESCE(SUM(status = 'ready'), 0),
			COALESCE(SUM(status IN ('queued','processing')), 0)
		FROM videos WHERE user_id = ?`, userID).Scan(&st.TotalVideos, &st.ProcessedVideos, &st.ProcessingCount)
	if err != nil {
		return st, err
	}
	err = s.DB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM summaries s JOIN videos v ON v.id = s.video_id WHERE v.user_id = ?`, userID).
		Scan(&st.Summaries)
	if err != nil {
		return st, err
	}
	err = s.DB.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(AVG(score), 0) FROM quiz_attempts
		WHERE user_id = ? AND completed_at IS NOT NULL`, userID).Scan(&st.QuizzesTaken, &st.AverageScore)
	return st, err
}
