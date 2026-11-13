package transcript

import (
	"errors"
	"strings"
	"testing"

	"github.com/quantamsage/ai-video-summarizer/internal/model"
)

const sampleVTT = "\xef\xbb\xbfWEBVTT\nKind: captions\n\nNOTE this is a comment\nspanning lines\n\n" +
	"1\n00:00:01.000 --> 00:00:03.500 align:start\n<v Speaker>Hello &amp; welcome</v>\n\n" +
	"00:00:03.500 --> 00:00:05.000\n[Music]\n\n" +
	"00:00:05.000 --> 00:00:08.000\nToday we learn <c.yellow>Go</c>\nconcurrency\n\n" +
	"01:00:08.000 --> 01:00:09.250\nLate cue\n"

const sampleSRT = "1\r\n00:00:00,500 --> 00:00:02,000\r\nFirst line\r\n\r\n2\r\n00:00:02,000 --> 00:00:04,100\r\nSecond\r\nline\r\n"

func TestParseVTT(t *testing.T) {
	segs, err := ParseCaptions([]byte(sampleVTT))
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 4 {
		t.Fatalf("expected 4 cues, got %d: %+v", len(segs), segs)
	}
	if segs[0].Start != 1 || segs[0].End != 3.5 {
		t.Fatalf("timing: %+v", segs[0])
	}
	if segs[3].Start != 3608 {
		t.Fatalf("hour timestamps: %v", segs[3].Start)
	}
	norm, err := Normalize(segs)
	if err != nil {
		t.Fatal(err)
	}
	if len(norm) != 3 {
		t.Fatalf("music annotation should be dropped: %+v", norm)
	}
	if norm[0].Text != "Hello & welcome" || norm[1].Text != "Today we learn Go concurrency" {
		t.Fatalf("cleaning failed: %q / %q", norm[0].Text, norm[1].Text)
	}
	for i, sg := range norm {
		if sg.Position != i {
			t.Fatalf("positions not renumbered")
		}
	}
}

func TestParseSRT(t *testing.T) {
	segs, err := ParseCaptions([]byte(sampleSRT))
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 2 || segs[1].Text != "Second line" || segs[0].Start != 0.5 || segs[1].End != 4.1 {
		t.Fatalf("srt parse: %+v", segs)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	if _, err := ParseCaptions([]byte("just some text\nwithout cues")); err == nil {
		t.Fatal("expected error")
	}
}

func TestNormalizeRollingCaptions(t *testing.T) {
	segs := []model.Segment{
		{Start: 0, End: 2, Text: "so the first thing"},
		{Start: 2, End: 4, Text: "so the first thing we need is"},
		{Start: 4, End: 6, Text: "thing we need is a channel"},
		{Start: 6, End: 7, Text: "thing we need is a channel"},
	}
	norm, err := Normalize(segs)
	if err != nil {
		t.Fatal(err)
	}
	got := FullText(norm)
	if got != "so the first thing we need is a channel" {
		t.Fatalf("rolling caption dedupe: %q (%+v)", got, norm)
	}
	if norm[len(norm)-1].End != 7 {
		t.Fatalf("duplicate should extend end time: %+v", norm)
	}
}

func TestNormalizeEmpty(t *testing.T) {
	_, err := Normalize([]model.Segment{{Text: "[Music]"}, {Text: "  "}, {Text: "<i></i>"}})
	if !errors.Is(err, ErrEmpty) {
		t.Fatalf("expected ErrEmpty, got %v", err)
	}
	if _, err := FromPlainText("   \n "); !errors.Is(err, ErrEmpty) {
		t.Fatalf("expected ErrEmpty for blank plain text, got %v", err)
	}
}

func TestChunksRespectLimitAndOverlap(t *testing.T) {
	var segs []model.Segment
	for i := 0; i < 100; i++ {
		segs = append(segs, model.Segment{Position: i, Start: float64(i * 10), End: float64(i*10 + 10),
			Text: strings.Repeat("word ", 10)})
	}
	chunks := Chunks(segs, 200, 30)
	if len(chunks) < 5 {
		t.Fatalf("expected several chunks, got %d", len(chunks))
	}
	for _, c := range chunks {
		if c.Words > 200 {
			t.Fatalf("chunk %d exceeds limit: %d", c.Index, c.Words)
		}
		if !strings.HasPrefix(c.Text, "[") {
			t.Fatalf("timed chunk should start with timestamp marker: %q", c.Text[:20])
		}
	}
	if chunks[1].Start >= chunks[0].End {
		t.Fatalf("expected overlap between chunks: %v >= %v", chunks[1].Start, chunks[0].End)
	}
	if last := chunks[len(chunks)-1]; last.End != 1000 {
		t.Fatalf("last chunk should reach the end: %v", last.End)
	}
}

func TestChunksOversizedSegment(t *testing.T) {
	segs := []model.Segment{{Text: strings.Repeat("x ", 500)}, {Text: "tail"}}
	chunks := Chunks(segs, 100, 10)
	if len(chunks) != 2 {
		t.Fatalf("oversized segment should be its own chunk, got %d", len(chunks))
	}
	if strings.Contains(chunks[0].Text, "[") {
		t.Fatal("untimed transcript must not get timestamp markers")
	}
}

func TestWindow(t *testing.T) {
	segs := []model.Segment{{Start: 0, End: 10}, {Start: 10, End: 20}, {Start: 20, End: 30}}
	end := 20.0
	if w := Window(segs, 10, &end); len(w) != 1 || w[0].Start != 10 {
		t.Fatalf("window: %+v", w)
	}
	if w := Window(segs, 15, nil); len(w) != 2 {
		t.Fatalf("open window: %+v", w)
	}
}
