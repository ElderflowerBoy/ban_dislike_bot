package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ElderflowerBoy/ban_dislike_bot/internal/core"
	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

func Open(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 5000",
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("configure sqlite: %w", err)
		}
	}
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS chat_settings (
  chat_id INTEGER PRIMARY KEY,
  enabled INTEGER NOT NULL DEFAULT 0 CHECK (enabled IN (0, 1)),
  threshold INTEGER NOT NULL DEFAULT 10 CHECK (threshold BETWEEN 1 AND 1000),
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS tracked_messages (
  chat_id INTEGER NOT NULL,
  message_id INTEGER NOT NULL,
  author_id INTEGER NOT NULL,
  author_name TEXT NOT NULL,
  content TEXT NOT NULL DEFAULT '',
  dislike_count INTEGER NOT NULL DEFAULT 0 CHECK (dislike_count >= 0),
  status TEXT NOT NULL DEFAULT 'tracking',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (chat_id, message_id)
);
CREATE TABLE IF NOT EXISTS processed_updates (
  update_id INTEGER PRIMARY KEY,
  processed_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS chat_participants (
  chat_id INTEGER NOT NULL,
  user_id INTEGER NOT NULL,
  first_seen_at INTEGER NOT NULL,
  joined_at INTEGER NOT NULL DEFAULT 0,
  message_count INTEGER NOT NULL DEFAULT 0 CHECK (message_count >= 0),
  is_bot INTEGER NOT NULL DEFAULT 0 CHECK (is_bot IN (0, 1)),
  is_admin INTEGER NOT NULL DEFAULT 0 CHECK (is_admin IN (0, 1)),
  active INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0, 1)),
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (chat_id, user_id)
);
CREATE TABLE IF NOT EXISTS message_votes (
  chat_id INTEGER NOT NULL,
  message_id INTEGER NOT NULL,
  voter_id INTEGER NOT NULL,
  counted INTEGER NOT NULL CHECK (counted IN (0, 1)),
  source TEXT NOT NULL DEFAULT 'human',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (chat_id, message_id, voter_id)
);
CREATE TABLE IF NOT EXISTS moderation_jobs (
  chat_id INTEGER NOT NULL,
  message_id INTEGER NOT NULL,
  author_id INTEGER NOT NULL,
  author_name TEXT NOT NULL,
  notification_user_id INTEGER NOT NULL DEFAULT 0,
  dislikes INTEGER NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  ban_done INTEGER NOT NULL DEFAULT 0,
  delete_done INTEGER NOT NULL DEFAULT 0,
  notify_done INTEGER NOT NULL DEFAULT 0,
  protect_author INTEGER NOT NULL DEFAULT 0 CHECK (protect_author IN (0, 1)),
  state TEXT NOT NULL DEFAULT 'pending',
  next_attempt_at INTEGER NOT NULL,
  last_error TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (chat_id, message_id)
);
CREATE INDEX IF NOT EXISTS idx_jobs_due ON moderation_jobs(state, next_attempt_at);
CREATE TABLE IF NOT EXISTS moderation_samples (
  chat_id INTEGER NOT NULL,
  message_id INTEGER NOT NULL,
  content TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (chat_id, message_id)
);
CREATE TABLE IF NOT EXISTS schema_migrations (
  name TEXT PRIMARY KEY,
  applied_at INTEGER NOT NULL
);
`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("migrate sqlite: %w", err)
	}
	if err := s.ensureColumn(ctx, "moderation_jobs", "protect_author", "INTEGER NOT NULL DEFAULT 0 CHECK (protect_author IN (0, 1))"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "moderation_jobs", "notification_user_id", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "tracked_messages", "content", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "message_votes", "source", "TEXT NOT NULL DEFAULT 'human'"); err != nil {
		return err
	}
	return s.migrateTrustedVotes(ctx)
}

func (s *Store) ensureColumn(ctx context.Context, table, column, definition string) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return fmt.Errorf("inspect %s schema: %w", table, err)
	}
	found := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, dataType string
		var defaultValue any
		if scanErr := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); scanErr != nil {
			_ = rows.Close()
			return fmt.Errorf("scan %s schema: %w", table, scanErr)
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("read %s schema: %w", table, err)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE `+table+` ADD COLUMN `+column+` `+definition); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, column, err)
	}
	return nil
}

