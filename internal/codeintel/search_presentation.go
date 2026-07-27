package codeintel

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/spolnik/RepoKarta/internal/querylang"
)

const maximumSourceIndexRankingWeight = 40.0

func finalizeSearchResponse(response *SearchResponse, parsed querylang.Query) {
	rankSearchResponse(response, parsed)
	buildSearchFacets(response)
	compactSearchResponse(response)
}

// compactSearchResponse keeps discovery results cheap to transport and
// consume. Completeness, commit identity, paths, line numbers, citations, and
// typed reference metadata remain available; selected source can be fetched
// through get_file after the candidate set has been narrowed.
func compactSearchResponse(response *SearchResponse) {
	if !response.Compact {
		return
	}
	for index := range response.Matches {
		match := &response.Matches[index]
		match.Score = 0
		match.Ranking = nil
		match.Actions = nil
		for lineIndex := range match.Lines {
			line := &match.Lines[lineIndex]
			line.Text = ""
			line.Before = ""
			line.After = ""
			line.Fragments = nil
		}
	}
	for index := range response.Items {
		item := &response.Items[index]
		item.Summary = ""
		item.Detail = ""
		item.Metadata = nil
		item.Score = 0
		item.Ranking = nil
		item.Actions = nil
	}
	response.Facets = nil
	response.FacetCoverage = SearchFacetCoverage{}
}

func rankSearchResponse(response *SearchResponse, parsed querylang.Query) {
	needle := strings.ToLower(strings.TrimSpace(parsed.Text))
	queryTerms := rankingQueryTerms(parsed.Text)
	sourceRanks, rankedSourceCount := sourceIndexRanks(response.Matches)
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
		if match.ResultType == "file_path" && needle != "" {
			match.Ranking = append(match.Ranking, RankingSignal{
				Name: "filename_only_match", Weight: 10,
				Detail: "The filename-only candidate set matched this path without relying on file content.",
			})
		}
		if match.ResultType == "symbol_definition" && needle != "" {
			match.Ranking = append(match.Ranking, RankingSignal{
				Name: "exact_symbol", Weight: 90,
				Detail: "The symbol index matched the exact requested definition name.",
			})
		}
		if match.ResultType == "reference" || match.ResultType == "implementation" {
			match.Ranking = append(match.Ranking, RankingSignal{
				Name: "exact_ast_target", Weight: 90,
				Detail: "Persisted syntax evidence matched the exact target name.",
			})
		}
		match.Ranking = append(match.Ranking, identifierPathRankingSignals(match.Path, queryTerms)...)
		if len(match.Lines) > 1 {
			weight := float64(min((len(match.Lines)-1)*10, 30))
			match.Ranking = append(match.Ranking, RankingSignal{
				Name:   "file_match_coherence",
				Weight: weight,
				Detail: fmt.Sprintf(
					"%d distinct matching lines make this file a coherent query candidate.",
					len(match.Lines),
				),
			})
		}
		match.Ranking = append(match.Ranking, nonPrimaryPathRankingSignals(
			match.Path,
			parsed,
			queryTerms,
		)...)
		if rank := sourceRanks[index]; rank > 0 {
			weight := maximumSourceIndexRankingWeight / float64(rank)
			match.Ranking = append(match.Ranking, RankingSignal{
				Name: "source_index_score", Weight: weight,
				Detail: fmt.Sprintf(
					"Source index score %.3f is rank %d of %d and contributes normalized weight %.3f.",
					match.Score,
					rank,
					rankedSourceCount,
					weight,
				),
			})
		}
	}
	sort.SliceStable(response.Matches, func(left, right int) bool {
		leftClass := rankingClass(response.Matches[left].Ranking)
		rightClass := rankingClass(response.Matches[right].Ranking)
		if leftClass != rightClass {
			return leftClass > rightClass
		}
		leftTyped := rankingTypedPriority(response.Matches[left].Ranking)
		rightTyped := rankingTypedPriority(response.Matches[right].Ranking)
		if leftTyped != rightTyped {
			return leftTyped > rightTyped
		}
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
		item.Ranking = append(item.Ranking, identifierPathRankingSignals(item.Path, queryTerms)...)
		item.Ranking = append(item.Ranking, nonPrimaryPathRankingSignals(
			item.Path,
			parsed,
			queryTerms,
		)...)
		item.Score = rankingWeight(item.Ranking)
	}
	sort.SliceStable(response.Items, func(left, right int) bool {
		return response.Items[left].Score > response.Items[right].Score
	})
}

