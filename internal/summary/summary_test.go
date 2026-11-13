package summary

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/quantamsage/ai-video-summarizer/internal/ai"
	"github.com/quantamsage/ai-video-summarizer/internal/ai/aitest"
	"github.com/quantamsage/ai-video-summarizer/internal/model"
	"github.com/quantamsage/ai-video-summarizer/internal/transcript"
)

const goodFinal = `{
  "overview": "An introduction to Go channels.",
  "detailed_summary": [{"heading": "Channels", "content": "Channels connect goroutines."}, {"heading": "", "content": "  "}],
  "key_takeaways": ["Use channels to communicate", "use channels to communicate", "Close channels from the sender"],
  "concepts": [{"term": "Channel", "definition": "A typed conduit", "category": "TERM"},
               {"term": "channel", "definition": "dup", "category": "term"},
               {"term": "select", "definition": "Waits on multiple channel operations", "category": "weird"}],
  "topics": [{"title": "Closing", "summary": "s2", "start_seconds": 60, "end_seconds": 30},
             {"title": "Intro", "summary": "s1", "start_seconds": 0, "end_seconds": null},
             {"title": "Bogus", "summary": "s3", "start_seconds": 99999, "end_seconds": null}],
  "learning_notes": [{"note": "Similar to Unix pipes", "is_external": true}, {"note": " ", "is_external": false}]
}`

func segments(n int, wordsEach int) []model.Segment {
	var segs []model.Segment
	for i := 0; i < n; i++ {
		segs = append(segs, model.Segment{Start: float64(i * 10), End: float64(i*10 + 10),
			Text: strings.TrimSpace(strings.Repeat("channel ", wordsEach))})
	}
	return segs
}

func TestSinglePassSummaryCleaned(t *testing.T) {
	mock := (&aitest.Mock{}).On("FULL TRANSCRIPT", goodFinal)
	g := &Generator{AI: mock}
	sum, err := g.Generate(context.Background(), Input{Title: "Go", Duration: 120, Segments: segments(12, 5)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(mock.Calls) != 1 {
		t.Fatalf("short transcript should use one call, got %d", len(mock.Calls))
	}
	if mock.Calls[0].Schema == nil || mock.Calls[0].Effort != ai.EffortHigh {
		t.Fatal("final request should use a schema and high effort")
	}
	if len(sum.KeyTakeaways) != 2 {
		t.Errorf("duplicate takeaways not removed: %v", sum.KeyTakeaways)
	}
	if len(sum.Concepts) != 2 || sum.Concepts[0].Category != "term" || sum.Concepts[1].Category != "term" {
		t.Errorf("concepts not cleaned: %+v", sum.Concepts)
	}
	if sum.Sections[0].Title != "Intro" || *sum.Sections[0].End != 60 {
		t.Errorf("sections should be sorted with end filled from next start: %+v", sum.Sections[0])
	}
	if sum.Sections[1].End != nil {
		t.Errorf("invalid end (before start) should be dropped: %v", *sum.Sections[1].End)
	}
	for _, s := range sum.Sections {
		if s.Title == "Bogus" && s.Start != nil {
			t.Errorf("out-of-range timestamp should be cleared")
		}
	}
	if !strings.HasPrefix(sum.Detailed, "## Channels") {
		t.Errorf("detailed summary: %q", sum.Detailed)
	}
	if len(sum.LearningNotes) != 1 || !sum.LearningNotes[0].IsExternal {
		t.Errorf("learning notes: %+v", sum.LearningNotes)
	}
	if sum.Provider != "mock" {
		t.Errorf("provider not recorded")
	}
}

func TestLongTranscriptUsesChunkNotesThenSynthesis(t *testing.T) {
	notes := `{"topics":[{"title":"Part topic","start_seconds":10,"points":["a point"]}],"terms":[]}`
	mock := (&aitest.Mock{}).On("TRANSCRIPT PART", notes).On("SECTION NOTES", goodFinal)
	g := &Generator{AI: mock}
	// 2,000 segments x 10 words = 20,000 words > SinglePassWords
	in := Input{Title: "Long", Duration: 20000, Segments: segments(2000, 10)}
	var stages []string
	sum, err := g.Generate(context.Background(), in, func(f float64, s string) { stages = append(stages, s) })
	if err != nil {
		t.Fatal(err)
	}
	parts := mock.CallCount("TRANSCRIPT PART")
	if parts < 3 {
		t.Fatalf("expected several chunk calls, got %d", parts)
	}
	if mock.CallCount("SECTION NOTES") != 1 {
		t.Fatal("expected exactly one synthesis call")
	}
	final := mock.Calls[len(mock.Calls)-1].Prompt
	if strings.Count(final, "### Notes") != parts {
		t.Fatalf("synthesis prompt should include notes from every chunk")
	}
	if sum.Overview == "" || len(stages) == 0 {
		t.Fatal("missing output or progress")
	}
}

func TestMalformedResponseFails(t *testing.T) {
	mock := (&aitest.Mock{}).On("FULL TRANSCRIPT", `{"overview": ""}`)
	g := &Generator{AI: mock}
	_, err := g.Generate(context.Background(), Input{Title: "x", Segments: segments(3, 3)}, nil)
	if !errors.Is(err, ai.ErrMalformed) {
		t.Fatalf("expected ErrMalformed, got %v", err)
	}
	if len(mock.Calls) != 2 {
		t.Fatalf("expected one repair retry, got %d calls", len(mock.Calls))
	}
}

func TestEmptyTranscript(t *testing.T) {
	g := &Generator{AI: &aitest.Mock{}}
	_, err := g.Generate(context.Background(), Input{Segments: []model.Segment{{Text: ""}}}, nil)
	if !errors.Is(err, transcript.ErrEmpty) {
		t.Fatalf("expected ErrEmpty, got %v", err)
	}
}

func TestUntimedTranscriptDropsTimestamps(t *testing.T) {
	mock := (&aitest.Mock{}).On("FULL TRANSCRIPT", goodFinal)
	g := &Generator{AI: mock}
	sum, err := g.Generate(context.Background(), Input{Title: "x", Segments: []model.Segment{{Text: "hello world"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sum.Sections {
		if s.Start != nil || s.End != nil {
			t.Fatalf("untimed transcript should not have section times: %+v", s)
		}
	}
	if !strings.Contains(mock.Calls[0].Prompt, "Timestamps: not available") {
		t.Fatal("prompt should state that timestamps are unavailable")
	}
}
