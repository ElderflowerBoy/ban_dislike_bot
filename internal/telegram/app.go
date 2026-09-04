package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/ElderflowerBoy/ban_dislike_bot/internal/core"
	"github.com/ElderflowerBoy/ban_dislike_bot/internal/moderation"
	"github.com/ElderflowerBoy/ban_dislike_bot/internal/spam"
	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

type Store interface {
	TrackMessage(context.Context, core.TrackedMessage) error
	Settings(context.Context, int64) (core.Settings, error)
	SetEnabled(context.Context, int64, bool) error
	SetThreshold(context.Context, int64, int) error
	RecordMember(context.Context, core.MemberChange) error
	moderation.Store
}

type SpamDetector interface {
	Detect(string) spam.Result
	LearnSpam(int64, int, string) error
}

type App struct {
	bot        *bot.Bot
	store      Store
	moderation *moderation.Service
	spam       SpamDetector
	logger     *slog.Logger
}

func New(token string, store Store, spamDetector SpamDetector, logger *slog.Logger) (*App, error) {
	a := &App{store: store, spam: spamDetector, logger: logger}
	a.moderation = moderation.New(store, a, logger)
	b, err := bot.New(token,
		bot.WithDefaultHandler(a.handleUpdate),
		bot.WithNotAsyncHandlers(),
		bot.WithAllowedUpdates(bot.AllowedUpdates{
			models.AllowedUpdateMessage,
			models.AllowedUpdateMessageReaction,
			models.AllowedUpdateChatMember,
			models.AllowedUpdateMyChatMember,
		}),
		bot.WithErrorsHandler(func(err error) { logger.Error("telegram polling", "error", err) }),
	)
	if err != nil {
		return nil, err
	}
	a.bot = b
	return a, nil
}

func (a *App) Start(ctx context.Context) {
	go a.moderation.Run(ctx)
	a.moderation.Wake()
	a.bot.Start(ctx)
}

func (a *App) handleUpdate(ctx context.Context, _ *bot.Bot, update *models.Update) {
	if update.Message != nil {
		a.handleMessage(ctx, update.Message)
	}
	if update.MessageReaction != nil {
		r := update.MessageReaction
		if r.User == nil || r.ActorChat != nil {
			return
		}
		change := core.ReactionChange{
			UpdateID:   update.ID,
			ChatID:     r.Chat.ID,
			MessageID:  r.MessageID,
			ActorID:    r.User.ID,
			ActorIsBot: r.User.IsBot,
			Delta:      dislikePresent(r.NewReaction) - dislikePresent(r.OldReaction),
		}
		if change.Delta == 0 || change.ActorIsBot {
			return
		}
		if change.Delta > 0 {
			if admin, err := a.IsAdministrator(ctx, r.Chat.ID, r.User.ID); err == nil {
				change.ActorIsAdmin = admin
			}
		}
		a.applyReaction(ctx, change)
	}
	if update.ChatMember != nil && isGroup(update.ChatMember.Chat.Type) {
		if member, ok := memberChange(update.ChatMember); ok {
			if err := a.store.RecordMember(ctx, member); err != nil {
				a.logger.Error("record chat member", "chat_id", member.ChatID, "user_id", member.UserID, "error", err)
			}
		}
	}
	if update.MyChatMember != nil && isGroup(update.MyChatMember.Chat.Type) {
		member := update.MyChatMember.NewChatMember
		if member.Type == models.ChatMemberTypeLeft || member.Type == models.ChatMemberTypeBanned {
			if err := a.store.SetEnabled(ctx, update.MyChatMember.Chat.ID, false); err != nil {
				a.logger.Error("disable removed bot", "chat_id", update.MyChatMember.Chat.ID, "error", err)
			}
		}
	}
}

