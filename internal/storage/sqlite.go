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
CREATE TABLE IF NOT EXISTS moderation_jobs (
  chat_id INTEGER NOT NULL,
  message_id INTEGER NOT NULL,
  author_id INTEGER NOT NULL,
  author_name TEXT NOT NULL,
  dislikes INTEGER NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  ban_done INTEGER NOT NULL DEFAULT 0,
  delete_done INTEGER NOT NULL DEFAULT 0,
  notify_done INTEGER NOT NULL DEFAULT 0,
  state TEXT NOT NULL DEFAULT 'pending',
  next_attempt_at INTEGER NOT NULL,
  last_error TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (chat_id, message_id)
);
CREATE INDEX IF NOT EXISTS idx_jobs_due ON moderation_jobs(state, next_attempt_at);
`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("migrate sqlite: %w", err)
	}
	return nil
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
	_, execErr := tx.ExecContext(ctx, `INSERT OR IGNORE INTO tracked_messages(chat_id, message_id, author_id, author_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`, message.ChatID, message.MessageID, message.AuthorID, message.AuthorName, now, now)
	if execErr != nil {
		return fmt.Errorf("track message: %w", execErr)
	}
	return tx.Commit()
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
	tx, beginErr := s.db.BeginTx(ctx, nil)
	if beginErr != nil {
		return core.ApplyResult{}, beginErr
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().Unix()
	result, execErr := tx.ExecContext(ctx, `INSERT OR IGNORE INTO processed_updates(update_id, processed_at) VALUES (?, ?)`, change.UpdateID, now)
	if execErr != nil {
		return core.ApplyResult{}, fmt.Errorf("record update: %w", execErr)
	}
	inserted, _ := result.RowsAffected()
	if inserted == 0 {
		return core.ApplyResult{Duplicate: true}, tx.Commit()
	}

	var message core.TrackedMessage
	queryErr := tx.QueryRowContext(ctx, `SELECT author_id, author_name, dislike_count, status FROM tracked_messages WHERE chat_id=? AND message_id=?`, change.ChatID, change.MessageID).Scan(&message.AuthorID, &message.AuthorName, &message.DislikeCount, &message.Status)
	if errors.Is(queryErr, sql.ErrNoRows) {
		return core.ApplyResult{}, tx.Commit()
	}
	if queryErr != nil {
		return core.ApplyResult{}, fmt.Errorf("read tracked message: %w", queryErr)
	}
	message.ChatID, message.MessageID = change.ChatID, change.MessageID
	count := message.DislikeCount + change.Delta
	if change.Exact != nil {
		count = *change.Exact
	}
	if count < 0 {
		count = 0
	}
	if _, updateErr := tx.ExecContext(ctx, `UPDATE tracked_messages SET dislike_count=?, updated_at=? WHERE chat_id=? AND message_id=?`, count, now, change.ChatID, change.MessageID); updateErr != nil {
		return core.ApplyResult{}, fmt.Errorf("update dislike count: %w", updateErr)
	}

	var enabled int
	threshold := core.DefaultThreshold
	settingsErr := tx.QueryRowContext(ctx, `SELECT enabled, threshold FROM chat_settings WHERE chat_id=?`, change.ChatID).Scan(&enabled, &threshold)
	if settingsErr != nil && !errors.Is(settingsErr, sql.ErrNoRows) {
		return core.ApplyResult{}, fmt.Errorf("read settings: %w", settingsErr)
	}
	queued := false
	if enabled != 0 && count >= threshold && message.Status == "tracking" {
		res, reserveErr := tx.ExecContext(ctx, `UPDATE tracked_messages SET status='pending', updated_at=? WHERE chat_id=? AND message_id=? AND status='tracking'`, now, change.ChatID, change.MessageID)
		if reserveErr != nil {
			return core.ApplyResult{}, fmt.Errorf("reserve moderation: %w", reserveErr)
		}
		changed, _ := res.RowsAffected()
		if changed == 1 {
			_, queueErr := tx.ExecContext(ctx, `INSERT INTO moderation_jobs(chat_id, message_id, author_id, author_name, dislikes, next_attempt_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, change.ChatID, change.MessageID, message.AuthorID, message.AuthorName, count, now, now, now)
			if queueErr != nil {
				return core.ApplyResult{}, fmt.Errorf("queue moderation: %w", queueErr)
			}
			queued = true
		}
	}
	if err := tx.Commit(); err != nil {
		return core.ApplyResult{}, err
	}
	return core.ApplyResult{Known: true, Queued: queued, Count: count}, nil
}

func (s *Store) DueJobs(ctx context.Context, limit int) ([]core.ModerationJob, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT chat_id, message_id, author_id, author_name, dislikes, attempts, ban_done, delete_done, notify_done, next_attempt_at FROM moderation_jobs WHERE state='pending' AND next_attempt_at <= ? ORDER BY next_attempt_at LIMIT ?`, time.Now().Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var jobs []core.ModerationJob
	for rows.Next() {
		var job core.ModerationJob
		var banDone, deleteDone, notifyDone int
		var next int64
		if err := rows.Scan(&job.ChatID, &job.MessageID, &job.AuthorID, &job.AuthorName, &job.Dislikes, &job.Attempts, &banDone, &deleteDone, &notifyDone, &next); err != nil {
			return nil, err
		}
		job.BanDone, job.DeleteDone, job.NotifyDone = banDone != 0, deleteDone != 0, notifyDone != 0
		job.NextAttempt = time.Unix(next, 0)
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
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
