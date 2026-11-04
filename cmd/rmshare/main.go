// Command rmshare is a sidecar for rmfakecloud that implements the reMarkable
// "Convert and Share" endpoint (POST /share/v1/link).
//
// Usage:
//
//	rmshare                 run the service (configuration from the environment)
//	rmshare -retry <id>     re-queue a failed share; the running service picks it up within a minute
//	rmshare -version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gurghet/rmfakecloud-share/internal/auth"
	"github.com/gurghet/rmfakecloud-share/internal/config"
	"github.com/gurghet/rmfakecloud-share/internal/convert"
	"github.com/gurghet/rmfakecloud-share/internal/server"
	"github.com/gurghet/rmfakecloud-share/internal/store"
	"github.com/gurghet/rmfakecloud-share/internal/worker"
)

var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	retry := flag.String("retry", "", "re-queue the failed share with this id and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("rmshare", version)
		return
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	var err error
	if *retry != "" {
		err = retryShare(*retry)
	} else {
		err = run(log)
	}
	if err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func retryShare(id string) error {
	dir := os.Getenv("RMSHARE_DATA_DIR")
	if dir == "" {
		return errors.New("RMSHARE_DATA_DIR is required")
	}
	st, err := store.New(dir)
	if err != nil {
		return err
	}
	if err := st.Requeue(id); err != nil {
		return fmt.Errorf("re-queueing %s: %w", id, err)
	}
	fmt.Println("share", id, "is pending again; the running service converts it within a minute (or on its next start)")
	return nil
}

func run(log *slog.Logger) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("configuration error:\n%w", err)
	}
	st, err := store.New(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("opening data dir: %w", err)
	}
	st.Log = log
	verifier, err := auth.NewVerifier(cfg.JWTSecret, cfg.JWTKeyMode)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	deps := server.Deps{
		Store: st, Verifier: verifier, PublicURL: cfg.PublicURL,
		MaxBody: cfg.MaxBodyBytes, MaxPages: cfg.MaxPages, MaxPageBytes: cfg.MaxPageBytes,
		MaxConcurrentUploads: cfg.MaxConcurrentUploads, MaxPagesPerDay: cfg.MaxPagesPerDay, Log: log,
	}
	var w *worker.Worker
	if cfg.AnthropicKey != "" {
		conv := convert.NewClaude(convert.ClaudeOptions{APIKey: cfg.AnthropicKey, Model: cfg.Model, MaxRetries: -1})
		w = worker.New(st, conv, log)
		if err := w.Start(ctx); err != nil {
			return fmt.Errorf("starting worker: %w", err)
		}
		deps.Enqueuer = w
		log.Info("conversion enabled: page images are sent to the Anthropic API", "model", conv.Model())
	} else {
		log.Warn("ANTHROPIC_API_KEY not set: conversion disabled, shares show page images only")
	}
	if cfg.RetentionDays > 0 {
		go st.PruneLoop(ctx, time.Hour, time.Duration(cfg.RetentionDays)*24*time.Hour, time.Now)
		log.Info("retention enabled", "days", cfg.RetentionDays)
	}

	httpSrv := server.NewHTTPServer(cfg.Listen, server.New(deps), server.DefaultTimeouts())
	errc := make(chan error, 1)
	go func() { errc <- httpSrv.ListenAndServe() }()
	log.Info("listening", "addr", cfg.Listen, "public_url", cfg.PublicURL, "version", version)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down", "grace", cfg.ShutdownGrace)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("http shutdown", "err", err)
	}
	if w != nil {
		// Let a conversion in flight finish so a restart does not pay for it twice.
		w.Shutdown(cfg.ShutdownGrace)
	}
	return nil
}
