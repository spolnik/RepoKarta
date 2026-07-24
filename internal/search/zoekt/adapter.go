// Package zoekt adapts the pinned upstream Zoekt engine to RepoKarta's stable
// search interfaces.
package zoekt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	stdregexp "regexp"
	"regexp/syntax"
	"strings"
	"sync"
	"time"

	grafanaregexp "github.com/grafana/regexp"
	upstream "github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/gitindex"
	"github.com/sourcegraph/zoekt/index"
	"github.com/sourcegraph/zoekt/query"
	zoektsearch "github.com/sourcegraph/zoekt/search"
	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/search"
)

const (
	defaultLimit   = 100
	maximumLimit   = 500
	maximumMatches = 5_000
	searchTimeout  = 15 * time.Second
)

// Revision is the exact upstream Zoekt revision pinned by RepoKarta.
const Revision = "2b2ce2e398e6bee68d67143f567b6c6199340c7f"

// Adapter indexes Git HEADs into RepoKarta-owned Zoekt shards.
type Adapter struct {
	indexDirectory string
	mu             sync.RWMutex
	searcher       upstream.Streamer
}

// New creates a native Zoekt adapter.
func New(indexDirectory string) (*Adapter, error) {
	if err := os.MkdirAll(indexDirectory, 0o755); err != nil {
		return nil, fmt.Errorf("create Zoekt index directory: %w", err)
	}
	return &Adapter{indexDirectory: indexDirectory}, nil
}

// Close releases adapter resources.
func (a *Adapter) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.searcher != nil {
		a.searcher.Close()
		a.searcher = nil
	}
	return nil
}

// Index incrementally indexes the repository's current HEAD.
func (a *Adapter) Index(ctx context.Context, repository catalog.Repository) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.searcher != nil {
		a.searcher.Close()
		a.searcher = nil
	}

	options := gitindex.Options{
		RepoDir:     repository.Path,
		Incremental: true,
		Submodules:  false,
		Branches:    []string{"HEAD"},
		BuildOptions: index.Options{
			IndexDir:            a.indexDirectory,
			ShardPrefixOverride: shardPrefix(repository),
			DisableCTags:        true,
			RepositoryDescription: upstream.Repository{
				Name:   repository.Name,
				Source: repository.Path,
			},
		},
	}
	updated, err := gitindex.IndexGitRepo(options)
	if err != nil {
		return false, fmt.Errorf("Zoekt index %s: %w", repository.Name, err)
	}
	if err := ctx.Err(); err != nil {
		return updated, err
	}
	return updated, nil
}

// Search executes a bounded Zoekt query against all current shards.
func (a *Adapter) Search(ctx context.Context, request search.Query) (search.Result, error) {
	started := time.Now()
	if strings.TrimSpace(request.Text) == "" {
		return search.Result{}, errors.New("search query is required")
	}

	parsed, err := buildQuery(request)
	if err != nil {
		return search.Result{}, err
	}

	limit := request.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maximumLimit {
		limit = maximumLimit
	}

	boundedContext, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()

	searcher, release, err := a.acquireSearcher()
	if err != nil {
		return search.Result{}, fmt.Errorf("open Zoekt shards: %w", err)
	}
	defer release()

	response, err := searcher.Search(boundedContext, parsed, &upstream.SearchOptions{
		TotalMaxMatchCount:   maximumMatches,
		MaxDocDisplayCount:   limit,
		MaxMatchDisplayCount: maximumMatches,
		NumContextLines:      1,
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(boundedContext.Err(), context.DeadlineExceeded) {
			return search.Result{}, fmt.Errorf("search exceeded %s", searchTimeout)
		}
		return search.Result{}, fmt.Errorf("search Zoekt shards: %w", err)
	}

	result := search.Result{
		Duration:  time.Since(started),
		Truncated: len(response.Files) >= limit,
	}
	for _, file := range response.Files {
		match := search.FileMatch{
			Repository: file.Repository,
			Revision:   file.Version,
			Path:       filepath.ToSlash(file.FileName),
			Language:   file.Language,
			Score:      file.Score,
		}
		for _, line := range file.LineMatches {
			lineMatch := search.LineMatch{
				Number: line.LineNumber,
				Text:   trimLineEnding(string(line.Line)),
				Before: trimLineEnding(string(line.Before)),
				After:  trimLineEnding(string(line.After)),
			}
			for _, fragment := range line.LineFragments {
				start := fragment.LineOffset
				end := start + fragment.MatchLength
				if start < 0 || end < start || end > len(lineMatch.Text) {
					continue
				}
				lineMatch.Fragments = append(lineMatch.Fragments, search.Fragment{Start: start, End: end})
			}
			match.Lines = append(match.Lines, lineMatch)
			result.MatchCount += len(line.LineFragments)
		}
		result.Matches = append(result.Matches, match)
	}
	return result, nil
}

func (a *Adapter) acquireSearcher() (upstream.Streamer, func(), error) {
	for {
		a.mu.RLock()
		if a.searcher != nil {
			return a.searcher, a.mu.RUnlock, nil
		}
		a.mu.RUnlock()

		a.mu.Lock()
		if a.searcher == nil {
			searcher, err := zoektsearch.NewDirectorySearcher(a.indexDirectory)
			if err != nil {
				a.mu.Unlock()
				return nil, nil, err
			}
			a.searcher = searcher
		}
		a.mu.Unlock()
	}
}

func buildQuery(request search.Query) (query.Q, error) {
	var children []query.Q
	switch request.Mode {
	case "zoekt":
		parsed, err := query.Parse(request.Text)
		if err != nil {
			return nil, fmt.Errorf("invalid Zoekt query: %w", err)
		}
		children = append(children, parsed)
	case "regex":
		regularExpression, err := syntax.Parse(request.Text, syntax.ClassNL|syntax.PerlX|syntax.UnicodeGroups)
		if err != nil {
			return nil, fmt.Errorf("invalid regular expression: %w", err)
		}
		children = append(children, &query.Regexp{Regexp: regularExpression})
	default:
		children = append(children, &query.Substring{Pattern: request.Text})
	}

	if repository := strings.TrimSpace(request.Repository); repository != "" {
		expression, err := grafanaregexp.Compile(`(?i)(^|[\\/])` + stdregexp.QuoteMeta(repository) + `$`)
		if err != nil {
			return nil, fmt.Errorf("invalid repository filter: %w", err)
		}
		children = append(children, &query.Repo{Regexp: expression})
	}
	if language := strings.TrimSpace(request.Language); language != "" {
		children = append(children, &query.Language{Language: language})
	}
	if path := strings.TrimSpace(request.Path); path != "" {
		children = append(children, &query.Substring{Pattern: filepath.ToSlash(path), FileName: true})
	}
	if file := strings.TrimSpace(request.File); file != "" {
		children = append(children, &query.Substring{Pattern: file, FileName: true})
	}
	return &query.And{Children: children}, nil
}

func shardPrefix(repository catalog.Repository) string {
	return fmt.Sprintf("repo-%d", repository.ID)
}

func trimLineEnding(value string) string {
	return strings.TrimSuffix(strings.TrimSuffix(value, "\n"), "\r")
}