func sourceIndexRanks(matches []SearchMatch) (map[int]int, int) {
	indices := make([]int, 0, len(matches))
	for index := range matches {
		if matches[index].Score > 0 {
			indices = append(indices, index)
		}
	}
	sort.SliceStable(indices, func(left, right int) bool {
		return matches[indices[left]].Score > matches[indices[right]].Score
	})
	ranks := make(map[int]int, len(indices))
	rank := 0
	previousScore := 0.0
	for position, index := range indices {
		if position == 0 || matches[index].Score != previousScore {
			rank = position + 1
			previousScore = matches[index].Score
		}
		ranks[index] = rank
	}
	return ranks, len(indices)
}

func identifierPathRankingSignals(path string, queryTerms []string) []RankingSignal {
	if len(queryTerms) == 0 {
		return nil
	}
	pathTerms := rankingPathTerms(path)
	matched := 0
	for _, queryTerm := range queryTerms {
		for pathTerm := range pathTerms {
			if rankingTermsOverlap(queryTerm, pathTerm) {
				matched++
				break
			}
		}
	}
	if matched == 0 {
		return nil
	}
	weight := 40 * float64(matched) / float64(len(queryTerms))
	return []RankingSignal{{
		Name:   "identifier_path_match",
		Weight: weight,
		Detail: fmt.Sprintf(
			"%d of %d meaningful query identifiers match the filename or its parent directory.",
			matched,
			len(queryTerms),
		),
	}}
}

func rankingQueryTerms(value string) []string {
	stopwords := map[string]struct{}{
		"a": {}, "an": {}, "and": {}, "are": {}, "as": {}, "at": {}, "be": {},
		"by": {}, "do": {}, "does": {}, "for": {}, "from": {}, "has": {}, "have": {},
		"how": {}, "if": {}, "in": {}, "is": {}, "it": {}, "not": {}, "of": {},
		"on": {}, "or": {}, "the": {}, "to": {}, "was": {}, "what": {}, "when": {},
		"where": {}, "which": {}, "who": {}, "why": {}, "with": {},
	}
	seen := make(map[string]struct{})
	output := make([]string, 0)
	for _, term := range splitRankingIdentifiers(value) {
		if len([]rune(term)) < 3 {
			continue
		}
		if _, skipped := stopwords[term]; skipped {
			continue
		}
		if _, duplicate := seen[term]; duplicate {
			continue
		}
		seen[term] = struct{}{}
		output = append(output, term)
	}
	return output
}

func rankingPathTerms(value string) map[string]struct{} {
	normalized := strings.Trim(strings.ReplaceAll(value, "\\", "/"), "/")
	parts := strings.Split(normalized, "/")
	relevant := parts
	if len(relevant) > 2 {
		relevant = relevant[len(relevant)-2:]
	}
	output := make(map[string]struct{})
	for index, part := range relevant {
		if index == len(relevant)-1 {
			if extension := strings.LastIndex(part, "."); extension > 0 {
				part = part[:extension]
			}
		}
		for _, term := range splitRankingIdentifiers(part) {
			if len([]rune(term)) >= 2 {
				output[term] = struct{}{}
			}
		}
	}
	return output
}

func splitRankingIdentifiers(value string) []string {
	runes := []rune(strings.TrimSpace(value))
	output := make([]string, 0)
	start := -1
	flush := func(end int) {
		if start < 0 || end <= start {
			return
		}
		output = append(output, strings.ToLower(string(runes[start:end])))
		start = -1
	}
	for index, current := range runes {
		if !unicode.IsLetter(current) && !unicode.IsDigit(current) {
			flush(index)
			continue
		}
		if start < 0 {
			start = index
			continue
		}
		previous := runes[index-1]
		var next rune
		if index+1 < len(runes) {
			next = runes[index+1]
		}
		if unicode.IsUpper(current) &&
			(unicode.IsLower(previous) || unicode.IsDigit(previous) ||
				(unicode.IsUpper(previous) && unicode.IsLower(next))) {
			flush(index)
			start = index
		}
	}
	flush(len(runes))
	return output
}

func rankingTermsOverlap(left, right string) bool {
	if left == right {
		return true
	}
	shorter, longer := left, right
	if len(shorter) > len(longer) {
		shorter, longer = longer, shorter
	}
	return len(shorter) >= 3 && strings.HasPrefix(longer, shorter)
}

