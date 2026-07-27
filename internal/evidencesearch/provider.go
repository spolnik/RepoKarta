// Package evidencesearch connects deterministic derived artifacts to unified
// search without introducing package cycles or a second authorization path.
package evidencesearch

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/spolnik/RepoKarta/internal/codeintel"
	"github.com/spolnik/RepoKarta/internal/contextscope"
	"github.com/spolnik/RepoKarta/internal/dependencies"
	"github.com/spolnik/RepoKarta/internal/docs"
	"github.com/spolnik/RepoKarta/internal/graph"
	"github.com/spolnik/RepoKarta/internal/insights"
	"github.com/spolnik/RepoKarta/internal/querylang"
	"github.com/spolnik/RepoKarta/internal/search"
)

type GraphReader interface {
	ReadDependencySnapshot(context.Context, int64) (graph.Snapshot, graph.ArtifactProgress, error)
	ReadRouteSnapshot(context.Context, int64) (graph.Snapshot, graph.ArtifactProgress, error)
}

type DependencyReader interface {
	Inventory(context.Context, graph.Snapshot, dependencies.Options) (dependencies.Inventory, error)
}

type WikiReader interface {
	Pages(context.Context, int64) ([]docs.Page, error)
	Page(context.Context, int64, string) (docs.Page, error)
}

type InsightReader interface {
	Query(context.Context, insights.Filter) (insights.QueryResponse, error)
}

// Provider reads existing permission-aware artifacts and observations.
type Provider struct {
	graphs       GraphReader
	dependencies DependencyReader
	wiki         WikiReader
	insights     InsightReader
	mu           sync.RWMutex
	baseURL      string
}

func New(
	graphs GraphReader,
	dependencyReader DependencyReader,
	wiki WikiReader,
	insightReader InsightReader,
	baseURL string,
) *Provider {
	return &Provider{
		graphs:       graphs,
		dependencies: dependencyReader,
		wiki:         wiki,
		insights:     insightReader,
		baseURL:      strings.TrimRight(baseURL, "/"),
	}
}

func (p *Provider) SetBaseURL(baseURL string) {
	p.mu.Lock()
	p.baseURL = strings.TrimRight(baseURL, "/")
	p.mu.Unlock()
}

func (p *Provider) SearchDerivedEvidence(
	ctx context.Context,
	request codeintel.DerivedEvidenceRequest,
) (codeintel.DerivedEvidenceResult, error) {
	filters, err := compileFilters(request)
	if err != nil {
		return codeintel.DerivedEvidenceResult{}, err
	}
	switch request.ResultType {
	case "dependency":
		return p.searchDependencies(ctx, request, filters)
	case "route":
		return p.searchRoutes(ctx, request, filters)
	case "wiki_page":
		return p.searchWiki(ctx, request, filters)
	case "code_insight":
		return p.searchInsights(ctx, request, filters)
	default:
		return codeintel.DerivedEvidenceResult{}, fmt.Errorf(
			"unsupported derived result type %q",
			request.ResultType,
		)
	}
}

type compiledFilters struct {
	includeText      []string
	excludeText      []string
	includePaths     []string
	excludePaths     []string
	includeFiles     []string
	excludeFiles     []string
	includeLanguages []string
	excludeLanguages []string
	includeOwners    []string
	excludeOwners    []string
}

