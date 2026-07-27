package codeintel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spolnik/RepoKarta/internal/contextscope"
	"github.com/spolnik/RepoKarta/internal/querylang"
	"github.com/spolnik/RepoKarta/internal/search"
)

// DerivedEvidenceRepository is the permission-filtered repository identity a
// derived evidence provider may inspect. Local filesystem paths never cross
// this boundary.
type DerivedEvidenceRepository struct {
	ID       int64
	Name     string
	Revision string
}

// DerivedEvidenceRequest is the shared, bounded provider contract for
// dependency, route, Wiki, and code-insight evidence.
type DerivedEvidenceRequest struct {
	ResultType   string
	Query        querylang.Query
	Repositories []DerivedEvidenceRepository
	Contexts     []contextscope.Context
	Language     string
	Path         string
	File         string
	Limit        int
}

// DerivedEvidenceResult reports deterministic items and coverage.
type DerivedEvidenceResult struct {
	Items      []SearchItem
	Truncated  bool
	TotalExact bool
	Warnings   []search.Warning
}

// DerivedEvidenceSearcher connects existing deterministic artifact services
// without making codeintel depend on their concrete packages.
type DerivedEvidenceSearcher interface {
	SearchDerivedEvidence(context.Context, DerivedEvidenceRequest) (DerivedEvidenceResult, error)
}

func (s *Service) searchDerivedEvidence(
	ctx context.Context,
	request SearchRequest,
	parsed querylang.Query,
	resultType string,
) (SearchResponse, error) {
	if s.derived == nil {
		return SearchResponse{}, fmt.Errorf("result_type %q is not configured", resultType)
	}
	if mode := strings.TrimSpace(request.Mode); mode != "" && !strings.EqualFold(mode, "literal") {
		return SearchResponse{}, fmt.Errorf("%s results currently support literal query mode", resultType)
	}
	repositories, effective, err := s.selectDerivedRepositories(ctx, request, parsed)
	if err != nil {
		return SearchResponse{}, err
	}
	limit := normalizeLimit(request.Limit, DefaultSearchLimit, MaximumSearchLimit)
	if len(repositories) == 0 {
		return SearchResponse{
			Limit:           limit,
			TotalFilesExact: true,
			Warnings:        []search.Warning{},
			Matches:         []SearchMatch{},
			Items:           []SearchItem{},
			Contexts:        effective.Contexts,
			NamedContexts:   effective.NamedContexts,
			QueryLanguage:   &parsed,
			ResultType:      resultType,
		}, nil
	}
	started := time.Now()
	result, err := s.derived.SearchDerivedEvidence(ctx, DerivedEvidenceRequest{
		ResultType:   resultType,
		Query:        parsed,
		Repositories: repositories,
		Contexts:     effective.Contexts,
		Language:     strings.TrimSpace(request.Language),
		Path:         strings.TrimSpace(strings.ReplaceAll(request.Path, "\\", "/")),
		File:         strings.TrimSpace(strings.ReplaceAll(request.File, "\\", "/")),
		Limit:        limit,
	})
	if err != nil {
		return SearchResponse{}, err
	}
	allowed := make(map[int64]DerivedEvidenceRepository, len(repositories))
	for _, repository := range repositories {
		allowed[repository.ID] = repository
	}
	items := make([]SearchItem, 0, min(limit, len(result.Items)))
	dropped := false
	for _, item := range result.Items {
		repository, ok := allowed[item.RepositoryID]
		if !ok {
			dropped = true
			continue
		}
		if item.ResultType == "" {
			item.ResultType = resultType
		}
		if item.ResultType != resultType {
			return SearchResponse{}, fmt.Errorf(
				"derived evidence provider returned %q for %q search",
				item.ResultType,
				resultType,
			)
		}
		item.Repository = repository.Name
		if item.Revision == "" {
			item.Revision = repository.Revision
		}
		items = append(items, item)
		if len(items) >= limit {
			if len(result.Items) > len(items) {
				result.Truncated = true
			}
			break
		}
	}
	if result.Warnings == nil {
		result.Warnings = []search.Warning{}
	}
	if dropped {
		result.Truncated = true
		result.TotalExact = false
		result.Warnings = append(result.Warnings, search.Warning{
			Code:    "unauthorized_derived_evidence",
			Message: "One or more derived items were removed because their repository is not visible.",
		})
	}
	return SearchResponse{
		MatchCount:      len(items),
		ReturnedItems:   len(items),
		Limit:           limit,
		Truncated:       result.Truncated,
		TotalFilesExact: result.TotalExact && !result.Truncated,
		DurationMS:      float64(time.Since(started).Microseconds()) / 1000,
		Warnings:        result.Warnings,
		Matches:         []SearchMatch{},
		Items:           items,
		Contexts:        effective.Contexts,
		NamedContexts:   effective.NamedContexts,
		QueryLanguage:   &parsed,
		ResultType:      resultType,
	}, nil
}

