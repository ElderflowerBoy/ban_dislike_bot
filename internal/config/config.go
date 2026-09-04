package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
)

const DefaultSpamThreshold = 0.95

type Config struct {
	Token             string
	DBPath            string
	LogLevel          slog.Level
	SpamFilterEnabled bool
	SpamThreshold     float64
}

func Load() (Config, error) {
	cfg := Config{
		Token:         strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN")),
		DBPath:        strings.TrimSpace(os.Getenv("DB_PATH")),
		SpamThreshold: DefaultSpamThreshold,
	}
	if cfg.Token == "" {
		return Config{}, errors.New("TELEGRAM_BOT_TOKEN is required")
	}
	if cfg.DBPath == "" {
		cfg.DBPath = "./data/bot.db"
	}
	if raw := strings.TrimSpace(os.Getenv("SPAM_FILTER_ENABLED")); raw != "" {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, errors.New("SPAM_FILTER_ENABLED must be true or false")
		}
		cfg.SpamFilterEnabled = enabled
	}
	if raw := strings.TrimSpace(os.Getenv("SPAM_SCORE_THRESHOLD")); raw != "" {
		threshold, err := strconv.ParseFloat(raw, 64)
		if err != nil || threshold < 0.5 || threshold > 1 {
			return Config{}, fmt.Errorf("SPAM_SCORE_THRESHOLD must be a number between 0.5 and 1")
		}
		cfg.SpamThreshold = threshold
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
