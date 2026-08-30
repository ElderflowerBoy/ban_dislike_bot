package core

import "time"

const (
	DefaultThreshold = 10
	MinThreshold     = 1
	MaxThreshold     = 1000
)

type Settings struct {
	ChatID    int64
	Enabled   bool
	Threshold int
}

type TrackedMessage struct {
	ChatID       int64
	MessageID    int
	AuthorID     int64
	AuthorName   string
	DislikeCount int
	Status       string
}

type ReactionChange struct {
	UpdateID  int64
	ChatID    int64
	MessageID int
	Delta     int
	Exact     *int
}

type ApplyResult struct {
	Duplicate bool
	Known     bool
	Queued    bool
	Count     int
}

type ModerationJob struct {
	ChatID      int64
	MessageID   int
	AuthorID    int64
	AuthorName  string
	Dislikes    int
	Attempts    int
	BanDone     bool
	DeleteDone  bool
	NotifyDone  bool
	NextAttempt time.Time
}

type BotRights struct {
	Administrator bool
	CanRestrict   bool
	CanDelete     bool
}