func (a *App) applyReaction(ctx context.Context, change core.ReactionChange) {
	result, err := a.moderation.ApplyReaction(ctx, change)
	if err != nil {
		a.logger.Error("apply reaction", "chat_id", change.ChatID, "message_id", change.MessageID, "error", err)
		return
	}
	if result.Queued {
		a.logger.Info("moderation queued", "chat_id", change.ChatID, "message_id", change.MessageID, "dislikes", result.Count)
	}
}

func (a *App) handleMessage(ctx context.Context, message *models.Message) {
	if !isGroup(message.Chat.Type) {
		if commandName(message.Text) != "" {
			a.send(ctx, message.Chat.ID, helpText)
		}
		return
	}
	if a.checkChannelSpam(ctx, message) {
		return
	}
	trackable := message.From != nil && message.SenderChat == nil
	if trackable {
		tracked := core.TrackedMessage{
			ChatID: message.Chat.ID, MessageID: message.ID,
			AuthorID: message.From.ID, AuthorName: displayName(message.From), AuthorIsBot: message.From.IsBot,
			Content: messageContent(message),
		}
		if err := a.store.TrackMessage(ctx, tracked); err != nil {
			a.logger.Error("track message", "chat_id", message.Chat.ID, "message_id", message.ID, "error", err)
		}
	}
	if commandName(message.Text) != "" {
		a.handleCommand(ctx, message)
		return
	}
	if trackable {
		a.checkSpam(ctx, message)
	}
}

type channelSpamMatch struct {
	channel models.Chat
	result  spam.ChannelResult
	ban     bool
}

func findChannelSpam(message *models.Message) (channelSpamMatch, bool) {
	if message.IsAutomaticForward || (message.SenderChat != nil && message.SenderChat.ID == message.Chat.ID) {
		return channelSpamMatch{}, false
	}
	if message.SenderChat != nil &&
		message.SenderChat.Type == models.ChatTypeChannel &&
		message.SenderChat.ID != message.Chat.ID {
		result := spam.DetectChannel(message.SenderChat.Title, message.SenderChat.Username)
		if result.Spam {
			return channelSpamMatch{channel: *message.SenderChat, result: result, ban: true}, true
		}
	}

	if message.ForwardOrigin == nil {
		return channelSpamMatch{}, false
	}
	var channel *models.Chat
	switch message.ForwardOrigin.Type {
	case models.MessageOriginTypeChannel:
		if origin := message.ForwardOrigin.MessageOriginChannel; origin != nil {
			channel = &origin.Chat
		}
	case models.MessageOriginTypeChat:
		if origin := message.ForwardOrigin.MessageOriginChat; origin != nil && origin.SenderChat.Type == models.ChatTypeChannel {
			channel = &origin.SenderChat
		}
	case models.MessageOriginTypeUser, models.MessageOriginTypeHiddenUser:
		return channelSpamMatch{}, false
	}
	if channel == nil {
		return channelSpamMatch{}, false
	}
	result := spam.DetectChannel(channel.Title, channel.Username)
	if !result.Spam {
		return channelSpamMatch{}, false
	}
	return channelSpamMatch{channel: *channel, result: result}, true
}

