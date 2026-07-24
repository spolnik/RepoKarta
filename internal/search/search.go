// Package search defines RepoKarta-owned search and indexing interfaces.
//
// Zoekt deliberately does not promise a stable v1 API, so the rest of
// RepoKarta must depend on these types rather than upstream implementation
// details.
package search

import (
	"context"
	"time"

	"github.com/spolnik/RepoKarta/internal/catalog"
)

// Query is a bounded code-search request.
type Query struct {
	Text       string
	Repository string
	Language   string
	Path       string
	File       string
	Mode       string
	Limit      int
}

// Result contains source matches tied to exact repository revisions.
type Result struct {
	Matches         []FileMatch
	MatchCount      int
	FileCount       int
	EstimatedFiles  int
	ReturnedFiles   int
	FilesSkipped    int
	ShardsSkipped   int
	Limit           int
	Duration        time.Duration
	Truncated       bool
	TotalFilesExact bool
	Warnings        []Warning
}

// Warning makes a search capability or completeness limitation explicit.
type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// FileMatch is one matched source file.
type FileMatch struct {
	Repository string
	Revision   string
	Path       string
	Language   string
	Score      float64
	Lines      []LineMatch
}

// LineMatch is one cited line and its exact matching byte ranges.
type LineMatch struct {
	Number    int
	Text      string
	Before    string
	After     string
	Fragments []Fragment
}

// Fragment is a half-open byte range within LineMatch.Text.
type Fragment struct {
	Start int
	End   int
}

// Engine hides the concrete search implementation from RepoKarta services.
type Engine interface {
	Index(context.Context, catalog.Repository) (updated bool, err error)
	Search(context.Context, Query) (Result, error)
	Close() error
}
