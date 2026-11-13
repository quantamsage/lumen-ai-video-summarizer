// Package quiz generates, validates and grades quizzes.
package quiz

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/quantamsage/ai-video-summarizer/internal/ai"
	"github.com/quantamsage/ai-video-summarizer/internal/model"
	"github.com/quantamsage/ai-video-summarizer/internal/transcript"
)

const (
	MinQuestions = 1
	MaxQuestions = 30
	// maxSourceWords caps how much transcript is sent with a quiz request;
	// beyond it the summary carries the remaining content.
	maxSourceWords = 60000
)

// ErrNoQuestions is returned when no valid questions could be produced.
var ErrNoQuestions = errors.New("no valid quiz questions could be generated from this content")

// ValidateConfig normalizes and checks a quiz configuration.
func ValidateConfig(c *model.QuizConfig) error {
	if c.Count < MinQuestions || c.Count > MaxQuestions {
		return fmt.Errorf("number of questions must be between %d and %d", MinQuestions, MaxQuestions)
	}
	c.Difficulty = strings.ToLower(strings.TrimSpace(c.Difficulty))
	ok := false
	for _, d := range model.Difficulties {
		ok = ok || d == c.Difficulty
	}
	if !ok {
		return errors.New("difficulty must be easy, medium or hard")
	}
	seen := map[string]bool{}
	var types []string
	for _, t := range c.Types {
		valid := false
		for _, v := range model.QuestionTypes {
			valid = valid || v == t
		}
		if !valid {
			return fmt.Errorf("unknown question type %q", t)
		}
		if !seen[t] {
			seen[t] = true
			types = append(types, t)
		}
	}
	if len(types) == 0 {
		return errors.New("choose at least one question type")
	}
	c.Types = types
	return nil
}

// Material is the source content a quiz is generated from.
type Material struct {
	Title    string
	Duration float64
	Segments []model.Segment
	Summary  *model.Summary
	// Section, when non-nil, scopes the quiz to one topic section.
	Section *model.Section
}

// Generator produces quizzes with an AI provider.
type Generator struct {
	AI ai.Provider
}

