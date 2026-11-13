package stt

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/quantamsage/ai-video-summarizer/internal/apperr"
)

func writeAudio(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "a.mp3")
	if err := os.WriteFile(p, []byte("fake audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOpenAITranscribe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/transcriptions" || r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "bad", 400)
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "no file", 400)
			return
		}
		b, _ := io.ReadAll(f)
		if string(b) != "fake audio" || r.FormValue("response_format") != "verbose_json" {
			http.Error(w, "unexpected form", 400)
			return
		}
		w.Write([]byte(`{"language":"english","text":"hi there","segments":[{"start":0,"end":1.5,"text":" hi "},{"start":1.5,"end":3,"text":"there"}]}`))
	}))
	defer srv.Close()

	p := NewOpenAI(srv.URL, "k", "whisper-1")
	res, err := p.Transcribe(context.Background(), writeAudio(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Segments) != 2 || res.Segments[0].Text != "hi" || res.Segments[1].End != 3 {
		t.Fatalf("segments: %+v", res.Segments)
	}
}

func TestOpenAIErrorsClassified(t *testing.T) {
	status := http.StatusTooManyRequests
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"x"}`, status)
	}))
	defer srv.Close()
	p := NewOpenAI(srv.URL, "k", "whisper-1")
	audio := writeAudio(t)

	_, err := p.Transcribe(context.Background(), audio)
	if !apperr.IsTemporary(err) {
		t.Fatalf("429 should be temporary: %v", err)
	}
	status = http.StatusUnauthorized
	_, err = p.Transcribe(context.Background(), audio)
	if err == nil || apperr.IsTemporary(err) {
		t.Fatalf("401 should be permanent: %v", err)
	}
}

func TestParseResponsePlainText(t *testing.T) {
	res, err := parseResponse([]byte(`{"text":"only text"}`))
	if err != nil || len(res.Segments) != 1 || res.Segments[0].Text != "only text" {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := parseResponse([]byte(`<html>`)); err == nil {
		t.Fatal("malformed response should error")
	}
}