func nonPrimaryPathRankingSignals(
	path string,
	parsed querylang.Query,
	queryTerms []string,
) []RankingSignal {
	if path == "" {
		return nil
	}
	normalized := strings.ToLower(strings.ReplaceAll(path, "\\", "/"))
	base := normalized
	if separator := strings.LastIndex(base, "/"); separator >= 0 {
		base = base[separator+1:]
	}
	intentTerms := append(append([]string{}, queryTerms...), rankingPathFilterTerms(parsed)...)
	queryHas := func(values ...string) bool {
		for _, queryTerm := range intentTerms {
			for _, value := range values {
				if rankingTermsOverlap(queryTerm, value) {
					return true
				}
			}
		}
		return false
	}
	signals := make([]RankingSignal, 0, 2)
	if isTestPath(normalized, base) && !queryHas("test", "tests", "testing", "spec") {
		signals = append(signals, RankingSignal{
			Name:   "test_path_penalty",
			Weight: -30,
			Detail: "A conventional test path is demoted because the query did not ask for tests.",
		})
	}
	if (pathHasComponent(normalized, "compat") ||
		pathHasComponent(normalized, "_compat") ||
		pathHasComponent(normalized, "legacy")) &&
		!queryHas("compat", "compatibility", "legacy") {
		signals = append(signals, RankingSignal{
			Name:   "compatibility_path_penalty",
			Weight: -20,
			Detail: "A compatibility or legacy path is demoted because the query did not request it.",
		})
	}
	if (pathHasComponent(normalized, "example") ||
		pathHasComponent(normalized, "examples") ||
		pathHasComponent(normalized, "_example") ||
		pathHasComponent(normalized, "_examples") ||
		pathHasComponent(normalized, "doc_src") ||
		pathHasComponent(normalized, "docs_src")) &&
		!queryHas("example", "examples", "documentation", "docs") {
		signals = append(signals, RankingSignal{
			Name:   "example_path_penalty",
			Weight: -20,
			Detail: "Example or documentation-source code is demoted because the query did not request it.",
		})
	}
	if (strings.HasSuffix(base, ".d.ts") ||
		base == "__init__.py" ||
		base == "package-info.java") &&
		!queryHas("declaration", "declarations", "types", "typing", "init", "package", "metadata") {
		signals = append(signals, RankingSignal{
			Name:   "stub_path_penalty",
			Weight: -10,
			Detail: "A declaration stub or package metadata file is mildly demoted.",
		})
	}
	return signals
}

func rankingPathFilterTerms(parsed querylang.Query) []string {
	output := make([]string, 0)
	for _, filter := range parsed.Filters {
		if !filter.Negative &&
			(filter.Field == querylang.FieldPath || filter.Field == querylang.FieldFile) {
			output = append(output, rankingQueryTerms(filter.Value)...)
		}
	}
	return output
}

func pathHasComponent(path, component string) bool {
	path = strings.Trim(path, "/")
	for _, candidate := range strings.Split(path, "/") {
		if candidate == component {
			return true
		}
	}
	return false
}

func isTestPath(path, base string) bool {
	for _, component := range strings.Split(strings.Trim(path, "/"), "/") {
		switch component {
		case "test", "tests", "__tests__", "spec", "testing":
			return true
		}
	}
	if strings.HasPrefix(base, "test_") ||
		strings.Contains(base, ".test.") ||
		strings.Contains(base, ".spec.") {
		return true
	}
	for _, suffix := range []string{
		"_test.go", "_test.py", "_test.rb", "_spec.rb", "_test.cpp", "_test.c",
		"_test.dart", "_test.lua", "_spec.lua", "test.java", "tests.java",
		"test.kt", "tests.kt", "spec.kt", "test.swift", "tests.swift", "spec.swift",
		"test.cs", "tests.cs", "spec.scala", "suite.scala", "test.scala",
	} {
		if strings.HasSuffix(base, suffix) {
			return true
		}
	}
	return false
}

func rankingTypedPriority(signals []RankingSignal) int {
	for _, signal := range signals {
		switch signal.Name {
		case "filename_only_match", "exact_symbol", "exact_ast_target":
			return 1
		}
	}
	return 0
}

func rankingClass(signals []RankingSignal) int {
	class := 0
	for _, signal := range signals {
		switch signal.Name {
		case "exact_path", "exact_symbol", "exact_ast_target":
			class = max(class, 3)
		case "filename_prefix":
			class = max(class, 2)
		case "path_contains":
			class = max(class, 1)
		}
	}
	return class
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
