// Package mcpserver exposes RepoKarta's deterministic, read-only code tools.
package mcpserver

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"sort"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spolnik/RepoKarta/internal/agent"
	"github.com/spolnik/RepoKarta/internal/codeintel"
	"github.com/spolnik/RepoKarta/internal/docs"
	"github.com/spolnik/RepoKarta/internal/graph"
)

const (
	maxCitations = 12
)

// Config controls the local MCP endpoint.
type Config struct {
	Version   string
	BaseURL   string
	Token     string
	Artifacts ArtifactReader
}

// Intelligence is the shared surface implemented by both the in-process
// service and the JSON API client.
type Intelligence interface {
	Repositories(context.Context) (codeintel.RepositoryList, error)
	Search(context.Context, codeintel.SearchRequest) (codeintel.SearchResponse, error)
	FindSymbol(context.Context, codeintel.SymbolRequest) (codeintel.SymbolResponse, error)
	GetFile(context.Context, codeintel.FileRequest) (codeintel.FileResponse, error)
	ListTree(context.Context, codeintel.TreeRequest) (codeintel.TreeResponse, error)
	GitLog(context.Context, codeintel.GitLogRequest) (codeintel.GitLogResponse, error)
	GitDiff(context.Context, codeintel.GitDiffRequest) (codeintel.GitDiffResponse, error)
}

// ArtifactReader exposes the two higher-level, evidence-backed M3/M4 artifacts.
type ArtifactReader interface {
	RepositoryMap(context.Context, int64) (graph.Snapshot, error)
	GeneratedDocument(context.Context, int64, string) (docs.Page, error)
}

// MapReader supplies commit-pinned structural maps.
type MapReader interface {
	Snapshot(context.Context, int64, bool) (graph.Snapshot, error)
}

// DocumentReader supplies generated repository pages.
type DocumentReader interface {
	Page(context.Context, int64, string) (docs.Page, error)
}

// Artifacts adapts in-process map and documentation services to MCP tools.
type Artifacts struct {
	Maps      MapReader
	Documents DocumentReader
}

// RepositoryMap reads one cached or deterministically generated repository map.
func (a Artifacts) RepositoryMap(ctx context.Context, repositoryID int64) (graph.Snapshot, error) {
	return a.Maps.Snapshot(ctx, repositoryID, false)
}

// GeneratedDocument reads one persisted generated page.
func (a Artifacts) GeneratedDocument(ctx context.Context, repositoryID int64, slug string) (docs.Page, error) {
	return a.Documents.Page(ctx, repositoryID, slug)
}

// NewToken generates an unguessable bearer token for the loopback MCP endpoint.
func NewToken() (string, error) {
	var bytes [32]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}

// CitationTracker records exact source URLs returned by MCP tools.
type CitationTracker struct {
	mu       sync.Mutex
	byThread map[string]map[string]agent.Citation
}

// NewCitationTracker constructs an empty citation recorder.
func NewCitationTracker() *CitationTracker {
	return &CitationTracker{byThread: make(map[string]map[string]agent.Citation)}
}

// Record adds one exact citation to a conversation.
func (t *CitationTracker) Record(conversationID string, citation agent.Citation) {
	if t == nil || conversationID == "" || citation.URL == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.byThread[conversationID] == nil {
		t.byThread[conversationID] = make(map[string]agent.Citation)
	}
	t.byThread[conversationID][citation.URL] = citation
}

// List returns a stable copy of citations observed for a conversation.
func (t *CitationTracker) List(conversationID string) []agent.Citation {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	values := make([]agent.Citation, 0, len(t.byThread[conversationID]))
	for _, citation := range t.byThread[conversationID] {
		values = append(values, citation)
	}
	sort.Slice(values, func(left, right int) bool {
		return values[left].URL < values[right].URL
	})
	if len(values) > maxCitations {
		values = values[:maxCitations]
	}
	return values
}

// Clear discards citations for one ephemeral turn.
func (t *CitationTracker) Clear(conversationID string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	delete(t.byThread, conversationID)
	t.mu.Unlock()
}

