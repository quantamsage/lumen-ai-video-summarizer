// Package config loads application configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime configuration.
type Config struct {
	Addr          string // HTTP listen address, e.g. ":8080"
	BaseURL       string // public URL used in password-reset links
	DatabasePath  string
	SessionSecret []byte
	SessionTTL    time.Duration
	SecureCookies bool
	LogLevel      string
	LogFormat     string // "text" or "json"

	AIProvider string // "anthropic" or "openai"
	AIAPIKey   string
	AIModel    string
	AIBaseURL  string // optional override for OpenAI-compatible endpoints

	STTProvider string // "openai" (Whisper-compatible) or "" for none
	STTAPIKey   string
	STTModel    string
	STTBaseURL  string

	FFmpegPath  string
	FFprobePath string

	UploadDir      string
	MaxUploadBytes int64

	ProcessingConcurrency int
	ProcessingTimeout     time.Duration
	JobMaxAttempts        int

	SMTPHost     string
	SMTPPort     int
	SMTPUsername string
	SMTPPassword string
	SMTPFrom     string
}

// Load reads configuration from the environment, applying defaults and
// returning an error that lists every missing or invalid setting.
func Load() (*Config, error) {
	var errs []error
	c := &Config{
		Addr:         env("APP_ADDR", ":8080"),
		BaseURL:      strings.TrimRight(env("APP_BASE_URL", "http://localhost:8080"), "/"),
		DatabasePath: env("DATABASE_PATH", "data/app.db"),
		LogLevel:     strings.ToLower(env("LOG_LEVEL", "info")),
		LogFormat:    strings.ToLower(env("LOG_FORMAT", "text")),

		AIProvider: strings.ToLower(env("AI_PROVIDER", "anthropic")),
		AIAPIKey:   os.Getenv("AI_API_KEY"),
		AIModel:    os.Getenv("AI_MODEL"),
		AIBaseURL:  os.Getenv("AI_BASE_URL"),

		STTProvider: strings.ToLower(env("STT_PROVIDER", "openai")),
		STTAPIKey:   os.Getenv("STT_API_KEY"),
		STTModel:    env("STT_MODEL", "whisper-1"),
		STTBaseURL:  env("STT_BASE_URL", "https://api.openai.com/v1"),

		FFmpegPath:  env("FFMPEG_PATH", "ffmpeg"),
		FFprobePath: env("FFPROBE_PATH", "ffprobe"),

		UploadDir: env("UPLOAD_DIR", "data/uploads"),

		SMTPHost:     os.Getenv("SMTP_HOST"),
		SMTPUsername: os.Getenv("SMTP_USERNAME"),
		SMTPPassword: os.Getenv("SMTP_PASSWORD"),
		SMTPFrom:     os.Getenv("SMTP_FROM"),
	}

	secret := os.Getenv("SESSION_SECRET")
	switch {
	case secret == "":
		errs = append(errs, errors.New("SESSION_SECRET is required (use at least 32 random characters)"))
	case len(secret) < 32:
		errs = append(errs, errors.New("SESSION_SECRET must be at least 32 characters"))
	}
	c.SessionSecret = []byte(secret)

	switch c.AIProvider {
	case "anthropic":
		if c.AIModel == "" {
			c.AIModel = "claude-opus-5-5"
		}
	case "openai":
		if c.AIModel == "" {
			c.AIModel = "gpt-4o"
		}
		if c.AIBaseURL == "" {
			c.AIBaseURL = "https://api.openai.com/v1"
		}
	default:
		errs = append(errs, fmt.Errorf("AI_PROVIDER %q is not supported (use anthropic or openai)", c.AIProvider))
	}
	if c.AIAPIKey == "" {
		errs = append(errs, errors.New("AI_API_KEY is required"))
	}

	switch c.STTProvider {
	case "openai":
		if c.STTAPIKey == "" {
			errs = append(errs, errors.New("STT_API_KEY is required when STT_PROVIDER=openai (set STT_PROVIDER=none to disable speech-to-text)"))
		}
	case "none", "":
		c.STTProvider = "none"
	default:
		errs = append(errs, fmt.Errorf("STT_PROVIDER %q is not supported (use openai or none)", c.STTProvider))
	}

	if c.LogFormat != "text" && c.LogFormat != "json" {
		errs = append(errs, fmt.Errorf("LOG_FORMAT must be text or json, got %q", c.LogFormat))
	}

	c.SessionTTL = durationVar("SESSION_TTL", 7*24*time.Hour, &errs)
	c.SecureCookies = boolVar("SECURE_COOKIES", strings.HasPrefix(c.BaseURL, "https://"), &errs)
	c.MaxUploadBytes = int64(intVar("MAX_UPLOAD_MB", 500, 1, 1<<20, &errs)) << 20
	c.ProcessingConcurrency = intVar("PROCESSING_CONCURRENCY", 2, 1, 64, &errs)
	c.ProcessingTimeout = durationVar("PROCESSING_TIMEOUT", 60*time.Minute, &errs)
	c.JobMaxAttempts = intVar("JOB_MAX_ATTEMPTS", 3, 1, 20, &errs)
	c.SMTPPort = intVar("SMTP_PORT", 587, 1, 65535, &errs)
	if c.SMTPHost != "" && c.SMTPFrom == "" {
		errs = append(errs, errors.New("SMTP_FROM is required when SMTP_HOST is set"))
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("invalid configuration:\n  - %w", joinErrs(errs))
	}
	return c, nil
}

// EmailEnabled reports whether outgoing email (password resets) is configured.
func (c *Config) EmailEnabled() bool { return c.SMTPHost != "" }

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func intVar(key string, def, min, max int, errs *[]error) int {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < min || n > max {
		*errs = append(*errs, fmt.Errorf("%s must be an integer between %d and %d", key, min, max))
		return def
	}
	return n
}

func durationVar(key string, def time.Duration, errs *[]error) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		*errs = append(*errs, fmt.Errorf("%s must be a positive Go duration such as 30m or 2h", key))
		return def
	}
	return d
}

func boolVar(key string, def bool, errs *[]error) bool {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s must be true or false", key))
		return def
	}
	return b
}

func joinErrs(errs []error) error {
	msgs := make([]string, len(errs))
	for i, e := range errs {
		msgs[i] = e.Error()
	}
	return errors.New(strings.Join(msgs, "\n  - "))
}
