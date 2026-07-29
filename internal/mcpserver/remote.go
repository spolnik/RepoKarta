package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spolnik/RepoKarta/internal/dependencies"
	"github.com/spolnik/RepoKarta/internal/docs"
	"github.com/spolnik/RepoKarta/internal/graph"
	"github.com/spolnik/RepoKarta/internal/insights"
)

const maximumRemoteResponse = 512 << 20

// RemoteServices gives the stdio transport the same deterministic derived
// artifact tools as the in-process HTTP transport.
type RemoteServices struct {
	baseURL string
	client  *http.Client
}

func NewRemoteServices(baseURL string) *RemoteServices {
	return &RemoteServices{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (r *RemoteServices) get(ctx context.Context, path string, query url.Values, output any) error {
	target := r.baseURL + path
	if encoded := query.Encode(); encoded != "" {
		target += "?" + encoded
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	response, err := r.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		return fmt.Errorf("RepoKarta API %s returned %s: %s", path, response.Status, strings.TrimSpace(string(detail)))
	}
	return json.NewDecoder(io.LimitReader(response.Body, maximumRemoteResponse)).Decode(output)
}

func repositoryQuery(repositoryID int64) url.Values {
	query := url.Values{}
	if repositoryID > 0 {
		query.Set("repository", strconv.FormatInt(repositoryID, 10))
	}
	return query
}

func (r *RemoteServices) RepositoryMap(ctx context.Context, repositoryID int64) (graph.Snapshot, error) {
	var result graph.Snapshot
	err := r.get(ctx, "/api/maps", repositoryQuery(repositoryID), &result)
	return result, err
}

func (r *RemoteServices) CodeReachability(
	ctx context.Context,
	repositoryID int64,
) (graph.ReachabilityReport, error) {
	var result graph.ReachabilityReport
	err := r.get(ctx, "/api/reachability", repositoryQuery(repositoryID), &result)
	return result, err
}

func (r *RemoteServices) DependencySnapshot(ctx context.Context, repositoryID int64) (graph.Snapshot, error) {
	return r.RepositoryMap(ctx, repositoryID)
}

func (r *RemoteServices) TopologySnapshot(
	ctx context.Context,
	repositoryID int64,
) (graph.Snapshot, graph.ArtifactProgress, error) {
	snapshot, err := r.RepositoryMap(ctx, repositoryID)
	return snapshot, graph.ArtifactProgress{State: "ready", RequestedRepositories: 1, ReadyRepositories: 1}, err
}

func (r *RemoteServices) GeneratedDocuments(ctx context.Context, repositoryID int64) (docs.Site, error) {
	var result docs.Site
	err := r.get(ctx, "/api/wiki", repositoryQuery(repositoryID), &result)
	return result, err
}

func (r *RemoteServices) GeneratedDocument(ctx context.Context, repositoryID int64, slug string) (docs.Page, error) {
	var result docs.Page
	err := r.get(
		ctx,
		"/api/wiki/"+strconv.FormatInt(repositoryID, 10)+"/"+url.PathEscape(slug),
		nil,
		&result,
	)
	return result, err
}

func (r *RemoteServices) Query(ctx context.Context, filter insights.Filter) (insights.QueryResponse, error) {
	query := repositoryQuery(filter.RepositoryID)
	query.Set("revision", filter.Revision)
	query.Set("branch", filter.Branch)
	query.Set("directory", filter.Directory)
	query.Set("file", filter.File)
	query.Set("language", filter.Language)
	query.Set("tool", filter.Tool)
	query.Set("rule", filter.Rule)
	query.Set("severity", filter.Severity)
	query.Set("owner", filter.Owner)
	query.Set("kind", filter.Kind)
	if filter.Limit > 0 {
		query.Set("limit", strconv.Itoa(filter.Limit))
	}
	if !filter.Since.IsZero() {
		query.Set("since", filter.Since.Format(time.RFC3339))
	}
	if !filter.Until.IsZero() {
		query.Set("until", filter.Until.Format(time.RFC3339))
	}
	if filter.IncludeQuarantined {
		query.Set("include_quarantined", "true")
	}
	var result insights.QueryResponse
	err := r.get(ctx, "/api/insights", query, &result)
	return result, err
}

func (r *RemoteServices) Compare(
	ctx context.Context,
	repositoryID int64,
	fromRevision, toRevision string,
) (insights.Comparison, error) {
	query := repositoryQuery(repositoryID)
	query.Set("from_revision", fromRevision)
	query.Set("to_revision", toRevision)
	var result insights.Comparison
	err := r.get(ctx, "/api/insights/compare", query, &result)
	return result, err
}

func (r *RemoteServices) EvaluateThresholds(context.Context, int64) ([]insights.ThresholdEvaluation, error) {
	return nil, nil
}

func (r *RemoteServices) Findings(
	ctx context.Context,
	snapshot graph.Snapshot,
	options dependencies.AdvisoryOptions,
) (dependencies.FindingResponse, error) {
	query := repositoryQuery(snapshot.Scope.RequestedRepositoryID)
	query.Set("query", options.Query)
	query.Set("ecosystem", options.Ecosystem)
	query.Set("severity", options.Severity)
	query.Set("usage", options.Usage)
	query.Set("package", options.Package)
	query.Set("offset", strconv.Itoa(options.Offset))
	if options.Limit > 0 {
		query.Set("limit", strconv.Itoa(options.Limit))
	}
	var result dependencies.FindingResponse
	err := r.get(ctx, "/api/dependencies/findings", query, &result)
	return result, err
}

func (r *RemoteServices) Topology(
	ctx context.Context,
	snapshot graph.Snapshot,
	_ graph.ArtifactProgress,
	options dependencies.TopologyOptions,
) (dependencies.Topology, error) {
	query := repositoryQuery(snapshot.Scope.RequestedRepositoryID)
	query.Set("query", options.Query)
	query.Set("protocol", options.Protocol)
	query.Set("origin", options.Origin)
	query.Set("environment", options.Environment)
	query.Set("provider", options.Provider)
	query.Set("direction", options.Direction)
	if options.Depth > 0 {
		query.Set("depth", strconv.Itoa(options.Depth))
	}
	if !options.ObservedFrom.IsZero() {
		query.Set("observed_from", options.ObservedFrom.Format(time.RFC3339))
	}
	if !options.ObservedTo.IsZero() {
		query.Set("observed_to", options.ObservedTo.Format(time.RFC3339))
	}
	var result dependencies.Topology
	err := r.get(ctx, "/api/dependencies/topology", query, &result)
	return result, err
}
