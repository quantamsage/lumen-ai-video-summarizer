package pipeline

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/quantamsage/ai-video-summarizer/internal/apperr"
	"github.com/quantamsage/ai-video-summarizer/internal/storage"
	"github.com/quantamsage/ai-video-summarizer/internal/transcript"
)

// VideoExtensions lists accepted upload extensions and their MIME types.
var VideoExtensions = map[string]string{
	".mp4":  "video/mp4",
	".m4v":  "video/mp4",
	".mov":  "video/quicktime",
	".webm": "video/webm",
	".mkv":  "video/x-matroska",
	".ogv":  "video/ogg",
	".avi":  "video/x-msvideo",
	".mpeg": "video/mpeg",
	".mpg":  "video/mpeg",
}

// CaptionExtensions lists accepted caption file extensions.
var CaptionExtensions = map[string]bool{".vtt": true, ".srt": true}

// MaxCaptionBytes bounds caption file uploads.
const MaxCaptionBytes = 5 << 20

func MimeForExt(ext string) string {
	if m, ok := VideoExtensions[strings.ToLower(ext)]; ok {
		return m
	}
	return "application/octet-stream"
}

// ErrUploadTooLarge is returned when an upload exceeds its size limit.
var ErrUploadTooLarge = errors.New("upload too large")

// SaveVideoUpload streams an uploaded video to storage, enforcing the size
// limit and verifying the file signature. It returns the storage key, size
// and MIME type. Partially written files are removed on any error.
func SaveVideoUpload(files *storage.Local, userID int64, filename string, r io.Reader, maxBytes int64) (string, int64, string, error) {
	ext := strings.ToLower(filepath.Ext(filename))
	if _, ok := VideoExtensions[ext]; !ok {
		return "", 0, "", apperr.User("Unsupported file type. Upload an MP4, MOV, WebM, MKV, AVI, OGV or MPEG video.", nil)
	}
	key, err := files.NewKey(userID, ext)
	if err != nil {
		return "", 0, "", err
	}
	n, err := writeLimited(files, key, r, maxBytes)
	if err != nil {
		return "", 0, "", err
	}
	if n == 0 {
		files.Remove(key)
		return "", 0, "", apperr.User("The uploaded file is empty.", nil)
	}
	if err := checkMagic(files, key); err != nil {
		files.Remove(key)
		return "", 0, "", err
	}
	return key, n, MimeForExt(ext), nil
}

// SaveCaptionUpload validates and stores a caption file.
func SaveCaptionUpload(files *storage.Local, userID int64, filename string, r io.Reader) (string, error) {
	ext := strings.ToLower(filepath.Ext(filename))
	if !CaptionExtensions[ext] {
		return "", apperr.User("Caption files must be WebVTT (.vtt) or SubRip (.srt).", nil)
	}
	data, err := io.ReadAll(io.LimitReader(r, MaxCaptionBytes+1))
	if err != nil {
		return "", apperr.User("The caption upload was interrupted.", err)
	}
	if len(data) > MaxCaptionBytes {
		return "", apperr.User("Caption files must be smaller than 5 MB.", ErrUploadTooLarge)
	}
	segs, err := transcript.ParseCaptions(data)
	if err != nil {
		return "", apperr.User("The caption file could not be parsed. Use a WebVTT (.vtt) or SubRip (.srt) file.", err)
	}
	if _, err := transcript.Normalize(segs); err != nil {
		return "", apperr.User("The caption file contains no text.", err)
	}
	key, err := files.NewKey(userID, ext)
	if err != nil {
		return "", err
	}
	if _, err := writeLimited(files, key, bytes.NewReader(data), MaxCaptionBytes); err != nil {
		return "", err
	}
	return key, nil
}

func writeLimited(files *storage.Local, key string, r io.Reader, maxBytes int64) (int64, error) {
	f, err := files.Create(key)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, io.LimitReader(r, maxBytes+1))
	cerr := f.Close()
	if err == nil {
		err = cerr
	}
	if err != nil {
		files.Remove(key)
		return 0, apperr.User("The upload was interrupted before it finished. Please try again.", err)
	}
	if n > maxBytes {
		files.Remove(key)
		return 0, apperr.User(fmt.Sprintf("The file is larger than the %d MB limit.", maxBytes>>20), ErrUploadTooLarge)
	}
	return n, nil
}

// checkMagic verifies that a stored file starts with a known video
// container signature, rejecting renamed non-video files.
func checkMagic(files *storage.Local, key string) error {
	path, err := files.Path(key)
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	head := make([]byte, 16)
	n, _ := io.ReadFull(f, head)
	if !IsVideoSignature(head[:n]) {
		return apperr.User("The file does not appear to be a valid video.", nil)
	}
	return nil
}

// IsVideoSignature reports whether b begins with a supported container
// signature (ISO BMFF/QuickTime, Matroska/WebM, Ogg, AVI, MPEG-PS).
func IsVideoSignature(b []byte) bool {
	switch {
	case len(b) >= 8 && (bytes.Equal(b[4:8], []byte("ftyp")) || bytes.Equal(b[4:8], []byte("moov")) ||
		bytes.Equal(b[4:8], []byte("mdat")) || bytes.Equal(b[4:8], []byte("wide")) || bytes.Equal(b[4:8], []byte("free"))):
		return true
	case len(b) >= 4 && bytes.Equal(b[:4], []byte{0x1A, 0x45, 0xDF, 0xA3}):
		return true
	case len(b) >= 4 && bytes.Equal(b[:4], []byte("OggS")):
		return true
	case len(b) >= 12 && bytes.Equal(b[:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("AVI ")):
		return true
	case len(b) >= 4 && bytes.Equal(b[:3], []byte{0, 0, 1}) && (b[3] == 0xBA || b[3] == 0xB3):
		return true
	}
	return false
}
