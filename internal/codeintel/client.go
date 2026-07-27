package codeintel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spolnik/RepoKarta/internal/contextscope"
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
	if len(request.Contexts) > 0 || len(request.NamedContextIDs) > 0 || request.UseDefaultContexts != nil {
		var output SearchResponse
		err := c.post(ctx, "/api/search", request, &output)
		return output, err
	}
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
	if request.Compact {
		values.Set("compact", "true")
	}
	var output SearchResponse
	err := c.get(ctx, "/api/search", values, &output)
	return output, err
}

func (c *Client) post(ctx context.Context, endpoint string, input, output any) error {
	content, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("encode RepoKarta API request: %w", err)
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		c.baseURL+endpoint,
		bytes.NewReader(content),
	)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	return c.do(request, output)
}

// FindSymbol calls GET /api/symbol.
func (c *Client) FindSymbol(ctx context.Context, request SymbolRequest) (SymbolResponse, error) {
	if len(request.Contexts) > 0 || len(request.NamedContextIDs) > 0 || request.UseDefaultContexts != nil {
		var output SymbolResponse
		err := c.post(ctx, "/api/symbol", request, &output)
		return output, err
	}
	values := url.Values{
		"symbol": []string{request.Symbol},
		"repo":   []string{repositorySelector(request.RepositoryID, request.Repository)},
		"lang":   []string{request.Language},
	}
	if request.Limit > 0 {
		values.Set("limit", strconv.Itoa(request.Limit))
	}
	if request.Compact {
		values.Set("compact", "true")
	}
	var output SymbolResponse
	err := c.get(ctx, "/api/symbol", values, &output)
	return output, err
}

// FindReferences searches persisted AST relations through the shared search API.
func (c *Client) FindReferences(ctx context.Context, request ReferenceRequest) (ReferenceResponse, error) {
	return c.Search(ctx, SearchRequest{
		Query:              request.Symbol,
		RepositoryID:       request.RepositoryID,
		Repository:         request.Repository,
		Language:           request.Language,
		Path:               request.Path,
		File:               request.File,
		Mode:               "references",
		Limit:              request.Limit,
		Compact:            request.Compact,
		Contexts:           request.Contexts,
		NamedContextIDs:    request.NamedContextIDs,
		UseDefaultContexts: request.UseDefaultContexts,
	})
}

// ListNamedContexts calls GET /api/contexts/named.
func (c *Client) ListNamedContexts(ctx context.Context) (contextscope.NamedContextList, error) {
	var output contextscope.NamedContextList
	err := c.get(ctx, "/api/contexts/named", nil, &output)
	return output, err
}

// ResolveEffectiveContexts expands explicit, named, and default contexts.
func (c *Client) ResolveEffectiveContexts(
	ctx context.Context,
	request contextscope.EffectiveRequest,
) (contextscope.EffectiveResponse, error) {
	var output contextscope.EffectiveResponse
	err := c.post(ctx, "/api/contexts/resolve", request, &output)
	return output, err
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
	if request.Offset > 0 {
		values.Set("offset", strconv.Itoa(request.Offset))
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
	return c.decode(response, err, output)
}

func (c *Client) do(request *http.Request, output any) error {
	response, err := c.client.Do(request)
	return c.decode(response, err, output)
}

func (c *Client) decode(response *http.Response, requestErr error, output any) error {
	if requestErr != nil {
		return requestErr
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