func (a *App) checkChannelSpam(ctx context.Context, message *models.Message) bool {
	match, found := findChannelSpam(message)
	if !found {
		return false
	}
	settings, err := a.store.Settings(ctx, message.Chat.ID)
	if err != nil {
		a.logger.Error("read settings for channel spam", "chat_id", message.Chat.ID, "message_id", message.ID, "error", err)
		return false
	}
	if !settings.Enabled {
		return false
	}

	if !match.ban {
		if message.From == nil {
			return false
		}
		admin, err := a.IsAdministrator(ctx, message.Chat.ID, message.From.ID)
		if err != nil {
			a.logger.Error("check channel spam forwarder", "chat_id", message.Chat.ID, "message_id", message.ID, "user_id", message.From.ID, "error", err)
			return false
		}
		if admin {
			return false
		}
	}

	if match.ban {
		if err := a.banSenderChat(ctx, message.Chat.ID, match.channel.ID); err != nil {
			a.logger.Error("ban spam sender channel", "chat_id", message.Chat.ID, "message_id", message.ID, "sender_chat_id", match.channel.ID, "channel_title", match.channel.Title, "channel_username", match.channel.Username, "keyword", match.result.Keyword, "error", err)
		}
	}
	if err := a.DeleteMessage(ctx, message.Chat.ID, message.ID); err != nil {
		a.logger.Error("delete channel spam", "chat_id", message.Chat.ID, "message_id", message.ID, "sender_chat_id", match.channel.ID, "channel_title", match.channel.Title, "channel_username", match.channel.Username, "keyword", match.result.Keyword, "ban_sender_chat", match.ban, "error", err)
		return true
	}
	a.logger.Info("channel spam deleted", "chat_id", message.Chat.ID, "message_id", message.ID, "sender_chat_id", match.channel.ID, "channel_title", match.channel.Title, "channel_username", match.channel.Username, "keyword", match.result.Keyword, "ban_sender_chat", match.ban)
	return true
}

func (a *App) checkSpam(ctx context.Context, message *models.Message) {
	if a.spam == nil {
		return
	}
	text := messageContent(message)
	if text == "" {
		return
	}

	settings, err := a.store.Settings(ctx, message.Chat.ID)
	if err != nil {
		a.logger.Error("read settings for spam check", "chat_id", message.Chat.ID, "message_id", message.ID, "error", err)
		return
	}
	if !settings.Enabled {
		return
	}

	result := a.spam.Detect(text)
	if !result.Spam {
		return
	}
	admin, err := a.IsAdministrator(ctx, message.Chat.ID, message.From.ID)
	if err != nil {
		a.logger.Error("check spam author status", "chat_id", message.Chat.ID, "message_id", message.ID, "user_id", message.From.ID, "error", err)
		return
	}
	if admin {
		return
	}
	if reactionErr := a.setDislike(ctx, message.Chat.ID, message.ID); reactionErr != nil {
		a.logger.Error("mark spam message", "chat_id", message.Chat.ID, "message_id", message.ID, "user_id", message.From.ID, "spam_score", result.Score, "signals", result.Signals, "error", reactionErr)
		return
	}
	change := core.ReactionChange{ChatID: message.Chat.ID, MessageID: message.ID, ActorID: a.bot.ID(), ActorIsBot: true, Automated: true, Delta: 1}
	resultVote, err := a.moderation.ApplyReaction(ctx, change)
	if err != nil {
		a.logger.Error("count bot spam vote", "chat_id", message.Chat.ID, "message_id", message.ID, "error", err)
		return
	}
	a.logger.Info("spam message marked", "chat_id", message.Chat.ID, "message_id", message.ID, "user_id", message.From.ID, "spam_score", result.Score, "signals", result.Signals, "dislikes", resultVote.Count)
}

func messageContent(message *models.Message) string {
	if text := strings.TrimSpace(message.Text); text != "" {
		return text
	}
	return strings.TrimSpace(message.Caption)
}

func (a *App) setDislike(ctx context.Context, chatID int64, messageID int) error {
	ok, err := a.bot.SetMessageReaction(ctx, &bot.SetMessageReactionParams{
		ChatID:    chatID,
		MessageID: messageID,
		Reaction: []models.ReactionType{{
			Type:              models.ReactionTypeTypeEmoji,
			ReactionTypeEmoji: &models.ReactionTypeEmoji{Emoji: "👎"},
		}},
	})
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("telegram returned false while setting dislike")
	}
	return nil
}

