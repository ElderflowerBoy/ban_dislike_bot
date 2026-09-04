package config

import (
	"log/slog"
	"testing"
)

func TestLoad(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", " token ")
	t.Setenv("DB_PATH", "")
	t.Setenv("LOG_LEVEL", "warn")
	t.Setenv("SPAM_FILTER_ENABLED", "true")
	t.Setenv("SPAM_SCORE_THRESHOLD", "0.97")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "token" || cfg.DBPath != "./data/bot.db" || cfg.LogLevel != slog.LevelWarn || !cfg.SpamFilterEnabled || cfg.SpamThreshold != 0.97 {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestLoadRequiresToken(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected an error")
	}
}

func TestLoadRejectsInvalidSpamConfiguration(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "token")
	t.Setenv("SPAM_FILTER_ENABLED", "sometimes")
	if _, err := Load(); err == nil {
		t.Fatal("expected invalid boolean error")
	}

	t.Setenv("SPAM_FILTER_ENABLED", "true")
	t.Setenv("SPAM_SCORE_THRESHOLD", "1.1")
	if _, err := Load(); err == nil {
		t.Fatal("expected invalid threshold error")
	}
}
