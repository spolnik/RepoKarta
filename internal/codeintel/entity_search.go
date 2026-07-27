package codeintel

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/contextscope"
	"github.com/spolnik/RepoKarta/internal/querylang"
	"github.com/spolnik/RepoKarta/internal/search"
)

const (
	maximumUnifiedDiffResults = 25
	maximumSearchItemDetail   = 12_000
)

type entitySearchSelection struct {
	repositories []catalog.Repository
	contexts     []contextscope.Context
	named        []contextscope.NamedContext
	includeText  []string
	excludeText  []string
	path         string
}

func (s *Service) searchEntityEvidence(
	ctx context.Context,
	request SearchRequest,
	parsed querylang.Query,
	resultType string,
) (SearchResponse, error) {
	if mode := strings.TrimSpace(request.Mode); mode != "" && !strings.EqualFold(mode, "literal") {
		return SearchResponse{}, fmt.Errorf("%s results currently support literal query mode", resultType)
	}
	selection, err := s.selectEntitySearchEvidence(ctx, request, parsed, resultType)
	if err != nil {
		return SearchResponse{}, err
	}
	limit := normalizeLimit(request.Limit, DefaultSearchLimit, MaximumSearchLimit)
	switch resultType {
	case "repository":
		return s.searchRepositories(parsed, selection, limit), nil
	case "commit":
		return s.searchCommits(ctx, parsed, selection, limit)
	case "diff":
		return s.searchDiffs(ctx, parsed, selection, limit)
	default:
		return SearchResponse{}, fmt.Errorf("unsupported entity result type %q", resultType)
	}
}

func (s *Service) selectEntitySearchEvidence(
	ctx context.Context,
	request SearchRequest,
	parsed querylang.Query,
	resultType string,
) (entitySearchSelection, error) {
	if strings.TrimSpace(request.Language) != "" {
		return entitySearchSelection{}, fmt.Errorf("language filters do not apply to %s results", resultType)
	}
	visible, err := s.store.ListRepositories(ctx)
	if err != nil {
		return entitySearchSelection{}, err
	}
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
		return entitySearchSelection{}, err
	}
	if len(effective.Contexts) > 0 &&
		(request.RepositoryID > 0 || strings.TrimSpace(request.Repository) != "") {
		return entitySearchSelection{}, errors.New(
			"structured contexts cannot be combined with the legacy repository selector",
		)
	}
	contextRepositoryIDs := make(map[int64]struct{}, len(effective.Contexts))
	for _, resolved := range effective.Contexts {
		if resolved.Kind != contextscope.KindRepository {
			return entitySearchSelection{}, fmt.Errorf(
				"%s results currently require repository contexts; %s context %q would broaden the query",
				resultType,
				resolved.Kind,
				resolved.Label,
			)
		}
		contextRepositoryIDs[resolved.RepositoryID] = struct{}{}
	}

	var positiveRepositories, negativeRepositories []string
	var positiveRevisions, negativeRevisions []string
	selection := entitySearchSelection{
		contexts:    effective.Contexts,
		named:       effective.NamedContexts,
		includeText: []string{},
		excludeText: []string{},
	}
	var paths []string
	if value := strings.TrimSpace(request.Path); value != "" {
		paths = append(paths, value)
	}
	if value := strings.TrimSpace(request.File); value != "" {
		paths = append(paths, value)
	}
	for _, filter := range parsed.Filters {
		target := func(positive, negative *[]string) {
			if filter.Negative {
				*negative = append(*negative, filter.Value)
			} else {
				*positive = append(*positive, filter.Value)
			}
		}
		switch filter.Field {
		case querylang.FieldContent:
			target(&selection.includeText, &selection.excludeText)
		case querylang.FieldRepository:
			target(&positiveRepositories, &negativeRepositories)
		case querylang.FieldRevision:
			target(&positiveRevisions, &negativeRevisions)
		case querylang.FieldPath, querylang.FieldFile:
			if resultType == "repository" {
				return entitySearchSelection{}, fmt.Errorf("%s filters do not apply to repository results", filter.Field)
			}
			if filter.Negative {
				return entitySearchSelection{}, fmt.Errorf(
					"negative %s filters are not connected to Git history evidence yet",
					filter.Field,
				)
			}
			paths = append(paths, filter.Value)
		case querylang.FieldLanguage, querylang.FieldSymbolKind, querylang.FieldOwner:
			return entitySearchSelection{}, fmt.Errorf(
				"%s filters do not apply to %s results",
				filter.Field,
				resultType,
			)
		case querylang.FieldResultType:
			// Already validated by requestedResultType.
		default:
			return entitySearchSelection{}, fmt.Errorf("unsupported query field %q", filter.Field)
		}
	}
	paths = compactFoldedStrings(paths)
	if len(paths) > 1 {
		return entitySearchSelection{}, fmt.Errorf(
			"%s results currently accept one path or filename filter",
			resultType,
		)
	}
	if len(paths) == 1 {
		selection.path = strings.TrimSpace(strings.ReplaceAll(paths[0], "\\", "/"))
	}

	positiveIDs, err := repositoryFilterIDs(visible, positiveRepositories)
	if err != nil {
		return entitySearchSelection{}, err
	}
	revisionIDs, err := revisionFilterIDs(visible, positiveRevisions)
	if err != nil {
		return entitySearchSelection{}, err
	}
	positiveIDs = intersectOptionalIDs(positiveIDs, revisionIDs)
	positiveActive := len(positiveRepositories) > 0 || len(positiveRevisions) > 0
	negativeIDs, err := repositoryFilterIDs(visible, negativeRepositories)
	if err != nil {
		return entitySearchSelection{}, err
	}
	negativeRevisionIDs, err := revisionFilterIDs(visible, negativeRevisions)
	if err != nil {
		return entitySearchSelection{}, err
	}
	denied := idSet(unionIDs(negativeIDs, negativeRevisionIDs))
	allowed := idSet(positiveIDs)

	var legacyID int64
	if request.RepositoryID > 0 || strings.TrimSpace(request.Repository) != "" {
		repository, selectErr := s.selectRepository(ctx, request.RepositoryID, request.Repository)
		if selectErr != nil {
			return entitySearchSelection{}, selectErr
		}
		legacyID = repository.ID
	}
	for _, repository := range visible {
		if legacyID > 0 && repository.ID != legacyID {
			continue
		}
		if len(contextRepositoryIDs) > 0 {
			if _, ok := contextRepositoryIDs[repository.ID]; !ok {
				continue
			}
		}
		if _, excluded := denied[uint32(repository.ID)]; excluded {
			continue
		}
		if positiveActive {
			if _, included := allowed[uint32(repository.ID)]; !included {
				continue
			}
		}
		selection.repositories = append(selection.repositories, repository)
	}
	return selection, nil
}