func (a *App) handleCommand(ctx context.Context, message *models.Message) {
	command := commandName(message.Text)
	if command == "help" || command == "start" {
		a.send(ctx, message.Chat.ID, helpText)
		return
	}
	if command != "status" && command != "enable" && command != "disable" && command != "set_threshold" {
		return
	}
	if message.From == nil {
		return
	}
	admin, err := a.IsAdministrator(ctx, message.Chat.ID, message.From.ID)
	if err != nil {
		a.send(ctx, message.Chat.ID, "Не удалось проверить права администратора. Попробуйте позже.")
		return
	}
	if !admin {
		a.send(ctx, message.Chat.ID, "Эта команда доступна только администраторам группы.")
		return
	}

	switch command {
	case "status":
		a.commandStatus(ctx, message.Chat.ID)
	case "enable":
		rights, err := a.BotRights(ctx, message.Chat.ID)
		if err != nil {
			a.send(ctx, message.Chat.ID, "Не удалось проверить права бота. Попробуйте позже.")
			return
		}
		if !rights.Administrator || !rights.CanRestrict || !rights.CanDelete {
			a.send(ctx, message.Chat.ID, rightsProblem(rights))
			return
		}
		if err := a.store.SetEnabled(ctx, message.Chat.ID, true); err != nil {
			a.storageError(ctx, message.Chat.ID, err)
			return
		}
		a.send(ctx, message.Chat.ID, "Автомодерация включена.")
	case "disable":
		if err := a.store.SetEnabled(ctx, message.Chat.ID, false); err != nil {
			a.storageError(ctx, message.Chat.ID, err)
			return
		}
		a.send(ctx, message.Chat.ID, "Автомодерация выключена. Накопленные данные сохранены.")
	case "set_threshold":
		fields := strings.Fields(message.Text)
		if len(fields) != 2 {
			a.send(ctx, message.Chat.ID, "Использование: /set_threshold <число от 1 до 1000>")
			return
		}
		threshold, err := strconv.Atoi(fields[1])
		if err != nil || threshold < core.MinThreshold || threshold > core.MaxThreshold {
			a.send(ctx, message.Chat.ID, "Порог должен быть целым числом от 1 до 1000.")
			return
		}
		if err := a.store.SetThreshold(ctx, message.Chat.ID, threshold); err != nil {
			a.storageError(ctx, message.Chat.ID, err)
			return
		}
		a.send(ctx, message.Chat.ID, fmt.Sprintf("Новый порог: %d 👎.", threshold))
	}
}

func (a *App) commandStatus(ctx context.Context, chatID int64) {
	settings, err := a.store.Settings(ctx, chatID)
	if err != nil {
		a.storageError(ctx, chatID, err)
		return
	}
	rights, rightsErr := a.BotRights(ctx, chatID)
	state := "выключена"
	if settings.Enabled {
		state = "включена"
	}
	rightsText := "достаточно"
	if rightsErr != nil || !rights.Administrator || !rights.CanRestrict || !rights.CanDelete {
		rightsText = "недостаточно"
	}
	a.send(ctx, chatID, fmt.Sprintf("Автомодерация: %s\nПорог: %d 👎\nПрава бота: %s", state, settings.Threshold, rightsText))
}

func (a *App) storageError(ctx context.Context, chatID int64, err error) {
	a.logger.Error("store command settings", "chat_id", chatID, "error", err)
	a.send(ctx, chatID, "Не удалось сохранить настройку. Попробуйте позже.")
}

func (a *App) send(ctx context.Context, chatID int64, text string) {
	if _, err := a.bot.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: text}); err != nil {
		a.logger.Error("send message", "chat_id", chatID, "error", err)
	}
}

func (a *App) IsAdministrator(ctx context.Context, chatID, userID int64) (bool, error) {
	member, err := a.bot.GetChatMember(ctx, &bot.GetChatMemberParams{ChatID: chatID, UserID: userID})
	if err != nil {
		return false, err
	}
	return member.Type == models.ChatMemberTypeOwner || member.Type == models.ChatMemberTypeAdministrator, nil
}