func (s *Store) migrateTrustedVotes(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().Unix()
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO schema_migrations(name, applied_at) VALUES ('trusted_votes_v1', ?)`, now)
	if err != nil {
		return fmt.Errorf("record trusted vote migration: %w", err)
	}
	inserted, _ := result.RowsAffected()
	if inserted == 0 {
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tracked_messages SET status='tracking', dislike_count=0, updated_at=? WHERE status='pending' AND EXISTS (SELECT 1 FROM moderation_jobs j WHERE j.chat_id=tracked_messages.chat_id AND j.message_id=tracked_messages.message_id AND j.state='pending' AND j.ban_done=0 AND j.delete_done=0 AND j.notify_done=0)`, now); err != nil {
		return fmt.Errorf("release legacy moderation messages: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM moderation_jobs WHERE state='pending' AND ban_done=0 AND delete_done=0 AND notify_done=0`); err != nil {
		return fmt.Errorf("cancel legacy moderation jobs: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tracked_messages SET dislike_count=0, updated_at=? WHERE status='tracking'`, now); err != nil {
		return fmt.Errorf("reset legacy dislike counts: %w", err)
	}
	return tx.Commit()
}

func (s *Store) TrackMessage(ctx context.Context, message core.TrackedMessage) error {
	now := time.Now().Unix()
	tx, beginErr := s.db.BeginTx(ctx, nil)
	if beginErr != nil {
		return beginErr
	}
	defer func() { _ = tx.Rollback() }()
	if _, execErr := tx.ExecContext(ctx, `INSERT OR IGNORE INTO chat_settings(chat_id, enabled, threshold, updated_at) VALUES (?, 0, ?, ?)`, message.ChatID, core.DefaultThreshold, now); execErr != nil {
		return fmt.Errorf("ensure chat settings: %w", execErr)
	}
	result, execErr := tx.ExecContext(ctx, `INSERT OR IGNORE INTO tracked_messages(chat_id, message_id, author_id, author_name, content, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, message.ChatID, message.MessageID, message.AuthorID, message.AuthorName, message.Content, now, now)
	if execErr != nil {
		return fmt.Errorf("track message: %w", execErr)
	}
	inserted, _ := result.RowsAffected()
	bot := boolInt(message.AuthorIsBot)
	if _, execErr := tx.ExecContext(ctx, `INSERT INTO chat_participants(chat_id, user_id, first_seen_at, message_count, is_bot, active, updated_at) VALUES (?, ?, ?, ?, ?, 1, ?) ON CONFLICT(chat_id, user_id) DO UPDATE SET message_count=chat_participants.message_count+excluded.message_count, is_bot=excluded.is_bot, active=1, updated_at=excluded.updated_at`, message.ChatID, message.AuthorID, now, inserted, bot, now); execErr != nil {
		return fmt.Errorf("track message author: %w", execErr)
	}
	return tx.Commit()
}

func (s *Store) RecordMember(ctx context.Context, change core.MemberChange) error {
	if change.UserID == 0 {
		return nil
	}
	now := time.Now().Unix()
	joinedAt := int64(0)
	if !change.JoinedAt.IsZero() {
		joinedAt = change.JoinedAt.Unix()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO chat_participants(chat_id, user_id, first_seen_at, joined_at, is_bot, is_admin, active, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(chat_id, user_id) DO UPDATE SET joined_at=CASE WHEN excluded.joined_at > 0 THEN excluded.joined_at ELSE chat_participants.joined_at END, is_bot=excluded.is_bot, is_admin=excluded.is_admin, active=excluded.active, updated_at=excluded.updated_at`, change.ChatID, change.UserID, now, joinedAt, boolInt(change.IsBot), boolInt(change.Administrator), boolInt(change.Active), now)
	if err != nil {
		return fmt.Errorf("record chat member: %w", err)
	}
	return nil
}

func (s *Store) Settings(ctx context.Context, chatID int64) (core.Settings, error) {
	var enabled int
	settings := core.Settings{ChatID: chatID}
	err := s.db.QueryRowContext(ctx, `SELECT enabled, threshold FROM chat_settings WHERE chat_id = ?`, chatID).Scan(&enabled, &settings.Threshold)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Settings{ChatID: chatID, Threshold: core.DefaultThreshold}, nil
	}
	if err != nil {
		return core.Settings{}, fmt.Errorf("get settings: %w", err)
	}
	settings.Enabled = enabled != 0
	return settings, nil
}

