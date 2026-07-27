// Package zoekt adapts the pinned upstream Zoekt engine to RepoKarta's stable
// search interfaces.
package zoekt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	stdregexp "regexp"
	"regexp/syntax"
	"strconv"
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
	ctagsPath      string
	symbolsEnabled bool
	mu             sync.RWMutex
	searcher       upstream.Streamer
}

// New creates a native Zoekt adapter.
func New(indexDirectory string) (*Adapter, error) {
	if err := os.MkdirAll(indexDirectory, 0o755); err != nil {
		return nil, fmt.Errorf("create Zoekt index directory: %w", err)
	}
	ctagsPath := discoverCTags()
	return &Adapter{
		indexDirectory: indexDirectory,
		ctagsPath:      ctagsPath,
		symbolsEnabled: ctagsPath != "",
	}, nil
}

func discoverCTags() string {
	return discoverCTagsWith(
		strings.TrimSpace(os.Getenv("CTAGS_COMMAND")),
		exec.LookPath,
		isUniversalCTags,
	)
}

func discoverCTagsWith(configured string, lookPath func(string) (string, error), verify func(string) bool) string {
	candidates := []string{configured, "universal-ctags", "ctags"}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if resolved, err := lookPath(candidate); err == nil && verify(resolved) {
			return resolved
		}
	}
	return ""
}

func isUniversalCTags(command string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, command, "--version").CombinedOutput()
	return err == nil && isUniversalCTagsVersion(output)
}

func isUniversalCTagsVersion(output []byte) bool {
	return strings.Contains(strings.ToLower(string(output)), "universal ctags")
}

