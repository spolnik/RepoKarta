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

const (
	ScopeKindRepository = "repository"
	ScopeKindFile       = "file"
	ScopeKindDirectory  = "directory"
	ScopeKindSymbol     = "symbol"
)

// Query is a bounded code-search request.
type Query struct {
	Text        string
	IncludeText []string
	ExcludeText []string
	Repository  string
	// Repositories is an internal exact allow-list of canonical repository
	// identities. External query syntax never populates it.
	Repositories []string
	// RepositoryIDs is the collision-safe internal allow-list stored in Zoekt
	// shard metadata.
	RepositoryIDs []uint32
	// ExcludeRepositoryIDs is an internal deny-list resolved after access
	// control and never accepts caller-supplied shard identities directly.
	ExcludeRepositoryIDs []uint32
	// Scopes is an internal union of exact repository and optional file
	// identities resolved from structured contexts. External query syntax never
	// populates it.
	Scopes           []Scope
	Language         string
	Languages        []string
	ExcludeLanguages []string
	Path             string
	Paths            []string
	ExcludePaths     []string
	File             string
	Files            []string
	ExcludeFiles     []string
	FileNameOnly     bool
	Mode             string
	Limit            int
}

// Scope is one exact repository identity and an optional exact committed path
// or source declaration range.
type Scope struct {
	RepositoryID uint32
	Repository   string
	Kind         string
	Path         string
	Symbol       string
	StartLine    int
	EndLine      int
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
	RepositoryID int64
	Repository   string
	Revision     string
	Path         string
	Language     string
	Score        float64
	Lines        []LineMatch
}

// LineMatch is one cited line and its exact matching byte ranges.
type LineMatch struct {
	Number              int
	Text                string
	Before              string
	After               string
	Fragments           []Fragment
	ReferenceKind       string
	ReferenceTarget     string
	ReferenceReceiver   string
	ReferenceConfidence string
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

// ArtifactGarbageCollector is implemented by engines that own repository-keyed
// derived files. Catalogue refresh invokes it only after durable sync succeeds.
type ArtifactGarbageCollector interface {
	PruneRepositories(context.Context, map[int64]struct{}) error
}