func (a *App) BotRights(ctx context.Context, chatID int64) (core.BotRights, error) {
	member, err := a.bot.GetChatMember(ctx, &bot.GetChatMemberParams{ChatID: chatID, UserID: a.bot.ID()})
	if err != nil {
		return core.BotRights{}, err
	}
	rights := core.BotRights{Administrator: member.Type == models.ChatMemberTypeAdministrator}
	if member.Administrator != nil {
		rights.CanRestrict = member.Administrator.CanRestrictMembers
		rights.CanDelete = member.Administrator.CanDeleteMessages
	}
	return rights, nil
}

func (a *App) Ban(ctx context.Context, chatID, userID int64) error {
	ok, err := a.bot.BanChatMember(ctx, &bot.BanChatMemberParams{ChatID: chatID, UserID: userID, RevokeMessages: true})
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("telegram returned false while banning member")
	}
	return nil
}

func (a *App) banSenderChat(ctx context.Context, chatID, senderChatID int64) error {
	ok, err := a.bot.BanChatSenderChat(ctx, &bot.BanChatSenderChatParams{ChatID: chatID, SenderChatID: senderChatID})
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("telegram returned false while banning sender chat")
	}
	return nil
}

func (a *App) DeleteMessage(ctx context.Context, chatID int64, messageID int) error {
	ok, err := a.bot.DeleteMessage(ctx, &bot.DeleteMessageParams{ChatID: chatID, MessageID: messageID})
	if err != nil {
		// In supergroups banChatMember can revoke the message before this explicit step.
		if errors.Is(err, bot.ErrorBadRequest) && strings.Contains(strings.ToLower(err.Error()), "message to delete not found") {
			return nil
		}
		return err
	}
	if !ok {
		return errors.New("telegram returned false while deleting message")
	}
	return nil
}

func (a *App) NotifyBanned(ctx context.Context, job core.ModerationJob) error {
	if job.NotificationUserID == 0 {
		return nil
	}
	text := fmt.Sprintf("Сообщение пользователя %s удалено: оно набрало %d уникальных 👎. Пользователь не заблокирован автоматически.", job.AuthorName, job.Dislikes)
	if !job.ProtectAuthor {
		text = fmt.Sprintf("Пользователь %s заблокирован бессрочно: сообщение набрало %d уникальных 👎.", job.AuthorName, job.Dislikes)
	}
	_, err := a.bot.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: job.ChatID,
		Text:   text,
		EphemeralMessageParameters: &models.EphemeralMessageParameters{
			ReceiverUserID: job.NotificationUserID,
		},
	})
	return err
}

func (a *App) NotifyFailure(ctx context.Context, job core.ModerationJob, cause error) error {
	text := fmt.Sprintf("Не удалось автоматически обработать сообщение %s с %d уникальными 👎. Администратору нужно проверить права бота и выполнить модерацию вручную.", job.AuthorName, job.Dislikes)
	if job.ProtectAuthor {
		text = fmt.Sprintf("Сообщение активного участника %s набрало %d доверенных 👎, но бот не смог его удалить. Проверьте права бота.", job.AuthorName, job.Dislikes)
	}
	if job.BanDone {
		text = fmt.Sprintf("Пользователь %s заблокирован за сообщение с %d доверенных 👎, но бот не смог завершить удаление сообщения или отправку уведомления. Проверьте права бота.", job.AuthorName, job.Dislikes)
		if job.ProtectAuthor {
			text = fmt.Sprintf("Сообщение активного участника %s удалено за %d доверенных 👎, но бот не смог завершить уведомление. Проверьте права бота.", job.AuthorName, job.Dislikes)
		}
	}
	_, err := a.bot.SendMessage(ctx, &bot.SendMessageParams{ChatID: job.ChatID, Text: text})
	return err
}

func (a *App) LearnSpam(sample core.SpamSample) error {
	if a.spam == nil {
		return nil
	}
	return a.spam.LearnSpam(sample.ChatID, sample.MessageID, sample.Content)
}

