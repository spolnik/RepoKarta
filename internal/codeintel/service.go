// Package codeintel is RepoKarta's protocol-independent, read-only code
// intelligence surface. HTML, JSON, and MCP adapters all use this service.
package codeintel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/spolnik/RepoKarta/internal/access"
	"github.com/spolnik/RepoKarta/internal/analysis"
	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/contextscope"
	"github.com/spolnik/RepoKarta/internal/graph"
	"github.com/spolnik/RepoKarta/internal/querylang"
	"github.com/spolnik/RepoKarta/internal/search"
	"github.com/spolnik/RepoKarta/internal/source"
)

const (
	DefaultSearchLimit       = 100
	MaximumSearchLimit       = 500
	DefaultGitLogLimit       = 50
	MaximumGitLogLimit       = 200
	MaximumGitLogBytes       = 1 << 20
	MaximumSourceLines       = 500
	MaximumTreeEntries       = 500
	MaximumDiffBytes         = 1 << 20
	DefaultDiffContext       = 3
	MaximumDiffContext       = 20
	DefaultContextLimit      = 12
	MaximumContextLimit      = 50
	maximumContextTreeBytes  = 4 << 20
	maximumContextFileCaches = 32
	sourceWindowLines        = 200
	sourceContextBefore      = 80
	maxReferenceLinesPerFile = 50
)

// RepositoryStore supplies repository metadata.
type RepositoryStore interface {
	ListRepositories(context.Context) ([]catalog.Repository, error)
	RepositoryByID(context.Context, int64) (catalog.Repository, error)
}

// CodeSearcher executes indexed code searches.
type CodeSearcher interface {
	Search(context.Context, search.Query) (search.Result, error)
}

// StructuralReader supplies the cached, commit-pinned syntax inventory used by
// reference search. Implementations must not execute repository code.
type StructuralReader interface {
	ReadStructure(context.Context, int64) (graph.StructuralIndex, error)
}

// Service owns the shared behavior exposed by all external adapters.
type Service struct {
	store         RepositoryStore
	searcher      CodeSearcher
	structure     StructuralReader
	derived       DerivedEvidenceSearcher
	namedContexts NamedContextStore
	mu            sync.RWMutex
	baseURL       string

	contextFileMu     sync.Mutex
	contextFileCache  map[string]contextFileCacheEntry
	contextFileLoads  map[string]*contextFileLoad
	contextFileLoader func(context.Context, catalog.Repository, string) ([]string, bool, error)
}

type contextFileCacheEntry struct {
	paths     []string
	truncated bool
	lastUsed  time.Time
}

type contextFileLoad struct {
	done      chan struct{}
	paths     []string
	truncated bool
	err       error
}

// SetBaseURL changes the absolute URL used for newly returned source evidence.
func (s *Service) SetBaseURL(baseURL string) {
	s.mu.Lock()
	s.baseURL = strings.TrimRight(baseURL, "/")
	s.mu.Unlock()
}

// New creates a protocol-independent code-intelligence service.
func New(store RepositoryStore, searcher CodeSearcher, baseURL string) *Service {
	return &Service{
		store:             store,
		searcher:          searcher,
		baseURL:           strings.TrimRight(baseURL, "/"),
		contextFileCache:  make(map[string]contextFileCacheEntry),
		contextFileLoads:  make(map[string]*contextFileLoad),
		contextFileLoader: gitFiles,
	}
}

// UseStructure enables syntax-backed reference search over persisted maps.
func (s *Service) UseStructure(structure StructuralReader) *Service {
	s.structure = structure
	return s
}

// UseDerivedEvidence enables deterministic non-source result families backed
// by existing permission-aware artifact services.
func (s *Service) UseDerivedEvidence(searcher DerivedEvidenceSearcher) *Service {
	s.derived = searcher
	return s
}

// Repository describes one indexed repository without exposing its local path.
type Repository struct {
	ID              int64  `json:"id"`
	Name            string `json:"name"`
	OriginURL       string `json:"origin_url,omitempty"`
	DefaultRevision string `json:"default_revision,omitempty"`
	HeadCommit      string `json:"head_commit,omitempty"`
	IndexedCommit   string `json:"indexed_commit,omitempty"`
	ScanState       string `json:"scan_state"`
	ScanError       string `json:"scan_error,omitempty"`
	IndexState      string `json:"index_state"`
	IndexError      string `json:"index_error,omitempty"`
}

// RepositoryList is the JSON and MCP repository-list contract.
type RepositoryList struct {
	Repositories []Repository `json:"repositories"`
}

// ContextSuggestionRequest selects permission-aware repository, file,
// directory, or symbol autocomplete results.
type ContextSuggestionRequest struct {
	Kind         string
	Query        string
	RepositoryID int64
	Limit        int
}

// ContextSuggestion is one stable selector clients can turn into a chip.
type ContextSuggestion struct {
	Context contextscope.Selector `json:"context"`
	Label   string                `json:"label"`
	Detail  string                `json:"detail,omitempty"`
}

// ContextSuggestionList is a bounded autocomplete response.
type ContextSuggestionList struct {
	Suggestions []ContextSuggestion `json:"suggestions"`
	Truncated   bool                `json:"truncated"`
}

// SearchRequest is the shared query contract.
type SearchRequest struct {
	Query              string                  `json:"query"`
	RepositoryID       int64                   `json:"repository_id,omitempty"`
	Repository         string                  `json:"repository,omitempty"`
	Language           string                  `json:"language,omitempty"`
	Path               string                  `json:"path,omitempty"`
	File               string                  `json:"file,omitempty"`
	Mode               string                  `json:"mode,omitempty"`
	Limit              int                     `json:"limit,omitempty"`
	Contexts           []contextscope.Selector `json:"contexts,omitempty"`
	NamedContextIDs    []string                `json:"named_context_ids,omitempty"`
	UseDefaultContexts *bool                   `json:"use_default_contexts,omitempty"`
}

// SearchResponse explicitly reports evidence completeness.
type SearchResponse struct {
	MatchCount          int                         `json:"match_count"`
	MatchingFiles       int                         `json:"matching_files"`
	EstimatedTotalFiles int                         `json:"estimated_total_files"`
	ReturnedFiles       int                         `json:"returned_files"`
	ReturnedItems       int                         `json:"returned_items"`
	Limit               int                         `json:"limit"`
	Truncated           bool                        `json:"truncated"`
	TotalFilesExact     bool                        `json:"total_files_exact"`
	FilesSkipped        int                         `json:"files_skipped"`
	ShardsSkipped       int                         `json:"shards_skipped"`
	DurationMS          float64                     `json:"duration_ms"`
	Warnings            []search.Warning            `json:"warnings,omitempty"`
	Matches             []SearchMatch               `json:"matches"`
	Items               []SearchItem                `json:"items"`
	SearchKind          string                      `json:"search_kind,omitempty"`
	ReferenceResolution string                      `json:"reference_resolution,omitempty"`
	ReferenceIndex      *ReferenceIndex             `json:"reference_index,omitempty"`
	Contexts            []contextscope.Context      `json:"contexts,omitempty"`
	NamedContexts       []contextscope.NamedContext `json:"named_contexts,omitempty"`
	QueryLanguage       *querylang.Query            `json:"query_language,omitempty"`
	ResultType          string                      `json:"result_type"`
}

// SearchItem is one non-source result from the permission-filtered catalogue
// or another deterministic evidence store. Source matches remain in Matches
// so existing clients keep their commit-pinned line contract.
type SearchItem struct {
	ResultType   string               `json:"result_type"`
	RepositoryID int64                `json:"repository_id,omitempty"`
	Repository   string               `json:"repository,omitempty"`
	Revision     string               `json:"revision,omitempty"`
	Path         string               `json:"path,omitempty"`
	Title        string               `json:"title"`
	Summary      string               `json:"summary,omitempty"`
	Detail       string               `json:"detail,omitempty"`
	Citation     string               `json:"citation,omitempty"`
	SourceURL    string               `json:"source_url,omitempty"`
	Metadata     []SearchItemMetadata `json:"metadata,omitempty"`
}

// SearchItemMetadata is stable display metadata for a non-source result.
type SearchItemMetadata struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// ReferenceIndex reports whether every requested repository has a persisted
// structural artifact. A building response is immediately usable but partial.
type ReferenceIndex struct {
	State                 string `json:"state"`
	RequestedRepositories int    `json:"requested_repositories"`
	ReadyRepositories     int    `json:"ready_repositories"`
	PendingRepositories   int    `json:"pending_repositories"`
}

// SymbolRequest selects bounded symbol-index matches.
type SymbolRequest struct {
	Symbol             string                  `json:"symbol"`
	RepositoryID       int64                   `json:"repository_id,omitempty"`
	Repository         string                  `json:"repository,omitempty"`
	Language           string                  `json:"language,omitempty"`
	Limit              int                     `json:"limit,omitempty"`
	Contexts           []contextscope.Selector `json:"contexts,omitempty"`
	NamedContextIDs    []string                `json:"named_context_ids,omitempty"`
	UseDefaultContexts *bool                   `json:"use_default_contexts,omitempty"`
}

