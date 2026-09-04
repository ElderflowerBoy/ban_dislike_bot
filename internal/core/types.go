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
	AuthorIsBot  bool
	Content      string
	DislikeCount int
	Status       string
}

type ReactionChange struct {
	UpdateID     int64
	ChatID       int64
	MessageID    int
	ActorID      int64
	ActorIsBot   bool
	ActorIsAdmin bool
	Automated    bool
	Delta        int
}

type ApplyResult struct {
	Duplicate bool
	Known     bool
	Queued    bool
	Counted   bool
	Count     int
	Required  int
}

type MemberChange struct {
	ChatID        int64
	UserID        int64
	IsBot         bool
	Administrator bool
	Active        bool
	JoinedAt      time.Time
}

type ModerationJob struct {
	ChatID             int64
	MessageID          int
	AuthorID           int64
	AuthorName         string
	NotificationUserID int64
	Content            string
	Dislikes           int
	Attempts           int
	BanDone            bool
	DeleteDone         bool
	NotifyDone         bool
	ProtectAuthor      bool
	NextAttempt        time.Time
}

type SpamSample struct {
	ChatID    int64
	MessageID int
	Content   string
}

type BotRights struct {
	Administrator bool
	CanRestrict   bool
	CanDelete     bool
}
