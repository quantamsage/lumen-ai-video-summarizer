// Package summary generates structured learning summaries from transcripts.
//
// Short transcripts are summarized in a single request. Long transcripts use
// a map-reduce approach: each chunk is turned into structured notes (not a
// prose summary), notes are condensed hierarchically if needed, and a final
// synthesis request writes one coherent learning document from the complete
// set of notes. Chunk outputs are never concatenated directly.
package summary

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/quantamsage/ai-video-summarizer/internal/ai"
	"github.com/quantamsage/ai-video-summarizer/internal/model"
	"github.com/quantamsage/ai-video-summarizer/internal/transcript"
)

// Tunables for chunking. A single request handles transcripts up to
// SinglePassWords; longer ones are split into ChunkWords-sized chunks.
const (
	SinglePassWords = 12000
	ChunkWords      = 6000
	ChunkOverlap    = 200
	maxNotesWords   = 15000
)

// Input is the material to summarize.
type Input struct {
	Title       string
	Description string
	Duration    float64
	Segments    []model.Segment
}

// Progress is called with a 0-1 fraction as work proceeds.
type Progress func(fraction float64, stage string)

// Generator produces summaries with an AI provider.
type Generator struct {
	AI ai.Provider
}

// Generate builds a summary for the input transcript.
func (g *Generator) Generate(ctx context.Context, in Input, progress Progress) (*model.Summary, error) {
	if progress == nil {
		progress = func(float64, string) {}
	}
	words := 0
	for _, s := range in.Segments {
		words += transcript.WordCount(s.Text)
	}
	if words == 0 {
		return nil, transcript.ErrEmpty
	}
	timed := transcript.HasTiming(in.Segments)

	var material string
	if words <= SinglePassWords {
		material = "FULL TRANSCRIPT:\n" + transcript.Annotate(in.Segments, timed)
	} else {
		chunks := transcript.Chunks(in.Segments, ChunkWords, ChunkOverlap)
		notes := make([]string, len(chunks))
		for i, c := range chunks {
			progress(float64(i)/float64(len(chunks)+1)*0.8, fmt.Sprintf("Analyzing part %d of %d", i+1, len(chunks)))
			n, err := g.chunkNotes(ctx, in, c, len(chunks), timed)
			if err != nil {
				return nil, fmt.Errorf("analyze part %d: %w", i+1, err)
			}
			notes[i] = n
		}
		var err error
		notes, err = g.condense(ctx, in, notes)
		if err != nil {
			return nil, err
		}
		material = "SECTION NOTES (in chronological order, extracted from the complete transcript):\n\n" +
			strings.Join(notes, "\n\n")
	}

	progress(0.85, "Writing summary")
	var out finalSummary
	err := ai.CompleteJSON(ctx, g.AI, ai.Request{
		System:    systemPrompt,
		Prompt:    finalPrompt(in, timed, material),
		MaxTokens: 32000,
		Effort:    ai.EffortHigh,
		Schema:    finalSchema,
	}, &out, out.validate)
	if err != nil {
		return nil, err
	}
	sum := out.toModel(in.Duration, timed)
	sum.Provider = g.AI.Name()
	sum.Model = g.AI.Model()
	progress(1, "Summary ready")
	return sum, nil
}

const systemPrompt = `You are an expert educator who turns video transcripts into accurate study materials.
Rules you must always follow:
- Use only information that is actually present in the provided material. Never invent facts, names, numbers, examples or claims that were not discussed.
- Preserve important technical details exactly (commands, formulas, numbers, names, definitions).
- When you add explanatory context that was NOT stated in the video, you must mark it as external (is_external: true). Everything else must be grounded in the material.
- Transcripts may contain speech-recognition errors; silently correct obvious mis-transcriptions only when the intended word is clear from context.
- Write clearly and concisely for a learner. Avoid repetition across sections.`

