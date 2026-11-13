package urlimport

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/quantamsage/ai-video-summarizer/internal/apperr"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		in   string
		kind Kind
		id   string
		ok   bool
	}{
		{"https://www.youtube.com/watch?v=dQw4w9WgXcQ&t=10", KindYouTube, "dQw4w9WgXcQ", true},
		{"https://youtu.be/dQw4w9WgXcQ", KindYouTube, "dQw4w9WgXcQ", true},
		{"https://www.youtube.com/shorts/dQw4w9WgXcQ", KindYouTube, "dQw4w9WgXcQ", true},
		{"https://vimeo.com/123456", KindVimeo, "123456", true},
		{"https://cdn.example.com/talks/intro.MP4?sig=1", KindDirect, "", true},
		{"https://example.com/page.html", "", "", false},
		{"ftp://example.com/a.mp4", "", "", false},
		{"https://user:pass@example.com/a.mp4", "", "", false},
		{"http://127.0.0.1/a.mp4", "", "", false},
		{"http://169.254.169.254/latest/meta-data/a.mp4", "", "", false},
		{"http://[::1]/a.mp4", "", "", false},
		{"http://10.1.2.3/a.mp4", "", "", false},
		{"http://localhost/a.mp4", "", "", false},
		{"http://metadata.internal/a.mp4", "", "", false},
		{"not a url", "", "", false},
	}
	for _, c := range cases {
		src, err := Classify(c.in)
		if c.ok != (err == nil) {
			t.Errorf("Classify(%q): ok=%v err=%v", c.in, c.ok, err)
			continue
		}
		if !c.ok {
			if apperr.UserMessage(err, "") == "" {
				t.Errorf("Classify(%q) should give a user-facing reason", c.in)
			}
			continue
		}
		if src.Kind != c.kind || src.VideoID != c.id {
			t.Errorf("Classify(%q) = %+v", c.in, src)
		}
	}
}

func TestEmbedURL(t *testing.T) {
	if got := EmbedURL("https://youtu.be/dQw4w9WgXcQ", 75); !strings.Contains(got, "youtube-nocookie.com/embed/dQw4w9WgXcQ?start=75") {
		t.Fatalf("youtube embed: %s", got)
	}
	if got := EmbedURL("https://example.com/a.mp4", 0); got != "" {
		t.Fatalf("direct links have no embed: %s", got)
	}
}

func TestPublicAddr(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.0.0.1", "172.16.5.4", "192.168.1.1", "169.254.169.254", "100.64.0.1",
		"::1", "fe80::1", "fd00::1", "0.0.0.0", "::ffff:127.0.0.1", "224.0.0.1"} {
		if publicAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if !publicAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
}

func TestDialerBlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("secret"))
	}))
	defer srv.Close()
	c := NewClient(5 * time.Second)
	var buf bytes.Buffer
	_, _, err := c.Download(context.Background(), srv.URL+"/a.mp4", &buf, 1<<20, nil)
	if !errors.Is(err, ErrBlockedHost) {
		t.Fatalf("expected loopback to be blocked at dial time, got %v", err)
	}
	if buf.Len() != 0 {
		t.Fatal("no data should be read from a blocked host")
	}
}

func TestDownloadLimitsAndContentType(t *testing.T) {
	body := strings.Repeat("x", 2048)
	ct := "video/mp4"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", ct)
		w.Write([]byte(body))
	}))
	defer srv.Close()
	// Allow loopback only for this test server.
	c := newClient(5*time.Second, func(netip.Addr) bool { return true })

	var buf bytes.Buffer
	gotCT, n, err := c.Download(context.Background(), srv.URL+"/a.mp4", &buf, 4096, nil)
	if err != nil || n != 2048 || gotCT != "video/mp4" {
		t.Fatalf("download: %s %d %v", gotCT, n, err)
	}
	buf.Reset()
	if _, _, err := c.Download(context.Background(), srv.URL+"/a.mp4", &buf, 1024, nil); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("expected ErrTooLarge, got %v", err)
	}
	ct = "text/html"
	if _, _, err := c.Download(context.Background(), srv.URL+"/a.mp4", &buf, 4096, nil); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("expected non-video content to be rejected, got %v", err)
	}
}