func (s *Store) SetEnabled(ctx context.Context, chatID int64, enabled bool) error {
	value := 0
	if enabled {
		value = 1
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO chat_settings(chat_id, enabled, threshold, updated_at) VALUES (?, ?, ?, ?) ON CONFLICT(chat_id) DO UPDATE SET enabled=excluded.enabled, updated_at=excluded.updated_at`, chatID, value, core.DefaultThreshold, time.Now().Unix())
	return err
}

func (s *Store) SetThreshold(ctx context.Context, chatID int64, threshold int) error {
	if threshold < core.MinThreshold || threshold > core.MaxThreshold {
		return fmt.Errorf("threshold must be between %d and %d", core.MinThreshold, core.MaxThreshold)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO chat_settings(chat_id, enabled, threshold, updated_at) VALUES (?, 0, ?, ?) ON CONFLICT(chat_id) DO UPDATE SET threshold=excluded.threshold, updated_at=excluded.updated_at`, chatID, threshold, time.Now().Unix())
	return err
}

func (s *Store) ApplyReaction(ctx context.Context, change core.ReactionChange) (core.ApplyResult, error) {
	if change.ActorID == 0 {
		return core.ApplyResult{}, nil
	}
	tx, beginErr := s.db.BeginTx(ctx, nil)
	if beginErr != nil {
		return core.ApplyResult{}, beginErr
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().Unix()
	if change.UpdateID != 0 {
		result, execErr := tx.ExecContext(ctx, `INSERT OR IGNORE INTO processed_updates(update_id, processed_at) VALUES (?, ?)`, change.UpdateID, now)
		if execErr != nil {
			return core.ApplyResult{}, fmt.Errorf("record update: %w", execErr)
		}
		inserted, _ := result.RowsAffected()
		if inserted == 0 {
			return core.ApplyResult{Duplicate: true}, tx.Commit()
		}
	}

	var message core.TrackedMessage
	queryErr := tx.QueryRowContext(ctx, `SELECT author_id, author_name, content, status FROM tracked_messages WHERE chat_id=? AND message_id=?`, change.ChatID, change.MessageID).Scan(&message.AuthorID, &message.AuthorName, &message.Content, &message.Status)
	if errors.Is(queryErr, sql.ErrNoRows) {
		return core.ApplyResult{}, tx.Commit()
	}
	if queryErr != nil {
		return core.ApplyResult{}, fmt.Errorf("read tracked message: %w", queryErr)
	}
	message.ChatID, message.MessageID = change.ChatID, change.MessageID
	if _, err := tx.ExecContext(ctx, `INSERT INTO chat_participants(chat_id, user_id, first_seen_at, is_bot, is_admin, active, updated_at) VALUES (?, ?, ?, ?, ?, 1, ?) ON CONFLICT(chat_id, user_id) DO UPDATE SET is_bot=excluded.is_bot, is_admin=MAX(chat_participants.is_admin, excluded.is_admin), active=1, updated_at=excluded.updated_at`, change.ChatID, change.ActorID, now, boolInt(change.ActorIsBot), boolInt(change.ActorIsAdmin), now); err != nil {
		return core.ApplyResult{}, fmt.Errorf("ensure reaction voter: %w", err)
	}

	counted := false
	if change.Delta > 0 {
		if change.Automated {
			counted = true
		} else {
			trusted, trustErr := trustedVoter(tx, change.ChatID, change.ActorID, message.AuthorID, now)
			if trustErr != nil {
				return core.ApplyResult{}, fmt.Errorf("check voter reputation: %w", trustErr)
			}
			counted = trusted
		}
		source := "human"
		if change.Automated {
			source = "bot"
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO message_votes(chat_id, message_id, voter_id, counted, source, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, change.ChatID, change.MessageID, change.ActorID, boolInt(counted), source, now, now); err != nil {
			return core.ApplyResult{}, fmt.Errorf("record reaction vote: %w", err)
		}
	} else if change.Delta < 0 {
		var previous int
		err := tx.QueryRowContext(ctx, `SELECT counted FROM message_votes WHERE chat_id=? AND message_id=? AND voter_id=?`, change.ChatID, change.MessageID, change.ActorID).Scan(&previous)
		if errors.Is(err, sql.ErrNoRows) {
			previous = 0
		} else if err != nil {
			return core.ApplyResult{}, fmt.Errorf("read reaction vote: %w", err)
		}
		counted = previous != 0
		if _, err := tx.ExecContext(ctx, `DELETE FROM message_votes WHERE chat_id=? AND message_id=? AND voter_id=?`, change.ChatID, change.MessageID, change.ActorID); err != nil {
			return core.ApplyResult{}, fmt.Errorf("remove reaction vote: %w", err)
		}
	}

	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM message_votes WHERE chat_id=? AND message_id=? AND counted=1`, change.ChatID, change.MessageID).Scan(&count); err != nil {
		return core.ApplyResult{}, fmt.Errorf("count trusted votes: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tracked_messages SET dislike_count=?, updated_at=? WHERE chat_id=? AND message_id=?`, count, now, change.ChatID, change.MessageID); err != nil {
		return core.ApplyResult{}, fmt.Errorf("update dislike count: %w", err)
	}
	var humanCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM message_votes WHERE chat_id=? AND message_id=? AND counted=1 AND source='human'`, change.ChatID, change.MessageID).Scan(&humanCount); err != nil {
		return core.ApplyResult{}, fmt.Errorf("count trusted human votes: %w", err)
	}

	var enabled int
	threshold := core.DefaultThreshold
	settingsErr := tx.QueryRowContext(ctx, `SELECT enabled, threshold FROM chat_settings WHERE chat_id=?`, change.ChatID).Scan(&enabled, &threshold)
	if settingsErr != nil && !errors.Is(settingsErr, sql.ErrNoRows) {
		return core.ApplyResult{}, fmt.Errorf("read settings: %w", settingsErr)
	}
	protected, err := trustedParticipant(tx, change.ChatID, message.AuthorID, now)
	if err != nil {
		return core.ApplyResult{}, fmt.Errorf("check author reputation: %w", err)
	}
	required := threshold
	if protected {
		required *= core.ProtectedAuthorMultiplier
	}
	queued := false
	if enabled != 0 && count >= required && humanCount > 0 && message.Status == "tracking" {
		res, reserveErr := tx.ExecContext(ctx, `UPDATE tracked_messages SET status='pending', updated_at=? WHERE chat_id=? AND message_id=? AND status='tracking'`, now, change.ChatID, change.MessageID)
		if reserveErr != nil {
			return core.ApplyResult{}, fmt.Errorf("reserve moderation: %w", reserveErr)
		}
		changed, _ := res.RowsAffected()
		if changed == 1 {
			notificationUserID := change.ActorID
			if change.Automated {
				if err := tx.QueryRowContext(ctx, `SELECT voter_id FROM message_votes WHERE chat_id=? AND message_id=? AND counted=1 AND source='human' ORDER BY updated_at DESC, voter_id DESC LIMIT 1`, change.ChatID, change.MessageID).Scan(&notificationUserID); err != nil {
					return core.ApplyResult{}, fmt.Errorf("select notification recipient: %w", err)
				}
			}
			_, queueErr := tx.ExecContext(ctx, `INSERT INTO moderation_jobs(chat_id, message_id, author_id, author_name, notification_user_id, dislikes, protect_author, next_attempt_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, change.ChatID, change.MessageID, message.AuthorID, message.AuthorName, notificationUserID, count, boolInt(protected), now, now, now)
			if queueErr != nil {
				return core.ApplyResult{}, fmt.Errorf("queue moderation: %w", queueErr)
			}
			queued = true
		}
	}
	if err := tx.Commit(); err != nil {
		return core.ApplyResult{}, err
	}
	return core.ApplyResult{Known: true, Queued: queued, Counted: counted, Count: count, Required: required}, nil
}

