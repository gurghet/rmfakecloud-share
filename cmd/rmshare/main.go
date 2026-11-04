// Command rmshare is a sidecar for rmfakecloud that implements the reMarkable
// "Convert and Share" endpoint (POST /share/v1/link).
package main

import (
	"context"
	"errors"
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
	if len(os.Args) > 1 && (os.Args[1] == "-version" || os.Args[1] == "--version") {
		fmt.Println("rmshare", version)
		return
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
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
	verifier, err := auth.NewVerifier(cfg.JWTSecret, cfg.JWTKeyMode)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	deps := server.Deps{
		Store: st, Verifier: verifier, PublicURL: cfg.PublicURL,
		MaxBody: cfg.MaxBodyBytes, MaxPages: cfg.MaxPages, Log: log,
	}
	if cfg.AnthropicKey != "" {
		conv := convert.NewClaude(convert.ClaudeOptions{APIKey: cfg.AnthropicKey, Model: cfg.Model, MaxRetries: -1})
		w := worker.New(st, conv, log)
		if err := w.Start(ctx); err != nil {
			return fmt.Errorf("starting worker: %w", err)
		}
		deps.Enqueuer = w
		log.Info("conversion enabled: page images are sent to the Anthropic API", "model", conv.Model())
	} else {
		log.Warn("ANTHROPIC_API_KEY not set: conversion disabled, shares show page images only")
	}

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(deps),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute, // uploads over slow links
		WriteTimeout:      time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    32 << 10,
	}
	errc := make(chan error, 1)
	go func() { errc <- httpSrv.ListenAndServe() }()
	log.Info("listening", "addr", cfg.Listen, "public_url", cfg.PublicURL, "version", version)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
