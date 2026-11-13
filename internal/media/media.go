// Package media wraps the external ffprobe and ffmpeg tools used to inspect
// videos, extract embedded subtitles and audio, and render thumbnails.
package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ErrToolMissing is returned when ffmpeg or ffprobe cannot be executed.
var ErrToolMissing = errors.New("ffmpeg/ffprobe is not installed or not executable; set FFMPEG_PATH and FFPROBE_PATH")

// Tools locates the ffmpeg binaries.
type Tools struct {
	FFmpeg  string
	FFprobe string
}

// Metadata describes a media file.
type Metadata struct {
	Duration        float64
	Width           int
	Height          int
	Format          string
	HasVideo        bool
	HasAudio        bool
	Title           string
	Comment         string
	SubtitleStreams []SubtitleStream
}

type SubtitleStream struct {
	Index    int
	Codec    string
	Language string
}

// Check verifies that both tools can be executed.
func (t Tools) Check(ctx context.Context) error {
	for _, bin := range []string{t.FFmpeg, t.FFprobe} {
		if err := exec.CommandContext(ctx, bin, "-version").Run(); err != nil {
			return fmt.Errorf("%w (%s: %v)", ErrToolMissing, bin, err)
		}
	}
	return nil
}

func (t Tools) run(ctx context.Context, bin string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		var execErr *exec.Error
		if errors.As(err, &execErr) {
			return nil, ErrToolMissing
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%s failed: %v: %s", filepath.Base(bin), err, lastLines(stderr.String(), 3))
	}
	return stdout.Bytes(), nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

type probeOutput struct {
	Format struct {
		FormatName string            `json:"format_name"`
		Duration   string            `json:"duration"`
		Tags       map[string]string `json:"tags"`
	} `json:"format"`
	Streams []struct {
		Index     int               `json:"index"`
		CodecType string            `json:"codec_type"`
		CodecName string            `json:"codec_name"`
		Width     int               `json:"width"`
		Height    int               `json:"height"`
		Duration  string            `json:"duration"`
		Tags      map[string]string `json:"tags"`
		// Disposition flags attached pictures (cover art), which are not video.
		Disposition map[string]int `json:"disposition"`
	} `json:"streams"`
}

// Probe extracts metadata with ffprobe.
func (t Tools) Probe(ctx context.Context, path string) (*Metadata, error) {
	out, err := t.run(ctx, t.FFprobe, "-v", "error", "-print_format", "json",
		"-show_format", "-show_streams", "--", path)
	if err != nil {
		return nil, err
	}
	return parseProbe(out)
}

func parseProbe(out []byte) (*Metadata, error) {
	var p probeOutput
	if err := json.Unmarshal(out, &p); err != nil {
		return nil, fmt.Errorf("parse ffprobe output: %w", err)
	}
	m := &Metadata{Format: p.Format.FormatName}
	m.Duration, _ = strconv.ParseFloat(p.Format.Duration, 64)
	m.Title = tag(p.Format.Tags, "title")
	m.Comment = tag(p.Format.Tags, "comment")
	if m.Comment == "" {
		m.Comment = tag(p.Format.Tags, "description")
	}
	for _, s := range p.Streams {
		switch s.CodecType {
		case "video":
			if s.Disposition["attached_pic"] == 1 {
				continue
			}
			m.HasVideo = true
			if m.Width == 0 {
				m.Width, m.Height = s.Width, s.Height
			}
		case "audio":
			m.HasAudio = true
		case "subtitle":
			m.SubtitleStreams = append(m.SubtitleStreams, SubtitleStream{
				Index: s.Index, Codec: s.CodecName, Language: tag(s.Tags, "language"),
			})
		}
		if m.Duration == 0 {
			if d, err := strconv.ParseFloat(s.Duration, 64); err == nil && d > m.Duration {
				m.Duration = d
			}
		}
	}
	if !m.HasVideo && !m.HasAudio {
		return nil, errors.New("file contains no audio or video streams")
	}
	return m, nil
}

func tag(tags map[string]string, key string) string {
	for k, v := range tags {
		if strings.EqualFold(k, key) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// textSubtitleCodecs are subtitle formats ffmpeg can convert to WebVTT.
// Image-based formats (PGS, VobSub) cannot be converted to text.
var textSubtitleCodecs = map[string]bool{
	"subrip": true, "srt": true, "webvtt": true, "ass": true, "ssa": true, "mov_text": true, "text": true,
}

// PreferredSubtitle picks a text subtitle stream, preferring English.
func (m *Metadata) PreferredSubtitle() (SubtitleStream, bool) {
	var candidates []SubtitleStream
	for _, s := range m.SubtitleStreams {
		if textSubtitleCodecs[s.Codec] {
			candidates = append(candidates, s)
		}
	}
	if len(candidates) == 0 {
		return SubtitleStream{}, false
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return isEnglish(candidates[i].Language) && !isEnglish(candidates[j].Language)
	})
	return candidates[0], true
}

func isEnglish(lang string) bool {
	l := strings.ToLower(lang)
	return l == "eng" || l == "en" || strings.HasPrefix(l, "en-")
}

// ExtractSubtitles converts an embedded subtitle stream to WebVTT.
func (t Tools) ExtractSubtitles(ctx context.Context, path string, streamIndex int) ([]byte, error) {
	return t.run(ctx, t.FFmpeg, "-v", "error", "-nostdin", "-i", path,
		"-map", fmt.Sprintf("0:%d", streamIndex), "-f", "webvtt", "pipe:1")
}

// AudioChunk is one segment of extracted audio and its offset in the video.
type AudioChunk struct {
	Path   string
	Offset float64
}

// ExtractAudio writes mono 16 kHz MP3 audio split into segments of at most
// segmentSeconds into dir. Small mono speech-optimised segments keep each
// upload well under speech-to-text API size limits (~4 MB per 10 minutes).
func (t Tools) ExtractAudio(ctx context.Context, path, dir string, segmentSeconds int) ([]AudioChunk, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	pattern := filepath.Join(dir, "chunk_%04d.mp3")
	_, err := t.run(ctx, t.FFmpeg, "-v", "error", "-nostdin", "-y", "-i", path,
		"-vn", "-ac", "1", "-ar", "16000", "-c:a", "libmp3lame", "-b:a", "48k",
		"-f", "segment", "-segment_time", strconv.Itoa(segmentSeconds), "-reset_timestamps", "1", pattern)
	if err != nil {
		return nil, err
	}
	matches, err := filepath.Glob(filepath.Join(dir, "chunk_*.mp3"))
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	if len(matches) == 0 {
		return nil, errors.New("ffmpeg produced no audio; the video may have no audio track")
	}
	chunks := make([]AudioChunk, len(matches))
	for i, m := range matches {
		chunks[i] = AudioChunk{Path: m, Offset: float64(i * segmentSeconds)}
	}
	return chunks, nil
}

// Thumbnail renders a JPEG frame at the given time (seconds) to outPath.
func (t Tools) Thumbnail(ctx context.Context, path, outPath string, at float64) error {
	_, err := t.run(ctx, t.FFmpeg, "-v", "error", "-nostdin", "-y", "-ss", strconv.FormatFloat(at, 'f', 2, 64),
		"-i", path, "-frames:v", "1", "-vf", "scale=640:-2", "-q:v", "4", outPath)
	return err
}