func trustedVoter(tx *sql.Tx, chatID, voterID, authorID int64, now int64) (bool, error) {
	if voterID == authorID {
		return false, nil
	}
	return trustedParticipant(tx, chatID, voterID, now)
}

func trustedParticipant(tx *sql.Tx, chatID, userID, now int64) (bool, error) {
	var firstSeen, joined int64
	var messages, bot, admin, active int
	err := tx.QueryRow(`SELECT first_seen_at, joined_at, message_count, is_bot, is_admin, active FROM chat_participants WHERE chat_id=? AND user_id=?`, chatID, userID).Scan(&firstSeen, &joined, &messages, &bot, &admin, &active)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if bot != 0 || active == 0 {
		return false, nil
	}
	if admin != 0 {
		return true, nil
	}
	if messages < core.MinTrustedMessages {
		return false, nil
	}
	if joined > 0 {
		firstSeen = joined
	}
	return firstSeen <= now-int64(core.NewMemberPeriod/time.Second), nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (s *Store) DueJobs(ctx context.Context, limit int) ([]core.ModerationJob, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT j.chat_id, j.message_id, j.author_id, j.author_name, j.notification_user_id, m.content, j.dislikes, j.attempts, j.ban_done, j.delete_done, j.notify_done, j.protect_author, j.next_attempt_at FROM moderation_jobs j JOIN tracked_messages m ON m.chat_id=j.chat_id AND m.message_id=j.message_id WHERE j.state='pending' AND j.next_attempt_at <= ? ORDER BY j.next_attempt_at LIMIT ?`, time.Now().Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var jobs []core.ModerationJob
	for rows.Next() {
		var job core.ModerationJob
		var banDone, deleteDone, notifyDone, protectAuthor int
		var next int64
		if err := rows.Scan(&job.ChatID, &job.MessageID, &job.AuthorID, &job.AuthorName, &job.NotificationUserID, &job.Content, &job.Dislikes, &job.Attempts, &banDone, &deleteDone, &notifyDone, &protectAuthor, &next); err != nil {
			return nil, err
		}
		job.BanDone, job.DeleteDone, job.NotifyDone, job.ProtectAuthor = banDone != 0, deleteDone != 0, notifyDone != 0, protectAuthor != 0
		job.NextAttempt = time.Unix(next, 0)
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (s *Store) RecordSpamSample(ctx context.Context, sample core.SpamSample) error {
	if sample.Content == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO moderation_samples(chat_id, message_id, content, created_at) VALUES (?, ?, ?, ?)`, sample.ChatID, sample.MessageID, sample.Content, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("record spam sample: %w", err)
	}
	return nil
}