// SymbolResponse uses the same explicit completeness and citation contract as
// deterministic code search.
type SymbolResponse = SearchResponse

// ReferenceRequest selects bounded syntax-backed target-name matches.
type ReferenceRequest struct {
	Symbol             string                  `json:"symbol"`
	RepositoryID       int64                   `json:"repository_id,omitempty"`
	Repository         string                  `json:"repository,omitempty"`
	Language           string                  `json:"language,omitempty"`
	Path               string                  `json:"path,omitempty"`
	File               string                  `json:"file,omitempty"`
	Limit              int                     `json:"limit,omitempty"`
	Contexts           []contextscope.Selector `json:"contexts,omitempty"`
	NamedContextIDs    []string                `json:"named_context_ids,omitempty"`
	UseDefaultContexts *bool                   `json:"use_default_contexts,omitempty"`
	RelationKinds      []string                `json:"-"`
}

// ReferenceResponse uses the normal search evidence and completeness contract.
type ReferenceResponse = SearchResponse

// SearchMatch is one commit-pinned matched file.
type SearchMatch struct {
	ResultType string       `json:"result_type"`
	Repository string       `json:"repository"`
	Revision   string       `json:"revision"`
	Path       string       `json:"path"`
	Language   string       `json:"language,omitempty"`
	Score      float64      `json:"score,omitempty"`
	Lines      []SearchLine `json:"lines"`
	Citation   string       `json:"citation"`
	SourceURL  string       `json:"source_url,omitempty"`
}

// SearchLine is one line of source evidence.
type SearchLine struct {
	Number              int               `json:"number"`
	Text                string            `json:"text"`
	Before              string            `json:"before,omitempty"`
	After               string            `json:"after,omitempty"`
	Fragments           []search.Fragment `json:"fragments,omitempty"`
	ReferenceKind       string            `json:"reference_kind,omitempty"`
	ReferenceTarget     string            `json:"reference_target,omitempty"`
	ReferenceReceiver   string            `json:"reference_receiver,omitempty"`
	ReferenceConfidence string            `json:"reference_confidence,omitempty"`
}

// FileRequest selects a bounded, commit-pinned file range.
type FileRequest struct {
	RepositoryID int64
	Repository   string
	Revision     string
	Path         string
	StartLine    int
	EndLine      int
}

// FileResponse is a commit-pinned source range.
type FileResponse struct {
	Repository string       `json:"repository"`
	Revision   string       `json:"revision"`
	Path       string       `json:"path"`
	Language   string       `json:"language"`
	StartLine  int          `json:"start_line"`
	EndLine    int          `json:"end_line"`
	TotalLines int          `json:"total_lines"`
	Lines      []SourceLine `json:"lines"`
	Citation   string       `json:"citation"`
	SourceURL  string       `json:"source_url,omitempty"`
}

// SourceLine is one numbered source line.
type SourceLine struct {
	Number int    `json:"number"`
	Text   string `json:"text"`
}

// TreeRequest selects a directory at a pinned revision.
type TreeRequest struct {
	RepositoryID int64
	Repository   string
	Revision     string
	Path         string
}

// TreeResponse lists a bounded repository directory.
type TreeResponse struct {
	Repository string      `json:"repository"`
	Revision   string      `json:"revision"`
	Path       string      `json:"path,omitempty"`
	Entries    []TreeEntry `json:"entries"`
	Truncated  bool        `json:"truncated"`
	Limit      int         `json:"limit"`
}

// TreeEntry is one Git tree item.
type TreeEntry struct {
	Type string `json:"type"`
	Path string `json:"path"`
}

// GitLogRequest selects a bounded history walk from a reachable commit.
type GitLogRequest struct {
	RepositoryID int64
	Repository   string
	Revision     string
	Path         string
	Limit        int
}

// GitLogResponse is a newest-first, explicitly bounded commit history.
type GitLogResponse struct {
	Repository      string      `json:"repository"`
	Revision        string      `json:"revision"`
	Path            string      `json:"path,omitempty"`
	Commits         []GitCommit `json:"commits"`
	Truncated       bool        `json:"truncated"`
	OutputTruncated bool        `json:"output_truncated"`
	Limit           int         `json:"limit"`
	ReturnedBytes   int         `json:"returned_bytes"`
	MaximumBytes    int         `json:"maximum_bytes"`
}

// GitCommit is immutable metadata for one reachable commit.
type GitCommit struct {
	Revision    string   `json:"revision"`
	Parents     []string `json:"parents"`
	AuthorName  string   `json:"author_name"`
	AuthorEmail string   `json:"author_email"`
	AuthoredAt  string   `json:"authored_at"`
	Subject     string   `json:"subject"`
	Body        string   `json:"body,omitempty"`
}

// GitDiffRequest selects an exact commit-to-commit patch.
type GitDiffRequest struct {
	RepositoryID int64
	Repository   string
	FromRevision string
	ToRevision   string
	Path         string
	ContextLines int
}

// GitDiffResponse is a bounded patch with explicit completeness metadata.
type GitDiffResponse struct {
	Repository    string `json:"repository"`
	FromRevision  string `json:"from_revision,omitempty"`
	ToRevision    string `json:"to_revision"`
	Path          string `json:"path,omitempty"`
	Patch         string `json:"patch"`
	FilesChanged  int    `json:"files_changed"`
	Insertions    int    `json:"insertions"`
	Deletions     int    `json:"deletions"`
	ContextLines  int    `json:"context_lines"`
	Truncated     bool   `json:"truncated"`
	ReturnedBytes int    `json:"returned_bytes"`
	MaximumBytes  int    `json:"maximum_bytes"`
}

// Repositories returns stable repository metadata.
func (s *Service) Repositories(ctx context.Context) (RepositoryList, error) {
	repositories, err := s.store.ListRepositories(ctx)
	if err != nil {
		return RepositoryList{}, err
	}
	output := RepositoryList{Repositories: make([]Repository, 0, len(repositories))}
	for _, repository := range repositories {
		output.Repositories = append(output.Repositories, Repository{
			ID:              repository.ID,
			Name:            repository.Name,
			OriginURL:       repository.OriginURL,
			DefaultRevision: repository.DefaultRevision,
			HeadCommit:      repository.HeadCommit,
			IndexedCommit:   repository.IndexedCommit,
			ScanState:       repository.ScanState,
			ScanError:       repository.ScanError,
			IndexState:      repository.IndexState,
			IndexError:      repository.IndexError,
		})
	}
	return output, nil
}

