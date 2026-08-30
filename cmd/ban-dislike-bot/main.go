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
	cfg, configErr := config.Load()
	if configErr != nil {
		slog.Error("invalid configuration", "error", configErr)
		os.Exit(1)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)

	if dir := filepath.Dir(cfg.DBPath); dir != "." {
		if mkdirErr := os.MkdirAll(dir, 0o750); mkdirErr != nil {
			logger.Error("create database directory", "error", mkdirErr)
			os.Exit(1)
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	store, storeErr := storage.Open(ctx, cfg.DBPath)
	if storeErr != nil {
		logger.Error("open storage", "error", storeErr)
		os.Exit(1)
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			logger.Error("close storage", "error", closeErr)
		}
	}()

	app, appErr := telegramapp.New(cfg.Token, store, logger)
	if appErr != nil {
		logger.Error("initialize telegram bot", "error", appErr)
		os.Exit(1)
	}
	logger.Info("bot started", "db_path", cfg.DBPath)
	app.Start(ctx)
	logger.Info("bot stopped")
}
