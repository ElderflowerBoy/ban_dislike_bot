package moderation

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/ElderflowerBoy/ban_dislike_bot/internal/core"
)

type fakeStore struct {
	jobs       []core.ModerationJob
	steps      []string
	samples    []core.SpamSample
	finalState string
	retries    int
}

func (s *fakeStore) ApplyReaction(context.Context, core.ReactionChange) (core.ApplyResult, error) {
	return core.ApplyResult{}, nil
}
func (s *fakeStore) RecordSpamSample(_ context.Context, sample core.SpamSample) error {
	s.samples = append(s.samples, sample)
	return nil
}
func (s *fakeStore) DueJobs(context.Context, int) ([]core.ModerationJob, error) {
	return s.jobs, nil
}
func (s *fakeStore) MarkStep(_ context.Context, _ int64, _ int, step string) error {
	s.steps = append(s.steps, step)
	return nil
}
func (s *fakeStore) FinishJob(_ context.Context, _ int64, _ int, state string) error {
	s.finalState = state
	return nil
}
func (s *fakeStore) RetryJob(context.Context, core.ModerationJob, error, time.Duration) error {
	s.retries++
	return nil
}
func (s *fakeStore) FailJob(context.Context, core.ModerationJob, error) error {
	return nil
}

type fakeTelegram struct {
	admin         bool
	adminErr      error
	banErr        error
	deleteErr     error
	notifyErr     error
	bans          int
	deletes       int
	notifications int
	failures      int
	learned       []core.SpamSample
}

func (t *fakeTelegram) IsAdministrator(context.Context, int64, int64) (bool, error) {
	return t.admin, t.adminErr
}
func (t *fakeTelegram) Ban(context.Context, int64, int64) error {
	t.bans++
	return t.banErr
}
func (t *fakeTelegram) DeleteMessage(context.Context, int64, int) error {
	t.deletes++
	return t.deleteErr
}
func (t *fakeTelegram) NotifyBanned(context.Context, core.ModerationJob) error {
	t.notifications++
	return t.notifyErr
}
func (t *fakeTelegram) NotifyFailure(context.Context, core.ModerationJob, error) error {
	t.failures++
	return nil
}
func (t *fakeTelegram) LearnSpam(sample core.SpamSample) error {
	t.learned = append(t.learned, sample)
	return nil
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestProcessJobCompletesAllSteps(t *testing.T) {
	store := &fakeStore{}
	telegram := &fakeTelegram{}
	service := New(store, telegram, testLogger())
	job := core.ModerationJob{ChatID: -1, MessageID: 2, AuthorID: 3, Dislikes: 10}

	if err := service.processJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if telegram.bans != 1 || telegram.deletes != 1 || telegram.notifications != 1 {
		t.Fatalf("unexpected telegram calls: %+v", telegram)
	}
	if len(store.steps) != 3 || store.steps[0] != "ban" || store.steps[1] != "delete" || store.steps[2] != "notify" {
		t.Fatalf("unexpected persisted steps: %v", store.steps)
	}
	if store.finalState != "completed" {
		t.Fatalf("final state = %q", store.finalState)
	}
}

func TestProcessJobLearnsFromBannedMessage(t *testing.T) {
	store := &fakeStore{}
	telegram := &fakeTelegram{}
	service := New(store, telegram, testLogger())
	job := core.ModerationJob{ChatID: -1, MessageID: 2, AuthorID: 3, Content: "spam text"}

	if err := service.processJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if len(store.samples) != 1 || store.samples[0].Content != job.Content {
		t.Fatalf("persisted samples = %+v", store.samples)
	}
	if len(telegram.learned) != 1 || telegram.learned[0] != store.samples[0] {
		t.Fatalf("cached samples = %+v", telegram.learned)
	}
}

func TestProcessJobProtectsAdministrator(t *testing.T) {
	store := &fakeStore{}
	telegram := &fakeTelegram{admin: true}
	service := New(store, telegram, testLogger())

	if err := service.processJob(context.Background(), core.ModerationJob{ChatID: -1, MessageID: 2, AuthorID: 3}); err != nil {
		t.Fatal(err)
	}
	if telegram.bans != 0 || telegram.deletes != 0 || telegram.notifications != 0 {
		t.Fatal("administrator was moderated")
	}
	if store.finalState != "exempt" {
		t.Fatalf("final state = %q", store.finalState)
	}
}

func TestProcessJobProtectsActiveAuthor(t *testing.T) {
	store := &fakeStore{}
	telegram := &fakeTelegram{}
	service := New(store, telegram, testLogger())

	if err := service.processJob(context.Background(), core.ModerationJob{ChatID: -1, MessageID: 2, AuthorID: 3, Content: "disliked but not banned", ProtectAuthor: true}); err != nil {
		t.Fatal(err)
	}
	if telegram.bans != 0 || telegram.deletes != 1 || telegram.notifications != 1 {
		t.Fatalf("unexpected protected author calls: %+v", telegram)
	}
	if len(store.steps) != 3 || store.steps[0] != "ban" || store.steps[1] != "delete" || store.steps[2] != "notify" {
		t.Fatalf("unexpected persisted steps: %v", store.steps)
	}
	if store.finalState != "completed" {
		t.Fatalf("final state = %q", store.finalState)
	}
	if len(store.samples) != 0 || len(telegram.learned) != 0 {
		t.Fatal("protected author message was used as a confirmed spam sample")
	}
}

func TestProcessDueRetriesTransientFailure(t *testing.T) {
	job := core.ModerationJob{ChatID: -1, MessageID: 2, AuthorID: 3}
	store := &fakeStore{jobs: []core.ModerationJob{job}}
	telegram := &fakeTelegram{banErr: errors.New("temporary failure")}
	service := New(store, telegram, testLogger())

	if err := service.processDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.retries != 1 || store.finalState != "" || telegram.failures != 0 {
		t.Fatalf("unexpected retry state: store=%+v telegram=%+v", store, telegram)
	}
}

func TestProcessDueMarksFifthFailurePermanent(t *testing.T) {
	job := core.ModerationJob{ChatID: -1, MessageID: 2, AuthorID: 3, Attempts: 4}
	store := &fakeStore{jobs: []core.ModerationJob{job}}
	telegram := &fakeTelegram{adminErr: errors.New("no access")}
	service := New(store, telegram, testLogger())

	if err := service.processDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.retries != 0 || store.finalState != "failed" || telegram.failures != 1 {
		t.Fatalf("unexpected permanent failure state: store=%+v telegram=%+v", store, telegram)
	}
}