// chunkNotes extracts structured notes from one chunk. These notes are an
// intermediate representation for the final synthesis, not user-facing text.
func (g *Generator) chunkNotes(ctx context.Context, in Input, c transcript.Chunk, total int, timed bool) (string, error) {
	var out chunkNotes
	timing := "The transcript has no timestamps; use null for all times."
	if timed {
		timing = fmt.Sprintf("This part spans %s to %s. Timestamps in [m:ss] markers are seconds offsets into the video; report start_seconds for topics using them.",
			transcript.Clock(c.Start), transcript.Clock(c.End))
	}
	prompt := fmt.Sprintf(`Video title: %s

This is part %d of %d of the transcript. %s
Parts overlap slightly at their boundaries.

Extract detailed structured notes from this part only. Capture every distinct topic in order, the key points and supporting details for each (keep technical specifics), and any terms or techniques that are defined or explained. These notes will later be merged with notes from the other parts into one document, so do not write an introduction or conclusion.

TRANSCRIPT PART:
%s`, in.Title, c.Index+1, total, timing, c.Text)
	err := ai.CompleteJSON(ctx, g.AI, ai.Request{
		System: systemPrompt, Prompt: prompt, MaxTokens: 8000, Effort: ai.EffortMedium, Schema: chunkSchema,
	}, &out, out.validate)
	if err != nil {
		return "", err
	}
	return out.render(c.Index+1, total), nil
}

// condense merges groups of notes until the combined notes fit comfortably
// in a single synthesis request.
func (g *Generator) condense(ctx context.Context, in Input, notes []string) ([]string, error) {
	for round := 0; wordsIn(notes) > maxNotesWords && len(notes) > 1; round++ {
		if round > 4 {
			return nil, errors.New("transcript is too long to summarize")
		}
		var merged []string
		for i := 0; i < len(notes); i += 4 {
			group := notes[i:min(i+4, len(notes))]
			var out chunkNotes
			prompt := fmt.Sprintf(`Video title: %s

Below are consecutive sets of notes from a long video. Merge them into a single set of structured notes covering the same span: keep every distinct topic in chronological order with its timestamp, merge duplicates caused by overlapping boundaries, and keep important technical details. Drop only redundancy.

NOTES:
%s`, in.Title, strings.Join(group, "\n\n"))
			err := ai.CompleteJSON(ctx, g.AI, ai.Request{
				System: systemPrompt, Prompt: prompt, MaxTokens: 8000, Effort: ai.EffortMedium, Schema: chunkSchema,
			}, &out, out.validate)
			if err != nil {
				return nil, fmt.Errorf("condense notes: %w", err)
			}
			merged = append(merged, out.render(len(merged)+1, (len(notes)+3)/4))
		}
		notes = merged
	}
	return notes, nil
}

func wordsIn(notes []string) int {
	n := 0
	for _, s := range notes {
		n += transcript.WordCount(s)
	}
	return n
}

func finalPrompt(in Input, timed bool, material string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Video title: %s\n", in.Title)
	if d := strings.TrimSpace(in.Description); d != "" {
		fmt.Fprintf(&b, "Video description (from the uploader, may be incomplete): %s\n", truncateWords(d, 150))
	}
	if in.Duration > 0 {
		fmt.Fprintf(&b, "Duration: %s (%.0f seconds)\n", transcript.Clock(in.Duration), in.Duration)
	}
	if timed {
		b.WriteString("Timestamps: available. [m:ss] markers give the time offset; set start_seconds/end_seconds (in seconds) for each topic from them.\n")
	} else {
		b.WriteString("Timestamps: not available. Use null for start_seconds and end_seconds.\n")
	}
	b.WriteString(`
Write a complete learning summary of this video as ONE coherent document:
- overview: 2-4 sentences explaining what the video is about and who it is for.
- detailed_summary: a structured explanation of the major topics in logical order. Use 3-8 parts, each with a short heading and 1-3 paragraphs. It must read as a single connected narrative, not a list of separate summaries.
- key_takeaways: the 4-10 most important lessons, each one sentence.
- concepts: important terms, definitions, techniques and ideas discussed (up to 20). Category is one of: term, technique, tool, principle, idea. Definitions must reflect how the video explains them.
- topics: the topic breakdown, dividing the whole video into logical sections in chronological order (3-15 sections), each with a title, a 1-3 sentence summary, and start/end times.
- learning_notes: 0-6 short notes that make difficult concepts easier to understand. Set is_external to true for any note that adds context not stated in the video.

`)
	b.WriteString(material)
	return b.String()
}