func compileFilters(request codeintel.DerivedEvidenceRequest) (compiledFilters, error) {
	output := compiledFilters{}
	if value := strings.TrimSpace(request.Path); value != "" {
		output.includePaths = append(output.includePaths, value)
	}
	if value := strings.TrimSpace(request.File); value != "" {
		output.includeFiles = append(output.includeFiles, value)
	}
	if value := strings.TrimSpace(request.Language); value != "" {
		output.includeLanguages = append(output.includeLanguages, value)
	}
	for _, filter := range request.Query.Filters {
		target := func(positive, negative *[]string) {
			if filter.Negative {
				*negative = append(*negative, filter.Value)
			} else {
				*positive = append(*positive, filter.Value)
			}
		}
		switch filter.Field {
		case querylang.FieldContent:
			target(&output.includeText, &output.excludeText)
		case querylang.FieldPath:
			target(&output.includePaths, &output.excludePaths)
		case querylang.FieldFile:
			target(&output.includeFiles, &output.excludeFiles)
		case querylang.FieldLanguage:
			target(&output.includeLanguages, &output.excludeLanguages)
		case querylang.FieldOwner:
			target(&output.includeOwners, &output.excludeOwners)
		case querylang.FieldSymbolKind:
			return output, fmt.Errorf(
				"symbol_kind filters do not apply to %s results",
				request.ResultType,
			)
		case querylang.FieldRepository, querylang.FieldRevision, querylang.FieldResultType:
			// Repository and revision filters were resolved before the provider
			// received the request.
		default:
			return output, fmt.Errorf("unsupported query field %q", filter.Field)
		}
	}
	return output, nil
}

func (p *Provider) searchDependencies(
	ctx context.Context,
	request codeintel.DerivedEvidenceRequest,
	filters compiledFilters,
) (codeintel.DerivedEvidenceResult, error) {
	if len(filters.includeLanguages)+len(filters.excludeLanguages) > 0 {
		return codeintel.DerivedEvidenceResult{}, fmt.Errorf(
			"language filters do not apply to dependency results; use dependency text or manifest paths",
		)
	}
	if len(filters.includeOwners)+len(filters.excludeOwners) > 0 {
		return codeintel.DerivedEvidenceResult{}, fmt.Errorf(
			"owner filters do not apply to dependency results",
		)
	}
	result := exactResult()
	for _, repository := range request.Repositories {
		snapshot, progress, err := p.graphs.ReadDependencySnapshot(ctx, repository.ID)
		if err != nil {
			return result, fmt.Errorf("read dependency evidence for %s: %w", repository.Name, err)
		}
		if progress.PendingRepositories > 0 {
			result.Truncated = true
			result.TotalExact = false
			result.Warnings = append(result.Warnings, artifactWarning("dependency", repository.Name, progress))
		}
		inventory, err := p.dependencies.Inventory(ctx, snapshot, dependencies.Options{
			Limit: dependencies.MaximumPageLimit,
		})
		if err != nil {
			return result, fmt.Errorf("read dependency inventory for %s: %w", repository.Name, err)
		}
		result.Truncated = result.Truncated || inventory.Truncated || inventory.HasMore
		if inventory.Truncated || inventory.HasMore {
			result.TotalExact = false
		}
		for _, declaration := range inventory.Declarations {
			path := firstNonEmpty(declaration.ManifestPath, declaration.Evidence.Path)
			if !pathAllowed(request.Contexts, declaration.RepositoryID, path) ||
				!matchesPath(path, filters) {
				continue
			}
			haystack := strings.Join([]string{
				declaration.Package,
				declaration.Declared,
				declaration.Resolved,
				declaration.LatestStable,
				declaration.Ecosystem,
				declaration.ManifestKind,
				declaration.ManifestPath,
				declaration.Usage,
				declaration.Relationship,
				declaration.Resolution,
				declaration.CheckStatus,
			}, "\n")
			if !matchesText(haystack, request.Query.Text, filters) {
				continue
			}
			revision := firstNonEmpty(declaration.Revision, repository.Revision)
			line := max(1, declaration.Evidence.Line)
			result.Items = append(result.Items, codeintel.SearchItem{
				ResultType:   "dependency",
				RepositoryID: declaration.RepositoryID,
				Revision:     revision,
				Path:         path,
				Title:        declaration.Package,
				Summary:      dependencySummary(declaration),
				Citation: codeintel.Citation(
					repository.Name,
					revision,
					path,
					line,
					line,
				),
				SourceURL: declaration.Evidence.URL,
				Metadata: []codeintel.SearchItemMetadata{
					{Label: "ecosystem", Value: declaration.Ecosystem},
					{Label: "usage", Value: declaration.Usage},
					{Label: "relationship", Value: declaration.Relationship},
					{Label: "resolution", Value: declaration.Resolution},
					{Label: "status", Value: declaration.CheckStatus},
				},
			})
			if len(result.Items) >= request.Limit {
				result.Truncated = true
				result.TotalExact = false
				return result, nil
			}
		}
	}
	return result, nil
}

