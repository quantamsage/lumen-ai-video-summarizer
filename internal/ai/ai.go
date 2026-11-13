// Package ai defines the interchangeable LLM provider interface and helpers
// for requesting and validating structured JSON output.
package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/quantamsage/ai-video-summarizer/internal/apperr"
)

// ErrMalformed is returned when the model's output cannot be parsed or does
// not satisfy validation.
var ErrMalformed = errors.New("AI returned a malformed response")

// Effort hints how much reasoning a request deserves. Providers that do
// not support effort control ignore it.
type Effort string

const (
	EffortLow    Effort = "low"
	EffortMedium Effort = "medium"
	EffortHigh   Effort = "high"
)

// Request is a single-turn completion request.
type Request struct {
	System    string
	Prompt    string
	MaxTokens int
	Effort    Effort
	// Schema, when set, is a JSON schema the response must conform to.
	// Schemas must list every property in "required" and set
	// "additionalProperties": false so they work with strict structured
	// output modes.
	Schema map[string]any
}

// Provider is an LLM backend.
type Provider interface {
	Name() string
	Model() string
	Complete(ctx context.Context, req Request) (string, error)
}

// CompleteJSON runs req and decodes the JSON response into out, then calls
// validate (if non-nil). If decoding or validation fails, it retries once,
// telling the model what was wrong. Errors from the provider itself are
// returned unchanged so temporary failures can be retried by the caller.
func CompleteJSON(ctx context.Context, p Provider, req Request, out any, validate func() error) error {
	raw, err := p.Complete(ctx, req)
	if err != nil {
		return err
	}
	problem := decodeAndValidate(raw, out, validate)
	if problem == nil {
		return nil
	}

	retry := req
	retry.Prompt = req.Prompt + "\n\n---\nYour previous response could not be used: " + problem.Error() +
		"\nRespond again with only a single valid JSON object that follows the required format exactly."
	raw, err = p.Complete(ctx, retry)
	if err != nil {
		return err
	}
	if problem := decodeAndValidate(raw, out, validate); problem != nil {
		return fmt.Errorf("%w: %v", ErrMalformed, problem)
	}
	return nil
}

func decodeAndValidate(raw string, out any, validate func() error) error {
	body, err := ExtractJSON(raw)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(strings.NewReader(body))
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("invalid JSON: %v", err)
	}
	if validate != nil {
		if err := validate(); err != nil {
			return err
		}
	}
	return nil
}

// ExtractJSON returns the JSON object in s, tolerating surrounding prose or
// Markdown code fences.
func ExtractJSON(s string) (string, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		if i := strings.LastIndex(s, "```"); i >= 0 {
			s = s[:i]
		}
		s = strings.TrimSpace(s)
	}
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return "", errors.New("response did not contain a JSON object")
	}
	return s[start : end+1], nil
}

// IsTemporary reports whether a provider error should be retried later.
func IsTemporary(err error) bool { return apperr.IsTemporary(err) }
