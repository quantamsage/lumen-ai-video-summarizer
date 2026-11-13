package media

import (
	"context"
	"errors"
	"testing"
)

const probeJSON = `{
  "streams": [
    {"index":0,"codec_type":"video","codec_name":"h264","width":1280,"height":720},
    {"index":1,"codec_type":"audio","codec_name":"aac"},
    {"index":2,"codec_type":"subtitle","codec_name":"hdmv_pgs_subtitle","tags":{"language":"eng"}},
    {"index":3,"codec_type":"subtitle","codec_name":"subrip","tags":{"language":"fre"}},
    {"index":4,"codec_type":"subtitle","codec_name":"mov_text","tags":{"language":"eng"}},
    {"index":5,"codec_type":"video","codec_name":"mjpeg","disposition":{"attached_pic":1}}
  ],
  "format": {"format_name":"mov,mp4","duration":"125.40","tags":{"title":"Intro to Go","Comment":"A talk"}}
}`

func TestParseProbe(t *testing.T) {
	m, err := parseProbe([]byte(probeJSON))
	if err != nil {
		t.Fatal(err)
	}
	if m.Duration != 125.4 || m.Width != 1280 || m.Height != 720 || !m.HasAudio || !m.HasVideo {
		t.Fatalf("metadata: %+v", m)
	}
	if m.Title != "Intro to Go" || m.Comment != "A talk" {
		t.Fatalf("tags: %+v", m)
	}
	s, ok := m.PreferredSubtitle()
	if !ok || s.Index != 4 {
		t.Fatalf("expected English text subtitle stream 4, got %+v %v", s, ok)
	}
}

func TestParseProbeNoStreams(t *testing.T) {
	if _, err := parseProbe([]byte(`{"streams":[],"format":{}}`)); err == nil {
		t.Fatal("expected error for file without streams")
	}
	if _, err := parseProbe([]byte(`not json`)); err == nil {
		t.Fatal("expected error for malformed output")
	}
}

func TestMissingTool(t *testing.T) {
	tools := Tools{FFmpeg: "definitely-not-ffmpeg-xyz", FFprobe: "definitely-not-ffprobe-xyz"}
	if err := tools.Check(context.Background()); !errors.Is(err, ErrToolMissing) {
		t.Fatalf("expected ErrToolMissing, got %v", err)
	}
	if _, err := tools.Probe(context.Background(), "x.mp4"); !errors.Is(err, ErrToolMissing) {
		t.Fatalf("expected ErrToolMissing, got %v", err)
	}
}