func (p *Provider) searchRoutes(
	ctx context.Context,
	request codeintel.DerivedEvidenceRequest,
	filters compiledFilters,
) (codeintel.DerivedEvidenceResult, error) {
	if len(filters.includeLanguages)+len(filters.excludeLanguages) > 0 {
		return codeintel.DerivedEvidenceResult{}, fmt.Errorf(
			"language filters are unavailable for route artifacts",
		)
	}
	if len(filters.includeOwners)+len(filters.excludeOwners) > 0 {
		return codeintel.DerivedEvidenceResult{}, fmt.Errorf("owner filters do not apply to route results")
	}
	result := exactResult()
	for _, repository := range request.Repositories {
		snapshot, progress, err := p.graphs.ReadRouteSnapshot(ctx, repository.ID)
		if err != nil {
			return result, fmt.Errorf("read route evidence for %s: %w", repository.Name, err)
		}
		if progress.PendingRepositories > 0 {
			result.Truncated = true
			result.TotalExact = false
			result.Warnings = append(result.Warnings, artifactWarning("route", repository.Name, progress))
		}
		if snapshot.Truncated || snapshot.StructureTruncated {
			result.Truncated = true
			result.TotalExact = false
		}
		for _, node := range snapshot.Nodes {
			if node.Kind != "route" {
				continue
			}
			evidence := firstEvidence(node.Evidence)
			repositoryID := node.RepositoryID
			if repositoryID == 0 {
				repositoryID = repository.ID
			}
			if !pathAllowed(request.Contexts, repositoryID, evidence.Path) ||
				!matchesPath(evidence.Path, filters) {
				continue
			}
			haystack := strings.Join([]string{
				node.Label,
				node.Subtitle,
				node.Layer,
				evidence.Path,
				evidence.Label,
			}, "\n")
			if !matchesText(haystack, request.Query.Text, filters) {
				continue
			}
			revision := firstNonEmpty(evidence.Revision, repository.Revision)
			line := max(1, evidence.Line)
			result.Items = append(result.Items, codeintel.SearchItem{
				ResultType:   "route",
				RepositoryID: repositoryID,
				Revision:     revision,
				Path:         evidence.Path,
				Title:        node.Label,
				Summary:      firstNonEmpty(node.Subtitle, "Served route captured in the repository map"),
				Citation: codeintel.Citation(
					repository.Name,
					revision,
					evidence.Path,
					line,
					line,
				),
				SourceURL: evidence.URL,
				Metadata: []codeintel.SearchItemMetadata{
					{Label: "layer", Value: node.Layer},
					{Label: "evidence", Value: evidence.Label},
				},
			})
			if len(result.Items) >= request.Limit {
				result.Truncated = true
				result.TotalExact = false
				return result, nil
			}
		}
	}
	return result, nil
}

