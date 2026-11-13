package quiz

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/quantamsage/ai-video-summarizer/internal/model"
)

// Sanitize converts raw model questions into validated questions. Invalid
// questions, questions of disallowed types and near-duplicates are dropped;
// the number dropped is returned.
func Sanitize(raw []RawQuestion, allowed []string, duration float64, timed bool) ([]model.Question, int) {
	allow := map[string]bool{}
	for _, t := range allowed {
		allow[t] = true
	}
	var out []model.Question
	var seen [][]string
	rejected := 0
	for _, r := range raw {
		q, err := ValidateQuestion(r)
		if err != nil || !allow[q.Type] {
			rejected++
			continue
		}
		words := keyWords(q.Prompt)
		dup := false
		for _, s := range seen {
			if jaccard(words, s) >= 0.8 {
				dup = true
				break
			}
		}
		if dup {
			rejected++
			continue
		}
		seen = append(seen, words)
		if timed && r.TimestampSeconds != nil && *r.TimestampSeconds >= 0 &&
			(duration <= 0 || *r.TimestampSeconds <= duration+1) {
			ts := math.Round(*r.TimestampSeconds)
			q.Timestamp = &ts
		}
		q.Position = len(out)
		out = append(out, q)
	}
	return out, rejected
}

// ValidateQuestion checks a single question's structure and normalizes it.
func ValidateQuestion(r RawQuestion) (model.Question, error) {
	q := model.Question{
		Type:         strings.TrimSpace(r.Type),
		Prompt:       strings.TrimSpace(r.Prompt),
		Explanation:  strings.TrimSpace(r.Explanation),
		SectionTitle: strings.TrimSpace(r.SectionTitle),
	}
	if q.Prompt == "" {
		return q, errors.New("question prompt is empty")
	}
	if q.Explanation == "" {
		return q, errors.New("question explanation is empty")
	}
	var opts []model.Option
	seen := map[string]bool{}
	for _, o := range r.Options {
		text := strings.TrimSpace(o.Text)
		key := strings.ToLower(text)
		if text == "" {
			return q, errors.New("empty option")
		}
		if seen[key] {
			return q, fmt.Errorf("duplicate option %q", text)
		}
		seen[key] = true
		opts = append(opts, model.Option{Text: text, IsCorrect: o.IsCorrect, Explanation: strings.TrimSpace(o.Explanation)})
	}
	correct := 0
	for _, o := range opts {
		if o.IsCorrect {
			correct++
		}
	}

	switch q.Type {
	case model.SingleChoice:
		if len(opts) < 3 || len(opts) > 6 {
			return q, errors.New("single choice needs 3-6 options")
		}
		if correct != 1 {
			return q, errors.New("single choice needs exactly one correct option")
		}
		if hasCatchAll(opts) {
			return q, errors.New("catch-all options are not allowed")
		}
	case model.MultipleAnswer:
		if len(opts) < 3 || len(opts) > 6 {
			return q, errors.New("multiple answer needs 3-6 options")
		}
		if correct < 2 || correct == len(opts) {
			return q, errors.New("multiple answer needs at least two correct and one incorrect option")
		}
		if hasCatchAll(opts) {
			return q, errors.New("catch-all options are not allowed")
		}
	case model.TrueFalse:
		if len(opts) != 2 || correct != 1 {
			return q, errors.New("true/false needs exactly two options with one correct")
		}
		var tOpt, fOpt *model.Option
		for i := range opts {
			switch strings.ToLower(opts[i].Text) {
			case "true":
				tOpt = &opts[i]
			case "false":
				fOpt = &opts[i]
			}
		}
		if tOpt == nil || fOpt == nil {
			return q, errors.New(`true/false options must be "True" and "False"`)
		}
		tOpt.Text, fOpt.Text = "True", "False"
		opts = []model.Option{*tOpt, *fOpt}
	case model.ShortAnswer:
		var accepted []string
		dedupe := map[string]bool{}
		for _, a := range r.AcceptedAnswers {
			a = strings.TrimSpace(a)
			n := NormalizeAnswer(a)
			if n != "" && !dedupe[n] {
				dedupe[n] = true
				accepted = append(accepted, a)
			}
		}
		if len(accepted) == 0 {
			return q, errors.New("short answer needs at least one accepted answer")
		}
		q.AcceptedAnswers = accepted
		opts = nil
	default:
		return q, fmt.Errorf("unknown question type %q", q.Type)
	}
	if q.Type != model.ShortAnswer && revealsAnswer(q.Prompt, opts) {
		return q, errors.New("question wording reveals the answer")
	}
	q.Options = opts
	return q, nil
}

func hasCatchAll(opts []model.Option) bool {
	for _, o := range opts {
		l := strings.ToLower(o.Text)
		if strings.Contains(l, "all of the above") || strings.Contains(l, "none of the above") {
			return true
		}
	}
	return false
}

// revealsAnswer flags single-correct questions whose prompt quotes the full
// text of the only correct option while no distractor appears in it.
func revealsAnswer(prompt string, opts []model.Option) bool {
	p := strings.ToLower(prompt)
	var correctHits, wrongHits, correct int
	for _, o := range opts {
		t := strings.ToLower(o.Text)
		if len(t) < 12 || t == "true" || t == "false" {
			continue
		}
		in := strings.Contains(p, t)
		if o.IsCorrect {
			correct++
			if in {
				correctHits++
			}
		} else if in {
			wrongHits++
		}
	}
	return correct == 1 && correctHits == 1 && wrongHits == 0
}

var stopWords = map[string]bool{"the": true, "a": true, "an": true, "of": true, "to": true, "in": true, "is": true,
	"what": true, "which": true, "does": true, "do": true, "for": true, "and": true, "or": true, "according": true,
	"video": true, "speaker": true, "following": true, "are": true, "on": true, "by": true, "with": true}

func keyWords(s string) []string {
	var out []string
	for _, w := range strings.Fields(NormalizeAnswer(s)) {
		if !stopWords[w] {
			out = append(out, w)
		}
	}
	return out
}

func jaccard(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	set := map[string]int{}
	for _, w := range a {
		set[w] |= 1
	}
	for _, w := range b {
		set[w] |= 2
	}
	inter := 0
	for _, v := range set {
		if v == 3 {
			inter++
		}
	}
	return float64(inter) / float64(len(set))
}
