// Command server runs the Lumen video learning web application.
//
// Usage:
//
//	server                     start the HTTP server and background workers
//	server migrate             apply database migrations and exit
//	server reset-link <email>  print a one-hour password reset link (for
//	                           operators when email is not configured)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/quantamsage/ai-video-summarizer/internal/ai"
	"github.com/quantamsage/ai-video-summarizer/internal/auth"
	"github.com/quantamsage/ai-video-summarizer/internal/config"
	"github.com/quantamsage/ai-video-summarizer/internal/db"
	"github.com/quantamsage/ai-video-summarizer/internal/jobs"
	"github.com/quantamsage/ai-video-summarizer/internal/library"
	"github.com/quantamsage/ai-video-summarizer/internal/media"
	"github.com/quantamsage/ai-video-summarizer/internal/pipeline"
	"github.com/quantamsage/ai-video-summarizer/internal/quiz"
	"github.com/quantamsage/ai-video-summarizer/internal/storage"
	"github.com/quantamsage/ai-video-summarizer/internal/store"
	"github.com/quantamsage/ai-video-summarizer/internal/stt"
	"github.com/quantamsage/ai-video-summarizer/internal/summary"
	"github.com/quantamsage/ai-video-summarizer/internal/urlimport"
	"github.com/quantamsage/ai-video-summarizer/internal/web"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if err := config.LoadDotEnv(".env"); err != nil {
		return fmt.Errorf("read .env: %w", err)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := newLogger(cfg)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	conn, err := db.Open(cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := db.Migrate(ctx, conn, log); err != nil {
		return err
	}
	st := store.New(conn)
	authSvc := auth.NewService(st, cfg.SessionTTL)

	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "migrate":
		log.Info("migrations complete")
		return nil
	case "reset-link":
		if len(args) != 2 {
			return errors.New("usage: server reset-link <email>")
		}
		token, u, err := authSvc.CreatePasswordReset(ctx, args[1])
		if err != nil {
			return err
		}
		if u == nil {
			return fmt.Errorf("no account found for %s", args[1])
		}
		fmt.Printf("Password reset link for %s (valid for one hour):\n%s/reset-password?token=%s\n",
			u.Email, cfg.BaseURL, url.QueryEscape(token))
		return nil
	case "serve":
		return serve(ctx, cfg, log, st, authSvc)
	default:
		return fmt.Errorf("unknown command %q (expected serve, migrate or reset-link)", cmd)
	}
}

func serve(ctx context.Context, cfg *config.Config, log *slog.Logger, st *store.Store, authSvc *auth.Service) error {
	files, err := storage.NewLocal(cfg.UploadDir)
	if err != nil {
		return err
	}
	if err := files.CleanTemp(); err != nil {
		log.Warn("clean temporary files", "err", err)
	}

	tools := media.Tools{FFmpeg: cfg.FFmpegPath, FFprobe: cfg.FFprobePath}
	if err := tools.Check(ctx); err != nil {
		log.Warn("FFmpeg is not available; uploaded videos without caption files cannot be processed", "err", err)
	}

	var provider ai.Provider
	switch cfg.AIProvider {
	case "anthropic":
		provider = ai.NewAnthropic(cfg.AIAPIKey, cfg.AIModel)
	case "openai":
		provider = ai.NewOpenAI(cfg.AIBaseURL, cfg.AIAPIKey, cfg.AIModel)
	}
	var speech stt.Provider = stt.Disabled{}
	if cfg.STTProvider == "openai" {
		speech = stt.NewOpenAI(cfg.STTBaseURL, cfg.STTAPIKey, cfg.STTModel)
	}
	log.Info("providers configured", "ai", provider.Name(), "model", provider.Model(), "stt", speech.Name())

	processor := &pipeline.Processor{
		Store:          st,
		Files:          files,
		Media:          tools,
		STT:            speech,
		Summarizer:     &summary.Generator{AI: provider},
		Quizzer:        &quiz.Generator{AI: provider},
		Fetch:          urlimport.NewClient(30 * time.Minute),
		MaxUploadBytes: cfg.MaxUploadBytes,
		Log:            log,
	}
	runner := &jobs.Runner{
		Store:       st,
		Handler:     processor,
		Concurrency: cfg.ProcessingConcurrency,
		Timeout:     cfg.ProcessingTimeout,
		Log:         log,
	}
	lib := &library.Service{Store: st, Files: files, Jobs: runner, MaxAttempts: cfg.JobMaxAttempts, Log: log}

	var mailer auth.Mailer
	if cfg.EmailEnabled() {
		mailer = &auth.SMTPMailer{Host: cfg.SMTPHost, Port: cfg.SMTPPort, Username: cfg.SMTPUsername,
			Password: cfg.SMTPPassword, From: cfg.SMTPFrom}
	} else {
		log.Info("SMTP not configured; password reset emails are disabled (use `server reset-link <email>`)")
	}

	srv, err := web.New(web.Deps{
		Config: cfg, Log: log, Store: st, Auth: authSvc, Library: lib, Files: files,
		Grader: &quiz.Grader{AI: provider}, Mailer: mailer,
	})
	if err != nil {
		return err
	}

	workerCtx, stopWorkers := context.WithCancel(context.Background())
	defer stopWorkers()
	if err := runner.Start(workerCtx); err != nil {
		return err
	}
	srv.StartMaintenance(ctx)

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	errCh := make(chan error, 1)
	go func() {
		log.Info("http server listening", "addr", cfg.Addr, "base_url", cfg.BaseURL)
		errCh <- httpSrv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		stopWorkers()
		runner.Wait()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err = httpSrv.Shutdown(shutdownCtx)
	// Stop workers; interrupted jobs are requeued and resume on next start.
	stopWorkers()
	runner.Wait()
	log.Info("shutdown complete")
	return err
}

func newLogger(cfg *config.Config) *slog.Logger {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	if cfg.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}
