package transcript

import (
	"fmt"
	"strings"

	"github.com/quantamsage/ai-video-summarizer/internal/model"
)

// Chunk is a contiguous slice of the transcript prepared for the AI.
type Chunk struct {
	Index int
	Start float64
	End   float64
	// Text is annotated with [mm:ss] markers when timing is available so the
	// model can attribute topics to timestamps.
	Text  string
	Words int
}

// Chunks splits segments into chunks of at most maxWords words (a single
// oversized segment becomes its own chunk). Consecutive chunks share up to
// overlapWords words of trailing context so ideas spanning a boundary are not
// lost.
func Chunks(segs []model.Segment, maxWords, overlapWords int) []Chunk {
	if maxWords <= 0 {
		maxWords = 3000
	}
	timed := HasTiming(segs)
	var chunks []Chunk
	i := 0
	for i < len(segs) {
		start := i
		words := 0
		for i < len(segs) {
			w := WordCount(segs[i].Text)
			if words > 0 && words+w > maxWords {
				break
			}
			words += w
			i++
		}
		chunks = append(chunks, Chunk{
			Index: len(chunks),
			Start: segs[start].Start,
			End:   segs[i-1].End,
			Text:  Annotate(segs[start:i], timed),
			Words: words,
		})
		if i >= len(segs) {
			break
		}
		// Step back to include overlap context in the next chunk.
		back, ow := i, 0
		for back > start+1 && ow < overlapWords {
			back--
			ow += WordCount(segs[back].Text)
		}
		if back > start && ow <= overlapWords && back < i {
			i = back
		}
	}
	return chunks
}

// Annotate renders segments as text, inserting a [mm:ss] marker roughly
// every 30 seconds when timed is true.
func Annotate(segs []model.Segment, timed bool) string {
	var b strings.Builder
	lastMark := -1e9
	for i, sg := range segs {
		if timed && sg.Start-lastMark >= 30 {
			if i > 0 {
				b.WriteString("\n")
			}
			fmt.Fprintf(&b, "[%s] ", Clock(sg.Start))
			lastMark = sg.Start
		} else if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(sg.Text)
	}
	return b.String()
}

// Clock formats seconds as m:ss or h:mm:ss.
func Clock(sec float64) string {
	s := int(sec)
	h, m, ss := s/3600, (s%3600)/60, s%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, ss)
	}
	return fmt.Sprintf("%d:%02d", m, ss)
}

// Window returns the segments overlapping [start, end). A nil end means
// "until the end of the transcript".
func Window(segs []model.Segment, start float64, end *float64) []model.Segment {
	var out []model.Segment
	for _, sg := range segs {
		if sg.End <= start && sg.Start < start {
			continue
		}
		if end != nil && sg.Start >= *end {
			continue
		}
		out = append(out, sg)
	}
	return out
}