func (p *Provider) searchWiki(
	ctx context.Context,
	request codeintel.DerivedEvidenceRequest,
	filters compiledFilters,
) (codeintel.DerivedEvidenceResult, error) {
	if len(filters.includeLanguages)+len(filters.excludeLanguages) > 0 {
		return codeintel.DerivedEvidenceResult{}, fmt.Errorf("language filters do not apply to Wiki pages")
	}
	if len(filters.includeOwners)+len(filters.excludeOwners) > 0 {
		return codeintel.DerivedEvidenceResult{}, fmt.Errorf("owner filters do not apply to Wiki pages")
	}
	result := exactResult()
	for _, repository := range request.Repositories {
		pages, err := p.wiki.Pages(ctx, repository.ID)
		if err != nil {
			return result, fmt.Errorf("read Wiki manifest for %s: %w", repository.Name, err)
		}
		for _, manifestPage := range pages {
			if manifestPage.Status != docs.StatusReady && manifestPage.Status != docs.StatusStale {
				continue
			}
			page, err := p.wiki.Page(ctx, repository.ID, manifestPage.Slug)
			if err != nil {
				return result, fmt.Errorf(
					"read Wiki page %s in %s: %w",
					manifestPage.Slug,
					repository.Name,
					err,
				)
			}
			if !wikiPathAllowed(request.Contexts, repository.ID, page.SupportingFiles) ||
				!matchesAnyPath(page.SupportingFiles, filters) {
				continue
			}
			haystack := strings.Join([]string{
				page.Slug,
				page.Title,
				page.Summary,
				page.Markdown,
				strings.Join(page.SupportingFiles, "\n"),
			}, "\n")
			if !matchesText(haystack, request.Query.Text, filters) {
				continue
			}
			revision := firstNonEmpty(page.Revision, repository.Revision)
			result.Items = append(result.Items, codeintel.SearchItem{
				ResultType:   "wiki_page",
				RepositoryID: repository.ID,
				Revision:     revision,
				Title:        page.Title,
				Summary:      page.Summary,
				Detail:       boundedDetail(page.Markdown),
				Citation:     repository.Name + "@" + shortRevision(revision) + ":wiki/" + page.Slug,
				SourceURL: p.pageURL(
					"/wiki",
					url.Values{"repository": {strconv.FormatInt(repository.ID, 10)}},
				),
				Metadata: []codeintel.SearchItemMetadata{
					{Label: "page", Value: page.Number},
					{Label: "status", Value: page.Status},
					{Label: "provider", Value: page.Provider},
					{Label: "model", Value: page.Model},
				},
			})
			if len(result.Items) >= request.Limit {
				result.Truncated = true
				result.TotalExact = false
				return result, nil
			}
		}
	}
	return result, nil
}

func (p *Provider) searchInsights(
	ctx context.Context,
	request codeintel.DerivedEvidenceRequest,
	filters compiledFilters,
) (codeintel.DerivedEvidenceResult, error) {
	result := exactResult()
	repositoryIDs := make([]int64, 0, len(request.Repositories))
	for _, repository := range request.Repositories {
		repositoryIDs = append(repositoryIDs, repository.ID)
	}
	response, err := p.insights.Query(ctx, insights.Filter{
		RepositoryIDs: repositoryIDs,
		Limit:         min(5000, max(request.Limit*10, 100)),
	})
	if err != nil {
		return result, fmt.Errorf("read captured code insights: %w", err)
	}
	result.Truncated = response.Truncated
	result.TotalExact = !response.Truncated
	for _, warning := range response.Warnings {
		result.Warnings = append(result.Warnings, search.Warning{
			Code:    "insight_coverage",
			Message: warning,
		})
	}
	repositoryNames := make(map[int64]string, len(request.Repositories))
	repositoryRevisions := make(map[int64]string, len(request.Repositories))
	for _, repository := range request.Repositories {
		repositoryNames[repository.ID] = repository.Name
		repositoryRevisions[repository.ID] = repository.Revision
	}
	for _, observation := range response.Current {
		if !pathAllowed(request.Contexts, observation.RepositoryID, observation.Path) ||
			!matchesPath(observation.Path, filters) ||
			!matchesAllowedValue(observation.Language, filters.includeLanguages, filters.excludeLanguages) ||
			!matchesAllowedValue(observation.Owner, filters.includeOwners, filters.excludeOwners) {
			continue
		}
		haystack := strings.Join([]string{
			observation.Key,
			observation.Message,
			observation.Tool,
			observation.Kind,
			observation.Severity,
			observation.State,
			observation.Language,
			observation.Owner,
			observation.Path,
			observation.Unit,
			insightValue(observation),
		}, "\n")
		if !matchesText(haystack, request.Query.Text, filters) {
			continue
		}
		revision := firstNonEmpty(observation.Revision, repositoryRevisions[observation.RepositoryID])
		summary := observation.Message
		if summary == "" {
			summary = insightValue(observation)
		}
		citation := repositoryNames[observation.RepositoryID] + "@" +
			shortRevision(revision) + ":insight/" + observation.Key
		if observation.Path != "" {
			citation = codeintel.Citation(
				repositoryNames[observation.RepositoryID],
				revision,
				observation.Path,
				max(1, observation.StartLine),
				max(max(1, observation.StartLine), observation.EndLine),
			)
		}
		sourceURL := observation.SourceURL
		if sourceURL == "" {
			sourceURL = p.pageURL(
				"/insights",
				url.Values{"repository": {strconv.FormatInt(observation.RepositoryID, 10)}},
			)
		}
		result.Items = append(result.Items, codeintel.SearchItem{
			ResultType:   "code_insight",
			RepositoryID: observation.RepositoryID,
			Revision:     revision,
			Path:         observation.Path,
			Title:        observation.Key,
			Summary:      summary,
			Citation:     citation,
			SourceURL:    sourceURL,
			Metadata: []codeintel.SearchItemMetadata{
				{Label: "kind", Value: observation.Kind},
				{Label: "tool", Value: observation.Tool},
				{Label: "severity", Value: observation.Severity},
				{Label: "state", Value: observation.State},
				{Label: "language", Value: observation.Language},
				{Label: "owner", Value: observation.Owner},
			},
		})
		if len(result.Items) >= request.Limit {
			result.Truncated = true
			result.TotalExact = false
			return result, nil
		}
	}
	return result, nil
}

