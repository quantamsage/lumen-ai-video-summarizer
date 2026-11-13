package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/quantamsage/ai-video-summarizer/internal/apperr"
)

// OpenAI calls an OpenAI-compatible Chat Completions endpoint.
type OpenAI struct {
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
}

func NewOpenAI(baseURL, apiKey, model string) *OpenAI {
	return &OpenAI{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		model:   model,
		client:  &http.Client{Timeout: 10 * time.Minute},
	}
}

func (o *OpenAI) Name() string  { return "openai" }
func (o *OpenAI) Model() string { return o.model }

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (o *OpenAI) Complete(ctx context.Context, req Request) (string, error) {
	body := map[string]any{
		"model":    o.model,
		"messages": []chatMessage{},
	}
	var msgs []chatMessage
	if req.System != "" {
		msgs = append(msgs, chatMessage{Role: "system", Content: req.System})
	}
	msgs = append(msgs, chatMessage{Role: "user", Content: req.Prompt})
	body["messages"] = msgs
	if req.MaxTokens > 0 {
		body["max_completion_tokens"] = req.MaxTokens
	}
	if req.Schema != nil {
		body["response_format"] = map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "response",
				"schema": req.Schema,
				"strict": true,
			},
		}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := o.client.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", apperr.Temporary(fmt.Errorf("openai request: %w", err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return "", apperr.Temporary(err)
	}
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("openai API error %d: %s", resp.StatusCode, truncate(string(data), 300))
		switch {
		case resp.StatusCode == 401 || resp.StatusCode == 403:
			return "", apperr.User("The AI provider rejected the API key. Check AI_API_KEY.", err)
		case apperr.HTTPStatusTemporary(resp.StatusCode):
			return "", apperr.Temporary(err)
		}
		return "", err
	}
	var out struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content string `json:"content"`
				Refusal string `json:"refusal"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("%w: no choices returned", ErrMalformed)
	}
	c := out.Choices[0]
	if c.Message.Refusal != "" {
		return "", apperr.User("The AI provider declined to process this content.", fmt.Errorf("refusal: %s", c.Message.Refusal))
	}
	if c.FinishReason == "length" {
		return "", fmt.Errorf("%w: response exceeded the output token limit", ErrMalformed)
	}
	if strings.TrimSpace(c.Message.Content) == "" {
		return "", fmt.Errorf("%w: empty response", ErrMalformed)
	}
	return c.Message.Content, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
