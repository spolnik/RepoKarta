package codeintel

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/querylang"
	"github.com/spolnik/RepoKarta/internal/search"
)

type compiledQueryFilters struct {
	includeText       []string
	excludeText       []string
	languages         []string
	excludeLanguages  []string
	paths             []string
	excludePaths      []string
	files             []string
	excludeFiles      []string
	repositoryAllow   []uint32
	repositoryLimited bool
	repositoryDeny    []uint32
}

func requestedResultType(parsed querylang.Query) (string, error) {
	resultType := "content"
	explicit := false
	for _, filter := range parsed.Filters {
		if filter.Field != querylang.FieldResultType {
			continue
		}
		if filter.Negative {
			return "", fmt.Errorf("negative result_type filters are not supported")
		}
		value := strings.ToLower(strings.TrimSpace(filter.Value))
		if explicit && value != resultType {
			return "", fmt.Errorf("search accepts one result_type at a time")
		}
		resultType = value
		explicit = true
	}
	switch resultType {
	case "content", "file_path", "repository", "symbol_definition", "reference", "implementation",
		"commit", "diff":
		return resultType, nil
	case "dependency", "route", "wiki_page", "code_insight":
		return "", fmt.Errorf("result_type %q is not connected to unified search yet", resultType)
	default:
		return "", fmt.Errorf("unknown result_type %q", resultType)
	}
}

func (s *Service) referenceRequestForQuery(
	ctx context.Context,
	request SearchRequest,
	parsed querylang.Query,
) (ReferenceRequest, error) {
	mode := strings.ToLower(strings.TrimSpace(request.Mode))
	if mode != "" && mode != "literal" && mode != "references" {
		return ReferenceRequest{}, errors.New(
			"reference and implementation results cannot be combined with regex or Zoekt query mode",
		)
	}
	output := ReferenceRequest{
		Symbol:             parsed.Text,
		RepositoryID:       request.RepositoryID,
		Repository:         request.Repository,
		Language:           request.Language,
		Path:               request.Path,
		File:               request.File,
		Limit:              request.Limit,
		Contexts:           request.Contexts,
		NamedContextIDs:    request.NamedContextIDs,
		UseDefaultContexts: request.UseDefaultContexts,
	}
	var repositories []string
	var revisions []string
	for _, filter := range parsed.Filters {
		if filter.Field == querylang.FieldResultType {
			continue
		}
		if filter.Negative {
			return output, fmt.Errorf(
				"negative %s filters are not supported for reference results yet",
				filter.Field,
			)
		}
		switch filter.Field {
		case querylang.FieldRepository:
			repositories = append(repositories, filter.Value)
		case querylang.FieldRevision:
			revisions = append(revisions, filter.Value)
		case querylang.FieldLanguage:
			if output.Language != "" && !strings.EqualFold(output.Language, filter.Value) {
				return output, fmt.Errorf("reference results currently accept one language")
			}
			output.Language = filter.Value
		case querylang.FieldPath:
			if output.Path != "" && !strings.EqualFold(output.Path, filter.Value) {
				return output, fmt.Errorf("reference results currently accept one path filter")
			}
			output.Path = filter.Value
		case querylang.FieldFile:
			if output.File != "" && !strings.EqualFold(output.File, filter.Value) {
				return output, fmt.Errorf("reference results currently accept one filename filter")
			}
			output.File = filter.Value
		case querylang.FieldContent:
			return output, fmt.Errorf("content filters cannot be combined with reference results")
		case querylang.FieldSymbolKind:
			return output, fmt.Errorf("symbol_kind is not available in syntax-backed reference evidence")
		case querylang.FieldOwner:
			return output, fmt.Errorf("owner filters require ownership evidence that is not indexed yet")
		}
	}
	if len(repositories)+len(revisions) == 0 {
		return output, nil
	}
	visible, err := s.store.ListRepositories(ctx)
	if err != nil {
		return output, err
	}
	repositoryIDs, err := repositoryFilterIDs(visible, repositories)
	if err != nil {
		return output, err
	}
	revisionIDs, err := revisionFilterIDs(visible, revisions)
	if err != nil {
		return output, err
	}
	allowed := intersectOptionalIDs(repositoryIDs, revisionIDs)
	if len(allowed) != 1 {
		return output, errors.New("reference results currently require filters that resolve to one repository")
	}
	if output.RepositoryID > 0 && uint32(output.RepositoryID) != allowed[0] {
		return output, errors.New("query repository filter conflicts with repository_id")
	}
	if output.Repository != "" {
		legacy, resolveErr := s.namedRepository(ctx, output.Repository)
		if resolveErr != nil {
			return output, resolveErr
		}
		if uint32(legacy.ID) != allowed[0] {
			return output, errors.New("query repository filter conflicts with repository")
		}
	}
	output.RepositoryID = int64(allowed[0])
	output.Repository = ""
	return output, nil
}

func containsFold(values []string, candidate string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(candidate)) {
			return true
		}
	}
	return false
}

