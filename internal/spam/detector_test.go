package spam

import (
	"math"
	"testing"
)

func TestDetector(t *testing.T) {
	detector, err := NewDetector(0.95)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		text string
		spam bool
	}{
		{name: "typical spam", text: "Удаленная подработка от 5000 рублей в день, пишите в ЛС: https://t.me/example", spam: true},
		{name: "obfuscated spam", text: "З.А.Р.А.Б.О.Т.О.К без вложений 10000 ₽, пишите в личку", spam: true},
		{name: "free vpn ad", text: "VPN с которым летает Telegram, TikTok, YouTube - БЕСПЛАТНО", spam: true},
		{name: "normal link", text: "Документация проекта лежит здесь: https://example.com/docs", spam: false},
		{name: "normal payment", text: "Я оплатил 5000 рублей за билеты, переведите свою часть", spam: false},
		{name: "vpn setup", text: "Как настроить VPN на роутере для домашней сети", spam: false},
		{name: "empty", text: "", spam: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := detector.Detect(tt.text)
			if result.Spam != tt.spam {
				t.Fatalf("Detect(%q) = %+v, want spam=%t", tt.text, result, tt.spam)
			}
			if math.IsNaN(result.Score) || result.Score < 0 || result.Score > 1 {
				t.Fatalf("invalid score: %v", result.Score)
			}
		})
	}
}

func TestDetectorRequiresIndependentSignals(t *testing.T) {
	detector, err := NewDetector(0.5)
	if err != nil {
		t.Fatal(err)
	}
	result := detector.Detect("Удаленная работа")
	if result.Spam {
		t.Fatalf("single signal triggered spam: %+v", result)
	}
}

func TestNewDetectorValidatesThreshold(t *testing.T) {
	if _, err := NewDetector(0.49); err == nil {
		t.Fatal("expected threshold validation error")
	}
}

func TestLearnSpamIsIdempotent(t *testing.T) {
	detector, err := NewDetector(0.95)
	if err != nil {
		t.Fatal(err)
	}
	if err := detector.LearnSpam(-1, 10, "новая рекламная схема пишите в личку"); err != nil {
		t.Fatal(err)
	}
	if err := detector.LearnSpam(-1, 10, "новая рекламная схема пишите в личку"); err != nil {
		t.Fatal(err)
	}
	if len(detector.learned) != 1 {
		t.Fatalf("learned samples = %d, want 1", len(detector.learned))
	}
}

func TestLearnedWordsBecomeConservativeSignal(t *testing.T) {
	detector, err := NewDetector(0.5)
	if err != nil {
		t.Fatal(err)
	}
	text := "суперсхема https://example.com"
	if result := detector.Detect(text); result.Spam {
		t.Fatalf("single link signal triggered spam before learning: %+v", result)
	}
	for messageID := 1; messageID <= learnedTokenFloor; messageID++ {
		if err := detector.LearnSpam(-1, messageID, "суперсхема приносит результат"); err != nil {
			t.Fatal(err)
		}
	}
	result := detector.Detect(text)
	if !result.Spam || !containsSignal(result.Signals, "learned") {
		t.Fatalf("learned token was not used conservatively: %+v", result)
	}
}

func containsSignal(signals []string, expected string) bool {
	for _, signal := range signals {
		if signal == expected {
			return true
		}
	}
	return false
}

func TestNormalize(t *testing.T) {
	if got := normalize("П.О.Д.Р.А.Б.О.Т.К.А привееееет\u200b"); got != "подработка привеет" {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}