func (s *Service) searchRepositories(
	parsed querylang.Query,
	selection entitySearchSelection,
	limit int,
) SearchResponse {
	items := make([]SearchItem, 0, min(limit, len(selection.repositories)))
	total := 0
	for _, repository := range selection.repositories {
		haystack := strings.Join([]string{
			repository.Name,
			repository.OriginURL,
			repository.DefaultRevision,
			repository.HeadCommit,
			repository.IndexedCommit,
			repository.ScanState,
			repository.IndexState,
		}, "\n")
		if !matchesEntityText(haystack, parsed.Text, selection.includeText, selection.excludeText) {
			continue
		}
		total++
		if len(items) >= limit {
			continue
		}
		revision := strings.TrimSpace(repository.IndexedCommit)
		if revision == "" {
			revision = strings.TrimSpace(repository.HeadCommit)
		}
		items = append(items, SearchItem{
			ResultType:   "repository",
			RepositoryID: repository.ID,
			Repository:   repository.Name,
			Revision:     revision,
			Title:        repository.Name,
			Summary:      repository.OriginURL,
			Citation:     entityCitation(repository.Name, revision, "repository"),
			SourceURL:    s.entityURL("/maps", url.Values{"repository": {strconv.FormatInt(repository.ID, 10)}}),
			Metadata: []SearchItemMetadata{
				{Label: "default revision", Value: repository.DefaultRevision},
				{Label: "scan state", Value: repository.ScanState},
				{Label: "index state", Value: repository.IndexState},
			},
		})
	}
	return entitySearchResponse(
		"repository",
		parsed,
		selection,
		items,
		limit,
		total > len(items),
		nil,
	)
}

