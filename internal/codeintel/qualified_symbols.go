package codeintel

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/scip-code/scip/bindings/go/scip"
	"github.com/spolnik/RepoKarta/internal/analysis"
	"github.com/spolnik/RepoKarta/internal/querylang"
	"github.com/spolnik/RepoKarta/internal/scipindex"
	"github.com/spolnik/RepoKarta/internal/search"
)

type qualifiedSymbolQuery struct {
	Text          string
	Package       string
	Type          string
	Method        string
	Member        string
	FullName      string
	MatchMode     string
	CaseSensitive bool
	Kind          string
	Language      string
	Path          string
	File          string
}

func (s *Service) searchQualifiedSymbols(
	ctx context.Context,
	request SearchRequest,
	parsed querylang.Query,
) (SearchResponse, error) {
	if mode := strings.ToLower(strings.TrimSpace(request.Mode)); mode != "" && mode != "literal" {
		return SearchResponse{}, errors.New("qualified symbol results support literal query mode")
	}
	query, err := parseQualifiedSymbolQuery(request, parsed)
	if err != nil {
		return SearchResponse{}, err
	}
	repositories, effective, err := s.selectDerivedRepositories(ctx, request, parsed)
	if err != nil {
		return SearchResponse{}, err
	}
	limit := normalizeLimit(request.Limit, DefaultSearchLimit, MaximumSearchLimit)
	started := time.Now()
	response := SearchResponse{
		Limit:           limit,
		TotalFilesExact: true,
		Warnings:        []search.Warning{},
		Matches:         []SearchMatch{},
		Items:           []SearchItem{},
		Contexts:        effective.Contexts,
		NamedContexts:   effective.NamedContexts,
		QueryLanguage:   &parsed,
		ResultType:      "symbol_definition",
	}
	if len(repositories) == 0 {
		return response, nil
	}
	selected := make(map[int64]DerivedEvidenceRepository, len(repositories))
	for _, repository := range repositories {
		selected[repository.ID] = repository
	}

	type candidate struct {
		repositoryID int64
		repository   string
		revision     string
		path         string
		line         int
		language     string
		details      SymbolDetails
	}
	uniqueCandidateFiles := func(items []candidate) int {
		files := make(map[string]struct{}, len(items))
		for _, item := range items {
			files[fmt.Sprintf("%d\x00%s", item.repositoryID, item.path)] = struct{}{}
		}
		return len(files)
	}
	compilerCandidates := make(map[string]candidate)
	scipReady := 0
	if s.scip != nil {
		for _, repository := range repositories {
			artifact, found, readErr := s.scip.Read(ctx, repository.ID, repository.Revision)
			if readErr != nil {
				return SearchResponse{}, readErr
			}
			if !found {
				continue
			}
			scipReady++
			locations := make(map[string][]scipindex.Reference)
			for _, document := range artifact.Documents {
				for _, occurrence := range document.Occurrences {
					if occurrence.SymbolRoles&int32(scip.SymbolRole_Definition) == 0 {
						continue
					}
					locations[occurrence.Symbol] = append(locations[occurrence.Symbol], scipindex.Reference{
						RepositoryID: artifact.RepositoryID,
						Revision:     artifact.Revision,
						Path:         document.Path,
						Language:     document.Language,
						Symbol:       occurrence.Symbol,
						Kind:         "definition",
						Line:         int(occurrence.StartLine) + 1,
					})
				}
			}
			for _, symbol := range artifact.Symbols {
				details := semanticSymbolDetails(symbol)
				if !qualifiedSymbolMatches(query, details) {
					continue
				}
				for _, location := range locations[symbol.ID] {
					key := qualifiedCandidateKey(repository.ID, location.Path, location.Line, details.Name)
					compilerCandidates[key] = candidate{
						repositoryID: repository.ID,
						repository:   repository.Name,
						revision:     repository.Revision,
						path:         location.Path,
						line:         location.Line,
						language:     location.Language,
						details:      details,
					}
				}
			}
		}
	}

	index, err := s.structure.ReadStructure(ctx, 0)
	if err != nil {
		return SearchResponse{}, err
	}
	allCandidates := make([]candidate, 0, len(compilerCandidates)+len(index.Structure))
	for _, item := range compilerCandidates {
		allCandidates = append(allCandidates, item)
	}
	for _, document := range index.Structure {
		repository, ok := selected[document.RepositoryID]
		if !ok || !symbolDocumentMatches(query, document.Language, document.Path) {
			continue
		}
		for _, symbol := range document.Symbols {
			details := syntaxSymbolDetails(document.Path, symbol)
			if !qualifiedSymbolMatches(query, details) {
				continue
			}
			key := qualifiedCandidateKey(
				document.RepositoryID, document.Path, max(1, symbol.Range.StartLine), details.Name,
			)
			if _, precise := compilerCandidates[key]; precise {
				continue
			}
			allCandidates = append(allCandidates, candidate{
				repositoryID: document.RepositoryID,
				repository:   repository.Name,
				revision:     repository.Revision,
				path:         document.Path,
				line:         max(1, symbol.Range.StartLine),
				language:     document.Language,
				details:      details,
			})
		}
	}
	sort.Slice(allCandidates, func(left, right int) bool {
		leftPrecise := allCandidates[left].details.Confidence == "compiler"
		rightPrecise := allCandidates[right].details.Confidence == "compiler"
		if leftPrecise != rightPrecise {
			return leftPrecise
		}
		if allCandidates[left].details.FullName != allCandidates[right].details.FullName {
			return strings.ToLower(allCandidates[left].details.FullName) <
				strings.ToLower(allCandidates[right].details.FullName)
		}
		if allCandidates[left].repository != allCandidates[right].repository {
			return strings.ToLower(allCandidates[left].repository) <
				strings.ToLower(allCandidates[right].repository)
		}
		if allCandidates[left].path != allCandidates[right].path {
			return allCandidates[left].path < allCandidates[right].path
		}
		return allCandidates[left].line < allCandidates[right].line
	})
	response.MatchCount = len(allCandidates)
	response.MatchingFiles = uniqueCandidateFiles(allCandidates)
	response.EstimatedTotalFiles = response.MatchingFiles
	response.TotalFilesExact = index.Scope.Complete && !index.StructureTruncated
	response.Truncated = len(allCandidates) > limit || !response.TotalFilesExact
	for _, item := range allCandidates[:min(limit, len(allCandidates))] {
		details := item.details
		response.Matches = append(response.Matches, SearchMatch{
			RepositoryID: item.repositoryID,
			Repository:   item.repository,
			Revision:     item.revision,
			Path:         item.path,
			Language:     item.language,
			Lines:        []SearchLine{{Number: item.line, Text: details.Signature}},
			Citation:     Citation(item.repository, item.revision, item.path, item.line, item.line),
			SourceURL:    s.SourceURL(item.repositoryID, item.revision, item.path, item.line, item.line),
			Symbol:       &details,
		})
	}
	response.ReturnedFiles = uniqueCandidateFiles(allCandidates[:min(limit, len(allCandidates))])
	response.ReturnedItems = len(response.Matches)
	response.DurationMS = float64(time.Since(started).Microseconds()) / 1000
	if scipReady < len(repositories) {
		response.Warnings = append(response.Warnings, search.Warning{
			Code: "qualified_symbol_syntax_fallback",
			Message: fmt.Sprintf(
				"Compiler-qualified symbols were available for %d of %d selected repositories; remaining results use labeled path-qualified syntax evidence.",
				scipReady,
				len(repositories),
			),
		})
	}
	if !response.TotalFilesExact {
		response.Warnings = append(response.Warnings, search.Warning{
			Code:    "qualified_symbol_index_partial",
			Message: "The persisted structural symbol scope is incomplete or truncated.",
		})
	}
	return response, nil
}

