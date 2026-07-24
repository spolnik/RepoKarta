// Package codeintel is RepoKarta's protocol-independent, read-only code
// intelligence surface. HTML, JSON, and MCP adapters all use this service.
package codeintel

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/search"
	"github.com/spolnik/RepoKarta/internal/source"
)

const (
	DefaultSearchLimit  = 100
	MaximumSearchLimit  = 500
	DefaultGitLogLimit  = 50
	MaximumGitLogLimit  = 200
	MaximumGitLogBytes  = 1 << 20
	MaximumSourceLines  = 500
	MaximumTreeEntries  = 500
	MaximumDiffBytes    = 1 << 20
	DefaultDiffContext  = 3
	MaximumDiffContext  = 20
	sourceWindowLines   = 200
	sourceContextBefore = 80
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

// Service owns the shared behavior exposed by all external adapters.
type Service struct {
	store    RepositoryStore
	searcher CodeSearcher
	baseURL  string
}

// New creates a protocol-independent code-intelligence service.
func New(store RepositoryStore, searcher CodeSearcher, baseURL string) *Service {
	return &Service{
		store:    store,
		searcher: searcher,
		baseURL:  strings.TrimRight(baseURL, "/"),
	}
}

// Repository describes one indexed repository without exposing its local path.
type Repository struct {
	ID              int64  `json:"id"`
	Name            string `json:"name"`
	OriginURL       string `json:"origin_url,omitempty"`
	DefaultRevision string `json:"default_revision,omitempty"`
	HeadCommit      string `json:"head_commit,omitempty"`
	IndexedCommit   string `json:"indexed_commit,omitempty"`
	IndexState      string `json:"index_state"`
	IndexError      string `json:"index_error,omitempty"`
}

// RepositoryList is the JSON and MCP repository-list contract.
type RepositoryList struct {
	Repositories []Repository `json:"repositories"`
}

// SearchRequest is the shared query contract.
type SearchRequest struct {
	Query      string `json:"query"`
	Repository string `json:"repository,omitempty"`
	Language   string `json:"language,omitempty"`
	Path       string `json:"path,omitempty"`
	File       string `json:"file,omitempty"`
	Mode       string `json:"mode,omitempty"`
	Limit      int    `json:"limit,omitempty"`
}

// SearchResponse explicitly reports evidence completeness.
type SearchResponse struct {
	MatchCount          int              `json:"match_count"`
	MatchingFiles       int              `json:"matching_files"`
	EstimatedTotalFiles int              `json:"estimated_total_files"`
	ReturnedFiles       int              `json:"returned_files"`
	Limit               int              `json:"limit"`
	Truncated           bool             `json:"truncated"`
	TotalFilesExact     bool             `json:"total_files_exact"`
	FilesSkipped        int              `json:"files_skipped"`
	ShardsSkipped       int              `json:"shards_skipped"`
	DurationMS          float64          `json:"duration_ms"`
	Warnings            []search.Warning `json:"warnings,omitempty"`
	Matches             []SearchMatch    `json:"matches"`
}

// SearchMatch is one commit-pinned matched file.
type SearchMatch struct {
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
	Number    int               `json:"number"`
	Text      string            `json:"text"`
	Before    string            `json:"before,omitempty"`
	After     string            `json:"after,omitempty"`
	Fragments []search.Fragment `json:"fragments,omitempty"`
}

// FileRequest selects a bounded, commit-pinned file range.
type FileRequest struct {
	Repository string
	Revision   string
	Path       string
	StartLine  int
	EndLine    int
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
	Repository string
	Revision   string
	Path       string
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
	Repository string
	Revision   string
	Path       string
	Limit      int
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
			IndexState:      repository.IndexState,
			IndexError:      repository.IndexError,
		})
	}
	return output, nil
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
	limit := normalizeLimit(request.Limit, DefaultSearchLimit, MaximumSearchLimit)
	result, err := s.searcher.Search(ctx, search.Query{
		Text:       strings.TrimSpace(request.Query),
		Repository: strings.TrimSpace(request.Repository),
		Language:   strings.TrimSpace(request.Language),
		Path:       strings.TrimSpace(request.Path),
		File:       strings.TrimSpace(request.File),
		Mode:       strings.TrimSpace(request.Mode),
		Limit:      limit,
	})
	if err != nil {
		return SearchResponse{}, err
	}
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
				Number:    line.Number,
				Text:      line.Text,
				Before:    line.Before,
				After:     line.After,
				Fragments: line.Fragments,
			})
		}
		if repository, ok := resolveRepository(repositories, match.Repository, match.Revision); ok {
			start, end := lineRange(match.Lines)
			outputMatch.Repository = repository.Name
			outputMatch.Citation = Citation(repository.Name, match.Revision, match.Path, start, end)
			outputMatch.SourceURL = s.SourceURL(repository.ID, match.Revision, match.Path, start, end)
		}
		output.Matches = append(output.Matches, outputMatch)
	}
	return output, nil
}

// GetFile reads a commit-pinned source range.
func (s *Service) GetFile(ctx context.Context, request FileRequest) (FileResponse, error) {
	repository, err := s.namedRepository(ctx, request.Repository)
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
	repository, err := s.namedRepository(ctx, request.Repository)
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
	repository, err := s.namedRepository(ctx, request.Repository)
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
	repository, err := s.namedRepository(ctx, request.Repository)
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
	return s.baseURL + "/source/" + strconv.FormatInt(repositoryID, 10) + "?" + values.Encode() + "#L" + strconv.Itoa(start)
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

func resolveRepository(repositories []catalog.Repository, searchName, revision string) (catalog.Repository, bool) {
	normalized := strings.ReplaceAll(searchName, "\\", "/")
	for _, repository := range repositories {
		if repository.IndexedCommit != revision && repository.HeadCommit != revision {
			continue
		}
		if normalized == repository.Name || strings.HasSuffix(strings.ToLower(normalized), "/"+strings.ToLower(repository.Name)) {
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
