package codeintel

import (
	"context"
	"strings"
	"sync"

	"github.com/spolnik/RepoKarta/internal/querylang"
	"github.com/spolnik/RepoKarta/internal/search"
)

func mixedSourceSearchEligible(request SearchRequest, parsed querylang.Query) bool {
	if strings.TrimSpace(parsed.Text) == "" || strings.TrimSpace(request.Mode) != "" {
		return false
	}
	if len(request.Contexts) > 0 || len(request.NamedContextIDs) > 0 {
		return false
	}
	for _, filter := range parsed.Filters {
		switch filter.Field {
		case querylang.FieldResultType, querylang.FieldContent,
			querylang.FieldSymbolKind, querylang.FieldOwner:
			return false
		}
	}
	return true
}

func (s *Service) searchMixedSourceEvidence(
	ctx context.Context,
	request SearchRequest,
	parsed querylang.Query,
) (SearchResponse, error) {
	resultTypes := []string{"content", "file_path"}
	if _, err := validSymbol(parsed.Text); err == nil {
		resultTypes = append(resultTypes, "symbol_definition")
	}
	limit := normalizeLimit(request.Limit, DefaultSearchLimit, MaximumSearchLimit)
	children := make([]SearchResponse, 0, len(resultTypes))
	children = make([]SearchResponse, len(resultTypes))
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var group sync.WaitGroup
	var errorMu sync.Mutex
	var firstError error
	for index, resultType := range resultTypes {
		group.Add(1)
		go func() {
			defer group.Done()
			childRequest := request
			childRequest.Query = strings.TrimSpace(request.Query) + " result_type:" + resultType
			childRequest.Limit = limit
			child, err := s.Search(childCtx, childRequest)
			if err != nil {
				errorMu.Lock()
				if firstError == nil {
					firstError = err
					cancel()
				}
				errorMu.Unlock()
				return
			}
			children[index] = child
		}()
	}
	group.Wait()
	if firstError != nil {
		return SearchResponse{}, firstError
	}

	output := SearchResponse{
		Limit:         limit,
		Warnings:      []search.Warning{},
		Matches:       []SearchMatch{},
		Items:         []SearchItem{},
		QueryLanguage: &parsed,
		ResultType:    "mixed",
		Compact:       request.Compact,
	}
	for index, child := range children {
		output.MatchCount += child.MatchCount
		output.MatchingFiles += child.MatchingFiles
		output.EstimatedTotalFiles += child.EstimatedTotalFiles
		output.FilesSkipped += child.FilesSkipped
		output.ShardsSkipped += child.ShardsSkipped
		output.DurationMS += child.DurationMS
		output.Truncated = output.Truncated || child.Truncated
		output.TotalFilesExact = (index == 0 || output.TotalFilesExact) && child.TotalFilesExact
		output.Warnings = append(output.Warnings, child.Warnings...)
		output.Matches = append(output.Matches, child.Matches...)
		if index == 0 {
			output.Contexts = child.Contexts
			output.NamedContexts = child.NamedContexts
		}
	}
	rankSearchResponse(&output, parsed)
	if len(output.Matches) > limit {
		output.Matches = output.Matches[:limit]
		output.Truncated = true
		output.TotalFilesExact = false
	}
	output.ReturnedFiles = len(output.Matches)
	if output.Truncated {
		output.TotalFilesExact = false
		output.Warnings = append(output.Warnings, search.Warning{
			Code:    "mixed_search_limit",
			Message: "Exact path, exact symbol, and content candidates share the returned result limit.",
		})
	}
	buildSearchFacets(&output)
	compactSearchResponse(&output)
	s.addSearchActions(&output, parsed)
	return output, nil
}
