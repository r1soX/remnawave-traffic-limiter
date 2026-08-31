package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"remnawave-traffic-limiter/internal/config"
	"remnawave-traffic-limiter/internal/engine"
	"remnawave-traffic-limiter/internal/httpapi"
	"remnawave-traffic-limiter/internal/reconcile"
	"remnawave-traffic-limiter/internal/state"
	"remnawave-traffic-limiter/internal/webhook"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		panic(err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel()}))
	slog.SetDefault(logger)

	store, err := state.NewSQLite(cfg.DatabasePath)
	if err != nil {
		panic(err)
	}
	defer store.Close()

	proc, err := engine.NewProcessor(cfg.PanelURL, cfg.APIToken, cfg.BasicSquad, cfg.WhitelistSquad)
	if err != nil {
		panic(err)
	}
	if cfg.EnablePairedWhiteList {
		if err := proc.ConfigurePairedWhiteList(store, cfg.LimitNoticeSquad); err != nil {
			panic(err)
		}
	}
	valid, err := webhook.NewValidator(cfg.WebhookSecret)
	if err != nil {
		panic(err)
	}

	api := httpapi.New(cfg, store, proc, valid)
	runner := reconcile.NewRunner(cfg.PollInterval, func() {
		if err := api.ReconcileWhiteListUsers(); err != nil {
			slog.Error("WhiteList reconciliation failed", "error", err)
		}
	})
	runner.Start()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- api.ListenAndServe(cfg.Port)
	}()

	slog.Info("service started", "port", cfg.Port, "database", cfg.DatabasePath)

	select {
	case <-ctx.Done():
		slog.Info("shutdown signal received")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := api.Shutdown(ctx); err != nil {
			slog.Error("graceful shutdown failed", "error", err)
		}
	case err := <-serverErr:
		if err != nil {
			slog.Error("server exited unexpectedly", "error", err)
			os.Exit(1)
		}
	}
}
