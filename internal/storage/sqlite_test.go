package storage

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

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

func trustParticipant(t *testing.T, store *Store, chatID, userID int64) {
	t.Helper()
	if err := store.RecordMember(context.Background(), core.MemberChange{ChatID: chatID, UserID: userID, JoinedAt: time.Now().Add(-4 * 24 * time.Hour), Active: true}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < core.MinTrustedMessages; i++ {
		if err := store.TrackMessage(context.Background(), core.TrackedMessage{ChatID: chatID, MessageID: int(userID*10) + i, AuthorID: userID, AuthorName: "voter"}); err != nil {
			t.Fatal(err)
		}
	}
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
	for i := range 10 {
		trustParticipant(t, store, message.ChatID, int64(100+i))
		wg.Add(1)
		go func(updateID int64) {
			defer wg.Done()
			_, err := store.ApplyReaction(ctx, core.ReactionChange{UpdateID: updateID, ChatID: message.ChatID, MessageID: message.MessageID, ActorID: 100 + updateID - 100, Delta: 1})
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
	settings, settingsErr := store.Settings(ctx, message.ChatID)
	if settingsErr != nil {
		t.Fatal(settingsErr)
	}
	if settings.Enabled || settings.Threshold != core.DefaultThreshold {
		t.Fatalf("unexpected defaults: %+v", settings)
	}
	if thresholdErr := store.SetThreshold(ctx, message.ChatID, 3); thresholdErr != nil {
		t.Fatal(thresholdErr)
	}
	if enableErr := store.SetEnabled(ctx, message.ChatID, true); enableErr != nil {
		t.Fatal(enableErr)
	}
	for _, voterID := range []int64{101, 102, 103, 104} {
		trustParticipant(t, store, message.ChatID, voterID)
	}

	first, err := store.ApplyReaction(ctx, core.ReactionChange{UpdateID: 1, ChatID: message.ChatID, MessageID: message.MessageID, ActorID: 101, Delta: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !first.Known || first.Queued || first.Count != 1 {
		t.Fatalf("unexpected first result: %+v", first)
	}
	duplicate, err := store.ApplyReaction(ctx, core.ReactionChange{UpdateID: 1, ChatID: message.ChatID, MessageID: message.MessageID, ActorID: 101, Delta: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !duplicate.Duplicate {
		t.Fatalf("expected duplicate, got %+v", duplicate)
	}
	second, err := store.ApplyReaction(ctx, core.ReactionChange{UpdateID: 2, ChatID: message.ChatID, MessageID: message.MessageID, ActorID: 102, Delta: 1})
	if err != nil {
		t.Fatal(err)
	}
	if second.Queued || second.Count != 2 {
		t.Fatalf("moderation queued too early: %+v", second)
	}
	triggered, err := store.ApplyReaction(ctx, core.ReactionChange{UpdateID: 3, ChatID: message.ChatID, MessageID: message.MessageID, ActorID: 103, Delta: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !triggered.Queued || triggered.Count != 3 {
		t.Fatalf("expected queued moderation: %+v", triggered)
	}
	again, err := store.ApplyReaction(ctx, core.ReactionChange{UpdateID: 4, ChatID: message.ChatID, MessageID: message.MessageID, ActorID: 104, Delta: 1})
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
	if len(jobs) != 1 || jobs[0].AuthorID != message.AuthorID || jobs[0].NotificationUserID != 103 || jobs[0].Dislikes != 3 {
		t.Fatalf("unexpected jobs: %+v", jobs)
	}
}

func TestApplyReactionIgnoresUnknownAndClampsAtZero(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	unknown, applyErr := store.ApplyReaction(ctx, core.ReactionChange{UpdateID: 10, ChatID: -1, MessageID: 1, ActorID: 10, Delta: 1})
	if applyErr != nil {
		t.Fatal(applyErr)
	}
	if unknown.Known {
		t.Fatal("unknown message was treated as tracked")
	}
	if trackErr := store.TrackMessage(ctx, core.TrackedMessage{ChatID: -1, MessageID: 2, AuthorID: 3, AuthorName: "user"}); trackErr != nil {
		t.Fatal(trackErr)
	}
	result, err := store.ApplyReaction(ctx, core.ReactionChange{UpdateID: 11, ChatID: -1, MessageID: 2, ActorID: 10, Delta: -1})
	if err != nil {
		t.Fatal(err)
	}
	if result.Count != 0 {
		t.Fatalf("count should be clamped to zero: %+v", result)
	}
}

func TestNewParticipantsCannotVoteImmediately(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	message := core.TrackedMessage{ChatID: -2, MessageID: 1, AuthorID: 20, AuthorName: "author"}
	if err := store.TrackMessage(ctx, message); err != nil {
		t.Fatal(err)
	}
	if err := store.SetThreshold(ctx, message.ChatID, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.SetEnabled(ctx, message.ChatID, true); err != nil {
		t.Fatal(err)
	}
	result, err := store.ApplyReaction(ctx, core.ReactionChange{UpdateID: 1, ChatID: message.ChatID, MessageID: message.MessageID, ActorID: 21, Delta: 1})
	if err != nil {
		t.Fatal(err)
	}
	if result.Counted || result.Count != 0 || result.Queued {
		t.Fatalf("new participant vote was accepted: %+v", result)
	}
}

func TestBotSpamVoteCountsButCannotModerateAlone(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	message := core.TrackedMessage{ChatID: -4, MessageID: 1, AuthorID: 20, AuthorName: "author", Content: "suspicious message"}
	if err := store.TrackMessage(ctx, message); err != nil {
		t.Fatal(err)
	}
	if err := store.SetThreshold(ctx, message.ChatID, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.SetEnabled(ctx, message.ChatID, true); err != nil {
		t.Fatal(err)
	}

	botVote, err := store.ApplyReaction(ctx, core.ReactionChange{ChatID: message.ChatID, MessageID: message.MessageID, ActorID: 999, ActorIsBot: true, Automated: true, Delta: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !botVote.Counted || botVote.Count != 1 || botVote.Queued {
		t.Fatalf("unexpected bot vote: %+v", botVote)
	}
	if jobs, jobsErr := store.DueJobs(ctx, 10); jobsErr != nil || len(jobs) != 0 {
		t.Fatalf("bot vote queued moderation alone: jobs=%+v err=%v", jobs, jobsErr)
	}

	trustParticipant(t, store, message.ChatID, 21)
	humanVote, err := store.ApplyReaction(ctx, core.ReactionChange{UpdateID: 10, ChatID: message.ChatID, MessageID: message.MessageID, ActorID: 21, Delta: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !humanVote.Queued || humanVote.Count != 2 {
		t.Fatalf("human confirmation did not queue moderation: %+v", humanVote)
	}
	jobs, err := store.DueJobs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].NotificationUserID != 21 || jobs[0].Content != message.Content {
		t.Fatalf("unexpected moderation job: %+v", jobs)
	}
}

func TestBotVoteCompletingThresholdNotifiesHumanVoter(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	message := core.TrackedMessage{ChatID: -6, MessageID: 1, AuthorID: 20, AuthorName: "author", Content: "suspicious message"}
	if err := store.TrackMessage(ctx, message); err != nil {
		t.Fatal(err)
	}
	if err := store.SetThreshold(ctx, message.ChatID, 2); err != nil {
		t.Fatal(err)
	}
	if err := store.SetEnabled(ctx, message.ChatID, true); err != nil {
		t.Fatal(err)
	}
	trustParticipant(t, store, message.ChatID, 21)
	if result, err := store.ApplyReaction(ctx, core.ReactionChange{UpdateID: 11, ChatID: message.ChatID, MessageID: message.MessageID, ActorID: 21, Delta: 1}); err != nil || result.Queued {
		t.Fatalf("unexpected human vote: result=%+v err=%v", result, err)
	}
	result, err := store.ApplyReaction(ctx, core.ReactionChange{ChatID: message.ChatID, MessageID: message.MessageID, ActorID: 999, ActorIsBot: true, Automated: true, Delta: 1})
	if err != nil || !result.Queued {
		t.Fatalf("bot vote did not complete threshold: result=%+v err=%v", result, err)
	}
	jobs, err := store.DueJobs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].NotificationUserID != 21 {
		t.Fatalf("unexpected moderation job: %+v", jobs)
	}
}

func TestSpamSamplesPersistAndDeduplicate(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	sample := core.SpamSample{ChatID: -5, MessageID: 7, Content: "confirmed spam"}
	if err := store.RecordSpamSample(ctx, sample); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSpamSample(ctx, sample); err != nil {
		t.Fatal(err)
	}
	samples, err := store.SpamSamples(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples[0] != sample {
		t.Fatalf("samples = %+v", samples)
	}
}

func TestActiveAuthorGetsProtectedDoubleThreshold(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	chatID := int64(-3)
	if err := store.RecordMember(ctx, core.MemberChange{ChatID: chatID, UserID: 30, JoinedAt: time.Now().Add(-4 * 24 * time.Hour), Active: true}); err != nil {
		t.Fatal(err)
	}
	for messageID := 1; messageID <= core.MinTrustedMessages; messageID++ {
		if err := store.TrackMessage(ctx, core.TrackedMessage{ChatID: chatID, MessageID: messageID, AuthorID: 30, AuthorName: "active"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SetThreshold(ctx, chatID, 3); err != nil {
		t.Fatal(err)
	}
	if err := store.SetEnabled(ctx, chatID, true); err != nil {
		t.Fatal(err)
	}
	for voterID := int64(40); voterID < 46; voterID++ {
		trustParticipant(t, store, chatID, voterID)
		result, err := store.ApplyReaction(ctx, core.ReactionChange{UpdateID: voterID, ChatID: chatID, MessageID: 1, ActorID: voterID, Delta: 1})
		if err != nil {
			t.Fatal(err)
		}
		if voterID < 45 && result.Queued {
			t.Fatalf("job queued before protected threshold: %+v", result)
		}
	}
	jobs, err := store.DueJobs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || !jobs[0].ProtectAuthor || jobs[0].NotificationUserID != 45 || jobs[0].Dislikes != 6 {
		t.Fatalf("unexpected protected job: %+v", jobs)
	}
}

func TestSettingsPersistAfterReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "persistent.db")
	store, openErr := Open(ctx, path)
	if openErr != nil {
		t.Fatal(openErr)
	}
	if thresholdErr := store.SetThreshold(ctx, -99, 27); thresholdErr != nil {
		t.Fatal(thresholdErr)
	}
	if enableErr := store.SetEnabled(ctx, -99, true); enableErr != nil {
		t.Fatal(enableErr)
	}
	if closeErr := store.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	store, openErr = Open(ctx, path)
	if openErr != nil {
		t.Fatal(openErr)
	}
	defer func() { _ = store.Close() }()
	settings, settingsErr := store.Settings(ctx, -99)
	if settingsErr != nil {
		t.Fatal(settingsErr)
	}
	if !settings.Enabled || settings.Threshold != 27 {
		t.Fatalf("settings did not persist: %+v", settings)
	}
}