func (s *Service) searchCommits(
	ctx context.Context,
	parsed querylang.Query,
	selection entitySearchSelection,
	limit int,
) (SearchResponse, error) {
	items := make([]SearchItem, 0, limit)
	truncated := false
	for _, repository := range selection.repositories {
		history, err := s.GitLog(ctx, GitLogRequest{
			RepositoryID: repository.ID,
			Path:         selection.path,
			Limit:        min(MaximumGitLogLimit, limit+1),
		})
		if err != nil {
			return SearchResponse{}, fmt.Errorf("search commits in %s: %w", repository.Name, err)
		}
		truncated = truncated || history.Truncated || history.OutputTruncated
		for _, commit := range history.Commits {
			haystack := strings.Join([]string{
				commit.Revision,
				commit.AuthorName,
				commit.AuthorEmail,
				commit.AuthoredAt,
				commit.Subject,
				commit.Body,
			}, "\n")
			if !matchesEntityText(haystack, parsed.Text, selection.includeText, selection.excludeText) {
				continue
			}
			items = append(items, SearchItem{
				ResultType:   "commit",
				RepositoryID: repository.ID,
				Repository:   repository.Name,
				Revision:     commit.Revision,
				Path:         selection.path,
				Title:        commit.Subject,
				Summary:      commit.Body,
				Citation:     entityCitation(repository.Name, commit.Revision, "commit"),
				SourceURL: s.entityURL(
					"/api/git/log/"+strconv.FormatInt(repository.ID, 10),
					url.Values{"revision": {commit.Revision}, "limit": {"1"}},
				),
				Metadata: []SearchItemMetadata{
					{Label: "author", Value: commit.AuthorName + " <" + commit.AuthorEmail + ">"},
					{Label: "authored", Value: commit.AuthoredAt},
					{Label: "parents", Value: strings.Join(commit.Parents, " ")},
				},
			})
		}
	}
	sort.SliceStable(items, func(left, right int) bool {
		return searchItemMetadata(items[left], "authored") > searchItemMetadata(items[right], "authored")
	})
	if len(items) > limit {
		items = items[:limit]
		truncated = true
	}
	return entitySearchResponse("commit", parsed, selection, items, limit, truncated, nil), nil
}

func (s *Service) searchDiffs(
	ctx context.Context,
	parsed querylang.Query,
	selection entitySearchSelection,
	limit int,
) (SearchResponse, error) {
	effectiveLimit := min(limit, maximumUnifiedDiffResults)
	warnings := []search.Warning{}
	if limit > effectiveLimit {
		warnings = append(warnings, search.Warning{
			Code: "diff_search_limit",
			Message: fmt.Sprintf(
				"Diff search is bounded to %d returned patches per request.",
				maximumUnifiedDiffResults,
			),
		})
	}
	items := make([]SearchItem, 0, effectiveLimit)
	truncated := limit > effectiveLimit
	for _, repository := range selection.repositories {
		historyLimit := min(MaximumGitLogLimit, max(20, effectiveLimit*4))
		history, err := s.GitLog(ctx, GitLogRequest{
			RepositoryID: repository.ID,
			Path:         selection.path,
			Limit:        historyLimit,
		})
		if err != nil {
			return SearchResponse{}, fmt.Errorf("enumerate diffs in %s: %w", repository.Name, err)
		}
		truncated = truncated || history.Truncated || history.OutputTruncated
		for _, commit := range history.Commits {
			if len(items) >= effectiveLimit {
				truncated = true
				break
			}
			diff, err := s.GitDiff(ctx, GitDiffRequest{
				RepositoryID: repository.ID,
				ToRevision:   commit.Revision,
				Path:         selection.path,
				ContextLines: DefaultDiffContext,
			})
			if err != nil {
				return SearchResponse{}, fmt.Errorf(
					"search diff %s in %s: %w",
					itemShortRevision(commit.Revision),
					repository.Name,
					err,
				)
			}
			haystack := strings.Join([]string{
				commit.Revision,
				commit.AuthorName,
				commit.AuthorEmail,
				commit.Subject,
				commit.Body,
				diff.Patch,
			}, "\n")
			if !matchesEntityText(haystack, parsed.Text, selection.includeText, selection.excludeText) {
				continue
			}
			truncated = truncated || diff.Truncated
			title := commit.Subject
			if title == "" {
				title = "Diff " + itemShortRevision(commit.Revision)
			}
			values := url.Values{"to": {commit.Revision}}
			if diff.FromRevision != "" {
				values.Set("from", diff.FromRevision)
			}
			if selection.path != "" {
				values.Set("path", selection.path)
			}
			items = append(items, SearchItem{
				ResultType:   "diff",
				RepositoryID: repository.ID,
				Repository:   repository.Name,
				Revision:     commit.Revision,
				Path:         selection.path,
				Title:        title,
				Summary: fmt.Sprintf(
					"%d files changed, %d insertions, %d deletions",
					diff.FilesChanged,
					diff.Insertions,
					diff.Deletions,
				),
				Detail:   boundedItemDetail(diff.Patch),
				Citation: entityCitation(repository.Name, commit.Revision, "diff"),
				SourceURL: s.entityURL(
					"/api/git/diff/"+strconv.FormatInt(repository.ID, 10),
					values,
				),
				Metadata: []SearchItemMetadata{
					{Label: "author", Value: commit.AuthorName + " <" + commit.AuthorEmail + ">"},
					{Label: "authored", Value: commit.AuthoredAt},
					{Label: "from", Value: diff.FromRevision},
					{Label: "to", Value: diff.ToRevision},
				},
			})
		}
	}
	return entitySearchResponse(
		"diff",
		parsed,
		selection,
		items,
		effectiveLimit,
		truncated,
		warnings,
	), nil
}

