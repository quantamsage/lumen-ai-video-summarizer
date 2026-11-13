package store

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/quantamsage/ai-video-summarizer/internal/model"
)

// SaveSummary stores a summary with its sections and concepts, replacing any
// existing summary for the video.
func (s *Store) SaveSummary(ctx context.Context, sum *model.Summary) error {
	takeaways, err := json.Marshal(nonNil(sum.KeyTakeaways))
	if err != nil {
		return err
	}
	notes, err := json.Marshal(nonNilNotes(sum.LearningNotes))
	if err != nil {
		return err
	}
	return s.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM summaries WHERE video_id = ?`, sum.VideoID); err != nil {
			return err
		}
		t := now()
		res, err := tx.ExecContext(ctx, `
			INSERT INTO summaries(video_id, overview, detailed, key_takeaways, learning_notes, provider, model, created_at)
			VALUES (?,?,?,?,?,?,?,?)`,
			sum.VideoID, sum.Overview, sum.Detailed, string(takeaways), string(notes), sum.Provider, sum.Model, t)
		if err != nil {
			return err
		}
		sum.ID, _ = res.LastInsertId()
		sum.CreatedAt = t
		for i, sec := range sum.Sections {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO summary_sections(summary_id, position, title, content, start_seconds, end_seconds)
				VALUES (?,?,?,?,?,?)`,
				sum.ID, i, sec.Title, sec.Content, floatPtrArg(sec.Start), floatPtrArg(sec.End)); err != nil {
				return err
			}
		}
		for i, c := range sum.Concepts {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO concepts(summary_id, position, term, definition, category) VALUES (?,?,?,?,?)`,
				sum.ID, i, c.Term, c.Definition, c.Category); err != nil {
				return err
			}
		}
		return nil
	})
}

// SummaryForVideo loads the full summary for a video.
func (s *Store) SummaryForVideo(ctx context.Context, videoID int64) (*model.Summary, error) {
	sum := &model.Summary{}
	var takeaways, notes string
	err := s.DB.QueryRowContext(ctx, `
		SELECT id, video_id, overview, detailed, key_takeaways, learning_notes, provider, model, created_at
		FROM summaries WHERE video_id = ?`, videoID).
		Scan(&sum.ID, &sum.VideoID, &sum.Overview, &sum.Detailed, &takeaways, &notes, &sum.Provider, &sum.Model, &sum.CreatedAt)
	if err != nil {
		return nil, notFound(err)
	}
	if err := json.Unmarshal([]byte(takeaways), &sum.KeyTakeaways); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(notes), &sum.LearningNotes); err != nil {
		return nil, err
	}

	rows, err := s.DB.QueryContext(ctx, `
		SELECT position, title, content, start_seconds, end_seconds FROM summary_sections
		WHERE summary_id = ? ORDER BY position`, sum.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var sec model.Section
		var start, end sql.NullFloat64
		if err := rows.Scan(&sec.Position, &sec.Title, &sec.Content, &start, &end); err != nil {
			rows.Close()
			return nil, err
		}
		sec.Start, sec.End = nullFloatPtr(start), nullFloatPtr(end)
		sum.Sections = append(sum.Sections, sec)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = s.DB.QueryContext(ctx, `
		SELECT term, definition, category FROM concepts WHERE summary_id = ? ORDER BY position`, sum.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c model.Concept
		if err := rows.Scan(&c.Term, &c.Definition, &c.Category); err != nil {
			return nil, err
		}
		sum.Concepts = append(sum.Concepts, c)
	}
	return sum, rows.Err()
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func nonNilNotes(n []model.LearningNote) []model.LearningNote {
	if n == nil {
		return []model.LearningNote{}
	}
	return n
}
