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
)

const (
	maxCitations = 12
)

// Config controls the local MCP endpoint.
type Config struct {
	Version string
	BaseURL string
	Token   string
}

// Intelligence is the shared surface implemented by both the in-process
// service and the JSON API client.
type Intelligence interface {
	Repositories(context.Context) (codeintel.RepositoryList, error)
	Search(context.Context, codeintel.SearchRequest) (codeintel.SearchResponse, error)
	GetFile(context.Context, codeintel.FileRequest) (codeintel.FileResponse, error)
	ListTree(context.Context, codeintel.TreeRequest) (codeintel.TreeResponse, error)
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

	return server
}

// RunStdio serves the same tools over stdio, normally backed by the JSON API.
func RunStdio(ctx context.Context, config Config, intelligence Intelligence) error {
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
