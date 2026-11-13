package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for _, k := range []string{"SESSION_SECRET", "AI_PROVIDER", "AI_API_KEY", "AI_MODEL", "STT_PROVIDER", "STT_API_KEY",
		"MAX_UPLOAD_MB", "PROCESSING_TIMEOUT", "PROCESSING_CONCURRENCY", "LOG_FORMAT", "SMTP_HOST", "SMTP_FROM", "APP_BASE_URL"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func TestLoadDefaults(t *testing.T) {
	setEnv(t, map[string]string{"SESSION_SECRET": strings.Repeat("x", 32), "AI_API_KEY": "k", "STT_PROVIDER": "none"})
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.AIModel != "claude-opus-5-5" || c.MaxUploadBytes != 500<<20 || c.ProcessingTimeout != time.Hour ||
		c.ProcessingConcurrency != 2 || c.SecureCookies || c.EmailEnabled() {
		t.Fatalf("defaults: %+v", c)
	}
}

func TestLoadReportsAllProblems(t *testing.T) {
	setEnv(t, map[string]string{"SESSION_SECRET": "short", "AI_PROVIDER": "bogus", "MAX_UPLOAD_MB": "-1",
		"PROCESSING_TIMEOUT": "soon", "SMTP_HOST": "smtp.example.com"})
	_, err := Load()
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"SESSION_SECRET", "AI_PROVIDER", "AI_API_KEY", "STT_API_KEY", "MAX_UPLOAD_MB", "PROCESSING_TIMEOUT", "SMTP_FROM"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %s: %v", want, err)
		}
	}
}

func TestSecureCookiesFollowHTTPS(t *testing.T) {
	setEnv(t, map[string]string{"SESSION_SECRET": strings.Repeat("x", 32), "AI_API_KEY": "k", "STT_PROVIDER": "none",
		"APP_BASE_URL": "https://learn.example.com/"})
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.SecureCookies || c.BaseURL != "https://learn.example.com" {
		t.Fatalf("%+v", c)
	}
}

func TestDotEnvDoesNotOverride(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(p, []byte("# comment\nDOTENV_A=from-file\nexport DOTENV_B=\"quoted value\"\nDOTENV_C=file\n"), 0o600)
	t.Setenv("DOTENV_C", "real-env")
	os.Unsetenv("DOTENV_A")
	os.Unsetenv("DOTENV_B")
	if err := LoadDotEnv(p); err != nil {
		t.Fatal(err)
	}
	defer os.Unsetenv("DOTENV_A")
	defer os.Unsetenv("DOTENV_B")
	if os.Getenv("DOTENV_A") != "from-file" || os.Getenv("DOTENV_B") != "quoted value" || os.Getenv("DOTENV_C") != "real-env" {
		t.Fatalf("dotenv: %q %q %q", os.Getenv("DOTENV_A"), os.Getenv("DOTENV_B"), os.Getenv("DOTENV_C"))
	}
	if err := LoadDotEnv(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatal("missing .env should not be an error")
	}
}