func truncateWords(s string, n int) string {
	w := strings.Fields(s)
	if len(w) <= n {
		return s
	}
	return strings.Join(w[:n], " ") + "…"
}

// Schemas -------------------------------------------------------------------

func obj(props map[string]any) map[string]any {
	req := make([]string, 0, len(props))
	for k := range props {
		req = append(req, k)
	}
	sort.Strings(req)
	return map[string]any{"type": "object", "properties": props, "required": req, "additionalProperties": false}
}

func arr(items any) map[string]any { return map[string]any{"type": "array", "items": items} }

var (
	str        = map[string]any{"type": "string"}
	boolean    = map[string]any{"type": "boolean"}
	nullableNo = map[string]any{"anyOf": []any{map[string]any{"type": "number"}, map[string]any{"type": "null"}}}
)

var chunkSchema = obj(map[string]any{
	"topics": arr(obj(map[string]any{
		"title":         str,
		"start_seconds": nullableNo,
		"points":        arr(str),
	})),
	"terms": arr(obj(map[string]any{"term": str, "explanation": str})),
})

var finalSchema = obj(map[string]any{
	"overview":         str,
	"detailed_summary": arr(obj(map[string]any{"heading": str, "content": str})),
	"key_takeaways":    arr(str),
	"concepts":         arr(obj(map[string]any{"term": str, "definition": str, "category": str})),
	"topics": arr(obj(map[string]any{
		"title": str, "summary": str, "start_seconds": nullableNo, "end_seconds": nullableNo,
	})),
	"learning_notes": arr(obj(map[string]any{"note": str, "is_external": boolean})),
})

// Response types ------------------------------------------------------------

type chunkNotes struct {
	Topics []struct {
		Title        string   `json:"title"`
		StartSeconds *float64 `json:"start_seconds"`
		Points       []string `json:"points"`
	} `json:"topics"`
	Terms []struct {
		Term        string `json:"term"`
		Explanation string `json:"explanation"`
	} `json:"terms"`
}

func (c *chunkNotes) validate() error {
	if len(c.Topics) == 0 {
		return errors.New("notes must contain at least one topic")
	}
	return nil
}

func (c *chunkNotes) render(part, total int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### Notes %d of %d\n", part, total)
	for _, t := range c.Topics {
		ts := ""
		if t.StartSeconds != nil {
			ts = fmt.Sprintf("[%s] ", transcript.Clock(*t.StartSeconds))
		}
		fmt.Fprintf(&b, "Topic: %s%s\n", ts, t.Title)
		for _, p := range t.Points {
			fmt.Fprintf(&b, "  - %s\n", p)
		}
	}
	if len(c.Terms) > 0 {
		b.WriteString("Terms:\n")
		for _, t := range c.Terms {
			fmt.Fprintf(&b, "  - %s: %s\n", t.Term, t.Explanation)
		}
	}
	return b.String()
}

type finalSummary struct {
	Overview        string `json:"overview"`
	DetailedSummary []struct {
		Heading string `json:"heading"`
		Content string `json:"content"`
	} `json:"detailed_summary"`
	KeyTakeaways []string `json:"key_takeaways"`
	Concepts     []struct {
		Term       string `json:"term"`
		Definition string `json:"definition"`
		Category   string `json:"category"`
	} `json:"concepts"`
	Topics []struct {
		Title        string   `json:"title"`
		Summary      string   `json:"summary"`
		StartSeconds *float64 `json:"start_seconds"`
		EndSeconds   *float64 `json:"end_seconds"`
	} `json:"topics"`
	LearningNotes []model.LearningNote `json:"learning_notes"`
}