func parseQualifiedSymbolQuery(request SearchRequest, parsed querylang.Query) (qualifiedSymbolQuery, error) {
	query := qualifiedSymbolQuery{
		Text:      strings.TrimSpace(parsed.Text),
		MatchMode: "exact",
		Language:  strings.TrimSpace(request.Language),
		Path:      strings.TrimSpace(strings.ReplaceAll(request.Path, "\\", "/")),
		File:      strings.TrimSpace(strings.ReplaceAll(request.File, "\\", "/")),
	}
	for _, filter := range parsed.Filters {
		if filter.Negative && filter.Field != querylang.FieldOwner &&
			filter.Field != querylang.FieldRepository &&
			filter.Field != querylang.FieldRevision {
			return query, fmt.Errorf("negative %s is not supported for qualified symbol search", filter.Field)
		}
		switch filter.Field {
		case querylang.FieldPackage:
			query.Package = filter.Value
		case querylang.FieldTypeName:
			query.Type = filter.Value
		case querylang.FieldMethod:
			query.Method = filter.Value
		case querylang.FieldMember:
			query.Member = filter.Value
		case querylang.FieldFullName:
			query.FullName = filter.Value
		case querylang.FieldMatch:
			query.MatchMode = strings.ToLower(strings.TrimSpace(filter.Value))
		case querylang.FieldCase:
			switch strings.ToLower(strings.TrimSpace(filter.Value)) {
			case "sensitive":
				query.CaseSensitive = true
			case "insensitive":
				query.CaseSensitive = false
			default:
				return query, errors.New("case must be sensitive or insensitive")
			}
		case querylang.FieldSymbolKind:
			query.Kind = filter.Value
		case querylang.FieldLanguage:
			query.Language = filter.Value
		case querylang.FieldPath:
			query.Path = filter.Value
		case querylang.FieldFile:
			query.File = filter.Value
		case querylang.FieldContent:
			return query, errors.New("content filters cannot be combined with qualified symbol search")
		}
	}
	if !containsFold([]string{"exact", "prefix", "contains"}, query.MatchMode) {
		return query, errors.New("match must be exact, prefix, or contains")
	}
	if query.Text == "" && query.Package == "" && query.Type == "" &&
		query.Method == "" && query.Member == "" && query.FullName == "" {
		return query, errors.New("qualified symbol search requires a name or qualified field")
	}
	return query, nil
}