// ResolveContexts validates stable selectors against the viewer's current
// catalogue and exact indexed commits. Any issue fails the whole set so a
// caller can never broaden a request by silently dropping a chip.
func (s *Service) ResolveContexts(ctx context.Context, selectors []contextscope.Selector) ([]contextscope.Context, error) {
	if len(selectors) == 0 {
		return []contextscope.Context{}, nil
	}
	if len(selectors) > contextscope.MaximumContexts {
		return nil, &contextscope.ResolutionError{Issues: []contextscope.Issue{{
			Index:   contextscope.MaximumContexts,
			Code:    "too_many",
			Message: fmt.Sprintf("at most %d structured contexts are allowed", contextscope.MaximumContexts),
		}}}
	}
	visibleRepositories, err := s.store.ListRepositories(ctx)
	if err != nil {
		return nil, err
	}
	repositoriesByID := make(map[int64]catalog.Repository, len(visibleRepositories))
	for _, repository := range visibleRepositories {
		repositoriesByID[repository.ID] = repository
	}
	resolved := make([]contextscope.Context, 0, len(selectors))
	issues := make([]contextscope.Issue, 0)
	seen := make(map[string]struct{}, len(selectors))
	for index, selector := range selectors {
		selector.Kind = strings.ToLower(strings.TrimSpace(selector.Kind))
		selector.Revision = strings.TrimSpace(selector.Revision)
		selector.Path = strings.TrimSpace(strings.ReplaceAll(selector.Path, "\\", "/"))
		selector.Symbol = strings.TrimSpace(selector.Symbol)
		selector.SymbolKind = strings.ToLower(strings.TrimSpace(selector.SymbolKind))
		issue := func(code, message string) {
			issues = append(issues, contextscope.Issue{
				Index: index, Code: code, Message: message, Selector: selector,
			})
		}
		switch selector.Kind {
		case contextscope.KindRepository, contextscope.KindFile,
			contextscope.KindDirectory, contextscope.KindSymbol:
		default:
			issue("invalid_kind", "context kind must be repository, file, directory, or symbol")
			continue
		}
		if selector.Line < 0 {
			issue("invalid_line", "context line must be a positive integer when provided")
			continue
		}
		if selector.RepositoryID <= 0 {
			issue("invalid_repository", "context repository_id must be a positive integer")
			continue
		}
		repository, available := repositoriesByID[selector.RepositoryID]
		if !available {
			// ListRepositories is permission filtered. Keep the message useful
			// without revealing whether an inaccessible numeric ID exists.
			issue("unavailable", fmt.Sprintf(
				"repository context %d is missing or unavailable to the current viewer",
				selector.RepositoryID,
			))
			continue
		}
		if repository.IndexState != "ready" || strings.TrimSpace(repository.IndexedCommit) == "" {
			issue("unindexed", fmt.Sprintf("repository %q does not have a ready indexed revision", repository.Name))
			continue
		}
		if selector.Revision != "" && selector.Revision != repository.IndexedCommit {
			issue("stale", fmt.Sprintf(
				"repository %q context is pinned to %s but the current indexed revision is %s",
				repository.Name,
				shortRevision(selector.Revision),
				shortRevision(repository.IndexedCommit),
			))
			continue
		}
		if selector.Kind != contextscope.KindSymbol &&
			(selector.Symbol != "" || selector.SymbolKind != "" || selector.Line != 0) {
			issue("invalid_symbol", "only symbol contexts can include symbol identity fields")
			continue
		}
		var (
			symbolStart int
			symbolEnd   int
		)
		switch selector.Kind {
		case contextscope.KindRepository:
			if selector.Path != "" {
				issue("invalid_path", "repository contexts cannot include a path")
				continue
			}
		case contextscope.KindFile, contextscope.KindDirectory:
			contextPath, pathErr := safeTreePath(selector.Path)
			if pathErr != nil || contextPath == "" {
				issue("invalid_path", fmt.Sprintf(
					"%s context path must be a safe repository-relative path",
					selector.Kind,
				))
				continue
			}
			expectedType := "blob"
			missingCode := "missing_file"
			if selector.Kind == contextscope.KindDirectory {
				expectedType = "tree"
				missingCode = "missing_directory"
			}
			objectType, objectErr := gitObjectType(ctx, repository, repository.IndexedCommit, contextPath)
			if objectErr != nil || objectType != expectedType {
				issue(missingCode, fmt.Sprintf(
					"%s %q is missing from repository %q at indexed revision %s",
					selector.Kind,
					contextPath,
					repository.Name,
					shortRevision(repository.IndexedCommit),
				))
				continue
			}
			selector.Path = contextPath
		case contextscope.KindSymbol:
			symbol, symbolErr := validSymbol(selector.Symbol)
			if symbolErr != nil {
				issue("invalid_symbol", symbolErr.Error())
				continue
			}
			selector.Symbol = symbol
			if selector.Path != "" {
				symbolPath, pathErr := safeTreePath(selector.Path)
				if pathErr != nil || symbolPath == "" {
					issue("invalid_path", "symbol context path must be a safe repository-relative file path")
					continue
				}
				selector.Path = symbolPath
			}
			candidates, incomplete, candidateErr := s.contextSymbolCandidates(ctx, repository, selector)
			if candidateErr != nil {
				issue("symbol_index_unavailable", candidateErr.Error())
				continue
			}
			if len(candidates) == 0 {
				if incomplete {
					issue("incomplete_symbol_index", fmt.Sprintf(
						"symbol %q could not be proven missing because the symbol context index for repository %q is incomplete",
						selector.Symbol,
						repository.Name,
					))
				} else {
					issue("missing_symbol", fmt.Sprintf(
						"symbol %q is missing from repository %q at indexed revision %s",
						selector.Symbol,
						repository.Name,
						shortRevision(repository.IndexedCommit),
					))
				}
				continue
			}
			if len(candidates) > 1 || (incomplete && (selector.Path == "" || selector.Line == 0)) {
				issue("ambiguous_symbol", fmt.Sprintf(
					"symbol %q is ambiguous in repository %q; choose a specific @symbol suggestion with its file and line",
					selector.Symbol,
					repository.Name,
				))
				continue
			}
			candidate := candidates[0]
			selector.Path = candidate.Path
			selector.SymbolKind = candidate.Symbol.Kind
			selector.Line = candidate.Symbol.Range.StartLine
			symbolStart = candidate.Symbol.Range.StartLine
			symbolEnd = max(symbolStart, candidate.Symbol.Range.EndLine)
		}
		key := fmt.Sprintf(
			"%s\x00%d\x00%s\x00%s\x00%s\x00%s\x00%d",
			selector.Kind,
			selector.RepositoryID,
			repository.IndexedCommit,
			selector.Path,
			selector.Symbol,
			selector.SymbolKind,
			selector.Line,
		)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		label := repositoryContextLabel(repository, visibleRepositories)
		switch selector.Kind {
		case contextscope.KindFile:
			label += ":" + selector.Path
		case contextscope.KindDirectory:
			label += ":" + strings.TrimSuffix(selector.Path, "/") + "/"
		case contextscope.KindSymbol:
			label = contextSymbolLabel(label, contextSymbolCandidate{
				Path: selector.Path,
				Symbol: analysis.Symbol{
					Name: selector.Symbol,
					Kind: selector.SymbolKind,
					Range: analysis.Range{
						StartLine: symbolStart,
						EndLine:   symbolEnd,
					},
				},
			})
		}
		context := contextscope.Context{
			Kind:         selector.Kind,
			RepositoryID: repository.ID,
			Repository:   repository.Name,
			Revision:     repository.IndexedCommit,
			Path:         selector.Path,
			Symbol:       selector.Symbol,
			SymbolKind:   selector.SymbolKind,
			Line:         selector.Line,
			StartLine:    symbolStart,
			EndLine:      symbolEnd,
			Label:        label,
			Sources:      []contextscope.Source{{Kind: contextscope.SourceExplicit}},
		}
		context.URL = s.ContextURL(context)
		resolved = append(resolved, context)
	}
	if len(issues) > 0 {
		return nil, &contextscope.ResolutionError{Issues: issues}
	}
	return resolved, nil
}

// SuggestContexts returns only selectors visible to the current viewer.
func (s *Service) SuggestContexts(ctx context.Context, request ContextSuggestionRequest) (ContextSuggestionList, error) {
	kind := strings.ToLower(strings.TrimSpace(request.Kind))
	queryText := strings.ToLower(strings.TrimSpace(request.Query))
	limit := normalizeLimit(request.Limit, DefaultContextLimit, MaximumContextLimit)
	output := ContextSuggestionList{Suggestions: []ContextSuggestion{}}
	switch kind {
	case contextscope.KindRepository:
		repositories, err := s.store.ListRepositories(ctx)
		if err != nil {
			return output, err
		}
		for _, repository := range repositories {
			haystack := strings.ToLower(repository.Name + "\n" + repository.OriginURL)
			if queryText != "" && !strings.Contains(haystack, queryText) {
				continue
			}
			if len(output.Suggestions) == limit {
				output.Truncated = true
				break
			}
			detail := shortRevision(repository.IndexedCommit)
			if repository.IndexState != "ready" || repository.IndexedCommit == "" {
				detail = "not indexed"
			}
			output.Suggestions = append(output.Suggestions, ContextSuggestion{
				Context: contextscope.Selector{
					Kind:         contextscope.KindRepository,
					RepositoryID: repository.ID,
					Revision:     repository.IndexedCommit,
				},
				Label:  repositoryContextLabel(repository, repositories),
				Detail: detail,
			})
		}
	case contextscope.KindFile, contextscope.KindDirectory:
		if request.RepositoryID <= 0 {
			return output, fmt.Errorf("repository_id is required for %s context suggestions", kind)
		}
		repository, err := s.store.RepositoryByID(ctx, request.RepositoryID)
		if err != nil {
			return output, err
		}
		repositories, err := s.store.ListRepositories(ctx)
		if err != nil {
			return output, err
		}
		repositoryLabel := repositoryContextLabel(repository, repositories)
		if repository.IndexState != "ready" || repository.IndexedCommit == "" {
			return output, fmt.Errorf("repository %q does not have a ready indexed revision", repository.Name)
		}
		paths, truncated, err := s.cachedContextFiles(ctx, repository, repository.IndexedCommit)
		if err != nil {
			return output, err
		}
		contextPaths := paths
		if kind == contextscope.KindDirectory {
			contextPaths = contextDirectories(paths)
		}
		labelSuffix := ""
		if kind == contextscope.KindDirectory {
			labelSuffix = "/"
		}
		for _, contextPath := range contextPaths {
			if queryText != "" && !strings.Contains(strings.ToLower(contextPath), queryText) {
				continue
			}
			if len(output.Suggestions) == limit {
				output.Truncated = true
				break
			}
			output.Suggestions = append(output.Suggestions, ContextSuggestion{
				Context: contextscope.Selector{
					Kind:         kind,
					RepositoryID: repository.ID,
					Revision:     repository.IndexedCommit,
					Path:         contextPath,
				},
				Label:  repositoryLabel + ":" + contextPath + labelSuffix,
				Detail: shortRevision(repository.IndexedCommit),
			})
		}
		output.Truncated = output.Truncated || truncated
	case contextscope.KindSymbol:
		if request.RepositoryID <= 0 {
			return output, errors.New("repository_id is required for symbol context suggestions")
		}
		repository, err := s.store.RepositoryByID(ctx, request.RepositoryID)
		if err != nil {
			return output, err
		}
		repositories, err := s.store.ListRepositories(ctx)
		if err != nil {
			return output, err
		}
		if repository.IndexState != "ready" || repository.IndexedCommit == "" {
			return output, fmt.Errorf("repository %q does not have a ready indexed revision", repository.Name)
		}
		return s.suggestSymbolContexts(
			ctx,
			repository,
			repositoryContextLabel(repository, repositories),
			queryText,
			limit,
		)
	default:
		return output, errors.New("context kind must be repository, file, directory, or symbol")
	}
	return output, nil
}

