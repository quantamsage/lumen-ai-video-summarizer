// Package stt provides speech-to-text providers behind a common interface.
package stt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/quantamsage/ai-video-summarizer/internal/apperr"
	"github.com/quantamsage/ai-video-summarizer/internal/model"
)

// ErrNotConfigured is returned when no speech-to-text provider is set up.
var ErrNotConfigured = errors.New("speech-to-text is not configured")

// Result is the transcription of one audio file. Segment times are relative
// to the start of that file.
type Result struct {
	Language string
	Text     string
	Segments []model.Segment
}

// Provider transcribes audio files.
type Provider interface {
	Name() string
	Transcribe(ctx context.Context, audioPath string) (*Result, error)
}

// Disabled is used when STT_PROVIDER=none.
type Disabled struct{}

func (Disabled) Name() string { return "none" }
func (Disabled) Transcribe(context.Context, string) (*Result, error) {
	return nil, ErrNotConfigured
}

// OpenAI calls an OpenAI-compatible /audio/transcriptions endpoint
// (OpenAI Whisper, or compatible services such as Groq).
type OpenAI struct {
	BaseURL string
	APIKey  string
	Model   string
	Client  *http.Client
}

func NewOpenAI(baseURL, apiKey, model string) *OpenAI {
	return &OpenAI{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		Client:  &http.Client{Timeout: 10 * time.Minute},
	}
}

func (o *OpenAI) Name() string { return "openai:" + o.Model }

type verboseResponse struct {
	Language string `json:"language"`
	Text     string `json:"text"`
	Segments []struct {
		Start float64 `json:"start"`
		End   float64 `json:"end"`
		Text  string  `json:"text"`
	} `json:"segments"`
}

func (o *OpenAI) Transcribe(ctx context.Context, audioPath string) (*Result, error) {
	f, err := os.Open(audioPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", filepath.Base(audioPath))
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(fw, f); err != nil {
		return nil, err
	}
	mw.WriteField("model", o.Model)
	// Only Whisper models support segment timestamps (verbose_json).
	timed := strings.Contains(strings.ToLower(o.Model), "whisper")
	if timed {
		mw.WriteField("response_format", "verbose_json")
		mw.WriteField("timestamp_granularities[]", "segment")
	} else {
		mw.WriteField("response_format", "json")
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/audio/transcriptions", &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+o.APIKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := o.Client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, apperr.Temporary(fmt.Errorf("speech-to-text request: %w", err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, apperr.Temporary(err)
	}
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("speech-to-text API returned %d: %s", resp.StatusCode, truncate(string(data), 300))
		if apperr.HTTPStatusTemporary(resp.StatusCode) {
			return nil, apperr.Temporary(err)
		}
		return nil, apperr.User("The speech-to-text service rejected the request. Check the STT configuration.", err)
	}
	return parseResponse(data)
}

func parseResponse(data []byte) (*Result, error) {
	var v verboseResponse
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("parse speech-to-text response: %w", err)
	}
	res := &Result{Language: v.Language, Text: strings.TrimSpace(v.Text)}
	for _, s := range v.Segments {
		if t := strings.TrimSpace(s.Text); t != "" {
			res.Segments = append(res.Segments, model.Segment{Start: s.Start, End: s.End, Text: t})
		}
	}
	if len(res.Segments) == 0 && res.Text != "" {
		res.Segments = []model.Segment{{Text: res.Text}}
	}
	return res, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