func (s *Service) compileQueryFilters(
	ctx context.Context,
	parsed querylang.Query,
) (compiledQueryFilters, error) {
	var compiled compiledQueryFilters
	var positiveRepositories []string
	var negativeRepositories []string
	var positiveRevisions []string
	var negativeRevisions []string
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
			target(&compiled.includeText, &compiled.excludeText)
		case querylang.FieldRepository:
			target(&positiveRepositories, &negativeRepositories)
		case querylang.FieldRevision:
			target(&positiveRevisions, &negativeRevisions)
		case querylang.FieldLanguage:
			target(&compiled.languages, &compiled.excludeLanguages)
		case querylang.FieldPath:
			target(&compiled.paths, &compiled.excludePaths)
		case querylang.FieldFile:
			target(&compiled.files, &compiled.excludeFiles)
		case querylang.FieldResultType:
			// Validated and dispatched by requestedResultType.
		case querylang.FieldSymbolKind:
			return compiled, fmt.Errorf(
				"symbol_kind requires a symbol, reference, or implementation result type",
			)
		case querylang.FieldOwner:
			return compiled, fmt.Errorf("owner filters require ownership evidence that is not indexed yet")
		default:
			return compiled, fmt.Errorf("unsupported query field %q", filter.Field)
		}
	}
	if len(positiveRepositories)+len(negativeRepositories)+
		len(positiveRevisions)+len(negativeRevisions) == 0 {
		return compiled, nil
	}
	repositories, err := s.store.ListRepositories(ctx)
	if err != nil {
		return compiled, err
	}
	repositoryPositive, err := repositoryFilterIDs(repositories, positiveRepositories)
	if err != nil {
		return compiled, err
	}
	revisionPositive, err := revisionFilterIDs(repositories, positiveRevisions)
	if err != nil {
		return compiled, err
	}
	compiled.repositoryAllow = intersectOptionalIDs(repositoryPositive, revisionPositive)
	compiled.repositoryLimited = len(positiveRepositories) > 0 || len(positiveRevisions) > 0
	repositoryNegative, err := repositoryFilterIDs(repositories, negativeRepositories)
	if err != nil {
		return compiled, err
	}
	revisionNegative, err := revisionFilterIDs(repositories, negativeRevisions)
	if err != nil {
		return compiled, err
	}
	compiled.repositoryDeny = unionIDs(repositoryNegative, revisionNegative)
	return compiled, nil
}

func repositoryFilterIDs(repositories []catalog.Repository, values []string) ([]uint32, error) {
	if len(values) == 0 {
		return nil, nil
	}
	var output []uint32
	for _, value := range values {
		value = strings.TrimSpace(value)
		var matches []catalog.Repository
		if id, err := strconv.ParseInt(value, 10, 64); err == nil {
			for _, repository := range repositories {
				if repository.ID == id {
					matches = append(matches, repository)
				}
			}
		} else {
			for _, repository := range repositories {
				if strings.EqualFold(repository.Name, value) {
					matches = append(matches, repository)
				}
			}
		}
		if len(matches) != 1 {
			return nil, fmt.Errorf("repository %q is not uniquely indexed and visible", value)
		}
		output = append(output, uint32(matches[0].ID))
	}
	return unionIDs(output), nil
}

func revisionFilterIDs(repositories []catalog.Repository, values []string) ([]uint32, error) {
	if len(values) == 0 {
		return nil, nil
	}
	var output []uint32
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		var matched bool
		for _, repository := range repositories {
			revision := strings.ToLower(strings.TrimSpace(repository.IndexedCommit))
			if value != "" && revision != "" && strings.HasPrefix(revision, value) {
				output = append(output, uint32(repository.ID))
				matched = true
			}
		}
		if !matched {
			return nil, fmt.Errorf("revision %q is not indexed and visible", value)
		}
	}
	return unionIDs(output), nil
}

func applyQueryRepositoryFilters(
	base []uint32,
	scopes []search.Scope,
	allowed []uint32,
	allowedActive bool,
	denied []uint32,
) ([]uint32, []search.Scope, bool) {
	deny := idSet(denied)
	allow := idSet(allowed)
	accept := func(id uint32) bool {
		if _, excluded := deny[id]; excluded {
			return false
		}
		if !allowedActive {
			return true
		}
		_, included := allow[id]
		return included
	}
	if len(scopes) > 0 {
		filtered := scopes[:0]
		for _, scope := range scopes {
			if accept(scope.RepositoryID) {
				filtered = append(filtered, scope)
			}
		}
		return base, filtered, len(filtered) == 0
	}
	if len(base) > 0 {
		filtered := base[:0]
		for _, id := range base {
			if accept(id) {
				filtered = append(filtered, id)
			}
		}
		return filtered, scopes, len(filtered) == 0
	}
	if allowedActive {
		filtered := make([]uint32, 0, len(allowed))
		for _, id := range allowed {
			if accept(id) {
				filtered = append(filtered, id)
			}
		}
		return filtered, scopes, len(filtered) == 0
	}
	return base, scopes, false
}

func intersectOptionalIDs(left, right []uint32) []uint32 {
	if len(left) == 0 {
		return right
	}
	if len(right) == 0 {
		return left
	}
	rightSet := idSet(right)
	var output []uint32
	for _, id := range left {
		if _, ok := rightSet[id]; ok {
			output = append(output, id)
		}
	}
	return output
}

func unionIDs(groups ...[]uint32) []uint32 {
	seen := make(map[uint32]struct{})
	var output []uint32
	for _, group := range groups {
		for _, id := range group {
			if _, duplicate := seen[id]; duplicate {
				continue
			}
			seen[id] = struct{}{}
			output = append(output, id)
		}
	}
	return output
}

func idSet(ids []uint32) map[uint32]struct{} {
	output := make(map[uint32]struct{}, len(ids))
	for _, id := range ids {
		output[id] = struct{}{}
	}
	return output
}