// IndexConfiguration changes when existing shards must be rebuilt.
func (a *Adapter) IndexConfiguration() string {
	if !a.symbolsEnabled {
		return "zoekt-" + Revision + ";repository-identity=id-v2;symbols=disabled"
	}
	return "zoekt-" + Revision + ";repository-identity=id-v2;symbols=universal-ctags"
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
	if repository.ID <= 0 || repository.ID > int64(1<<32-1) {
		return false, fmt.Errorf("repository ID %d cannot be represented in the Zoekt index", repository.ID)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.searcher != nil {
		a.searcher.Close()
		a.searcher = nil
	}

	shadowPath, err := a.prepareGitShadow(ctx, repository)
	if err != nil {
		return false, fmt.Errorf("prepare collision-safe Git shadow for %s: %w", repository.Name, err)
	}
	options := gitindex.Options{
		RepoDir:     shadowPath,
		Incremental: true,
		Submodules:  false,
		Branches:    []string{"HEAD"},
		BuildOptions: index.Options{
			IndexDir:            a.indexDirectory,
			ShardPrefixOverride: shardPrefix(repository),
			DisableCTags:        !a.symbolsEnabled,
			CTagsPath:           a.ctagsPath,
			RepositoryDescription: upstream.Repository{
				// Zoekt requires a unique repository identity. Display names
				// are not unique across roots, so using one here could let a
				// same-named private clone satisfy another repository's
				// authorization filter. Codeintel resolves this canonical
				// path back to the public display name before responding.
				ID:     uint32(repository.ID),
				Name:   filepath.ToSlash(repository.Path),
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
		Duration:        time.Since(started),
		MatchCount:      response.Stats.MatchCount,
		FileCount:       response.Stats.FileCount,
		EstimatedFiles:  response.Stats.FileCount + response.Stats.FilesSkipped,
		ReturnedFiles:   len(response.Files),
		FilesSkipped:    response.Stats.FilesSkipped,
		ShardsSkipped:   response.Stats.ShardsSkipped,
		Limit:           limit,
		TotalFilesExact: response.Stats.FilesSkipped == 0 && response.Stats.ShardsSkipped == 0 && response.Stats.Crashes == 0,
	}
	result.Truncated = result.ReturnedFiles < result.FileCount ||
		result.FilesSkipped > 0 ||
		result.ShardsSkipped > 0
	if containsSymbolQuery(parsed) && !a.symbolsEnabled {
		result.Warnings = append(result.Warnings, search.Warning{
			Code:    "symbol_index_disabled",
			Message: "Symbol search is unavailable because universal-ctags was not found when RepoKarta started.",
		})
	}
	for _, file := range response.Files {
		match := search.FileMatch{
			RepositoryID: int64(file.RepositoryID),
			Repository:   file.Repository,
			Revision:     file.Version,
			Path:         filepath.ToSlash(file.FileName),
			Language:     file.Language,
			Score:        file.Score,
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
	if request.Text != "" {
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
			children = append(children, &query.Regexp{
				Regexp:   regularExpression,
				FileName: request.FileNameOnly,
			})
		default:
			children = append(children, &query.Substring{
				Pattern:  request.Text,
				FileName: request.FileNameOnly,
			})
		}
	}
	if textQuery := stringAlternatives(request.IncludeText, func(value string) query.Q {
		return &query.Substring{Pattern: value}
	}); textQuery != nil {
		children = append(children, textQuery)
	}
	for _, text := range request.ExcludeText {
		children = append(children, &query.Not{Child: &query.Substring{Pattern: text}})
	}
	if len(children) == 0 {
		children = append(children, &query.Const{Value: true})
	}

	if repository := strings.TrimSpace(request.Repository); repository != "" {
		expression, err := grafanaregexp.Compile(`(?i)(^|[\\/])` + stdregexp.QuoteMeta(repository) + `$`)
		if err != nil {
			return nil, fmt.Errorf("invalid repository filter: %w", err)
		}
		children = append(children, &query.Repo{Regexp: expression})
	}
	if len(request.Repositories) > 0 {
		alternatives := make([]string, 0, len(request.Repositories))
		for _, repository := range request.Repositories {
			repository = strings.TrimSpace(filepath.ToSlash(repository))
			if repository != "" {
				alternatives = append(alternatives, stdregexp.QuoteMeta(repository))
			}
		}
		if len(alternatives) == 0 {
			return nil, errors.New("repository allow-list is empty")
		}
		expression, err := grafanaregexp.Compile(`(?i)^(?:` + strings.Join(alternatives, "|") + `)$`)
		if err != nil {
			return nil, fmt.Errorf("invalid repository allow-list: %w", err)
		}
		children = append(children, &query.Repo{Regexp: expression})
	}
	if len(request.RepositoryIDs) > 0 {
		children = append(children, query.NewRepoIDs(request.RepositoryIDs...))
	}
	if len(request.ExcludeRepositoryIDs) > 0 {
		children = append(children, &query.Not{Child: query.NewRepoIDs(request.ExcludeRepositoryIDs...)})
	}
	if len(request.Scopes) > 0 {
		scopeQueries := make([]query.Q, 0, len(request.Scopes))
		for _, scope := range request.Scopes {
			repository := strings.TrimSpace(filepath.ToSlash(scope.Repository))
			if scope.RepositoryID == 0 && repository == "" {
				return nil, errors.New("structured search scope has no repository identity")
			}
			var repositoryQuery query.Q
			if scope.RepositoryID > 0 {
				repositoryQuery = query.NewRepoIDs(scope.RepositoryID)
			} else {
				expression, err := grafanaregexp.Compile(`(?i)^` + stdregexp.QuoteMeta(repository) + `$`)
				if err != nil {
					return nil, fmt.Errorf("invalid structured repository scope: %w", err)
				}
				repositoryQuery = &query.Repo{Regexp: expression}
			}
			scopeChildren := []query.Q{repositoryQuery}
			if filePath := strings.TrimSpace(filepath.ToSlash(scope.Path)); filePath != "" {
				var fileQuery query.Q
				var err error
				if scope.Kind == search.ScopeKindDirectory {
					fileQuery, err = exactDirectoryQuery(filePath)
				} else {
					fileQuery, err = exactFileNameQuery(filePath)
				}
				if err != nil {
					return nil, err
				}
				scopeChildren = append(scopeChildren, fileQuery)
			}
			scopeQueries = append(scopeQueries, &query.And{Children: scopeChildren})
		}
		children = append(children, &query.Or{Children: scopeQueries})
	}
	if language := strings.TrimSpace(request.Language); language != "" {
		children = append(children, &query.Language{Language: language})
	}
	if languageQuery := stringAlternatives(request.Languages, func(value string) query.Q {
		return &query.Language{Language: value}
	}); languageQuery != nil {
		children = append(children, languageQuery)
	}
	for _, language := range compactStrings(request.ExcludeLanguages) {
		children = append(children, &query.Not{Child: &query.Language{Language: language}})
	}
	if path := strings.TrimSpace(request.Path); path != "" {
		children = append(children, &query.Substring{Pattern: filepath.ToSlash(path), FileName: true})
	}
	if pathQuery := stringAlternatives(request.Paths, func(value string) query.Q {
		return &query.Substring{Pattern: filepath.ToSlash(value), FileName: true}
	}); pathQuery != nil {
		children = append(children, pathQuery)
	}
	for _, path := range compactStrings(request.ExcludePaths) {
		children = append(children, &query.Not{Child: &query.Substring{
			Pattern: filepath.ToSlash(path), FileName: true,
		}})
	}
	if file := strings.TrimSpace(request.File); file != "" {
		children = append(children, &query.Substring{Pattern: file, FileName: true})
	}
	if fileQuery := stringAlternatives(request.Files, func(value string) query.Q {
		return &query.Substring{Pattern: value, FileName: true}
	}); fileQuery != nil {
		children = append(children, fileQuery)
	}
	for _, file := range compactStrings(request.ExcludeFiles) {
		children = append(children, &query.Not{Child: &query.Substring{Pattern: file, FileName: true}})
	}
	return &query.And{Children: children}, nil
}

func stringAlternatives(values []string, build func(string) query.Q) query.Q {
	values = compactStrings(values)
	if len(values) == 0 {
		return nil
	}
	if len(values) == 1 {
		return build(values[0])
	}
	children := make([]query.Q, 0, len(values))
	for _, value := range values {
		children = append(children, build(value))
	}
	return &query.Or{Children: children}
}

func compactStrings(values []string) []string {
	output := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		output = append(output, value)
	}
	return output
}

func exactDirectoryQuery(directory string) (query.Q, error) {
	parts := strings.Split(strings.TrimSuffix(filepath.ToSlash(directory), "/"), "/")
	escaped := make([]string, 0, len(parts))
	for _, part := range parts {
		escaped = append(escaped, stdregexp.QuoteMeta(part))
	}
	expression, err := syntax.Parse(
		`^`+strings.Join(escaped, `[/\\]`)+`[/\\]`,
		syntax.ClassNL|syntax.PerlX|syntax.UnicodeGroups,
	)
	if err != nil {
		return nil, fmt.Errorf("invalid structured directory scope: %w", err)
	}
	return &query.Regexp{Regexp: expression, FileName: true, CaseSensitive: true}, nil
}

func exactFileNameQuery(filePath string) (query.Q, error) {
	parts := strings.Split(filepath.ToSlash(filePath), "/")
	escaped := make([]string, 0, len(parts))
	for _, part := range parts {
		escaped = append(escaped, stdregexp.QuoteMeta(part))
	}
	expression, err := syntax.Parse(
		`^`+strings.Join(escaped, `[/\\]`)+`$`,
		syntax.ClassNL|syntax.PerlX|syntax.UnicodeGroups,
	)
	if err != nil {
		return nil, fmt.Errorf("invalid structured file scope: %w", err)
	}
	return &query.Regexp{Regexp: expression, FileName: true, CaseSensitive: true}, nil
}

func shardPrefix(repository catalog.Repository) string {
	return fmt.Sprintf("repo-%d", repository.ID)
}

func trimLineEnding(value string) string {
	return strings.TrimSuffix(strings.TrimSuffix(value, "\n"), "\r")
}

func containsSymbolQuery(parsed query.Q) bool {
	found := false
	query.VisitAtoms(parsed, func(atom query.Q) {
		if _, ok := atom.(*query.Symbol); ok {
			found = true
		}
	})
	return found
}

func (a *Adapter) prepareGitShadow(ctx context.Context, repository catalog.Repository) (string, error) {
	shadowRoot := filepath.Join(a.indexDirectory, "git-shadow")
	if err := os.MkdirAll(shadowRoot, 0o755); err != nil {
		return "", fmt.Errorf("create Git shadow directory: %w", err)
	}
	shadowPath := filepath.Join(shadowRoot, fmt.Sprintf("repo-%d.git", repository.ID))
	if _, err := os.Stat(filepath.Join(shadowPath, "HEAD")); errors.Is(err, os.ErrNotExist) {
		if err := runGitCommand(ctx, "", "init", "--bare", shadowPath); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", fmt.Errorf("inspect fallback repository: %w", err)
	}
	ref := "refs/heads/repokarta-index"
	if err := runGitCommand(
		ctx,
		shadowPath,
		"fetch",
		"--force",
		"--no-tags",
		"--prune",
		repository.Path,
		"+HEAD:"+ref,
	); err != nil {
		return "", err
	}
	if err := runGitCommand(ctx, shadowPath, "symbolic-ref", "HEAD", ref); err != nil {
		return "", err
	}
	if err := runGitCommand(ctx, shadowPath, "config", "zoekt.name", filepath.ToSlash(repository.Path)); err != nil {
		return "", err
	}
	if err := runGitCommand(ctx, shadowPath, "config", "zoekt.repoid", strconv.FormatInt(repository.ID, 10)); err != nil {
		return "", err
	}
	return shadowPath, nil
}

func runGitCommand(ctx context.Context, gitDirectory string, arguments ...string) error {
	commandArguments := make([]string, 0, len(arguments)+2)
	if gitDirectory != "" {
		commandArguments = append(commandArguments, "--git-dir", gitDirectory)
	}
	commandArguments = append(commandArguments, arguments...)
	command := exec.CommandContext(ctx, "git", commandArguments...)
	command.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return errors.New(message)
	}
	return nil
}