func (s *Service) selectDerivedRepositories(
	ctx context.Context,
	request SearchRequest,
	parsed querylang.Query,
) ([]DerivedEvidenceRepository, contextscope.EffectiveResponse, error) {
	visible, err := s.store.ListRepositories(ctx)
	if err != nil {
		return nil, contextscope.EffectiveResponse{}, err
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
		return nil, effective, err
	}
	if len(effective.Contexts) > 0 &&
		(request.RepositoryID > 0 || strings.TrimSpace(request.Repository) != "") {
		return nil, effective, errors.New(
			"structured contexts cannot be combined with the legacy repository selector",
		)
	}
	contextIDs := make(map[int64]struct{}, len(effective.Contexts))
	for _, resolved := range effective.Contexts {
		contextIDs[resolved.RepositoryID] = struct{}{}
	}

	var positiveRepositories, negativeRepositories []string
	var positiveRevisions, negativeRevisions []string
	for _, filter := range parsed.Filters {
		target := func(positive, negative *[]string) {
			if filter.Negative {
				*negative = append(*negative, filter.Value)
			} else {
				*positive = append(*positive, filter.Value)
			}
		}
		switch filter.Field {
		case querylang.FieldRepository:
			target(&positiveRepositories, &negativeRepositories)
		case querylang.FieldRevision:
			target(&positiveRevisions, &negativeRevisions)
		}
	}
	positiveIDs, err := repositoryFilterIDs(visible, positiveRepositories)
	if err != nil {
		return nil, effective, err
	}
	revisionIDs, err := revisionFilterIDs(visible, positiveRevisions)
	if err != nil {
		return nil, effective, err
	}
	positiveIDs = intersectOptionalIDs(positiveIDs, revisionIDs)
	positiveActive := len(positiveRepositories) > 0 || len(positiveRevisions) > 0
	negativeIDs, err := repositoryFilterIDs(visible, negativeRepositories)
	if err != nil {
		return nil, effective, err
	}
	negativeRevisionIDs, err := revisionFilterIDs(visible, negativeRevisions)
	if err != nil {
		return nil, effective, err
	}
	allowed := idSet(positiveIDs)
	denied := idSet(unionIDs(negativeIDs, negativeRevisionIDs))

	var legacyID int64
	if request.RepositoryID > 0 || strings.TrimSpace(request.Repository) != "" {
		selected, selectErr := s.selectRepository(ctx, request.RepositoryID, request.Repository)
		if selectErr != nil {
			return nil, effective, selectErr
		}
		legacyID = selected.ID
	}
	output := make([]DerivedEvidenceRepository, 0, len(visible))
	for _, repository := range visible {
		if legacyID > 0 && repository.ID != legacyID {
			continue
		}
		if len(contextIDs) > 0 {
			if _, ok := contextIDs[repository.ID]; !ok {
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
		revision := strings.TrimSpace(repository.IndexedCommit)
		if revision == "" {
			revision = strings.TrimSpace(repository.HeadCommit)
		}
		output = append(output, DerivedEvidenceRepository{
			ID:       repository.ID,
			Name:     repository.Name,
			Revision: revision,
		})
	}
	return output, effective, nil
}