func entitySearchResponse(
	resultType string,
	parsed querylang.Query,
	selection entitySearchSelection,
	items []SearchItem,
	limit int,
	truncated bool,
	warnings []search.Warning,
) SearchResponse {
	if items == nil {
		items = []SearchItem{}
	}
	if warnings == nil {
		warnings = []search.Warning{}
	}
	return SearchResponse{
		MatchCount:      len(items),
		ReturnedItems:   len(items),
		Limit:           limit,
		Truncated:       truncated,
		TotalFilesExact: !truncated,
		Warnings:        warnings,
		Matches:         []SearchMatch{},
		Items:           items,
		Contexts:        selection.contexts,
		NamedContexts:   selection.named,
		QueryLanguage:   &parsed,
		ResultType:      resultType,
	}
}

func matchesEntityText(haystack, ordinary string, includes, excludes []string) bool {
	haystack = strings.ToLower(haystack)
	if ordinary = strings.ToLower(strings.TrimSpace(ordinary)); ordinary != "" &&
		!strings.Contains(haystack, ordinary) {
		return false
	}
	for _, include := range includes {
		if !strings.Contains(haystack, strings.ToLower(strings.TrimSpace(include))) {
			return false
		}
	}
	for _, exclude := range excludes {
		if strings.Contains(haystack, strings.ToLower(strings.TrimSpace(exclude))) {
			return false
		}
	}
	return true
}

func compactFoldedStrings(values []string) []string {
	output := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		found := false
		for _, existing := range output {
			if strings.EqualFold(existing, value) {
				found = true
				break
			}
		}
		if !found {
			output = append(output, value)
		}
	}
	return output
}

func (s *Service) entityURL(route string, values url.Values) string {
	s.mu.RLock()
	baseURL := s.baseURL
	s.mu.RUnlock()
	if query := values.Encode(); query != "" {
		return baseURL + route + "?" + query
	}
	return baseURL + route
}

func entityCitation(repository, revision, kind string) string {
	if revision == "" {
		return repository + ":" + kind
	}
	return repository + "@" + itemShortRevision(revision) + ":" + kind
}

func itemShortRevision(revision string) string {
	revision = strings.TrimSpace(revision)
	if len(revision) > 8 {
		return revision[:8]
	}
	return revision
}

func searchItemMetadata(item SearchItem, label string) string {
	for _, metadata := range item.Metadata {
		if metadata.Label == label {
			return metadata.Value
		}
	}
	return ""
}

func boundedItemDetail(value string) string {
	runes := []rune(value)
	if len(runes) <= maximumSearchItemDetail {
		return value
	}
	return string(runes[:maximumSearchItemDetail]) + "\n…"
}