// Generate creates a quiz for the given configuration. Questions failing
// validation or verification are discarded; if fewer than requested remain,
// Quiz.ShortfallReason explains why.
func (g *Generator) Generate(ctx context.Context, cfg model.QuizConfig, m Material) (*model.Quiz, error) {
	if err := ValidateConfig(&cfg); err != nil {
		return nil, err
	}
	segs := m.Segments
	scope := "full"
	scopeLabel := "the entire video"
	if m.Section != nil {
		scope = fmt.Sprint(m.Section.Position)
		scopeLabel = fmt.Sprintf("only the section %q", m.Section.Title)
		if m.Section.Start != nil {
			segs = transcript.Window(m.Segments, *m.Section.Start, m.Section.End)
		}
	}
	if len(segs) == 0 {
		return nil, transcript.ErrEmpty
	}
	source := buildSource(m, segs)

	var gen generated
	err := ai.CompleteJSON(ctx, g.AI, ai.Request{
		System:    quizSystemPrompt,
		Prompt:    generationPrompt(cfg, m, scopeLabel, source),
		MaxTokens: 32000,
		Effort:    ai.EffortHigh,
		Schema:    generatedSchema,
	}, &gen, func() error {
		if len(gen.Questions) == 0 && strings.TrimSpace(gen.ShortfallReason) == "" {
			return errors.New("return at least one question, or explain in shortfall_reason why none can be written")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	questions, rejected := Sanitize(gen.Questions, cfg.Types, m.Duration, transcript.HasTiming(segs))
	if len(questions) > 0 {
		var removed int
		questions, removed, err = g.verify(ctx, questions, source)
		if err != nil {
			return nil, err
		}
		rejected += removed
	}
	if len(questions) > cfg.Count {
		questions = questions[:cfg.Count]
	}
	if len(questions) == 0 {
		reason := strings.TrimSpace(gen.ShortfallReason)
		if reason == "" {
			return nil, ErrNoQuestions
		}
		return nil, fmt.Errorf("%w: %s", ErrNoQuestions, reason)
	}

	q := &model.Quiz{
		Title:          quizTitle(m, cfg),
		Difficulty:     cfg.Difficulty,
		Scope:          scope,
		RequestedCount: cfg.Count,
		Questions:      questions,
	}
	if len(questions) < cfg.Count {
		reason := strings.TrimSpace(gen.ShortfallReason)
		if rejected > 0 {
			note := fmt.Sprintf("%d generated question(s) were discarded because they failed quality checks.", rejected)
			reason = strings.TrimSpace(reason + " " + note)
		}
		if reason == "" {
			reason = "The video does not contain enough distinct material for more questions at this difficulty."
		}
		q.ShortfallReason = reason
	}
	return q, nil
}

func quizTitle(m Material, cfg model.QuizConfig) string {
	diff := strings.ToUpper(cfg.Difficulty[:1]) + cfg.Difficulty[1:]
	if m.Section != nil {
		return fmt.Sprintf("%s quiz: %s", diff, m.Section.Title)
	}
	return fmt.Sprintf("%s quiz: %s", diff, m.Title)
}

// buildSource assembles the grounding material: the transcript (capped)
// plus the summary's topic breakdown for orientation.
func buildSource(m Material, segs []model.Segment) string {
	timed := transcript.HasTiming(segs)
	var b strings.Builder
	words := 0
	var kept []model.Segment
	for _, s := range segs {
		w := transcript.WordCount(s.Text)
		if words+w > maxSourceWords {
			break
		}
		words += w
		kept = append(kept, s)
	}
	b.WriteString("TRANSCRIPT")
	if len(kept) < len(segs) {
		b.WriteString(" (truncated; the summary below covers the remainder)")
	}
	b.WriteString(":\n")
	b.WriteString(transcript.Annotate(kept, timed))
	if m.Summary != nil {
		b.WriteString("\n\nSUMMARY OF THE VIDEO (derived from the transcript):\n")
		b.WriteString(m.Summary.Overview)
		for _, s := range m.Summary.Sections {
			ts := ""
			if s.Start != nil {
				ts = "[" + transcript.Clock(*s.Start) + "] "
			}
			fmt.Fprintf(&b, "\n- %s%s: %s", ts, s.Title, s.Content)
		}
		if len(kept) < len(segs) {
			b.WriteString("\n\n" + m.Summary.Detailed)
		}
	}
	return b.String()
}

const quizSystemPrompt = `You write rigorous assessment questions for learners, based strictly on a video's content.
Rules:
- Every question must be answerable from the provided video material alone. Never ask about facts that are absent from it.
- The correct answer must be explicitly supported by the material. Do not rely on outside knowledge for the answer key.
- Do not create duplicate or near-duplicate questions; each question should test a different idea.
- single_choice: 4 options, exactly one correct, three plausible distractors drawn from related ideas in the video.
- multiple_answer: 4-5 options, at least two correct and at least one incorrect; the correct set must be unambiguous.
- true_false: options must be exactly "True" and "False" with one correct; statements must be unambiguously true or false according to the video.
- short_answer: a question with a short factual answer (a term, name, number or short phrase); give 1-5 accepted_answers covering acceptable phrasings. options must be empty.
- Never let the wording of a question reveal its answer, and avoid "all of the above" / "none of the above".
- For every option, explain why it is correct or why it is wrong. The question explanation must justify the correct answer using the video.
- If the material cannot support the requested number of good questions, write fewer and explain why in shortfall_reason. Never pad with weak or fabricated questions.`

func generationPrompt(cfg model.QuizConfig, m Material, scopeLabel, source string) string {
	var difficulty string
	switch cfg.Difficulty {
	case "easy":
		difficulty = "EASY: test recall and recognition of key facts, definitions and main ideas stated directly in the video."
	case "medium":
		difficulty = "MEDIUM: test understanding — explaining why, comparing related ideas, and identifying correct applications of concepts from the video."
	case "hard":
		difficulty = "HARD: test analysis and application — multi-step reasoning, applying techniques from the video to new scenarios, distinguishing subtle differences, and spotting misconceptions. Distractors should be close but clearly wrong according to the video."
	}
	types := make([]string, len(cfg.Types))
	copy(types, cfg.Types)
	sort.Strings(types)
	return fmt.Sprintf(`Video title: %s
Write %d questions covering %s.
Allowed question types (use only these, mixing them sensibly): %s
Difficulty: %s
For each question set timestamp_seconds to the approximate time (in seconds, from the [m:ss] markers) where the answer is discussed, or null if unknown, and section_title to the topic it relates to.

%s`, m.Title, cfg.Count, scopeLabel, strings.Join(types, ", "), difficulty, source)
}

// verify asks the model to check each question against the source and
// drops any that are unsupported, ambiguous or have a wrong answer key. The
// answer key itself is never modified here.
func (g *Generator) verify(ctx context.Context, qs []model.Question, source string) ([]model.Question, int, error) {
	var b strings.Builder
	for i, q := range qs {
		fmt.Fprintf(&b, "Question %d (%s): %s\n", i, q.Type, q.Prompt)
		for _, o := range q.Options {
			mark := " "
			if o.IsCorrect {
				mark = "*"
			}
			fmt.Fprintf(&b, "  [%s] %s\n", mark, o.Text)
		}
		if q.Type == model.ShortAnswer {
			fmt.Fprintf(&b, "  Accepted answers: %s\n", strings.Join(q.AcceptedAnswers, " | "))
		}
	}
	var out struct {
		Results []struct {
			Index  int    `json:"index"`
			Valid  bool   `json:"valid"`
			Reason string `json:"reason"`
		} `json:"results"`
	}
	prompt := fmt.Sprintf(`Review these quiz questions against the source material. Correct options are marked [*].
A question is valid only if: it is answerable from the material, the marked answer key is correct according to the material, exactly the marked options are correct (no other option is also defensible), and the wording does not give the answer away.
Return one result per question index.

QUESTIONS:
%s
SOURCE MATERIAL:
%s`, b.String(), source)
	err := ai.CompleteJSON(ctx, g.AI, ai.Request{
		System: "You are a meticulous assessment reviewer. Judge only against the provided source material.",
		Prompt: prompt, MaxTokens: 8000, Effort: ai.EffortMedium, Schema: verifySchema,
	}, &out, func() error {
		if len(out.Results) == 0 {
			return errors.New("results must not be empty")
		}
		return nil
	})
	if err != nil {
		return nil, 0, fmt.Errorf("verify questions: %w", err)
	}
	invalid := map[int]bool{}
	for _, r := range out.Results {
		if !r.Valid {
			invalid[r.Index] = true
		}
	}
	var kept []model.Question
	for i, q := range qs {
		if !invalid[i] {
			kept = append(kept, q)
		}
	}
	return kept, len(qs) - len(kept), nil
}

// Schemas ---------------------------------------------------------------------

func obj(props map[string]any) map[string]any {
	req := make([]string, 0, len(props))
	for k := range props {
		req = append(req, k)
	}
	sort.Strings(req)
	return map[string]any{"type": "object", "properties": props, "required": req, "additionalProperties": false}
}

var (
	str      = map[string]any{"type": "string"}
	boolean  = map[string]any{"type": "boolean"}
	integer  = map[string]any{"type": "integer"}
	nullable = map[string]any{"anyOf": []any{map[string]any{"type": "number"}, map[string]any{"type": "null"}}}
)

var generatedSchema = obj(map[string]any{
	"questions": map[string]any{"type": "array", "items": obj(map[string]any{
		"type":              map[string]any{"type": "string", "enum": []string{model.SingleChoice, model.MultipleAnswer, model.TrueFalse, model.ShortAnswer}},
		"prompt":            str,
		"options":           map[string]any{"type": "array", "items": obj(map[string]any{"text": str, "is_correct": boolean, "explanation": str})},
		"accepted_answers":  map[string]any{"type": "array", "items": str},
		"explanation":       str,
		"timestamp_seconds": nullable,
		"section_title":     str,
	})},
	"shortfall_reason": str,
})

var verifySchema = obj(map[string]any{
	"results": map[string]any{"type": "array", "items": obj(map[string]any{
		"index": integer, "valid": boolean, "reason": str,
	})},
})

type generated struct {
	Questions       []RawQuestion `json:"questions"`
	ShortfallReason string        `json:"shortfall_reason"`
}

// RawQuestion is a question as returned by the model, before validation.
type RawQuestion struct {
	Type    string `json:"type"`
	Prompt  string `json:"prompt"`
	Options []struct {
		Text        string `json:"text"`
		IsCorrect   bool   `json:"is_correct"`
		Explanation string `json:"explanation"`
	} `json:"options"`
	AcceptedAnswers  []string `json:"accepted_answers"`
	Explanation      string   `json:"explanation"`
	TimestampSeconds *float64 `json:"timestamp_seconds"`
	SectionTitle     string   `json:"section_title"`
}