func exactResult() codeintel.DerivedEvidenceResult {
	return codeintel.DerivedEvidenceResult{
		Items:      []codeintel.SearchItem{},
		TotalExact: true,
		Warnings:   []search.Warning{},
	}
}

func artifactWarning(kind, repository string, progress graph.ArtifactProgress) search.Warning {
	return search.Warning{
		Code: kind + "_artifact_building",
		Message: fmt.Sprintf(
			"%s evidence for %s is partial while prepared artifacts are building (%d/%d ready).",
			kind,
			repository,
			progress.ReadyRepositories,
			progress.RequestedRepositories,
		),
	}
}

func matchesText(haystack, ordinary string, filters compiledFilters) bool {
	haystack = strings.ToLower(haystack)
	if ordinary = strings.ToLower(strings.TrimSpace(ordinary)); ordinary != "" &&
		!strings.Contains(haystack, ordinary) {
		return false
	}
	if len(filters.includeText) > 0 && !containsAnyFold(haystack, filters.includeText) {
		return false
	}
	for _, exclude := range filters.excludeText {
		if strings.Contains(haystack, strings.ToLower(strings.TrimSpace(exclude))) {
			return false
		}
	}
	return true
}

func matchesPath(value string, filters compiledFilters) bool {
	value = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(value), "\\", "/"))
	if len(filters.includePaths) > 0 {
		paths := make([]string, 0, len(filters.includePaths))
		for _, include := range filters.includePaths {
			paths = append(paths, strings.ReplaceAll(strings.TrimSpace(include), "\\", "/"))
		}
		if !containsAnyFold(value, paths) {
			return false
		}
	}
	fileName := value
	if separator := strings.LastIndex(fileName, "/"); separator >= 0 {
		fileName = fileName[separator+1:]
	}
	if len(filters.includeFiles) > 0 && !containsAnyFold(fileName, filters.includeFiles) {
		return false
	}
	for _, exclude := range filters.excludePaths {
		if strings.Contains(value, strings.ToLower(strings.ReplaceAll(strings.TrimSpace(exclude), "\\", "/"))) {
			return false
		}
	}
	for _, exclude := range filters.excludeFiles {
		if strings.Contains(fileName, strings.ToLower(strings.TrimSpace(exclude))) {
			return false
		}
	}
	return true
}