func (s *Service) cachedContextFiles(
	ctx context.Context,
	repository catalog.Repository,
	revision string,
) ([]string, bool, error) {
	key := strconv.FormatInt(repository.ID, 10) + "\x00" + revision
	s.contextFileMu.Lock()
	if cached, ok := s.contextFileCache[key]; ok {
		cached.lastUsed = time.Now()
		s.contextFileCache[key] = cached
		s.contextFileMu.Unlock()
		return cached.paths, cached.truncated, nil
	}
	if load, ok := s.contextFileLoads[key]; ok {
		s.contextFileMu.Unlock()
		select {
		case <-load.done:
			return load.paths, load.truncated, load.err
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
	}
	load := &contextFileLoad{done: make(chan struct{})}
	s.contextFileLoads[key] = load
	loader := s.contextFileLoader
	s.contextFileMu.Unlock()

	load.paths, load.truncated, load.err = loader(ctx, repository, revision)

	s.contextFileMu.Lock()
	delete(s.contextFileLoads, key)
	if load.err == nil {
		if len(s.contextFileCache) >= maximumContextFileCaches {
			oldestKey := ""
			var oldest time.Time
			for candidateKey, candidate := range s.contextFileCache {
				if oldestKey == "" || candidate.lastUsed.Before(oldest) {
					oldestKey = candidateKey
					oldest = candidate.lastUsed
				}
			}
			delete(s.contextFileCache, oldestKey)
		}
		s.contextFileCache[key] = contextFileCacheEntry{
			paths: load.paths, truncated: load.truncated, lastUsed: time.Now(),
		}
	}
	close(load.done)
	s.contextFileMu.Unlock()
	return load.paths, load.truncated, load.err
}

// CatalogRepositories returns the underlying metadata for HTML presentation.
func (s *Service) CatalogRepositories(ctx context.Context) ([]catalog.Repository, error) {
	return s.store.ListRepositories(ctx)
}

// RepositoryByID resolves repository metadata for the HTML source view.
func (s *Service) RepositoryByID(ctx context.Context, id int64) (catalog.Repository, error) {
	return s.store.RepositoryByID(ctx, id)
}

// Search performs a bounded search and resolves every match to a citation.
func (s *Service) Search(ctx context.Context, request SearchRequest) (SearchResponse, error) {
	parsedQuery, err := querylang.Parse(request.Query)
	if err != nil {
		return SearchResponse{}, err
	}
	resultType, err := requestedResultType(parsedQuery)
	if err != nil {
		return SearchResponse{}, err
	}
	switch resultType {
	case "repository", "commit", "diff":
		return s.searchEntityEvidence(ctx, request, parsedQuery, resultType)
	case "dependency", "route", "wiki_page", "code_insight":
		return s.searchDerivedEvidence(ctx, request, parsedQuery, resultType)
	}
	referenceMode := strings.EqualFold(strings.TrimSpace(request.Mode), "references")
	if referenceMode || resultType == "reference" || resultType == "implementation" {
		referenceRequest, referenceErr := s.referenceRequestForQuery(ctx, request, parsedQuery)
		if referenceErr != nil {
			return SearchResponse{}, referenceErr
		}
		if resultType == "implementation" {
			referenceRequest.RelationKinds = []string{"extends", "implements"}
		}
		response, referenceErr := s.FindReferences(ctx, referenceRequest)
		if resultType == "implementation" {
			response.SearchKind = "implementations"
			setSearchResultType(&response, "implementation")
		} else {
			setSearchResultType(&response, "reference")
		}
		response.QueryLanguage = &parsedQuery
		return response, referenceErr
	}
	limit := normalizeLimit(request.Limit, DefaultSearchLimit, MaximumSearchLimit)
	useDefaultContexts := request.UseDefaultContexts
	if useDefaultContexts == nil &&
		len(request.Contexts) == 0 &&
		len(request.NamedContextIDs) == 0 &&
		(request.RepositoryID > 0 || strings.TrimSpace(request.Repository) != "") {
		disabled := false
		useDefaultContexts = &disabled
	}
	effective, err := s.ResolveEffectiveContexts(ctx, contextscope.EffectiveRequest{
		Contexts:        request.Contexts,
		NamedContextIDs: request.NamedContextIDs,
		UseDefaults:     useDefaultContexts,
	})
	if err != nil {
		return SearchResponse{}, err
	}
	resolvedContexts := effective.Contexts
	if len(resolvedContexts) > 0 &&
		(request.RepositoryID > 0 || strings.TrimSpace(request.Repository) != "") {
		return SearchResponse{}, errors.New("structured contexts cannot be combined with the legacy repository selector")
	}
	repositoryFilter := strings.TrimSpace(request.Repository)
	queryFilters, err := s.compileQueryFilters(ctx, parsedQuery)
	if err != nil {
		return SearchResponse{}, err
	}
	var repositoryIDAllowList []uint32
	var structuredScopes []search.Scope
	if len(resolvedContexts) > 0 {
		repositoriesByID := make(map[int64]catalog.Repository, len(resolvedContexts))
		for _, resolved := range resolvedContexts {
			repository, ok := repositoriesByID[resolved.RepositoryID]
			if !ok {
				repository, err = s.store.RepositoryByID(ctx, resolved.RepositoryID)
				if err != nil {
					return SearchResponse{}, err
				}
				repositoriesByID[resolved.RepositoryID] = repository
			}
			structuredScopes = append(structuredScopes, search.Scope{
				RepositoryID: uint32(repository.ID),
				Repository:   filepath.ToSlash(repository.Path),
				Kind:         resolved.Kind,
				Path:         resolved.Path,
				Symbol:       resolved.Symbol,
				StartLine:    resolved.StartLine,
				EndLine:      resolved.EndLine,
			})
		}
		structuredScopes = compactSearchScopes(structuredScopes)
	}
	if request.RepositoryID > 0 {
		repository, err := s.store.RepositoryByID(ctx, request.RepositoryID)
		if err != nil {
			return SearchResponse{}, err
		}
		repositoryFilter = ""
		repositoryIDAllowList = []uint32{uint32(repository.ID)}
	} else if repositoryFilter != "" {
		repository, err := s.namedRepository(ctx, repositoryFilter)
		if err != nil {
			return SearchResponse{}, err
		}
		repositoryFilter = ""
		repositoryIDAllowList = []uint32{uint32(repository.ID)}
	} else if len(resolvedContexts) == 0 {
		_, restricted := access.ViewerFromContext(ctx)
		if restricted {
			repositories, err := s.store.ListRepositories(ctx)
			if err != nil {
				return SearchResponse{}, err
			}
			for _, repository := range repositories {
				repositoryIDAllowList = append(repositoryIDAllowList, uint32(repository.ID))
			}
			if len(repositoryIDAllowList) == 0 {
				return SearchResponse{
					Limit:           limit,
					Matches:         []SearchMatch{},
					Items:           []SearchItem{},
					TotalFilesExact: true,
					Warnings:        []search.Warning{},
					QueryLanguage:   &parsedQuery,
					ResultType:      resultType,
				}, nil
			}
		}
	}
	repositoryIDAllowList, structuredScopes, empty := applyQueryRepositoryFilters(
		repositoryIDAllowList,
		structuredScopes,
		queryFilters.repositoryAllow,
		queryFilters.repositoryLimited,
		queryFilters.repositoryDeny,
	)
	if empty {
		return SearchResponse{
			Limit:           limit,
			Matches:         []SearchMatch{},
			Items:           []SearchItem{},
			TotalFilesExact: true,
			Warnings:        []search.Warning{},
			Contexts:        resolvedContexts,
			NamedContexts:   effective.NamedContexts,
			QueryLanguage:   &parsedQuery,
			ResultType:      resultType,
		}, nil
	}
	engineText := parsedQuery.Text
	engineMode := strings.TrimSpace(request.Mode)
	fileNameOnly := resultType == "file_path"
	if fileNameOnly && engineMode != "" && !strings.EqualFold(engineMode, "literal") {
		return SearchResponse{}, errors.New("file_path results currently support literal query mode")
	}
	if resultType == "symbol_definition" {
		if engineMode != "" && !strings.EqualFold(engineMode, "literal") {
			return SearchResponse{}, errors.New("symbol_definition results currently support literal query mode")
		}
		if len(queryFilters.includeText)+len(queryFilters.excludeText) > 0 {
			return SearchResponse{}, errors.New("content filters cannot be combined with symbol_definition results")
		}
		symbol, symbolErr := validSymbol(parsedQuery.Text)
		if symbolErr != nil {
			return SearchResponse{}, symbolErr
		}
		escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(symbol)
		engineText = `sym:"` + escaped + `"`
		engineMode = "zoekt"
	}
	if len(parsedQuery.Filters) == 0 {
		switch strings.ToLower(engineMode) {
		case "zoekt", "regex":
			engineText = strings.TrimSpace(request.Query)
		}
	}
	result, err := s.searcher.Search(ctx, search.Query{
		Text:                 engineText,
		IncludeText:          queryFilters.includeText,
		ExcludeText:          queryFilters.excludeText,
		Repository:           repositoryFilter,
		RepositoryIDs:        repositoryIDAllowList,
		ExcludeRepositoryIDs: queryFilters.repositoryDeny,
		Scopes:               structuredScopes,
		Language:             strings.TrimSpace(request.Language),
		Languages:            queryFilters.languages,
		ExcludeLanguages:     queryFilters.excludeLanguages,
		Path:                 strings.TrimSpace(request.Path),
		Paths:                queryFilters.paths,
		ExcludePaths:         queryFilters.excludePaths,
		File:                 strings.TrimSpace(request.File),
		Files:                queryFilters.files,
		ExcludeFiles:         queryFilters.excludeFiles,
		FileNameOnly:         fileNameOnly,
		Mode:                 engineMode,
		Limit:                limit,
	})
	if err != nil {
		return SearchResponse{}, err
	}
	result = filterSearchResultToStructuredScopes(result, structuredScopes, true)
	response, err := s.searchResponse(ctx, result, limit)
	if err != nil {
		return SearchResponse{}, err
	}
	response.Contexts = resolvedContexts
	response.NamedContexts = effective.NamedContexts
	response.QueryLanguage = &parsedQuery
	setSearchResultType(&response, resultType)
	return response, nil
}

func setSearchResultType(response *SearchResponse, resultType string) {
	response.ResultType = resultType
	for index := range response.Matches {
		response.Matches[index].ResultType = resultType
	}
}

func (s *Service) searchResponse(ctx context.Context, result search.Result, limit int) (SearchResponse, error) {
	repositories, err := s.store.ListRepositories(ctx)
	if err != nil {
		return SearchResponse{}, err
	}
	output := SearchResponse{
		MatchCount:          result.MatchCount,
		MatchingFiles:       result.FileCount,
		EstimatedTotalFiles: result.EstimatedFiles,
		ReturnedFiles:       result.ReturnedFiles,
		Limit:               result.Limit,
		Truncated:           result.Truncated,
		TotalFilesExact:     result.TotalFilesExact,
		FilesSkipped:        result.FilesSkipped,
		ShardsSkipped:       result.ShardsSkipped,
		DurationMS:          float64(result.Duration.Microseconds()) / 1000,
		Warnings:            result.Warnings,
		Matches:             make([]SearchMatch, 0, len(result.Matches)),
		Items:               []SearchItem{},
	}
	if output.Limit == 0 {
		output.Limit = limit
	}
	if output.ReturnedFiles == 0 && len(result.Matches) > 0 {
		output.ReturnedFiles = len(result.Matches)
	}
	if output.MatchingFiles == 0 && len(result.Matches) > 0 {
		output.MatchingFiles = len(result.Matches)
		output.EstimatedTotalFiles = len(result.Matches)
		output.TotalFilesExact = !result.Truncated
	}
	restricted := false
	if _, ok := access.ViewerFromContext(ctx); ok {
		restricted = true
	}
	visibleMatches := 0
	visibleMatchCount := 0
	droppedMatches := false
	for _, match := range result.Matches {
		outputMatch := SearchMatch{
			Repository: match.Repository,
			Revision:   match.Revision,
			Path:       match.Path,
			Language:   match.Language,
			Score:      match.Score,
			Lines:      make([]SearchLine, 0, len(match.Lines)),
		}
		for _, line := range match.Lines {
			outputMatch.Lines = append(outputMatch.Lines, SearchLine{
				Number:              line.Number,
				Text:                line.Text,
				Before:              line.Before,
				After:               line.After,
				Fragments:           line.Fragments,
				ReferenceKind:       line.ReferenceKind,
				ReferenceTarget:     line.ReferenceTarget,
				ReferenceReceiver:   line.ReferenceReceiver,
				ReferenceConfidence: line.ReferenceConfidence,
			})
		}
		repository, visible := resolveRepository(repositories, match.RepositoryID, match.Repository, match.Revision, restricted)
		if restricted && !visible {
			droppedMatches = true
			continue
		}
		if visible {
			start, end := lineRange(match.Lines)
			outputMatch.Repository = repository.Name
			outputMatch.Citation = Citation(repository.Name, match.Revision, match.Path, start, end)
			outputMatch.SourceURL = s.SourceURL(repository.ID, match.Revision, match.Path, start, end)
		}
		output.Matches = append(output.Matches, outputMatch)
		visibleMatches++
		visibleMatchCount += len(match.Lines)
	}
	if restricted && droppedMatches {
		// Search may scan shared shards internally, but authorization metadata
		// and inaccessible match counts never cross the response boundary.
		output.MatchCount = visibleMatchCount
		output.MatchingFiles = visibleMatches
		output.EstimatedTotalFiles = visibleMatches
		output.ReturnedFiles = visibleMatches
		output.FilesSkipped = 0
		output.ShardsSkipped = 0
		output.Truncated = visibleMatches >= limit
		output.TotalFilesExact = !output.Truncated
	}
	output.ReturnedItems = len(output.Matches)
	return output, nil
}

// FindSymbol performs an explicit Zoekt symbol query. When Universal Ctags was
// unavailable at index time, the normal machine-readable warning is returned.
func (s *Service) FindSymbol(ctx context.Context, request SymbolRequest) (SymbolResponse, error) {
	symbol, err := validSymbol(request.Symbol)
	if err != nil {
		return SymbolResponse{}, err
	}
	escaped := strings.ReplaceAll(symbol, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	response, err := s.Search(ctx, SearchRequest{
		Query:              `sym:"` + escaped + `"`,
		RepositoryID:       request.RepositoryID,
		Repository:         request.Repository,
		Language:           request.Language,
		Mode:               "zoekt",
		Limit:              request.Limit,
		Contexts:           request.Contexts,
		NamedContextIDs:    request.NamedContextIDs,
		UseDefaultContexts: request.UseDefaultContexts,
	})
	setSearchResultType(&response, "symbol_definition")
	return response, err
}

// FindReferences searches the cached structural index for syntax-backed
// target-name matches. It deliberately does not pretend to perform type or
// overload resolution: each result reports the parser relation and confidence.
func (s *Service) FindReferences(ctx context.Context, request ReferenceRequest) (ReferenceResponse, error) {
	started := time.Now()
	symbol, err := validSymbol(request.Symbol)
	if err != nil {
		return ReferenceResponse{}, err
	}
	if s.structure == nil {
		return ReferenceResponse{}, errors.New("AST reference search is not configured")
	}
	repositoryID := request.RepositoryID
	useDefaultContexts := request.UseDefaultContexts
	if useDefaultContexts == nil &&
		len(request.Contexts) == 0 &&
		len(request.NamedContextIDs) == 0 &&
		(repositoryID > 0 || strings.TrimSpace(request.Repository) != "") {
		disabled := false
		useDefaultContexts = &disabled
	}
	effective, err := s.ResolveEffectiveContexts(ctx, contextscope.EffectiveRequest{
		Contexts:        request.Contexts,
		NamedContextIDs: request.NamedContextIDs,
		UseDefaults:     useDefaultContexts,
	})
	if err != nil {
		return ReferenceResponse{}, err
	}
	resolvedContexts := effective.Contexts
	if len(resolvedContexts) > 0 &&
		(repositoryID > 0 || strings.TrimSpace(request.Repository) != "") {
		return ReferenceResponse{}, errors.New("structured contexts cannot be combined with the legacy repository selector")
	}
	var structuredScopes []search.Scope
	if len(resolvedContexts) > 0 {
		repositoryIDs := make(map[int64]struct{})
		for _, resolved := range resolvedContexts {
			repositoryIDs[resolved.RepositoryID] = struct{}{}
			structuredScopes = append(structuredScopes, search.Scope{
				RepositoryID: uint32(resolved.RepositoryID),
				Kind:         resolved.Kind,
				Path:         resolved.Path,
				Symbol:       resolved.Symbol,
				StartLine:    resolved.StartLine,
				EndLine:      resolved.EndLine,
			})
		}
		if len(repositoryIDs) != 1 {
			return ReferenceResponse{}, errors.New("reference search currently accepts structured contexts from one repository")
		}
		for repositoryID = range repositoryIDs {
		}
		structuredScopes = compactSearchScopes(structuredScopes)
	}
	if repositoryID <= 0 && strings.TrimSpace(request.Repository) != "" {
		repository, resolveErr := s.namedRepository(ctx, request.Repository)
		if resolveErr != nil {
			return ReferenceResponse{}, resolveErr
		}
		repositoryID = repository.ID
	}
	index, err := s.structure.ReadStructure(ctx, repositoryID)
	if err != nil {
		return ReferenceResponse{}, fmt.Errorf("load AST reference index: %w", err)
	}
	result, err := s.referenceResult(ctx, index, symbol, request)
	if err != nil {
		return ReferenceResponse{}, err
	}
	result = filterSearchResultToStructuredScopes(result, structuredScopes, false)
	result.Duration = time.Since(started)
	output, err := s.searchResponse(ctx, result, normalizeLimit(request.Limit, DefaultSearchLimit, MaximumSearchLimit))
	if err != nil {
		return ReferenceResponse{}, err
	}
	output.SearchKind = "references"
	setSearchResultType(&output, "reference")
	output.ReferenceResolution = "syntax-target-name"
	output.ReferenceIndex = &ReferenceIndex{
		State:                 "ready",
		RequestedRepositories: index.Scope.TotalRepositories,
		ReadyRepositories:     index.Scope.AnalyzedRepositories,
		PendingRepositories:   index.Scope.OmittedRepositories,
	}
	if !index.Scope.Complete {
		output.ReferenceIndex.State = "building"
	}
	output.Contexts = resolvedContexts
	output.NamedContexts = effective.NamedContexts
	return output, nil
}

type structuralReference struct {
	kind       string
	target     string
	receiver   string
	confidence string
	line       int
}

type structuralReferenceFile struct {
	repositoryID int64
	repository   string
	revision     string
	path         string
	language     string
	references   []structuralReference
}

func (s *Service) referenceResult(
	ctx context.Context,
	index graph.StructuralIndex,
	symbol string,
	request ReferenceRequest,
) (search.Result, error) {
	limit := normalizeLimit(request.Limit, DefaultSearchLimit, MaximumSearchLimit)
	files := make(map[string]*structuralReferenceFile)
	matchCount := 0
	parsePartial := 0
	for _, document := range index.Structure {
		if request.RepositoryID > 0 && document.RepositoryID != request.RepositoryID {
			continue
		}
		if request.Language != "" && !strings.EqualFold(document.Language, strings.TrimSpace(request.Language)) {
			continue
		}
		if request.Path != "" && !strings.Contains(strings.ToLower(document.Path), strings.ToLower(strings.TrimSpace(request.Path))) {
			continue
		}
		if request.File != "" && !strings.Contains(strings.ToLower(path.Base(document.Path)), strings.ToLower(strings.TrimSpace(request.File))) {
			continue
		}
		if !document.ParseComplete || document.Truncated {
			parsePartial++
		}
		for _, relation := range document.Relations {
			if len(request.RelationKinds) > 0 && !containsFold(request.RelationKinds, relation.Kind) {
				continue
			}
			if !relationMatchesSymbol(relation.Target, symbol) {
				continue
			}
			key := strconv.FormatInt(document.RepositoryID, 10) + "\x00" + document.Path
			file := files[key]
			if file == nil {
				file = &structuralReferenceFile{
					repositoryID: document.RepositoryID,
					repository:   document.Repository,
					revision:     document.Revision,
					path:         document.Path,
					language:     document.Language,
					references:   []structuralReference{},
				}
				files[key] = file
			}
			file.references = append(file.references, structuralReference{
				kind:       relation.Kind,
				target:     relation.Target,
				receiver:   relation.Receiver,
				confidence: relation.Confidence,
				line:       max(1, relation.Range.StartLine),
			})
			matchCount++
		}
	}

	ordered := make([]*structuralReferenceFile, 0, len(files))
	for _, file := range files {
		sort.Slice(file.references, func(left, right int) bool {
			if file.references[left].line != file.references[right].line {
				return file.references[left].line < file.references[right].line
			}
			if file.references[left].kind != file.references[right].kind {
				return file.references[left].kind < file.references[right].kind
			}
			return file.references[left].target < file.references[right].target
		})
		ordered = append(ordered, file)
	}
	sort.Slice(ordered, func(left, right int) bool {
		if ordered[left].repository != ordered[right].repository {
			return strings.ToLower(ordered[left].repository) < strings.ToLower(ordered[right].repository)
		}
		return ordered[left].path < ordered[right].path
	})

	repositories, err := s.store.ListRepositories(ctx)
	if err != nil {
		return search.Result{}, err
	}
	repositoryByID := make(map[int64]catalog.Repository, len(repositories))
	for _, repository := range repositories {
		repositoryByID[repository.ID] = repository
	}

	result := search.Result{
		Matches:         []search.FileMatch{},
		MatchCount:      matchCount,
		FileCount:       len(ordered),
		EstimatedFiles:  len(ordered),
		Limit:           limit,
		TotalFilesExact: true,
		Warnings:        []search.Warning{},
	}
	if index.StructureTruncated {
		result.Truncated = true
		result.TotalFilesExact = false
		result.Warnings = append(result.Warnings, search.Warning{
			Code:    "ast_structure_truncated",
			Message: "The persisted structural index is bounded; additional references may exist outside captured AST documents or relations.",
		})
	}
	if index.Scope.TotalRepositories > 0 && !index.Scope.Complete {
		result.Truncated = true
		result.TotalFilesExact = false
		result.Warnings = append(result.Warnings, search.Warning{
			Code: "ast_index_building",
			Message: fmt.Sprintf(
				"AST reference artifacts are ready for %d of %d requested repositories; %d are still building in the background.",
				index.Scope.AnalyzedRepositories,
				index.Scope.TotalRepositories,
				index.Scope.OmittedRepositories,
			),
		})
	}
	if parsePartial > 0 {
		result.Truncated = true
		result.TotalFilesExact = false
		result.Warnings = append(result.Warnings, search.Warning{
			Code:    "ast_parse_partial",
			Message: fmt.Sprintf("%d filtered AST documents were partial or individually truncated.", parsePartial),
		})
	}
	if len(ordered) > limit {
		result.Truncated = true
		ordered = ordered[:limit]
	}

	linesTruncated := false
	sourceSkipped := false
	for _, indexedFile := range ordered {
		repository, ok := repositoryByID[indexedFile.repositoryID]
		if !ok {
			result.FilesSkipped++
			sourceSkipped = true
			continue
		}
		references := deduplicateReferences(indexedFile.references)
		if len(references) > maxReferenceLinesPerFile {
			references = references[:maxReferenceLinesPerFile]
			result.Truncated = true
			linesTruncated = true
		}
		minimumLine := references[0].line
		maximumLine := references[len(references)-1].line
		file, openErr := source.OpenFile(
			ctx,
			repository,
			indexedFile.revision,
			indexedFile.path,
			max(1, minimumLine-1),
			maximumLine+1,
		)
		if openErr != nil {
			result.FilesSkipped++
			sourceSkipped = true
			continue
		}
		sourceLines := make(map[int]string, len(file.Lines))
		for _, line := range file.Lines {
			sourceLines[line.Number] = line.Text
		}
		match := search.FileMatch{
			RepositoryID: indexedFile.repositoryID,
			Repository:   indexedFile.repository,
			Revision:     indexedFile.revision,
			Path:         indexedFile.path,
			Language:     indexedFile.language,
			Score:        1,
			Lines:        make([]search.LineMatch, 0, len(references)),
		}
		for _, reference := range references {
			text := sourceLines[reference.line]
			if text == "" {
				text = reference.target
			}
			match.Lines = append(match.Lines, search.LineMatch{
				Number:              reference.line,
				Text:                text,
				Before:              sourceLines[reference.line-1],
				After:               sourceLines[reference.line+1],
				Fragments:           literalFragments(text, symbol),
				ReferenceKind:       reference.kind,
				ReferenceTarget:     reference.target,
				ReferenceReceiver:   reference.receiver,
				ReferenceConfidence: reference.confidence,
			})
		}
		result.Matches = append(result.Matches, match)
	}
	if linesTruncated {
		result.Warnings = append(result.Warnings, search.Warning{
			Code:    "reference_lines_truncated",
			Message: fmt.Sprintf("At most %d distinct AST reference lines are returned per file.", maxReferenceLinesPerFile),
		})
	}
	if sourceSkipped {
		result.Warnings = append(result.Warnings, search.Warning{
			Code:    "reference_source_unavailable",
			Message: "One or more AST matches could not be reopened at the pinned revision and were skipped.",
		})
	}
	result.ReturnedFiles = len(result.Matches)
	return result, nil
}

func deduplicateReferences(references []structuralReference) []structuralReference {
	output := make([]structuralReference, 0, len(references))
	seen := make(map[string]struct{}, len(references))
	for _, reference := range references {
		key := fmt.Sprintf(
			"%d\x00%s\x00%s\x00%s",
			reference.line,
			reference.kind,
			reference.target,
			reference.receiver,
		)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		output = append(output, reference)
	}
	return output
}

func relationMatchesSymbol(target, symbol string) bool {
	target = strings.TrimSpace(target)
	if target == symbol {
		return true
	}
	for _, candidate := range strings.FieldsFunc(target, func(character rune) bool {
		return character != '_' && character != '$' &&
			!unicode.IsLetter(character) && !unicode.IsDigit(character)
	}) {
		if candidate == symbol {
			return true
		}
	}
	return false
}

func literalFragments(text, query string) []search.Fragment {
	if query == "" {
		return nil
	}
	fragments := make([]search.Fragment, 0)
	for offset := 0; offset <= len(text)-len(query); {
		index := strings.Index(text[offset:], query)
		if index < 0 {
			break
		}
		start := offset + index
		fragments = append(fragments, search.Fragment{Start: start, End: start + len(query)})
		offset = start + len(query)
	}
	return fragments
}

func validSymbol(value string) (string, error) {
	symbol := strings.TrimSpace(value)
	if symbol == "" {
		return "", errors.New("symbol is required")
	}
	if len([]rune(symbol)) > 200 || strings.ContainsAny(symbol, "\r\n\x00") {
		return "", errors.New("symbol is invalid")
	}
	return symbol, nil
}

// GetFile reads a commit-pinned source range.
func (s *Service) GetFile(ctx context.Context, request FileRequest) (FileResponse, error) {
	repository, err := s.selectRepository(ctx, request.RepositoryID, request.Repository)
	if err != nil {
		return FileResponse{}, err
	}
	start := max(1, request.StartLine)
	end := request.EndLine
	if end <= 0 {
		end = start + 199
	}
	if end < start {
		end = start
	}
	if end-start+1 > MaximumSourceLines {
		end = start + MaximumSourceLines - 1
	}
	file, err := source.OpenFile(ctx, repository, request.Revision, request.Path, start, end)
	if err != nil {
		return FileResponse{}, err
	}
	output := FileResponse{
		Repository: file.Repository.Name,
		Revision:   file.Revision,
		Path:       file.Path,
		Language:   file.Language,
		StartLine:  file.StartLine,
		EndLine:    file.EndLine,
		TotalLines: file.TotalLines,
		Lines:      make([]SourceLine, 0, len(file.Lines)),
		Citation:   Citation(file.Repository.Name, file.Revision, file.Path, file.StartLine, file.EndLine),
		SourceURL:  s.SourceURL(file.Repository.ID, file.Revision, file.Path, file.StartLine, file.EndLine),
	}
	for _, line := range file.Lines {
		output.Lines = append(output.Lines, SourceLine{Number: line.Number, Text: line.Text})
	}
	return output, nil
}

// ListTree lists a bounded directory at a pinned revision.
func (s *Service) ListTree(ctx context.Context, request TreeRequest) (TreeResponse, error) {
	repository, err := s.selectRepository(ctx, request.RepositoryID, request.Repository)
	if err != nil {
		return TreeResponse{}, err
	}
	revision := request.Revision
	if revision == "" {
		revision = repository.IndexedCommit
	}
	revision, err = source.ResolveCommit(ctx, repository, revision)
	if err != nil {
		return TreeResponse{}, err
	}
	treePath, err := safeTreePath(request.Path)
	if err != nil {
		return TreeResponse{}, err
	}
	entries, truncated, err := gitTree(ctx, repository, revision, treePath)
	if err != nil {
		return TreeResponse{}, err
	}
	return TreeResponse{
		Repository: repository.Name,
		Revision:   revision,
		Path:       treePath,
		Entries:    entries,
		Truncated:  truncated,
		Limit:      MaximumTreeEntries,
	}, nil
}

// GitLog returns bounded history from the indexed commit or a reachable
// historical commit, optionally limited to a repository-relative path.
func (s *Service) GitLog(ctx context.Context, request GitLogRequest) (GitLogResponse, error) {
	repository, err := s.selectRepository(ctx, request.RepositoryID, request.Repository)
	if err != nil {
		return GitLogResponse{}, err
	}
	revision, err := source.ResolveCommit(ctx, repository, request.Revision)
	if err != nil {
		return GitLogResponse{}, err
	}
	limit := normalizeLimit(request.Limit, DefaultGitLogLimit, MaximumGitLogLimit)
	commits, truncated, outputTruncated, returnedBytes, err := source.Log(
		ctx,
		repository,
		revision,
		request.Path,
		limit,
		MaximumGitLogBytes,
	)
	if err != nil {
		return GitLogResponse{}, err
	}
	output := GitLogResponse{
		Repository:      repository.Name,
		Revision:        revision,
		Path:            strings.TrimSpace(strings.ReplaceAll(request.Path, "\\", "/")),
		Commits:         make([]GitCommit, 0, len(commits)),
		Truncated:       truncated,
		OutputTruncated: outputTruncated,
		Limit:           limit,
		ReturnedBytes:   returnedBytes,
		MaximumBytes:    MaximumGitLogBytes,
	}
	for _, commit := range commits {
		output.Commits = append(output.Commits, GitCommit{
			Revision:    commit.Revision,
			Parents:     commit.Parents,
			AuthorName:  commit.AuthorName,
			AuthorEmail: commit.AuthorEmail,
			AuthoredAt:  commit.AuthoredAt,
			Subject:     commit.Subject,
			Body:        commit.Body,
		})
	}
	return output, nil
}

// GitDiff returns a bounded unified patch between reachable commits. The
// default comparison is the indexed commit against its first parent.
func (s *Service) GitDiff(ctx context.Context, request GitDiffRequest) (GitDiffResponse, error) {
	repository, err := s.selectRepository(ctx, request.RepositoryID, request.Repository)
	if err != nil {
		return GitDiffResponse{}, err
	}
	contextLines := request.ContextLines
	if contextLines <= 0 {
		contextLines = DefaultDiffContext
	}
	if contextLines > MaximumDiffContext {
		contextLines = MaximumDiffContext
	}
	diff, err := source.DiffCommits(
		ctx,
		repository,
		request.FromRevision,
		request.ToRevision,
		request.Path,
		contextLines,
		MaximumDiffBytes,
	)
	if err != nil {
		return GitDiffResponse{}, err
	}
	return GitDiffResponse{
		Repository:    repository.Name,
		FromRevision:  diff.FromRevision,
		ToRevision:    diff.ToRevision,
		Path:          strings.TrimSpace(strings.ReplaceAll(request.Path, "\\", "/")),
		Patch:         diff.Patch,
		FilesChanged:  diff.FilesChanged,
		Insertions:    diff.Insertions,
		Deletions:     diff.Deletions,
		ContextLines:  contextLines,
		Truncated:     diff.Truncated,
		ReturnedBytes: diff.ReturnedBytes,
		MaximumBytes:  MaximumDiffBytes,
	}, nil
}

// SourceURL builds a local, commit-pinned human source URL.
func (s *Service) SourceURL(repositoryID int64, revision, filePath string, start, end int) string {
	windowStart, windowEnd := SourceWindow(start, end)
	values := url.Values{
		"rev":   []string{revision},
		"path":  []string{filePath},
		"lines": []string{strconv.Itoa(windowStart) + "-" + strconv.Itoa(windowEnd)},
		"focus": []string{strconv.Itoa(start) + "-" + strconv.Itoa(end)},
	}
	s.mu.RLock()
	baseURL := s.baseURL
	s.mu.RUnlock()
	return baseURL + "/source/" + strconv.FormatInt(repositoryID, 10) + "?" + values.Encode() + "#L" + strconv.Itoa(start)
}

// SourceWindow returns a useful bounded viewing window around an exact cited
// range. The cited range remains separate so consumers can emphasize it.
func SourceWindow(start, end int) (int, int) {
	start = max(1, start)
	end = max(start, end)
	windowStart := max(1, start-sourceContextBefore)
	windowEnd := max(windowStart+sourceWindowLines-1, end)
	if windowEnd-windowStart+1 > MaximumSourceLines {
		windowStart = max(1, end-MaximumSourceLines+1)
		windowEnd = max(windowStart+sourceWindowLines-1, end)
	}
	return windowStart, windowEnd
}

// Citation formats a concise commit-pinned evidence label.
func Citation(repository, revision, filePath string, start, end int) string {
	if len(revision) > 8 {
		revision = revision[:8]
	}
	return fmt.Sprintf("%s@%s:%s#L%d-L%d", repository, revision, filePath, start, end)
}

// selectRepository resolves the repository a request targets. The stable
// numeric ID returned by list_repositories is authoritative; an exact name is
// accepted only for the HTML and JSON clients that still send one.
func (s *Service) selectRepository(ctx context.Context, id int64, name string) (catalog.Repository, error) {
	if id > 0 {
		return s.store.RepositoryByID(ctx, id)
	}
	if strings.TrimSpace(name) == "" {
		return catalog.Repository{}, errors.New("repository_id is required")
	}
	return s.namedRepository(ctx, name)
}

func (s *Service) namedRepository(ctx context.Context, name string) (catalog.Repository, error) {
	repositories, err := s.store.ListRepositories(ctx)
	if err != nil {
		return catalog.Repository{}, err
	}
	for _, repository := range repositories {
		if strings.EqualFold(repository.Name, strings.TrimSpace(name)) {
			return repository, nil
		}
	}
	return catalog.Repository{}, fmt.Errorf("repository %q is not indexed", name)
}

func resolveRepository(repositories []catalog.Repository, repositoryID int64, searchName, revision string, requireExactIdentity bool) (catalog.Repository, bool) {
	normalized := strings.ReplaceAll(searchName, "\\", "/")
	for _, repository := range repositories {
		if repository.IndexedCommit != revision && repository.HeadCommit != revision {
			continue
		}
		repositoryPath := filepath.ToSlash(filepath.Clean(repository.Path))
		pathMatches := normalized == repositoryPath
		if runtime.GOOS == "windows" {
			pathMatches = strings.EqualFold(normalized, repositoryPath)
		}
		idMatches := repositoryID > 0 && repository.ID == repositoryID
		if idMatches || pathMatches || (!requireExactIdentity &&
			(normalized == repository.Name ||
				strings.HasSuffix(strings.ToLower(normalized), "/"+strings.ToLower(repository.Name)))) {
			return repository, true
		}
	}
	return catalog.Repository{}, false
}

func lineRange(lines []search.LineMatch) (int, int) {
	if len(lines) == 0 {
		return 1, 1
	}
	start, end := lines[0].Number, lines[0].Number
	for _, line := range lines[1:] {
		start = min(start, line.Number)
		end = max(end, line.Number)
	}
	return start, end
}

func normalizeLimit(value, fallback, maximum int) int {
	if value <= 0 {
		return fallback
	}
	return min(value, maximum)
}

func shortRevision(revision string) string {
	revision = strings.TrimSpace(revision)
	if len(revision) > 8 {
		return revision[:8]
	}
	if revision == "" {
		return "unavailable"
	}
	return revision
}

func repositoryContextLabel(repository catalog.Repository, repositories []catalog.Repository) string {
	label := "@" + repository.Name
	collisions := 0
	for _, candidate := range repositories {
		if strings.EqualFold(strings.TrimSpace(candidate.Name), strings.TrimSpace(repository.Name)) {
			collisions++
		}
	}
	if collisions > 1 {
		label += fmt.Sprintf(" · repository %d", repository.ID)
	}
	return label
}

func compactSearchScopes(scopes []search.Scope) []search.Scope {
	output := make([]search.Scope, 0, len(scopes))
	for _, scope := range scopes {
		covered := false
		for _, existing := range output {
			if searchScopeCovers(existing, scope) {
				covered = true
				break
			}
		}
		if covered {
			continue
		}
		compacted := output[:0]
		for _, existing := range output {
			if !searchScopeCovers(scope, existing) {
				compacted = append(compacted, existing)
			}
		}
		output = append(compacted, scope)
	}
	return output
}

func searchScopeCovers(left, right search.Scope) bool {
	if left.RepositoryID != right.RepositoryID || left.Repository != right.Repository {
		return false
	}
	if left.Path == "" || left.Kind == search.ScopeKindRepository {
		return true
	}
	leftPath := strings.TrimSuffix(filepath.ToSlash(left.Path), "/")
	rightPath := strings.TrimSuffix(filepath.ToSlash(right.Path), "/")
	if left.Kind == search.ScopeKindDirectory {
		return rightPath == leftPath || strings.HasPrefix(rightPath, leftPath+"/")
	}
	if left.Kind == search.ScopeKindFile || left.Kind == "" {
		return rightPath == leftPath
	}
	return left.Kind == search.ScopeKindSymbol &&
		right.Kind == search.ScopeKindSymbol &&
		rightPath == leftPath &&
		left.StartLine == right.StartLine &&
		left.EndLine == right.EndLine &&
		left.Symbol == right.Symbol
}

func filterSearchResultToStructuredScopes(result search.Result, scopes []search.Scope, engineScoped bool) search.Result {
	if len(scopes) == 0 {
		return result
	}
	if engineScoped {
		hasSymbolScope := false
		for _, scope := range scopes {
			if scope.Kind == search.ScopeKindSymbol {
				hasSymbolScope = true
				break
			}
		}
		if !hasSymbolScope {
			return result
		}
	}
	filtered := result
	filtered.Matches = make([]search.FileMatch, 0, len(result.Matches))
	filtered.MatchCount = 0
	for _, match := range result.Matches {
		wholeFile := false
		ranges := make([]search.Scope, 0)
		matchPath := filepath.ToSlash(match.Path)
		for _, scope := range scopes {
			if scope.RepositoryID > 0 && int64(scope.RepositoryID) != match.RepositoryID {
				continue
			}
			scopePath := strings.TrimSuffix(filepath.ToSlash(scope.Path), "/")
			switch scope.Kind {
			case search.ScopeKindRepository:
				wholeFile = true
			case search.ScopeKindDirectory:
				if matchPath == scopePath || strings.HasPrefix(matchPath, scopePath+"/") {
					wholeFile = true
				}
			case search.ScopeKindSymbol:
				if matchPath == scopePath {
					ranges = append(ranges, scope)
				}
			default:
				if matchPath == scopePath {
					wholeFile = true
				}
			}
		}
		if !wholeFile && len(ranges) == 0 {
			continue
		}
		outputMatch := match
		if !wholeFile {
			outputMatch.Lines = outputMatch.Lines[:0]
			for _, line := range match.Lines {
				for _, scope := range ranges {
					if line.Number >= scope.StartLine && line.Number <= scope.EndLine {
						outputMatch.Lines = append(outputMatch.Lines, line)
						break
					}
				}
			}
			if len(outputMatch.Lines) == 0 {
				continue
			}
		}
		for _, line := range outputMatch.Lines {
			filtered.MatchCount += max(1, len(line.Fragments))
		}
		filtered.Matches = append(filtered.Matches, outputMatch)
	}
	filtered.FileCount = len(filtered.Matches)
	filtered.EstimatedFiles = len(filtered.Matches)
	filtered.ReturnedFiles = len(filtered.Matches)
	if filtered.TotalFilesExact {
		filtered.Truncated = false
	}
	return filtered
}

func safeTreePath(value string) (string, error) {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if value == "" {
		return "", nil
	}
	cleaned := path.Clean(value)
	if cleaned != value || path.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.ContainsRune(cleaned, 0) {
		return "", source.ErrUnsafePath
	}
	return cleaned, nil
}

func gitTree(ctx context.Context, repository catalog.Repository, revision, treePath string) ([]TreeEntry, bool, error) {
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	arguments := make([]string, 0, 9)
	if repository.Bare {
		arguments = append(arguments, "--git-dir", repository.Path)
	} else {
		arguments = append(arguments, "-C", repository.Path)
	}
	object := revision
	if treePath != "" {
		object += ":" + treePath
	}
	arguments = append(arguments, "ls-tree", "-z", object)
	command := exec.CommandContext(bounded, "git", arguments...)
	command.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, false, fmt.Errorf("list Git tree: %s", message)
	}
	records := strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00")
	entries := make([]TreeEntry, 0, min(len(records), MaximumTreeEntries))
	truncated := len(records) > MaximumTreeEntries
	for _, record := range records {
		metadata, name, ok := strings.Cut(record, "\t")
		if !ok || name == "" {
			continue
		}
		entryType := "file"
		fields := strings.Fields(metadata)
		if len(fields) >= 2 && fields[1] == "tree" {
			entryType = "directory"
		}
		entryPath := name
		if treePath != "" {
			entryPath = path.Join(treePath, name)
		}
		entries = append(entries, TreeEntry{Type: entryType, Path: entryPath})
		if len(entries) == MaximumTreeEntries {
			break
		}
	}
	sort.SliceStable(entries, func(left, right int) bool {
		if entries[left].Type != entries[right].Type {
			return entries[left].Type == "directory"
		}
		return entries[left].Path < entries[right].Path
	})
	return entries, truncated, nil
}

func gitObjectType(ctx context.Context, repository catalog.Repository, revision, filePath string) (string, error) {
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	arguments := make([]string, 0, 6)
	if repository.Bare {
		arguments = append(arguments, "--git-dir", repository.Path)
	} else {
		arguments = append(arguments, "-C", repository.Path)
	}
	arguments = append(arguments, "cat-file", "-t", revision+":"+filePath)
	command := exec.CommandContext(bounded, "git", arguments...)
	command.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return "", errors.New(message)
	}
	return strings.TrimSpace(string(output)), nil
}

