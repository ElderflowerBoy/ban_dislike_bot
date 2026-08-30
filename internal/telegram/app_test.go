package telegram

import (
	"testing"

	"github.com/go-telegram/bot/models"
)

func TestCommandName(t *testing.T) {
	tests := map[string]string{
		"":                         "",
		"hello":                    "",
		"/status":                  "status",
		"/SET_THRESHOLD@sample 12": "set_threshold",
	}
	for input, want := range tests {
		if got := commandName(input); got != want {
			t.Errorf("commandName(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestDislikeHelpers(t *testing.T) {
	dislike := models.ReactionType{Type: models.ReactionTypeTypeEmoji, ReactionTypeEmoji: &models.ReactionTypeEmoji{Emoji: "👎"}}
	like := models.ReactionType{Type: models.ReactionTypeTypeEmoji, ReactionTypeEmoji: &models.ReactionTypeEmoji{Emoji: "👍"}}
	if dislikePresent([]models.ReactionType{like, dislike}) != 1 {
		t.Fatal("dislike not detected")
	}
	if dislikePresent([]models.ReactionType{like}) != 0 {
		t.Fatal("like detected as dislike")
	}
	counts := []models.ReactionCount{{Type: like, TotalCount: 9}, {Type: dislike, TotalCount: 4}}
	if got := dislikeCount(counts); got != 4 {
		t.Fatalf("dislikeCount = %d, want 4", got)
	}
}

func TestDisplayName(t *testing.T) {
	if got := displayName(&models.User{ID: 1, Username: "alice", FirstName: "Alice"}); got != "@alice" {
		t.Fatalf("unexpected username: %q", got)
	}
	if got := displayName(&models.User{ID: 2, FirstName: "Ivan", LastName: "Petrov"}); got != "Ivan Petrov" {
		t.Fatalf("unexpected name: %q", got)
	}
}
