package ai

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"github.com/quantamsage/ai-video-summarizer/internal/apperr"
)

// Anthropic calls Claude through the official Anthropic Go SDK.
type Anthropic struct {
	client anthropic.Client
	model  string
}

// NewAnthropic creates a Claude provider. Extra options (such as a base URL
// for tests) are passed to the SDK client.
func NewAnthropic(apiKey, model string, opts ...option.RequestOption) *Anthropic {
	opts = append([]option.RequestOption{option.WithAPIKey(apiKey), option.WithMaxRetries(2)}, opts...)
	return &Anthropic{client: anthropic.NewClient(opts...), model: model}
}

func (a *Anthropic) Name() string  { return "anthropic" }
func (a *Anthropic) Model() string { return a.model }

func (a *Anthropic) Complete(ctx context.Context, req Request) (string, error) {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 16000
	}
	params := anthropic.BetaMessageNewParams{
		Model:     anthropic.Model(a.model),
		MaxTokens: int64(maxTokens),
		Messages: []anthropic.BetaMessageParam{
			anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(req.Prompt)),
		},
		// Server-side fallback: if a request is declined by a safety
		// classifier, the API re-serves it with a suitable fallback model.
		Fallbacks: anthropic.BetaFallbacksParamOfDefault(),
		Betas:     []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01},
	}
	if req.System != "" {
		params.System = []anthropic.BetaTextBlockParam{{Text: req.System}}
	}
	if req.Effort != "" {
		params.OutputConfig.Effort = anthropic.BetaOutputConfigEffort(req.Effort)
	}
	if req.Schema != nil {
		params.OutputConfig.Format = anthropic.BetaJSONOutputFormatParam{Schema: req.Schema}
	}

	// Stream to avoid HTTP timeouts on long generations, accumulating the
	// final message.
	stream := a.client.Beta.Messages.NewStreaming(ctx, params)
	msg := anthropic.BetaMessage{}
	for stream.Next() {
		if err := msg.Accumulate(stream.Current()); err != nil {
			return "", fmt.Errorf("accumulate stream: %w", err)
		}
	}
	if err := stream.Err(); err != nil {
		return "", classifyAnthropicError(ctx, err)
	}

	switch msg.StopReason {
	case anthropic.BetaStopReasonRefusal:
		return "", apperr.User("The AI provider declined to process this content.",
			fmt.Errorf("refusal: %s", msg.StopDetails.Explanation))
	case anthropic.BetaStopReasonMaxTokens:
		return "", fmt.Errorf("%w: response exceeded the output token limit", ErrMalformed)
	}
	var b strings.Builder
	for _, block := range msg.Content {
		if t, ok := block.AsAny().(anthropic.BetaTextBlock); ok {
			b.WriteString(t.Text)
		}
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("%w: empty response", ErrMalformed)
	}
	return b.String(), nil
}

func classifyAnthropicError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var apierr *anthropic.Error
	if errors.As(err, &apierr) {
		wrapped := fmt.Errorf("anthropic API error %d: %w", apierr.StatusCode, err)
		switch {
		case apierr.StatusCode == 401 || apierr.StatusCode == 403:
			return apperr.User("The AI provider rejected the API key. Check AI_API_KEY.", wrapped)
		case apperr.HTTPStatusTemporary(apierr.StatusCode):
			return apperr.Temporary(wrapped)
		default:
			return wrapped
		}
	}
	// Transport-level failures are worth retrying.
	return apperr.Temporary(fmt.Errorf("anthropic request: %w", err))
}