// NewHandler builds an authenticated Streamable HTTP MCP server.
func NewHandler(config Config, intelligence Intelligence, trackers ...*CitationTracker) http.Handler {
	var tracker *CitationTracker
	if len(trackers) > 0 {
		tracker = trackers[0]
	}
	handler := mcp.NewStreamableHTTPHandler(func(request *http.Request) *mcp.Server {
		return newServer(config, intelligence, tracker, request.URL.Query().Get("conversation_id"))
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	return bearerAuth(config.Token, handler)
}

func newServer(config Config, intelligence Intelligence, tracker *CitationTracker, conversationID string) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "repokarta",
		Title:   "RepoKarta local code intelligence",
		Version: config.Version,
	}, nil)
	readOnly := &mcp.ToolAnnotations{
		ReadOnlyHint:   true,
		IdempotentHint: true,
		OpenWorldHint:  boolPointer(false),
	}

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_repositories",
		Title:       "List indexed repositories",
		Description: "List the local Git repositories RepoKarta can search. Results include pinned indexed commits.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ listRepositoriesInput) (*mcp.CallToolResult, listRepositoriesOutput, error) {
		repositories, err := intelligence.Repositories(ctx)
		if err != nil {
			return nil, listRepositoriesOutput{}, err
		}
		return nil, repositories, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_code",
		Title:       "Search indexed code",
		Description: "Search committed source across local repositories. Supports literal, regex, and Zoekt syntax (repo:, lang:, file:, sym:, booleans, and negation). Completeness, skipped work, warnings, and pinned source URLs are explicit.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input searchCodeInput) (*mcp.CallToolResult, searchCodeOutput, error) {
		result, err := intelligence.Search(ctx, codeintel.SearchRequest{
			Query:      input.Query,
			Repository: input.Repository,
			Language:   input.Language,
			Path:       input.Path,
			File:       input.File,
			Mode:       input.Mode,
			Limit:      input.Limit,
		})
		if err != nil {
			return nil, searchCodeOutput{}, err
		}
		for _, match := range result.Matches {
			tracker.Record(conversationID, agent.Citation{Label: match.Citation, URL: match.SourceURL})
		}
		return nil, result, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_file",
		Title:       "Get committed source",
		Description: "Read a bounded line range from a repository at its pinned indexed commit. Returns numbered source and a citation URL.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input openFileInput) (*mcp.CallToolResult, openFileOutput, error) {
		file, err := intelligence.GetFile(ctx, codeintel.FileRequest{
			Repository: input.Repository,
			Revision:   input.Revision,
			Path:       input.Path,
			StartLine:  input.StartLine,
			EndLine:    input.EndLine,
		})
		if err != nil {
			return nil, openFileOutput{}, err
		}
		tracker.Record(conversationID, agent.Citation{Label: file.Citation, URL: file.SourceURL})
		return nil, file, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "find_symbol",
		Title:       "Find indexed symbols",
		Description: "Find definitions and other indexed symbol occurrences by exact symbol name. Results are bounded, commit-pinned, and include explicit warnings when Universal Ctags symbol indexing is unavailable.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input findSymbolInput) (*mcp.CallToolResult, findSymbolOutput, error) {
		result, err := intelligence.FindSymbol(ctx, codeintel.SymbolRequest{
			Symbol:     input.Symbol,
			Repository: input.Repository,
			Language:   input.Language,
			Limit:      input.Limit,
		})
		if err != nil {
			return nil, findSymbolOutput{}, err
		}
		for _, match := range result.Matches {
			tracker.Record(conversationID, agent.Citation{Label: match.Citation, URL: match.SourceURL})
		}
		return nil, result, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_tree",
		Title:       "List repository tree",
		Description: "List files and directories at a repository's pinned indexed commit. This never reads the worktree.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input listTreeInput) (*mcp.CallToolResult, listTreeOutput, error) {
		tree, err := intelligence.ListTree(ctx, codeintel.TreeRequest{
			Repository: input.Repository,
			Revision:   input.Revision,
			Path:       input.Path,
		})
		if err != nil {
			return nil, listTreeOutput{}, err
		}
		return nil, tree, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "git_log",
		Title:       "Read repository history",
		Description: "List newest-first commits reachable from a repository's pinned indexed or HEAD commit. Optionally filter history to one path. Returns exact commit SHAs and explicit truncation metadata.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input gitLogInput) (*mcp.CallToolResult, gitLogOutput, error) {
		history, err := intelligence.GitLog(ctx, codeintel.GitLogRequest{
			Repository: input.Repository,
			Revision:   input.Revision,
			Path:       input.Path,
			Limit:      input.Limit,
		})
		if err != nil {
			return nil, gitLogOutput{}, err
		}
		return nil, history, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "git_diff",
		Title:       "Read a historical diff",
		Description: "Read a bounded unified patch between exact reachable commits. Omit to_revision for the indexed commit; omit from_revision to compare its first parent. Returns change counts and explicit patch truncation metadata.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input gitDiffInput) (*mcp.CallToolResult, gitDiffOutput, error) {
		diff, err := intelligence.GitDiff(ctx, codeintel.GitDiffRequest{
			Repository:   input.Repository,
			FromRevision: input.FromRevision,
			ToRevision:   input.ToRevision,
			Path:         input.Path,
			ContextLines: input.ContextLines,
		})
		if err != nil {
			return nil, gitDiffOutput{}, err
		}
		return nil, diff, nil
	})

	if config.Artifacts != nil {
		mcp.AddTool(server, &mcp.Tool{
			Name:        "read_repository_map",
			Title:       "Read repository map",
			Description: "Read the deterministic, commit-pinned structural map for one repository. Every node and edge includes exact source evidence.",
			Annotations: readOnly,
		}, func(ctx context.Context, _ *mcp.CallToolRequest, input readRepositoryMapInput) (*mcp.CallToolResult, readRepositoryMapOutput, error) {
			snapshot, err := config.Artifacts.RepositoryMap(ctx, input.RepositoryID)
			if err != nil {
				return nil, readRepositoryMapOutput{}, err
			}
			for _, node := range snapshot.Nodes {
				for _, evidence := range node.Evidence {
					tracker.Record(conversationID, agent.Citation{
						Label: evidence.Repository + "@" + shortRevision(evidence.Revision) + ":" + evidence.Path,
						URL:   evidence.URL,
					})
				}
			}
			return nil, snapshot, nil
		})

		mcp.AddTool(server, &mcp.Tool{
			Name:        "read_generated_document",
			Title:       "Read generated documentation",
			Description: "Read one persisted repository wiki page with its source revision, generation status, supporting files, citations, and Markdown.",
			Annotations: readOnly,
		}, func(ctx context.Context, _ *mcp.CallToolRequest, input readGeneratedDocumentInput) (*mcp.CallToolResult, readGeneratedDocumentOutput, error) {
			page, err := config.Artifacts.GeneratedDocument(ctx, input.RepositoryID, input.Page)
			if err != nil {
				return nil, readGeneratedDocumentOutput{}, err
			}
			for _, evidence := range page.Citations {
				tracker.Record(conversationID, agent.Citation{
					Label: evidence.Repository + "@" + shortRevision(evidence.Revision) + ":" + evidence.Path,
					URL:   evidence.URL,
				})
			}
			return nil, page, nil
		})
	}

	return server
}

