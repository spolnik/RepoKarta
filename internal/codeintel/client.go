package codeintel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spolnik/RepoKarta/internal/docs"
	"github.com/spolnik/RepoKarta/internal/graph"
)

// Client consumes RepoKarta's JSON API. It lets protocol adapters remain thin
// and keeps capabilities available without MCP.
type Client struct {
	baseURL string
	client  *http.Client
}

// NewClient creates a JSON API client for a running RepoKarta instance.
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

// Repositories calls GET /api/repositories.
func (c *Client) Repositories(ctx context.Context) (RepositoryList, error) {
	var output RepositoryList
	err := c.get(ctx, "/api/repositories", nil, &output)
	return output, err
}

// Search calls GET /api/search.
func (c *Client) Search(ctx context.Context, request SearchRequest) (SearchResponse, error) {
	values := url.Values{
		"q":    []string{request.Query},
		"repo": []string{repositorySelector(request.RepositoryID, request.Repository)},
		"lang": []string{request.Language},
		"path": []string{request.Path},
		"file": []string{request.File},
		"mode": []string{request.Mode},
	}
	if request.Limit > 0 {
		values.Set("limit", strconv.Itoa(request.Limit))
	}
	var output SearchResponse
	err := c.get(ctx, "/api/search", values, &output)
	return output, err
}

// FindSymbol calls GET /api/symbol.
func (c *Client) FindSymbol(ctx context.Context, request SymbolRequest) (SymbolResponse, error) {
	values := url.Values{
		"symbol": []string{request.Symbol},
		"repo":   []string{repositorySelector(request.RepositoryID, request.Repository)},
		"lang":   []string{request.Language},
	}
	if request.Limit > 0 {
		values.Set("limit", strconv.Itoa(request.Limit))
	}
	var output SymbolResponse
	err := c.get(ctx, "/api/symbol", values, &output)
	return output, err
}

// FindReferences searches persisted AST relations through the shared search API.
func (c *Client) FindReferences(ctx context.Context, request ReferenceRequest) (ReferenceResponse, error) {
	return c.Search(ctx, SearchRequest{
		Query:        request.Symbol,
		RepositoryID: request.RepositoryID,
		Repository:   request.Repository,
		Language:     request.Language,
		Path:         request.Path,
		File:         request.File,
		Mode:         "references",
		Limit:        request.Limit,
	})
}

// GetFile calls GET /api/file/{repository}.
func (c *Client) GetFile(ctx context.Context, request FileRequest) (FileResponse, error) {
	values := url.Values{
		"rev":  []string{request.Revision},
		"path": []string{request.Path},
	}
	if request.StartLine > 0 || request.EndLine > 0 {
		values.Set("lines", fmt.Sprintf("%d-%d", request.StartLine, request.EndLine))
	}
	var output FileResponse
	err := c.get(ctx, "/api/file/"+url.PathEscape(repositorySelector(request.RepositoryID, request.Repository)), values, &output)
	return output, err
}

// ListTree calls GET /api/tree/{repository}.
func (c *Client) ListTree(ctx context.Context, request TreeRequest) (TreeResponse, error) {
	values := url.Values{
		"rev":  []string{request.Revision},
		"path": []string{request.Path},
	}
	var output TreeResponse
	err := c.get(ctx, "/api/tree/"+url.PathEscape(repositorySelector(request.RepositoryID, request.Repository)), values, &output)
	return output, err
}

// GitLog calls GET /api/git/log/{repository}.
func (c *Client) GitLog(ctx context.Context, request GitLogRequest) (GitLogResponse, error) {
	values := url.Values{
		"rev":  []string{request.Revision},
		"path": []string{request.Path},
	}
	if request.Limit > 0 {
		values.Set("limit", strconv.Itoa(request.Limit))
	}
	var output GitLogResponse
	err := c.get(ctx, "/api/git/log/"+url.PathEscape(repositorySelector(request.RepositoryID, request.Repository)), values, &output)
	return output, err
}

// GitDiff calls GET /api/git/diff/{repository}.
func (c *Client) GitDiff(ctx context.Context, request GitDiffRequest) (GitDiffResponse, error) {
	values := url.Values{
		"from": []string{request.FromRevision},
		"to":   []string{request.ToRevision},
		"path": []string{request.Path},
	}
	if request.ContextLines > 0 {
		values.Set("context", strconv.Itoa(request.ContextLines))
	}
	var output GitDiffResponse
	err := c.get(ctx, "/api/git/diff/"+url.PathEscape(repositorySelector(request.RepositoryID, request.Repository)), values, &output)
	return output, err
}

// RepositoryMap calls GET /api/maps for a single repository.
func (c *Client) RepositoryMap(ctx context.Context, repositoryID int64) (graph.Snapshot, error) {
	values := url.Values{"repository": []string{strconv.FormatInt(repositoryID, 10)}}
	var output graph.Snapshot
	err := c.get(ctx, "/api/maps", values, &output)
	return output, err
}

// GeneratedDocument calls GET /api/wiki/{repository}/{page}.
func (c *Client) GeneratedDocument(ctx context.Context, repositoryID int64, slug string) (docs.Page, error) {
	endpoint := "/api/wiki/" + strconv.FormatInt(repositoryID, 10) + "/" + url.PathEscape(slug)
	var output docs.Page
	err := c.get(ctx, endpoint, nil, &output)
	return output, err
}

// repositorySelector renders the stable repository ID when the caller supplied
// one and falls back to the repository name.
func repositorySelector(repositoryID int64, name string) string {
	if repositoryID > 0 {
		return strconv.FormatInt(repositoryID, 10)
	}
	return name
}

func (c *Client) get(ctx context.Context, endpoint string, values url.Values, output any) error {
	requestURL := c.baseURL + endpoint
	if len(values) > 0 {
		requestURL += "?" + values.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return err
	}
	response, err := c.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var apiError struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.NewDecoder(response.Body).Decode(&apiError); err == nil && apiError.Error.Message != "" {
			return fmt.Errorf("RepoKarta API: %s", apiError.Error.Message)
		}
		return fmt.Errorf("RepoKarta API: %s", response.Status)
	}
	if err := json.NewDecoder(response.Body).Decode(output); err != nil {
		return fmt.Errorf("decode RepoKarta API response: %w", err)
	}
	return nil
}