func (f *finalSummary) validate() error {
	var problems []string
	if strings.TrimSpace(f.Overview) == "" {
		problems = append(problems, "overview is empty")
	}
	nonEmpty := 0
	for _, d := range f.DetailedSummary {
		if strings.TrimSpace(d.Content) != "" {
			nonEmpty++
		}
	}
	if nonEmpty == 0 {
		problems = append(problems, "detailed_summary is empty")
	}
	if len(f.KeyTakeaways) == 0 {
		problems = append(problems, "key_takeaways is empty")
	}
	if len(f.Topics) == 0 {
		problems = append(problems, "topics is empty")
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

var validCategories = map[string]bool{"term": true, "technique": true, "tool": true, "principle": true, "idea": true}

// toModel cleans the AI output: trims text, removes duplicates, clamps
// timestamps to the video duration and sorts sections chronologically.
func (f *finalSummary) toModel(duration float64, timed bool) *model.Summary {
	sum := &model.Summary{Overview: strings.TrimSpace(f.Overview)}

	var parts []string
	for _, d := range f.DetailedSummary {
		content := strings.TrimSpace(d.Content)
		if content == "" {
			continue
		}
		if h := strings.TrimSpace(d.Heading); h != "" {
			parts = append(parts, "## "+strings.TrimLeft(h, "# "))
		}
		parts = append(parts, content)
	}
	sum.Detailed = strings.Join(parts, "\n\n")

	seen := map[string]bool{}
	for _, k := range f.KeyTakeaways {
		k = strings.TrimSpace(k)
		if k != "" && !seen[strings.ToLower(k)] {
			seen[strings.ToLower(k)] = true
			sum.KeyTakeaways = append(sum.KeyTakeaways, k)
		}
	}

	seen = map[string]bool{}
	for _, c := range f.Concepts {
		term, def := strings.TrimSpace(c.Term), strings.TrimSpace(c.Definition)
		key := strings.ToLower(term)
		if term == "" || def == "" || seen[key] {
			continue
		}
		seen[key] = true
		cat := strings.ToLower(strings.TrimSpace(c.Category))
		if !validCategories[cat] {
			cat = "term"
		}
		sum.Concepts = append(sum.Concepts, model.Concept{Term: term, Definition: def, Category: cat})
	}

	for _, t := range f.Topics {
		title := strings.TrimSpace(t.Title)
		if title == "" {
			continue
		}
		sec := model.Section{Title: title, Content: strings.TrimSpace(t.Summary)}
		if timed {
			sec.Start = clampTime(t.StartSeconds, duration)
			sec.End = clampTime(t.EndSeconds, duration)
		}
		sum.Sections = append(sum.Sections, sec)
	}
	if timed {
		sort.SliceStable(sum.Sections, func(i, j int) bool {
			a, b := sum.Sections[i].Start, sum.Sections[j].Start
			if a == nil || b == nil {
				return false
			}
			return *a < *b
		})
		// Fill missing end times from the next section's start.
		for i := range sum.Sections {
			s := &sum.Sections[i]
			if s.Start == nil {
				continue
			}
			if i+1 < len(sum.Sections) && sum.Sections[i+1].Start != nil {
				next := *sum.Sections[i+1].Start
				if s.End == nil || *s.End > next || *s.End <= *s.Start {
					s.End = &next
				}
			} else if s.End != nil && *s.End <= *s.Start {
				s.End = nil
			}
		}
	}
	for i := range sum.Sections {
		sum.Sections[i].Position = i
	}

	for _, n := range f.LearningNotes {
		if n.Note = strings.TrimSpace(n.Note); n.Note != "" {
			sum.LearningNotes = append(sum.LearningNotes, n)
		}
	}
	return sum
}

func clampTime(t *float64, duration float64) *float64 {
	if t == nil || math.IsNaN(*t) || *t < 0 {
		return nil
	}
	if duration > 0 && *t > duration+1 {
		return nil
	}
	v := math.Round(*t)
	return &v
}
