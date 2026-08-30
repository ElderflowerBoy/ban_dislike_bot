package moderation

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/ElderflowerBoy/ban_dislike_bot/internal/core"
)

const maxAttempts = 5

type Store interface {
	ApplyReaction(context.Context, core.ReactionChange) (core.ApplyResult, error)
	DueJobs(context.Context, int) ([]core.ModerationJob, error)
	MarkStep(context.Context, int64, int, string) error
	FinishJob(context.Context, int64, int, string) error
	RetryJob(context.Context, core.ModerationJob, error, time.Duration) error
	FailJob(context.Context, core.ModerationJob, error) error
}

type Telegram interface {
	IsAdministrator(context.Context, int64, int64) (bool, error)
	Ban(context.Context, int64, int64) error
	DeleteMessage(context.Context, int64, int) error
	NotifyBanned(context.Context, core.ModerationJob) error
	NotifyFailure(context.Context, core.ModerationJob, error) error
}

type Service struct {
	store    Store
	telegram Telegram
	logger   *slog.Logger
	wake     chan struct{}
}

func New(store Store, telegram Telegram, logger *slog.Logger) *Service {
	return &Service{store: store, telegram: telegram, logger: logger, wake: make(chan struct{}, 1)}
}

func (s *Service) ApplyReaction(ctx context.Context, change core.ReactionChange) (core.ApplyResult, error) {
	result, err := s.store.ApplyReaction(ctx, change)
	if err != nil {
		return core.ApplyResult{}, err
	}
	if result.Queued {
		s.Wake()
	}
	return result, nil
}

func (s *Service) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if err := s.processDue(ctx); err != nil && ctx.Err() == nil {
			s.logger.Error("process moderation jobs", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.wake:
		}
	}
}

func (s *Service) processDue(ctx context.Context) error {
	jobs, err := s.store.DueJobs(ctx, 20)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if err := s.processJob(ctx, job); err != nil {
			s.logger.Warn("moderation job postponed", "chat_id", job.ChatID, "message_id", job.MessageID, "attempt", job.Attempts+1, "error", err)
			if job.Attempts+1 >= maxAttempts {
				if failErr := s.store.FailJob(ctx, job, err); failErr != nil {
					return fmt.Errorf("record permanent failure: %w", failErr)
				}
				_ = s.telegram.NotifyFailure(ctx, job, err)
				if finishErr := s.store.FinishJob(ctx, job.ChatID, job.MessageID, "failed"); finishErr != nil {
					return fmt.Errorf("mark permanently failed: %w", finishErr)
				}
				continue
			}
			delay := time.Second * time.Duration(1<<min(job.Attempts, 4))
			if retryErr := s.store.RetryJob(ctx, job, err, delay); retryErr != nil {
				return fmt.Errorf("schedule retry: %w", retryErr)
			}
		}
	}
	return nil
}

func (s *Service) processJob(ctx context.Context, job core.ModerationJob) error {
	if !job.BanDone {
		admin, err := s.telegram.IsAdministrator(ctx, job.ChatID, job.AuthorID)
		if err != nil {
			return fmt.Errorf("check author status: %w", err)
		}
		if admin {
			return s.store.FinishJob(ctx, job.ChatID, job.MessageID, "exempt")
		}
		if err := s.telegram.Ban(ctx, job.ChatID, job.AuthorID); err != nil {
			return fmt.Errorf("ban author: %w", err)
		}
		if err := s.store.MarkStep(ctx, job.ChatID, job.MessageID, "ban"); err != nil {
			return fmt.Errorf("save ban result: %w", err)
		}
	}
	if !job.DeleteDone {
		if err := s.telegram.DeleteMessage(ctx, job.ChatID, job.MessageID); err != nil {
			return fmt.Errorf("delete message: %w", err)
		}
		if err := s.store.MarkStep(ctx, job.ChatID, job.MessageID, "delete"); err != nil {
			return fmt.Errorf("save deletion result: %w", err)
		}
	}
	if !job.NotifyDone {
		if err := s.telegram.NotifyBanned(ctx, job); err != nil {
			return fmt.Errorf("send notification: %w", err)
		}
		if err := s.store.MarkStep(ctx, job.ChatID, job.MessageID, "notify"); err != nil {
			return fmt.Errorf("save notification result: %w", err)
		}
	}
	return s.store.FinishJob(ctx, job.ChatID, job.MessageID, "completed")
}
