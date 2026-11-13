// Package transcript parses caption files and cleans, normalizes and chunks
// transcripts for display and AI processing.
package transcript

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/quantamsage/ai-video-summarizer/internal/model"
)

// ErrEmpty is returned when a transcript contains no usable text.
var ErrEmpty = errors.New("transcript contains no usable speech text")

var timingLine = regexp.MustCompile(`^\s*((?:\d+:)?\d{1,2}:\d{2}[.,]\d{1,3})\s*-->\s*((?:\d+:)?\d{1,2}:\d{2}[.,]\d{1,3})`)

// ParseCaptions parses WebVTT or SubRip (SRT) caption data.
func ParseCaptions(data []byte) ([]model.Segment, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)

	var segs []model.Segment
	var cur *model.Segment
	var text []string
	inNote := false
	flush := func() {
		if cur != nil {
			cur.Text = strings.Join(text, " ")
			segs = append(segs, *cur)
		}
		cur, text = nil, nil
	}
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			flush()
			inNote = false
			continue
		}
		if cur == nil && (strings.HasPrefix(trimmed, "NOTE") || strings.HasPrefix(trimmed, "STYLE") || strings.HasPrefix(trimmed, "REGION")) {
			inNote = true
		}
		if inNote || trimmed == "WEBVTT" || strings.HasPrefix(trimmed, "WEBVTT ") {
			continue
		}
		if m := timingLine.FindStringSubmatch(line); m != nil {
			flush()
			start, err1 := parseTimestamp(m[1])
			end, err2 := parseTimestamp(m[2])
			if err1 != nil || err2 != nil {
				return nil, fmt.Errorf("invalid caption timing %q", trimmed)
			}
			cur = &model.Segment{Start: start, End: end}
			continue
		}
		if cur != nil {
			text = append(text, trimmed)
		}
		// Lines outside a cue (cue identifiers, SRT indices, headers) are ignored.
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	flush()
	if len(segs) == 0 {
		return nil, errors.New("no caption cues found; expected WebVTT or SRT")
	}
	return segs, nil
}

func parseTimestamp(s string) (float64, error) {
	s = strings.Replace(s, ",", ".", 1)
	parts := strings.Split(s, ":")
	var total float64
	for _, p := range parts {
		v, err := strconv.ParseFloat(p, 64)
		if err != nil {
			return 0, err
		}
		total = total*60 + v
	}
	return total, nil
}

var (
	tagRe        = regexp.MustCompile(`<[^>]*>`)
	annotationRe = regexp.MustCompile(`^[\[(♪][^\])]*[\])♪]?$`)
	spaceRe      = regexp.MustCompile(`\s+`)
)

// CleanText removes markup and normalizes whitespace in a caption line.
func CleanText(s string) string {
	s = tagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimLeft(s, ">- ")
	return strings.TrimSpace(spaceRe.ReplaceAllString(s, " "))
}

// Normalize cleans segment text, drops non-speech annotations such as
// "[Music]", removes the duplicated text that rolling auto-captions repeat
// between consecutive cues, and renumbers positions. It returns ErrEmpty if
// nothing remains.
func Normalize(segs []model.Segment) ([]model.Segment, error) {
	out := make([]model.Segment, 0, len(segs))
	prevRaw := "" // previous cue's cleaned text before de-duplication
	for _, sg := range segs {
		t := CleanText(sg.Text)
		if t == "" || annotationRe.MatchString(t) {
			continue
		}
		raw := t
		if sg.End < sg.Start {
			sg.End = sg.Start
		}
		if n := len(out); n > 0 {
			prev := prevRaw
			prevRaw = raw
			if t == prev {
				out[n-1].End = max(out[n-1].End, sg.End)
				continue
			}
			// Rolling captions: the new cue begins with the previous one.
			if strings.HasPrefix(t, prev+" ") {
				t = strings.TrimSpace(strings.TrimPrefix(t, prev))
			} else if k := overlapLen(prev, t); k > 0 {
				t = strings.TrimSpace(t[k:])
			}
			if t == "" {
				out[n-1].End = max(out[n-1].End, sg.End)
				continue
			}
		}
		prevRaw = raw
		sg.Text = t
		out = append(out, sg)
	}
	if len(out) == 0 {
		return nil, ErrEmpty
	}
	for i := range out {
		out[i].Position = i
	}
	return out, nil
}

// overlapLen returns the byte length of the longest suffix of a (on a word
// boundary, at least three words long) that is a prefix of b.
func overlapLen(a, b string) int {
	aw := strings.Fields(a)
	bw := strings.Fields(b)
	for n := min(len(aw), len(bw)); n >= 3; n-- {
		match := true
		for i := 0; i < n; i++ {
			if aw[len(aw)-n+i] != bw[i] {
				match = false
				break
			}
		}
		if match {
			prefix := strings.Join(bw[:n], " ")
			if idx := strings.Index(b, prefix); idx == 0 {
				return len(prefix)
			}
		}
	}
	return 0
}

// FullText joins segment text into readable paragraphs, starting a new
// paragraph after long pauses or sentence ends past ~120 words.
func FullText(segs []model.Segment) string {
	var b strings.Builder
	words := 0
	for i, sg := range segs {
		if i > 0 {
			gap := sg.Start - segs[i-1].End
			prev := segs[i-1].Text
			endsSentence := strings.HasSuffix(prev, ".") || strings.HasSuffix(prev, "?") || strings.HasSuffix(prev, "!")
			if gap > 4 || (words > 120 && endsSentence) {
				b.WriteString("\n\n")
				words = 0
			} else {
				b.WriteByte(' ')
			}
		}
		b.WriteString(sg.Text)
		words += len(strings.Fields(sg.Text))
	}
	return b.String()
}

// WordCount counts whitespace-separated words.
func WordCount(s string) int { return len(strings.Fields(s)) }

// FromPlainText builds untimed segments from plain text, one per sentence
// group, for sources without timing.
func FromPlainText(s string) ([]model.Segment, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, ErrEmpty
	}
	var segs []model.Segment
	for _, para := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		if p := CleanText(para); p != "" {
			segs = append(segs, model.Segment{Text: p})
		}
	}
	return Normalize(segs)
}

// HasTiming reports whether segments carry meaningful timestamps.
func HasTiming(segs []model.Segment) bool {
	for _, sg := range segs {
		if sg.End > 0 {
			return true
		}
	}
	return false
}