func gitFiles(ctx context.Context, repository catalog.Repository, revision string) ([]string, bool, error) {
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	arguments := make([]string, 0, 8)
	if repository.Bare {
		arguments = append(arguments, "--git-dir", repository.Path)
	} else {
		arguments = append(arguments, "-C", repository.Path)
	}
	arguments = append(arguments, "ls-tree", "-r", "--name-only", "-z", revision)
	command := exec.CommandContext(bounded, "git", arguments...)
	command.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, false, fmt.Errorf("open Git tree output: %w", err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return nil, false, fmt.Errorf("list Git files: %w", err)
	}
	output, readErr := io.ReadAll(io.LimitReader(stdout, maximumContextTreeBytes+1))
	truncated := len(output) > maximumContextTreeBytes
	if truncated && command.Process != nil {
		_ = command.Process.Kill()
	}
	waitErr := command.Wait()
	if readErr != nil {
		return nil, false, fmt.Errorf("read Git file list: %w", readErr)
	}
	if waitErr != nil && !truncated {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = waitErr.Error()
		}
		return nil, false, fmt.Errorf("list Git files: %s", message)
	}
	if truncated {
		output = output[:maximumContextTreeBytes]
		if last := bytes.LastIndexByte(output, 0); last >= 0 {
			output = output[:last+1]
		} else {
			output = nil
		}
	}
	records := strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00")
	files := make([]string, 0, len(records))
	for _, filePath := range records {
		filePath = strings.TrimSpace(filepath.ToSlash(filePath))
		if filePath != "" {
			files = append(files, filePath)
		}
	}
	return files, truncated, nil
}