func matchesAnyPath(values []string, filters compiledFilters) bool {
	if len(filters.includePaths)+len(filters.includeFiles) == 0 {
		for _, value := range values {
			if matchesPath(value, filters) {
				return true
			}
		}
		return len(values) == 0 &&
			len(filters.excludePaths)+len(filters.excludeFiles) == 0
	}
	for _, value := range values {
		if matchesPath(value, filters) {
			return true
		}
	}
	return false
}

func pathAllowed(contexts []contextscope.Context, repositoryID int64, candidate string) bool {
	if len(contexts) == 0 {
		return true
	}
	candidate = strings.Trim(strings.ReplaceAll(candidate, "\\", "/"), "/")
	for _, selected := range contexts {
		if selected.RepositoryID != repositoryID {
			continue
		}
		switch selected.Kind {
		case contextscope.KindRepository:
			return true
		case contextscope.KindFile, contextscope.KindSymbol:
			if strings.EqualFold(strings.Trim(selected.Path, "/"), candidate) {
				return true
			}
		case contextscope.KindDirectory:
			directory := strings.Trim(strings.ReplaceAll(selected.Path, "\\", "/"), "/")
			if strings.EqualFold(candidate, directory) ||
				strings.HasPrefix(strings.ToLower(candidate), strings.ToLower(directory)+"/") {
				return true
			}
		}
	}
	return false
}

func wikiPathAllowed(contexts []contextscope.Context, repositoryID int64, paths []string) bool {
	if len(contexts) == 0 {
		return true
	}
	for _, selected := range contexts {
		if selected.RepositoryID == repositoryID && selected.Kind == contextscope.KindRepository {
			return true
		}
	}
	for _, path := range paths {
		if pathAllowed(contexts, repositoryID, path) {
			return true
		}
	}
	return false
}

func matchesAllowedValue(value string, includes, excludes []string) bool {
	if len(includes) > 0 {
		found := false
		for _, include := range includes {
			if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(include)) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	for _, exclude := range excludes {
		if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(exclude)) {
			return false
		}
	}
	return true
}

func containsAnyFold(haystack string, needles []string) bool {
	haystack = strings.ToLower(haystack)
	for _, needle := range needles {
		if strings.Contains(haystack, strings.ToLower(strings.TrimSpace(needle))) {
			return true
		}
	}
	return false
}

func firstEvidence(values []graph.Evidence) graph.Evidence {
	if len(values) == 0 {
		return graph.Evidence{}
	}
	return values[0]
}

func dependencySummary(declaration dependencies.Declaration) string {
	parts := []string{}
	if declaration.Declared != "" {
		parts = append(parts, "declared "+declaration.Declared)
	}
	if declaration.Resolved != "" {
		parts = append(parts, "resolved "+declaration.Resolved)
	}
	if declaration.LatestStable != "" {
		parts = append(parts, "latest stable "+declaration.LatestStable)
	}
	if len(parts) == 0 {
		return declaration.Resolution
	}
	return strings.Join(parts, " · ")
}

func insightValue(observation insights.Observation) string {
	if observation.Value == nil {
		return ""
	}
	value := strconv.FormatFloat(*observation.Value, 'g', -1, 64)
	if observation.Unit != "" {
		value += " " + observation.Unit
	}
	return value
}

func boundedDetail(value string) string {
	const limit = 4000
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= limit {
		return string(runes)
	}
	return string(runes[:limit]) + "\n…"
}

func shortRevision(revision string) string {
	revision = strings.TrimSpace(revision)
	if len(revision) > 8 {
		return revision[:8]
	}
	return revision
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func (p *Provider) pageURL(route string, values url.Values) string {
	p.mu.RLock()
	baseURL := p.baseURL
	p.mu.RUnlock()
	if query := values.Encode(); query != "" {
		return baseURL + route + "?" + query
	}
	return baseURL + route
}
