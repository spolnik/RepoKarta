// Package searchworkspace defines durable records for saved searches,
// per-author history, and bounded deterministic monitor snapshots.
package searchworkspace

import (
	"errors"
	"time"
)

var (
	ErrNotFound  = errors.New("search workspace record not found")
	ErrForbidden = errors.New("search workspace record is not editable")
	ErrConflict  = errors.New("search workspace record already exists")
)

type RecentRecord struct {
	ID          int64
	AuthorID    string
	RequestJSON string
	ResultCount int
	ExecutedAt  time.Time
}

type SavedRecord struct {
	ID             string
	AuthorID       string
	Title          string
	Description    string
	Visibility     string
	Managed        bool
	RevisionPolicy string
	RequestJSON    string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type MonitorRecord struct {
	ID            string
	SavedSearchID string
	AuthorID      string
	Enabled       bool
	HistoryLimit  int
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type RunRecord struct {
	ID                 int64
	MonitorID          string
	RevisionKey        string
	ResultKeysJSON     string
	AddedJSON          string
	RemovedJSON        string
	MatchCount         int
	Status             string
	NotificationStatus string
	Error              string
	CreatedAt          time.Time
}
