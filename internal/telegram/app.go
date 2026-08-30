package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/ElderflowerBoy/ban_dislike_bot/internal/core"
	"github.com/ElderflowerBoy/ban_dislike_bot/internal/moderation"
	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

type Store interface {
	TrackMessage(context.Context, core.TrackedMessage) error
	Settings(context.Context, int64) (core.Settings, error)
	SetEnabled(context.Context, int64, bool) error
	SetThreshold(context.Context, int64, int) error
	moderation.Store
}

type App struct {
	bot        *bot.Bot
	store      Store
	moderation *moderation.Service
	logger     *slog.Logger
}

func New(token string, store Store, logger *slog.Logger) (*App, error) {
	a := &App{store: store, logger: logger}
	a.moderation = moderation.New(store, a, logger)
	b, err := bot.New(token,
		bot.WithDefaultHandler(a.handleUpdate),
		bot.WithNotAsyncHandlers(),
		bot.WithAllowedUpdates(bot.AllowedUpdates{
			models.AllowedUpdateMessage,
			models.AllowedUpdateMessageReaction,
			models.AllowedUpdateMessageReactionCount,
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
		change := core.ReactionChange{
			UpdateID:  update.ID,
			ChatID:    r.Chat.ID,
			MessageID: r.MessageID,
			Delta:     dislikePresent(r.NewReaction) - dislikePresent(r.OldReaction),
		}
		a.applyReaction(ctx, change)
	}
	if update.MessageReactionCount != nil {
		r := update.MessageReactionCount
		count := dislikeCount(r.Reactions)
		a.applyReaction(ctx, core.ReactionChange{UpdateID: update.ID, ChatID: r.Chat.ID, MessageID: r.MessageID, Exact: &count})
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
	if message.From != nil && message.SenderChat == nil {
		tracked := core.TrackedMessage{
			ChatID: message.Chat.ID, MessageID: message.ID,
			AuthorID: message.From.ID, AuthorName: displayName(message.From),
		}
		if err := a.store.TrackMessage(ctx, tracked); err != nil {
			a.logger.Error("track message", "chat_id", message.Chat.ID, "message_id", message.ID, "error", err)
		}
	}
	if commandName(message.Text) != "" {
		a.handleCommand(ctx, message)
	}
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
	text := fmt.Sprintf("Пользователь %s заблокирован бессрочно: сообщение набрало %d 👎.", job.AuthorName, job.Dislikes)
	_, err := a.bot.SendMessage(ctx, &bot.SendMessageParams{ChatID: job.ChatID, Text: text})
	return err
}

func (a *App) NotifyFailure(ctx context.Context, job core.ModerationJob, cause error) error {
	text := fmt.Sprintf("Не удалось автоматически заблокировать %s за сообщение с %d 👎. Администратору нужно проверить права бота и выполнить модерацию вручную.", job.AuthorName, job.Dislikes)
	if job.BanDone {
		text = fmt.Sprintf("Пользователь %s заблокирован за сообщение с %d 👎, но бот не смог завершить удаление сообщения или отправку уведомления. Проверьте права бота.", job.AuthorName, job.Dislikes)
	}
	_, err := a.bot.SendMessage(ctx, &bot.SendMessageParams{ChatID: job.ChatID, Text: text})
	return err
}

func dislikePresent(reactions []models.ReactionType) int {
	for _, reaction := range reactions {
		if reaction.Type == models.ReactionTypeTypeEmoji && reaction.ReactionTypeEmoji != nil && reaction.ReactionTypeEmoji.Emoji == "👎" {
			return 1
		}
	}
	return 0
}

func dislikeCount(reactions []models.ReactionCount) int {
	for _, reaction := range reactions {
		if dislikePresent([]models.ReactionType{reaction.Type}) == 1 {
			return reaction.TotalCount
		}
	}
	return 0
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

const helpText = `Бот блокирует автора сообщения, когда сообщение набирает заданное количество 👎.

Команды администратора:
/status — состояние и текущий порог
/set_threshold <1..1000> — изменить порог
/enable — проверить права и включить автомодерацию
/disable — выключить автомодерацию

Бот должен быть администратором с правами блокировки участников и удаления сообщений. Учитываются только сообщения, полученные ботом после его запуска.`
