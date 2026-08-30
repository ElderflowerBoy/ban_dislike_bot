package config

import (
	"log/slog"
	"testing"
)

func TestLoad(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", " token ")
	t.Setenv("DB_PATH", "")
	t.Setenv("LOG_LEVEL", "warn")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "token" || cfg.DBPath != "./data/bot.db" || cfg.LogLevel != slog.LevelWarn {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestLoadRequiresToken(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected an error")
	}
}
