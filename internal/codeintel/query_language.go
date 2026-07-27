package codeintel

import (
	"context"
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
			if filter.Negative || !strings.EqualFold(filter.Value, "content") {
				return compiled, fmt.Errorf(
					"result_type %q is not indexed by deterministic code search yet",
					filter.Value,
				)
			}
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
