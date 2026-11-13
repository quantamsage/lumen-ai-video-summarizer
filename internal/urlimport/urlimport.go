// Package urlimport classifies user-submitted video URLs and fetches remote
// content safely.
//
// Only legitimate access methods are used: public oEmbed metadata endpoints
// for YouTube and Vimeo, and plain HTTP downloads of direct links to media
// files. The package never bypasses authentication, DRM or platform
// restrictions; platforms that do not offer transcripts to third parties
// must be paired with a caption file provided by the user.
//
// All outbound requests go through a client whose dialer refuses
// loopback, private, link-local and other internal addresses at connect
// time, protecting against SSRF (including DNS rebinding).
package urlimport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/quantamsage/ai-video-summarizer/internal/apperr"
)

// Kind identifies how a URL can be processed.
type Kind string

const (
	KindDirect  Kind = "direct"  // direct link to a media file
	KindYouTube Kind = "youtube" // metadata via oEmbed; captions from user
	KindVimeo   Kind = "vimeo"   // metadata via oEmbed; captions from user
)

// Source is a classified URL.
type Source struct {
	Kind    Kind
	URL     *url.URL
	VideoID string
}

// NeedsCaptions reports whether this source requires a user-supplied
// caption file because the platform does not let third-party apps download
// its media or transcripts.
func (s *Source) NeedsCaptions() bool { return s.Kind == KindYouTube || s.Kind == KindVimeo }

var (
	ErrUnsupported = errors.New("unsupported URL")
	ErrBlockedHost = errors.New("URL points to a private or internal address")
)

var (
	youtubeID = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
	vimeoID   = regexp.MustCompile(`^[0-9]{1,12}$`)
	mediaExts = map[string]bool{".mp4": true, ".m4v": true, ".mov": true, ".webm": true, ".mkv": true,
		".ogv": true, ".avi": true, ".mpeg": true, ".mpg": true}
)

// Classify validates a URL's syntax and determines how it can be processed.
// It does not perform network access.
func Classify(raw string) (*Source, error) {
	raw = strings.TrimSpace(raw)
	if len(raw) > 2048 {
		return nil, apperr.User("The URL is too long.", ErrUnsupported)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, apperr.User("Enter a valid http(s) URL.", ErrUnsupported)
	}
	if u.User != nil {
		return nil, apperr.User("URLs containing credentials are not allowed.", ErrUnsupported)
	}
	host := strings.ToLower(u.Hostname())
	if addr, err := netip.ParseAddr(host); err == nil && !publicAddr(addr) {
		return nil, apperr.User("That URL points to a private or internal address.", ErrBlockedHost)
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return nil, apperr.User("That URL points to a private or internal address.", ErrBlockedHost)
	}
	u.Fragment = ""

	switch host {
	case "youtube.com", "www.youtube.com", "m.youtube.com":
		id := u.Query().Get("v")
		if id == "" {
			if p := strings.TrimPrefix(u.Path, "/shorts/"); p != u.Path {
				id = strings.Trim(p, "/")
			} else if p := strings.TrimPrefix(u.Path, "/embed/"); p != u.Path {
				id = strings.Trim(p, "/")
			}
		}
		if youtubeID.MatchString(id) {
			return &Source{Kind: KindYouTube, URL: canonicalYouTube(id), VideoID: id}, nil
		}
	case "youtu.be":
		id := strings.Trim(u.Path, "/")
		if youtubeID.MatchString(id) {
			return &Source{Kind: KindYouTube, URL: canonicalYouTube(id), VideoID: id}, nil
		}
	case "vimeo.com", "www.vimeo.com", "player.vimeo.com":
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		id := parts[len(parts)-1]
		if vimeoID.MatchString(id) {
			cu, _ := url.Parse("https://vimeo.com/" + id)
			return &Source{Kind: KindVimeo, URL: cu, VideoID: id}, nil
		}
	default:
		if mediaExts[strings.ToLower(path.Ext(u.Path))] {
			return &Source{Kind: KindDirect, URL: u}, nil
		}
	}
	return nil, apperr.User("This URL isn't supported. Use a direct link to a video file (.mp4, .webm, .mov…), "+
		"or a YouTube/Vimeo link together with its caption file. You can also upload the video directly.", ErrUnsupported)
}

func canonicalYouTube(id string) *url.URL {
	u, _ := url.Parse("https://www.youtube.com/watch?v=" + id)
	return u
}

// EmbedURL returns an embeddable player URL starting at the given second
// for YouTube/Vimeo sources, or "" for other URLs.
func EmbedURL(sourceURL string, start int) string {
	src, err := Classify(sourceURL)
	if err != nil {
		return ""
	}
	switch src.Kind {
	case KindYouTube:
		u := "https://www.youtube-nocookie.com/embed/" + src.VideoID
		if start > 0 {
			u += fmt.Sprintf("?start=%d&autoplay=1", start)
		}
		return u
	case KindVimeo:
		u := "https://player.vimeo.com/video/" + src.VideoID
		if start > 0 {
			u += fmt.Sprintf("?autoplay=1#t=%ds", start)
		}
		return u
	}
	return ""
}

