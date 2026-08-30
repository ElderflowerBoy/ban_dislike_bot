package config

import (
	"errors"
	"log/slog"
	"os"
	"strings"
)

type Config struct {
	Token    string
	DBPath   string
	LogLevel slog.Level
}

func Load() (Config, error) {
	cfg := Config{
		Token:  strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN")),
		DBPath: strings.TrimSpace(os.Getenv("DB_PATH")),
	}
	if cfg.Token == "" {
		return Config{}, errors.New("TELEGRAM_BOT_TOKEN is required")
	}
	if cfg.DBPath == "" {
		cfg.DBPath = "./data/bot.db"
	}

	switch strings.ToLower(strings.TrimSpace(os.Getenv("LOG_LEVEL"))) {
	case "debug":
		cfg.LogLevel = slog.LevelDebug
	case "", "info":
		cfg.LogLevel = slog.LevelInfo
	case "warn", "warning":
		cfg.LogLevel = slog.LevelWarn
	case "error":
		cfg.LogLevel = slog.LevelError
	default:
		return Config{}, errors.New("LOG_LEVEL must be debug, info, warn, or error")
	}
	return cfg, nil
}
