package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ElderflowerBoy/ban_dislike_bot/internal/core"
	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

type recordingHTTPClient struct {
	request *http.Request
	result  string
}

func (c *recordingHTTPClient) Do(request *http.Request) (*http.Response, error) {
	c.request = request
	result := c.result
	if result == "" {
		result = "{}"
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewBufferString(`{"ok":true,"result":` + result + `}`)),
	}, nil
}

func TestNotifyBannedSendsEphemeralMessageToTriggeringVoter(t *testing.T) {
	client := &recordingHTTPClient{}
	telegramBot, err := bot.New("test-token", bot.WithHTTPClient(time.Second, client))
	if err != nil {
		t.Fatal(err)
	}
	client.request = nil
	app := &App{bot: telegramBot}
	job := core.ModerationJob{
		ChatID:             -100,
		AuthorName:         "@author",
		NotificationUserID: 103,
		Dislikes:           3,
	}

	if err := app.NotifyBanned(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if client.request == nil {
		t.Fatal("Telegram request was not sent")
	}
	if got := client.request.FormValue("chat_id"); got != "-100" {
		t.Fatalf("chat_id = %q, want -100", got)
	}
	if got := client.request.FormValue("text"); !strings.Contains(got, "@author") {
		t.Fatalf("notification text = %q", got)
	}
	var parameters models.EphemeralMessageParameters
	if err := json.Unmarshal([]byte(client.request.FormValue("ephemeral_message_parameters")), &parameters); err != nil {
		t.Fatal(err)
	}
	if parameters.ReceiverUserID != job.NotificationUserID {
		t.Fatalf("receiver_user_id = %d, want %d", parameters.ReceiverUserID, job.NotificationUserID)
	}
}

func TestNotifyBannedSkipsLegacyJobWithoutReceiver(t *testing.T) {
	client := &recordingHTTPClient{}
	telegramBot, err := bot.New("test-token", bot.WithHTTPClient(time.Second, client))
	if err != nil {
		t.Fatal(err)
	}
	client.request = nil

	if err := (&App{bot: telegramBot}).NotifyBanned(context.Background(), core.ModerationJob{}); err != nil {
		t.Fatal(err)
	}
	if client.request != nil {
		t.Fatal("legacy job sent a public notification")
	}
}

func TestSetDislikeUsesTelegramReaction(t *testing.T) {
	client := &recordingHTTPClient{}
	telegramBot, err := bot.New("test-token", bot.WithHTTPClient(time.Second, client))
	if err != nil {
		t.Fatal(err)
	}
	client.request = nil
	client.result = "true"

	if err := (&App{bot: telegramBot}).setDislike(context.Background(), -100, 42); err != nil {
		t.Fatal(err)
	}
	if client.request == nil {
		t.Fatal("Telegram request was not sent")
	}
	if got := client.request.FormValue("message_id"); got != "42" {
		t.Fatalf("message_id = %q, want 42", got)
	}
	if got := client.request.FormValue("reaction"); !strings.Contains(got, "👎") {
		t.Fatalf("reaction = %q", got)
	}
}

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
}

func TestDisplayName(t *testing.T) {
	if got := displayName(&models.User{ID: 1, Username: "alice", FirstName: "Alice"}); got != "@alice" {
		t.Fatalf("unexpected username: %q", got)
	}
	if got := displayName(&models.User{ID: 2, FirstName: "Ivan", LastName: "Petrov"}); got != "Ivan Petrov" {
		t.Fatalf("unexpected name: %q", got)
	}
}

func TestFindChannelSpam(t *testing.T) {
	group := models.Chat{ID: -100, Type: models.ChatTypeSupergroup, Title: "Group"}
	spamChannel := models.Chat{ID: -200, Type: models.ChatTypeChannel, Title: "Fast VPN"}
	forwardOrigin := &models.MessageOrigin{
		Type: models.MessageOriginTypeChannel,
		MessageOriginChannel: &models.MessageOriginChannel{Chat: models.Chat{
			ID: -300, Type: models.ChatTypeChannel, Title: "Cheap Proxy",
		}},
	}

	tests := []struct {
		name    string
		message models.Message
		found   bool
		ban     bool
	}{
		{name: "sender channel", message: models.Message{Chat: group, SenderChat: &spamChannel}, found: true, ban: true},
		{name: "anonymous admin", message: models.Message{Chat: group, SenderChat: &group, ForwardOrigin: forwardOrigin}, found: false},
		{name: "automatic forward", message: models.Message{Chat: group, SenderChat: &spamChannel, ForwardOrigin: forwardOrigin, IsAutomaticForward: true}, found: false},
		{
			name:    "forwarded channel",
			message: models.Message{Chat: group, ForwardOrigin: forwardOrigin},
			found:   true,
		},
		{name: "normal channel", message: models.Message{Chat: group, SenderChat: &models.Chat{ID: -400, Type: models.ChatTypeChannel, Title: "Local news"}}, found: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			match, found := findChannelSpam(&tt.message)
			if found != tt.found || match.ban != tt.ban {
				t.Fatalf("findChannelSpam() = (%+v, %t), want found=%t ban=%t", match, found, tt.found, tt.ban)
			}
		})
	}
}

func TestMemberChange(t *testing.T) {
	user := models.User{ID: 12, FirstName: "New"}
	change, ok := memberChange(&models.ChatMemberUpdated{
		Chat:          models.Chat{ID: -1, Type: models.ChatTypeSupergroup},
		Date:          int(time.Now().Unix()),
		OldChatMember: models.ChatMember{Type: models.ChatMemberTypeLeft, Left: &models.ChatMemberLeft{User: &user}},
		NewChatMember: models.ChatMember{Type: models.ChatMemberTypeMember, Member: &models.ChatMemberMember{User: &user}},
	})
	if !ok || change.UserID != user.ID || !change.Active || change.Administrator || change.JoinedAt.IsZero() {
		t.Fatalf("unexpected member change: %+v, ok=%t", change, ok)
	}

	admin := models.User{ID: 13, IsBot: false}
	change, ok = memberChange(&models.ChatMemberUpdated{
		Chat:          models.Chat{ID: -1, Type: models.ChatTypeSupergroup},
		OldChatMember: models.ChatMember{Type: models.ChatMemberTypeMember, Member: &models.ChatMemberMember{User: &admin}},
		NewChatMember: models.ChatMember{Type: models.ChatMemberTypeAdministrator, Administrator: &models.ChatMemberAdministrator{User: admin}},
	})
	if !ok || !change.Administrator || change.JoinedAt != (time.Time{}) {
		t.Fatalf("unexpected admin change: %+v, ok=%t", change, ok)
	}
}