// publicAddr reports whether addr is a globally routable unicast address.
func publicAddr(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() || addr.IsInterfaceLocalMulticast() || addr.IsMulticast() ||
		addr.IsUnspecified() {
		return false
	}
	for _, p := range blockedPrefixes {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}

var blockedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "255.255.255.255/32",
		"64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001:db8::/32", "fc00::/7", "fec0::/10",
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// Client performs SSRF-safe outbound HTTP requests.
type Client struct {
	HTTP *http.Client
}

// NewClient returns a client that refuses to connect to non-public
// addresses and limits redirects.
func NewClient(timeout time.Duration) *Client {
	return newClient(timeout, publicAddr)
}

func newClient(timeout time.Duration, allow func(netip.Addr) bool) *Client {
	dialer := &net.Dialer{
		Timeout: 15 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			addr, err := netip.ParseAddr(host)
			if err != nil || !allow(addr) {
				return ErrBlockedHost
			}
			return nil
		},
	}
	transport := &http.Transport{
		Proxy:                 nil, // never route through environment proxies
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConns:          10,
		IdleConnTimeout:       60 * time.Second,
	}
	return &Client{HTTP: &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return errors.New("redirect to unsupported scheme")
			}
			return nil
		},
	}}
}

// Metadata is information about a remote video.
type Metadata struct {
	Title        string
	Author       string
	ThumbnailURL string
}

// OEmbed fetches public metadata for YouTube and Vimeo sources.
func (c *Client) OEmbed(ctx context.Context, src *Source) (*Metadata, error) {
	var endpoint string
	switch src.Kind {
	case KindYouTube:
		endpoint = "https://www.youtube.com/oembed?format=json&url=" + url.QueryEscape(src.URL.String())
	case KindVimeo:
		endpoint = "https://vimeo.com/api/oembed.json?url=" + url.QueryEscape(src.URL.String())
	default:
		return nil, ErrUnsupported
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, apperr.Temporary(fmt.Errorf("fetch video metadata: %w", err))
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, apperr.User("The video could not be found or is private. Only public videos can be imported.",
			fmt.Errorf("oembed status %d", resp.StatusCode))
	case resp.StatusCode != http.StatusOK:
		err := fmt.Errorf("oembed status %d", resp.StatusCode)
		if apperr.HTTPStatusTemporary(resp.StatusCode) {
			return nil, apperr.Temporary(err)
		}
		return nil, err
	}
	var out struct {
		Title        string `json:"title"`
		AuthorName   string `json:"author_name"`
		ThumbnailURL string `json:"thumbnail_url"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("parse oembed: %w", err)
	}
	m := &Metadata{Title: strings.TrimSpace(out.Title), Author: strings.TrimSpace(out.AuthorName)}
	if tu, err := url.Parse(out.ThumbnailURL); err == nil && tu.Scheme == "https" {
		m.ThumbnailURL = tu.String()
	}
	return m, nil
}

// ErrTooLarge is returned when a download exceeds the size limit.
var ErrTooLarge = errors.New("file exceeds the maximum allowed size")

// Download streams a direct media URL into w, enforcing maxBytes. It
// verifies the response is a video (or generic binary) content type.
func (c *Client) Download(ctx context.Context, rawURL string, w io.Writer, maxBytes int64, progress func(n int64, total int64)) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", 0, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if errors.Is(err, ErrBlockedHost) {
			return "", 0, apperr.User("That URL points to a private or internal address.", err)
		}
		return "", 0, apperr.Temporary(fmt.Errorf("download video: %w", err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("download status %d", resp.StatusCode)
		if apperr.HTTPStatusTemporary(resp.StatusCode) {
			return "", 0, apperr.Temporary(err)
		}
		return "", 0, apperr.User(fmt.Sprintf("The server returned HTTP %d for this URL. It may require a login or not allow downloads; try uploading the file instead.", resp.StatusCode), err)
	}
	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if !strings.HasPrefix(ct, "video/") && ct != "application/octet-stream" && ct != "binary/octet-stream" && ct != "" {
		return "", 0, apperr.User("The URL does not point to a video file (got "+ct+").", ErrUnsupported)
	}
	if resp.ContentLength > maxBytes {
		return "", 0, apperr.User("The remote video is larger than the maximum upload size.", ErrTooLarge)
	}
	pw := &progressWriter{w: w, total: resp.ContentLength, fn: progress}
	n, err := io.Copy(pw, io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return "", n, ctx.Err()
		}
		return "", n, apperr.Temporary(fmt.Errorf("download interrupted: %w", err))
	}
	if n > maxBytes {
		return "", n, apperr.User("The remote video is larger than the maximum upload size.", ErrTooLarge)
	}
	return ct, n, nil
}

type progressWriter struct {
	w     io.Writer
	n     int64
	total int64
	last  time.Time
	fn    func(int64, int64)
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.n += int64(n)
	if p.fn != nil && time.Since(p.last) > time.Second {
		p.last = time.Now()
		p.fn(p.n, p.total)
	}
	return n, err
}

// CheckHost resolves the URL's host and verifies every address is public.
// Used to reject internal URLs at submission time; the dialer re-checks at
// connect time.
func CheckHost(ctx context.Context, u *url.URL) error {
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", u.Hostname())
	if err != nil {
		return apperr.User("The URL's host name could not be resolved.", err)
	}
	for _, a := range addrs {
		if !publicAddr(a) {
			return apperr.User("That URL points to a private or internal address.", ErrBlockedHost)
		}
	}
	return nil
}