func semanticSymbolDetails(symbol scipindex.Symbol) SymbolDetails {
	details := SymbolDetails{
		Name:          symbol.DisplayName,
		FullName:      symbol.ID,
		Kind:          strings.ToLower(symbol.Kind),
		Signature:     symbol.Signature,
		Documentation: append([]string(nil), symbol.Documentation...),
		Identity:      symbol.ID,
		Confidence:    "compiler",
	}
	parsed, err := scip.ParseSymbol(symbol.ID)
	if err != nil {
		return details
	}
	if parsed.Package != nil {
		details.Package = parsed.Package.Name
	}
	var names []string
	for _, descriptor := range parsed.Descriptors {
		if descriptor == nil {
			continue
		}
		names = append(names, descriptor.Name)
		switch descriptor.Suffix {
		case scip.Descriptor_Type:
			details.Type = descriptor.Name
		case scip.Descriptor_Method:
			details.Method = descriptor.Name
		case scip.Descriptor_Term:
			details.Member = descriptor.Name
		}
	}
	if len(names) > 0 {
		details.FullName = strings.Join(names, ".")
		if details.Package != "" {
			details.FullName = details.Package + "." + details.FullName
		}
	}
	return details
}

func syntaxSymbolDetails(filePath string, symbol analysis.Symbol) SymbolDetails {
	packageName := path.Dir(filePath)
	if packageName == "." {
		packageName = ""
	}
	fullName := symbol.Name
	if packageName != "" {
		fullName = strings.ReplaceAll(packageName, "/", ".") + "." + symbol.Name
	}
	details := SymbolDetails{
		Name: symbol.Name, FullName: fullName, Package: packageName,
		Kind: symbol.Kind, Confidence: "syntax",
	}
	switch strings.ToLower(symbol.Kind) {
	case "class", "interface", "enum", "struct", "type", "trait":
		details.Type = symbol.Name
	case "method", "function", "constructor":
		details.Method = symbol.Name
	case "field", "variable", "constant", "enum_member", "property":
		details.Member = symbol.Name
	}
	return details
}

func qualifiedSymbolMatches(query qualifiedSymbolQuery, details SymbolDetails) bool {
	match := func(actual, expected string) bool {
		if strings.TrimSpace(expected) == "" {
			return true
		}
		if !query.CaseSensitive {
			actual, expected = strings.ToLower(actual), strings.ToLower(expected)
		}
		switch query.MatchMode {
		case "prefix":
			return strings.HasPrefix(actual, expected)
		case "contains":
			return strings.Contains(actual, expected)
		default:
			return actual == expected
		}
	}
	if query.Text != "" && !match(details.Name, query.Text) && !match(details.FullName, query.Text) {
		return false
	}
	return match(details.Package, query.Package) &&
		match(details.Type, query.Type) &&
		match(details.Method, query.Method) &&
		match(details.Member, query.Member) &&
		match(details.FullName, query.FullName) &&
		match(details.Kind, query.Kind)
}

func symbolDocumentMatches(query qualifiedSymbolQuery, language, filePath string) bool {
	if query.Language != "" && !strings.EqualFold(language, query.Language) {
		return false
	}
	if query.Path != "" && !strings.Contains(strings.ToLower(filePath), strings.ToLower(query.Path)) {
		return false
	}
	return query.File == "" ||
		strings.Contains(strings.ToLower(path.Base(filePath)), strings.ToLower(query.File))
}

func qualifiedCandidateKey(repositoryID int64, filePath string, line int, name string) string {
	return fmt.Sprintf("%d\x00%s\x00%d\x00%s", repositoryID, filePath, line, strings.ToLower(name))
}
