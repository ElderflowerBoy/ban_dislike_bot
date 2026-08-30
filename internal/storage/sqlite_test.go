package storage

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/ElderflowerBoy/ban_dislike_bot/internal/core"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestConcurrentThresholdCrossingQueuesSingleJob(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	message := core.TrackedMessage{ChatID: -200, MessageID: 8, AuthorID: 55, AuthorName: "user"}
	if err := store.TrackMessage(ctx, message); err != nil {
		t.Fatal(err)
	}
	if err := store.SetThreshold(ctx, message.ChatID, 3); err != nil {
		t.Fatal(err)
	}
	if err := store.SetEnabled(ctx, message.ChatID, true); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(updateID int64) {
			defer wg.Done()
			_, err := store.ApplyReaction(ctx, core.ReactionChange{UpdateID: updateID, ChatID: message.ChatID, MessageID: message.MessageID, Delta: 1})
			errs <- err
		}(int64(100 + i))
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	jobs, err := store.DueJobs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("got %d moderation jobs, want 1", len(jobs))
	}
}

func TestApplyReactionQueuesOnceAndDeduplicatesUpdates(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	message := core.TrackedMessage{ChatID: -100, MessageID: 7, AuthorID: 42, AuthorName: "@author"}
	if err := store.TrackMessage(ctx, message); err != nil {
		t.Fatal(err)
	}
	settings, err := store.Settings(ctx, message.ChatID)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Enabled || settings.Threshold != core.DefaultThreshold {
		t.Fatalf("unexpected defaults: %+v", settings)
	}
	if err := store.SetThreshold(ctx, message.ChatID, 3); err != nil {
		t.Fatal(err)
	}
	if err := store.SetEnabled(ctx, message.ChatID, true); err != nil {
		t.Fatal(err)
	}

	first, err := store.ApplyReaction(ctx, core.ReactionChange{UpdateID: 1, ChatID: message.ChatID, MessageID: message.MessageID, Delta: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !first.Known || first.Queued || first.Count != 1 {
		t.Fatalf("unexpected first result: %+v", first)
	}
	duplicate, err := store.ApplyReaction(ctx, core.ReactionChange{UpdateID: 1, ChatID: message.ChatID, MessageID: message.MessageID, Delta: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !duplicate.Duplicate {
		t.Fatalf("expected duplicate, got %+v", duplicate)
	}
	exact := 3
	triggered, err := store.ApplyReaction(ctx, core.ReactionChange{UpdateID: 2, ChatID: message.ChatID, MessageID: message.MessageID, Exact: &exact})
	if err != nil {
		t.Fatal(err)
	}
	if !triggered.Queued || triggered.Count != 3 {
		t.Fatalf("expected queued moderation: %+v", triggered)
	}
	exact = 4
	again, err := store.ApplyReaction(ctx, core.ReactionChange{UpdateID: 3, ChatID: message.ChatID, MessageID: message.MessageID, Exact: &exact})
	if err != nil {
		t.Fatal(err)
	}
	if again.Queued {
		t.Fatal("moderation was queued twice")
	}
	jobs, err := store.DueJobs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].AuthorID != message.AuthorID || jobs[0].Dislikes != 3 {
		t.Fatalf("unexpected jobs: %+v", jobs)
	}
}

func TestApplyReactionIgnoresUnknownAndClampsAtZero(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	unknown, err := store.ApplyReaction(ctx, core.ReactionChange{UpdateID: 10, ChatID: -1, MessageID: 1, Delta: 1})
	if err != nil {
		t.Fatal(err)
	}
	if unknown.Known {
		t.Fatal("unknown message was treated as tracked")
	}
	if err := store.TrackMessage(ctx, core.TrackedMessage{ChatID: -1, MessageID: 2, AuthorID: 3, AuthorName: "user"}); err != nil {
		t.Fatal(err)
	}
	result, err := store.ApplyReaction(ctx, core.ReactionChange{UpdateID: 11, ChatID: -1, MessageID: 2, Delta: -1})
	if err != nil {
		t.Fatal(err)
	}
	if result.Count != 0 {
		t.Fatalf("count should be clamped to zero: %+v", result)
	}
}

func TestSettingsPersistAfterReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "persistent.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetThreshold(ctx, -99, 27); err != nil {
		t.Fatal(err)
	}
	if err := store.SetEnabled(ctx, -99, true); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	settings, err := store.Settings(ctx, -99)
	if err != nil {
		t.Fatal(err)
	}
	if !settings.Enabled || settings.Threshold != 27 {
		t.Fatalf("settings did not persist: %+v", settings)
	}
}