func dislikePresent(reactions []models.ReactionType) int {
	for _, reaction := range reactions {
		if reaction.Type == models.ReactionTypeTypeEmoji && reaction.ReactionTypeEmoji != nil && reaction.ReactionTypeEmoji.Emoji == "👎" {
			return 1
		}
	}
	return 0
}

func memberChange(update *models.ChatMemberUpdated) (core.MemberChange, bool) {
	user, active, admin, ok := memberInfo(update.NewChatMember)
	if !ok || user == nil {
		return core.MemberChange{}, false
	}
	_, wasActive, _, _ := memberInfo(update.OldChatMember)
	var joinedAt time.Time
	if active && !wasActive && update.Date > 0 {
		joinedAt = time.Unix(int64(update.Date), 0)
	}
	return core.MemberChange{
		ChatID:        update.Chat.ID,
		UserID:        user.ID,
		IsBot:         user.IsBot,
		Administrator: admin,
		Active:        active,
		JoinedAt:      joinedAt,
	}, true
}

func memberInfo(member models.ChatMember) (*models.User, bool, bool, bool) {
	switch member.Type {
	case models.ChatMemberTypeOwner:
		if member.Owner == nil {
			return nil, false, false, false
		}
		return member.Owner.User, true, true, member.Owner.User != nil
	case models.ChatMemberTypeAdministrator:
		if member.Administrator == nil {
			return nil, false, false, false
		}
		return &member.Administrator.User, true, true, true
	case models.ChatMemberTypeMember:
		if member.Member == nil {
			return nil, false, false, false
		}
		return member.Member.User, true, false, member.Member.User != nil
	case models.ChatMemberTypeRestricted:
		if member.Restricted == nil {
			return nil, false, false, false
		}
		return member.Restricted.User, member.Restricted.IsMember, false, member.Restricted.User != nil
	case models.ChatMemberTypeLeft:
		if member.Left == nil {
			return nil, false, false, false
		}
		return member.Left.User, false, false, member.Left.User != nil
	case models.ChatMemberTypeBanned:
		if member.Banned == nil {
			return nil, false, false, false
		}
		return member.Banned.User, false, false, member.Banned.User != nil
	default:
		return nil, false, false, false
	}
}

func displayName(user *models.User) string {
	if user.Username != "" {
		return "@" + user.Username
	}
	name := strings.TrimSpace(user.FirstName + " " + user.LastName)
	if name != "" {
		return name
	}
	return fmt.Sprintf("ID %d", user.ID)
}

func commandName(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return ""
	}
	name := strings.TrimPrefix(fields[0], "/")
	if at := strings.IndexByte(name, '@'); at >= 0 {
		name = name[:at]
	}
	return strings.ToLower(name)
}

func isGroup(chatType models.ChatType) bool {
	return chatType == models.ChatTypeGroup || chatType == models.ChatTypeSupergroup
}

func rightsProblem(rights core.BotRights) string {
	var missing []string
	if !rights.Administrator {
		missing = append(missing, "назначить бота администратором")
	} else {
		if !rights.CanRestrict {
			missing = append(missing, "выдать право блокировать участников")
		}
		if !rights.CanDelete {
			missing = append(missing, "выдать право удалять сообщения")
		}
	}
	return "Автомодерация не включена: нужно " + strings.Join(missing, ", ") + "."
}

const helpText = `Бот блокирует автора сообщения, когда сообщение набирает заданное количество уникальных персональных 👎. Голоса учитываются сразу, независимо от давности и активности участника. Каналы с VPN/ВПН/PROXY/ПРОКСИ в названии блокируются автоматически.

Команды администратора:
/status — состояние и текущий порог
/set_threshold <1..1000> — изменить порог
/enable — проверить права и включить автомодерацию
/disable — выключить автомодерацию

Бот должен быть администратором с правами блокировки участников и удаления сообщений. Учитываются только сообщения, полученные ботом после его запуска.`
