package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"github.com/quantamsage/ai-video-summarizer/internal/apperr"
)

type scripted struct {
	responses []string
	calls     []Request
}

func (s *scripted) Name() string  { return "scripted" }
func (s *scripted) Model() string { return "m" }
func (s *scripted) Complete(_ context.Context, r Request) (string, error) {
	s.calls = append(s.calls, r)
	if len(s.responses) == 0 {
		return "", errors.New("no more responses")
	}
	out := s.responses[0]
	s.responses = s.responses[1:]
	return out, nil
}

func TestExtractJSON(t *testing.T) {
	cases := map[string]string{
		`{"a":1}`:                         `{"a":1}`,
		"```json\n{\"a\":1}\n```":         `{"a":1}`,
		"Here you go:\n{\"a\":{\"b\":2}}": `{"a":{"b":2}}`,
	}
	for in, want := range cases {
		got, err := ExtractJSON(in)
		if err != nil || got != want {
			t.Errorf("ExtractJSON(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := ExtractJSON("no json here"); err == nil {
		t.Error("expected error")
	}
}

func TestCompleteJSONRetriesOnceThenFails(t *testing.T) {
	var out struct{ A int }
	p := &scripted{responses: []string{"not json", `{"A": 3}`}}
	if err := CompleteJSON(context.Background(), p, Request{Prompt: "p"}, &out, nil); err != nil {
		t.Fatal(err)
	}
	if out.A != 3 || len(p.calls) != 2 || !strings.Contains(p.calls[1].Prompt, "could not be used") {
		t.Fatalf("retry behaviour: %+v calls=%d", out, len(p.calls))
	}

	p = &scripted{responses: []string{`{"A": 0}`, `{"A": 0}`}}
	err := CompleteJSON(context.Background(), p, Request{Prompt: "p"}, &out, func() error {
		if out.A == 0 {
			return errors.New("A must be positive")
		}
		return nil
	})
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("expected ErrMalformed after failed validation, got %v", err)
	}
}

func sse(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, e := range events {
		var probe struct{ Type string }
		json.Unmarshal([]byte(e), &probe)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", probe.Type, e)
	}
}

func TestAnthropicRequestShapeAndStreaming(t *testing.T) {
	var body map[string]any
	var beta string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &body)
		beta = r.Header.Get("anthropic-beta")
		sse(w,
			`{"type":"message_start","message":{"id":"m1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"usage":{"input_tokens":5,"output_tokens":0}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"{\"ok\":"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"true}"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`,
			`{"type":"message_stop"}`,
		)
	}))
	defer srv.Close()

	p := NewAnthropic("test-key", "claude-opus-5-5", option.WithBaseURL(srv.URL))
	out, err := p.Complete(context.Background(), Request{
		System: "sys", Prompt: "hello", Effort: EffortHigh,
		Schema: map[string]any{"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}},
			"required": []string{"ok"}, "additionalProperties": false},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != `{"ok":true}` {
		t.Fatalf("accumulated text: %q", out)
	}
	if body["model"] != "claude-opus-5-5" || body["fallbacks"] != "default" {
		t.Fatalf("request body: %v", body)
	}
	oc, _ := body["output_config"].(map[string]any)
	if oc["effort"] != "high" || oc["format"] == nil {
		t.Fatalf("output_config: %v", body["output_config"])
	}
	if !strings.Contains(beta, "server-side-fallback") {
		t.Fatalf("missing fallback beta header: %q", beta)
	}
}

func TestAnthropicErrorClassification(t *testing.T) {
	status := http.StatusTooManyRequests
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(`{"type":"error","error":{"type":"x","message":"nope"}}`))
	}))
	defer srv.Close()
	p := NewAnthropic("k", "claude-opus-5-5", option.WithBaseURL(srv.URL), option.WithMaxRetries(0))
	_, err := p.Complete(context.Background(), Request{Prompt: "x"})
	if !apperr.IsTemporary(err) {
		t.Fatalf("429 should be temporary, got %v", err)
	}
	status = http.StatusUnauthorized
	_, err = p.Complete(context.Background(), Request{Prompt: "x"})
	if apperr.IsTemporary(err) || apperr.UserMessage(err, "") == "" {
		t.Fatalf("401 should be a permanent user-facing error, got %v", err)
	}
}

func TestOpenAIProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if rf, _ := body["response_format"].(map[string]any); rf["type"] != "json_schema" {
			http.Error(w, "missing schema", 400)
			return
		}
		w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"content":"{\"ok\":true}"}}]}`))
	}))
	defer srv.Close()
	p := NewOpenAI(srv.URL, "k", "gpt-test")
	out, err := p.Complete(context.Background(), Request{Prompt: "x", Schema: map[string]any{"type": "object"}})
	if err != nil || out != `{"ok":true}` {
		t.Fatalf("%q %v", out, err)
	}
}
