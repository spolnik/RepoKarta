package codeintel

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spolnik/RepoKarta/internal/querylang"
)

func finalizeSearchResponse(response *SearchResponse, parsed querylang.Query) {
	rankSearchResponse(response, parsed)
	buildSearchFacets(response)
}

func rankSearchResponse(response *SearchResponse, parsed querylang.Query) {
	needle := strings.ToLower(strings.TrimSpace(parsed.Text))
	for index := range response.Matches {
		match := &response.Matches[index]
		match.Ranking = []RankingSignal{}
		path := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(match.Path, "\\", "/")))
		fileName := path
		if separator := strings.LastIndex(fileName, "/"); separator >= 0 {
			fileName = fileName[separator+1:]
		}
		switch {
		case needle != "" && (needle == path || needle == fileName):
			match.Ranking = append(match.Ranking, RankingSignal{
				Name: "exact_path", Weight: 100,
				Detail: "Free text exactly matches the repository-relative path or filename.",
			})
		case needle != "" && strings.HasPrefix(fileName, needle):
			match.Ranking = append(match.Ranking, RankingSignal{
				Name: "filename_prefix", Weight: 60,
				Detail: "The filename starts with the free-text query.",
			})
		case needle != "" && strings.Contains(path, needle):
			match.Ranking = append(match.Ranking, RankingSignal{
				Name: "path_contains", Weight: 30,
				Detail: "The repository-relative path contains the free-text query.",
			})
		}
		if response.ResultType == "symbol_definition" && needle != "" {
			match.Ranking = append(match.Ranking, RankingSignal{
				Name: "exact_symbol", Weight: 90,
				Detail: "The symbol index matched the exact requested definition name.",
			})
		}
		if response.ResultType == "reference" || response.ResultType == "implementation" {
			match.Ranking = append(match.Ranking, RankingSignal{
				Name: "exact_ast_target", Weight: 90,
				Detail: "Persisted syntax evidence matched the exact target name.",
			})
		}
		if match.Score != 0 {
			match.Ranking = append(match.Ranking, RankingSignal{
				Name: "source_index_score", Weight: match.Score,
				Detail: fmt.Sprintf("Source index score %.3f.", match.Score),
			})
		}
	}
	sort.SliceStable(response.Matches, func(left, right int) bool {
		leftWeight := rankingWeight(response.Matches[left].Ranking)
		rightWeight := rankingWeight(response.Matches[right].Ranking)
		if leftWeight != rightWeight {
			return leftWeight > rightWeight
		}
		return response.Matches[left].Score > response.Matches[right].Score
	})

	for index := range response.Items {
		item := &response.Items[index]
		item.Ranking = []RankingSignal{}
		title := strings.ToLower(strings.TrimSpace(item.Title))
		switch {
		case needle != "" && title == needle:
			item.Ranking = append(item.Ranking, RankingSignal{
				Name: "exact_title", Weight: 100,
				Detail: "The result title exactly matches the free-text query.",
			})
		case needle != "" && strings.HasPrefix(title, needle):
			item.Ranking = append(item.Ranking, RankingSignal{
				Name: "title_prefix", Weight: 60,
				Detail: "The result title starts with the free-text query.",
			})
		case needle != "" && strings.Contains(title, needle):
			item.Ranking = append(item.Ranking, RankingSignal{
				Name: "title_contains", Weight: 30,
				Detail: "The result title contains the free-text query.",
			})
		}
		item.Score = rankingWeight(item.Ranking)
	}
	sort.SliceStable(response.Items, func(left, right int) bool {
		return response.Items[left].Score > response.Items[right].Score
	})
}

func rankingWeight(signals []RankingSignal) float64 {
	total := 0.0
	for _, signal := range signals {
		total += signal.Weight
	}
	return total
}

func buildSearchFacets(response *SearchResponse) {
	counts := make(map[string]int)
	add := func(field, value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		counts[field+"\x00"+value]++
	}
	for _, match := range response.Matches {
		add(querylang.FieldResultType, match.ResultType)
		add(querylang.FieldRepository, match.Repository)
		add(querylang.FieldLanguage, match.Language)
		add(querylang.FieldPath, facetPath(match.Path))
	}
	for _, item := range response.Items {
		add(querylang.FieldResultType, item.ResultType)
		add(querylang.FieldRepository, item.Repository)
		add(querylang.FieldPath, facetPath(item.Path))
	}
	response.Facets = make([]SearchFacet, 0, len(counts))
	for key, count := range counts {
		field, value, _ := strings.Cut(key, "\x00")
		response.Facets = append(response.Facets, SearchFacet{
			Field: field,
			Value: value,
			Count: count,
		})
	}
	sort.Slice(response.Facets, func(left, right int) bool {
		if response.Facets[left].Field != response.Facets[right].Field {
			return response.Facets[left].Field < response.Facets[right].Field
		}
		if response.Facets[left].Count != response.Facets[right].Count {
			return response.Facets[left].Count > response.Facets[right].Count
		}
		return strings.ToLower(response.Facets[left].Value) <
			strings.ToLower(response.Facets[right].Value)
	})
	complete := !response.Truncated && response.TotalFilesExact
	response.FacetCoverage = SearchFacetCoverage{
		Scope:    "returned_results",
		Complete: complete,
	}
	if complete {
		response.FacetCoverage.Scope = "all_results"
	}
}

func facetPath(value string) string {
	value = strings.Trim(strings.ReplaceAll(strings.TrimSpace(value), "\\", "/"), "/")
	if value == "" {
		return ""
	}
	if separator := strings.Index(value, "/"); separator >= 0 {
		return value[:separator]
	}
	return value
}