func (s *Store) SpamSamples(ctx context.Context) ([]core.SpamSample, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT chat_id, message_id, content FROM moderation_samples ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("read spam samples: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var samples []core.SpamSample
	for rows.Next() {
		var sample core.SpamSample
		if err := rows.Scan(&sample.ChatID, &sample.MessageID, &sample.Content); err != nil {
			return nil, err
		}
		samples = append(samples, sample)
	}
	return samples, rows.Err()
}

func (s *Store) MarkStep(ctx context.Context, chatID int64, messageID int, step string) error {
	columns := map[string]string{"ban": "ban_done", "delete": "delete_done", "notify": "notify_done"}
	column, ok := columns[step]
	if !ok {
		return fmt.Errorf("unknown moderation step %q", step)
	}
	_, err := s.db.ExecContext(ctx, `UPDATE moderation_jobs SET `+column+`=1, updated_at=? WHERE chat_id=? AND message_id=?`, time.Now().Unix(), chatID, messageID)
	return err
}

func (s *Store) FinishJob(ctx context.Context, chatID int64, messageID int, state string) error {
	if state != "completed" && state != "exempt" && state != "failed" {
		return fmt.Errorf("invalid final state %q", state)
	}
	tx, beginErr := s.db.BeginTx(ctx, nil)
	if beginErr != nil {
		return beginErr
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE moderation_jobs SET state=?, updated_at=? WHERE chat_id=? AND message_id=?`, state, time.Now().Unix(), chatID, messageID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tracked_messages SET status=?, updated_at=? WHERE chat_id=? AND message_id=?`, state, time.Now().Unix(), chatID, messageID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RetryJob(ctx context.Context, job core.ModerationJob, cause error, delay time.Duration) error {
	_, err := s.db.ExecContext(ctx, `UPDATE moderation_jobs SET attempts=attempts+1, next_attempt_at=?, last_error=?, updated_at=? WHERE chat_id=? AND message_id=?`, time.Now().Add(delay).Unix(), cause.Error(), time.Now().Unix(), job.ChatID, job.MessageID)
	return err
}

func (s *Store) FailJob(ctx context.Context, job core.ModerationJob, cause error) error {
	_, err := s.db.ExecContext(ctx, `UPDATE moderation_jobs SET attempts=attempts+1, last_error=?, updated_at=? WHERE chat_id=? AND message_id=?`, cause.Error(), time.Now().Unix(), job.ChatID, job.MessageID)
	return err
}