// RunStdio serves the same tools over stdio, normally backed by the JSON API.
func RunStdio(ctx context.Context, config Config, intelligence Intelligence) error {
	if artifacts, ok := intelligence.(ArtifactReader); ok {
		config.Artifacts = artifacts
	}
	err := newServer(config, intelligence, nil, "").Run(ctx, &mcp.StdioTransport{})
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

type listRepositoriesInput struct{}
type listRepositoriesOutput = codeintel.RepositoryList

type searchCodeInput struct {
	Query      string `json:"query" jsonschema:"required,The source text symbol or regular expression to find."`
	Repository string `json:"repository,omitempty" jsonschema:"Optional exact repository name."`
	Language   string `json:"language,omitempty" jsonschema:"Optional programming language filter."`
	Path       string `json:"path,omitempty" jsonschema:"Optional substring required in the path."`
	File       string `json:"file,omitempty" jsonschema:"Optional substring required in the filename."`
	Mode       string `json:"mode,omitempty" jsonschema:"Search mode: literal regex or zoekt."`
	Limit      int    `json:"limit,omitempty" jsonschema:"Maximum files to return from 1 to 500. Defaults to 100."`
}
type searchCodeOutput = codeintel.SearchResponse

type readRepositoryMapInput struct {
	RepositoryID int64 `json:"repository_id" jsonschema:"required,Repository ID returned by list_repositories."`
}

type readRepositoryMapOutput = graph.Snapshot

type readGeneratedDocumentInput struct {
	RepositoryID int64  `json:"repository_id" jsonschema:"required,Repository ID returned by list_repositories."`
	Page         string `json:"page" jsonschema:"required,Page slug from the documentation plan such as overview architecture or dependencies."`
}

type readGeneratedDocumentOutput = docs.Page

type findSymbolInput struct {
	Symbol     string `json:"symbol" jsonschema:"required,Exact symbol name to find."`
	Repository string `json:"repository,omitempty" jsonschema:"Optional exact repository name."`
	Language   string `json:"language,omitempty" jsonschema:"Optional programming language filter."`
	Limit      int    `json:"limit,omitempty" jsonschema:"Maximum files to return from 1 to 500. Defaults to 100."`
}

type findSymbolOutput = codeintel.SymbolResponse

type openFileInput struct {
	Repository string `json:"repository" jsonschema:"required,Exact repository name returned by list_repositories."`
	Path       string `json:"path" jsonschema:"required,Repository-relative source path."`
	Revision   string `json:"revision,omitempty" jsonschema:"Pinned indexed commit. Omit to use the current indexed commit."`
	StartLine  int    `json:"start_line,omitempty" jsonschema:"First one-based line to return."`
	EndLine    int    `json:"end_line,omitempty" jsonschema:"Last one-based line to return. At most 500 lines."`
}

type openFileOutput = codeintel.FileResponse

type listTreeInput struct {
	Repository string `json:"repository" jsonschema:"required,Exact repository name returned by list_repositories."`
	Path       string `json:"path,omitempty" jsonschema:"Optional repository-relative directory."`
	Revision   string `json:"revision,omitempty" jsonschema:"Pinned indexed commit. Omit to use the current indexed commit."`
}

type listTreeOutput = codeintel.TreeResponse

type gitLogInput struct {
	Repository string `json:"repository" jsonschema:"required,Exact repository name returned by list_repositories."`
	Revision   string `json:"revision,omitempty" jsonschema:"Exact reachable commit SHA to start from. Omit to use the indexed commit."`
	Path       string `json:"path,omitempty" jsonschema:"Optional repository-relative file or directory whose history should be returned."`
	Limit      int    `json:"limit,omitempty" jsonschema:"Maximum commits to return from 1 to 200. Defaults to 50."`
}

type gitLogOutput = codeintel.GitLogResponse

type gitDiffInput struct {
	Repository   string `json:"repository" jsonschema:"required,Exact repository name returned by list_repositories."`
	FromRevision string `json:"from_revision,omitempty" jsonschema:"Exact reachable base commit SHA. Omit to use the first parent of to_revision."`
	ToRevision   string `json:"to_revision,omitempty" jsonschema:"Exact reachable target commit SHA. Omit to use the indexed commit."`
	Path         string `json:"path,omitempty" jsonschema:"Optional repository-relative file or directory to diff."`
	ContextLines int    `json:"context_lines,omitempty" jsonschema:"Unified diff context lines from 1 to 20. Defaults to 3."`
}

type gitDiffOutput = codeintel.GitDiffResponse

func bearerAuth(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		expected := "Bearer " + token
		actual := request.Header.Get("Authorization")
		if token == "" || len(actual) != len(expected) || subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) != 1 {
			response.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(response, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(response, request)
	})
}

func boolPointer(value bool) *bool {
	return &value
}

func shortRevision(value string) string {
	if len(value) > 8 {
		return value[:8]
	}
	return value
}
