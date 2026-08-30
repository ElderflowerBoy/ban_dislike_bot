package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/ElderflowerBoy/ban_dislike_bot/internal/config"
	"github.com/ElderflowerBoy/ban_dislike_bot/internal/storage"
	telegramapp "github.com/ElderflowerBoy/ban_dislike_bot/internal/telegram"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)

	if dir := filepath.Dir(cfg.DBPath); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			logger.Error("create database directory", "error", err)
			os.Exit(1)
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	store, err := storage.Open(ctx, cfg.DBPath)
	if err != nil {
		logger.Error("open storage", "error", err)
		os.Exit(1)
	}
	defer store.Close()

	app, err := telegramapp.New(cfg.Token, store, logger)
	if err != nil {
		logger.Error("initialize telegram bot", "error", err)
		os.Exit(1)
	}
	logger.Info("bot started", "db_path", cfg.DBPath)
	app.Start(ctx)
	logger.Info("bot stopped")
}
