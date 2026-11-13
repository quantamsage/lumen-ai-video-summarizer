package store

import (
	"context"
	"database/sql"
	"strings"

	"github.com/quantamsage/ai-video-summarizer/internal/model"
)

// SaveTranscript stores a transcript and its segments, replacing any
// previous transcript for the video.
func (s *Store) SaveTranscript(ctx context.Context, t *model.Transcript, segs []model.Segment) error {
	return s.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM transcripts WHERE video_id = ?`, t.VideoID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO transcripts(video_id, source, language, has_timestamps, full_text, word_count, created_at)
			VALUES (?,?,?,?,?,?,?)`,
			t.VideoID, t.Source, t.Language, boolInt(t.HasTimestamps), t.FullText, t.WordCount, now())
		if err != nil {
			return err
		}
		t.ID, _ = res.LastInsertId()
		stmt, err := tx.PrepareContext(ctx, `
			INSERT INTO transcript_segments(transcript_id, position, start_seconds, end_seconds, text)
			VALUES (?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for i, sg := range segs {
			if _, err := stmt.ExecContext(ctx, t.ID, i, sg.Start, sg.End, sg.Text); err != nil {
				return err
			}
		}
		return nil
	})
}

// TranscriptForVideo returns the transcript (without segments) for a video.
func (s *Store) TranscriptForVideo(ctx context.Context, videoID int64) (*model.Transcript, error) {
	t := &model.Transcript{}
	var ts int
	err := s.DB.QueryRowContext(ctx, `
		SELECT id, video_id, source, language, has_timestamps, full_text, word_count, created_at
		FROM transcripts WHERE video_id = ?`, videoID).
		Scan(&t.ID, &t.VideoID, &t.Source, &t.Language, &ts, &t.FullText, &t.WordCount, &t.CreatedAt)
	if err != nil {
		return nil, notFound(err)
	}
	t.HasTimestamps = ts == 1
	return t, nil
}

// Segments returns all segments of a transcript in order.
func (s *Store) Segments(ctx context.Context, transcriptID int64) ([]model.Segment, error) {
	segs, _, err := s.SegmentsPage(ctx, transcriptID, "", 0, -1)
	return segs, err
}

// SegmentsPage returns one page of segments, optionally filtered by a
// case-insensitive search query, and the total number of matches.
func (s *Store) SegmentsPage(ctx context.Context, transcriptID int64, query string, offset, limit int) ([]model.Segment, int, error) {
	cond := "transcript_id = ?"
	args := []any{transcriptID}
	if q := strings.TrimSpace(query); q != "" {
		cond += ` AND text LIKE ? ESCAPE '\'`
		args = append(args, "%"+escapeLike(q)+"%")
	}
	var total int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM transcript_segments WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT position, start_seconds, end_seconds, text FROM transcript_segments
		WHERE `+cond+` ORDER BY position LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []model.Segment
	for rows.Next() {
		var sg model.Segment
		if err := rows.Scan(&sg.Position, &sg.Start, &sg.End, &sg.Text); err != nil {
			return nil, 0, err
		}
		out = append(out, sg)
	}
	return out, total, rows.Err()
}
