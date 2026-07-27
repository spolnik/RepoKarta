package codeintel

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/spolnik/RepoKarta/internal/analysis"
	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/contextscope"
)

type contextSymbolCandidate struct {
	Path   string
	Symbol analysis.Symbol
}

func contextDirectories(files []string) []string {
	seen := make(map[string]struct{})
	for _, filePath := range files {
		directory := path.Dir(filePath)
		for directory != "." && directory != "/" && directory != "" {
			seen[directory] = struct{}{}
			parent := path.Dir(directory)
			if parent == directory {
				break
			}
			directory = parent
		}
	}
	directories := make([]string, 0, len(seen))
	for directory := range seen {
		directories = append(directories, directory)
	}
	sort.Strings(directories)
	return directories
}

func (s *Service) contextSymbolCandidates(
	ctx context.Context,
	repository catalog.Repository,
	selector contextscope.Selector,
) ([]contextSymbolCandidate, bool, error) {
	if s.structure == nil {
		return nil, true, fmt.Errorf("symbol context index is unavailable for repository %q", repository.Name)
	}
	index, err := s.structure.ReadStructure(ctx, repository.ID)
	if err != nil {
		return nil, true, fmt.Errorf("load symbol context index for repository %q: %w", repository.Name, err)
	}
	incomplete := index.StructureTruncated || !index.Scope.Complete
	currentDocuments := 0
	candidates := make([]contextSymbolCandidate, 0)
	for _, document := range index.Structure {
		if document.RepositoryID != repository.ID || document.Revision != repository.IndexedCommit {
			continue
		}
		currentDocuments++
		if selector.Path != "" && document.Path != selector.Path {
			continue
		}
		for _, symbol := range document.Symbols {
			if symbol.Name != selector.Symbol {
				continue
			}
			if selector.SymbolKind != "" && !strings.EqualFold(symbol.Kind, selector.SymbolKind) {
				continue
			}
			if selector.Line > 0 && symbol.Range.StartLine != selector.Line {
				continue
			}
			candidates = append(candidates, contextSymbolCandidate{Path: document.Path, Symbol: symbol})
		}
	}
	if currentDocuments == 0 {
		return nil, true, fmt.Errorf(
			"symbol context index for repository %q is not ready at indexed revision %s",
			repository.Name,
			shortRevision(repository.IndexedCommit),
		)
	}
	return candidates, incomplete, nil
}

func contextSymbolLabel(repositoryLabel string, candidate contextSymbolCandidate) string {
	return fmt.Sprintf(
		"%s:%s#%s:%d",
		repositoryLabel,
		candidate.Path,
		candidate.Symbol.Name,
		candidate.Symbol.Range.StartLine,
	)
}

func (s *Service) suggestSymbolContexts(
	ctx context.Context,
	repository catalog.Repository,
	repositoryLabel string,
	queryText string,
	limit int,
) (ContextSuggestionList, error) {
	output := ContextSuggestionList{Suggestions: []ContextSuggestion{}}
	if s.structure == nil {
		return output, fmt.Errorf("symbol context index is unavailable for repository %q", repository.Name)
	}
	index, err := s.structure.ReadStructure(ctx, repository.ID)
	if err != nil {
		return output, fmt.Errorf("load symbol context index for repository %q: %w", repository.Name, err)
	}
	type suggestionCandidate struct {
		path   string
		symbol analysis.Symbol
	}
	candidates := make([]suggestionCandidate, 0)
	currentDocuments := 0
	for _, document := range index.Structure {
		if document.RepositoryID != repository.ID || document.Revision != repository.IndexedCommit {
			continue
		}
		currentDocuments++
		for _, symbol := range document.Symbols {
			haystack := strings.ToLower(symbol.Name + "\n" + symbol.Kind + "\n" + document.Path)
			if queryText != "" && !strings.Contains(haystack, queryText) {
				continue
			}
			candidates = append(candidates, suggestionCandidate{path: document.Path, symbol: symbol})
		}
	}
	if currentDocuments == 0 {
		return output, fmt.Errorf(
			"symbol context index for repository %q is not ready at indexed revision %s",
			repository.Name,
			shortRevision(repository.IndexedCommit),
		)
	}
	sort.SliceStable(candidates, func(left, right int) bool {
		leftName := strings.ToLower(candidates[left].symbol.Name)
		rightName := strings.ToLower(candidates[right].symbol.Name)
		if leftName != rightName {
			return leftName < rightName
		}
		if candidates[left].path != candidates[right].path {
			return candidates[left].path < candidates[right].path
		}
		return candidates[left].symbol.Range.StartLine < candidates[right].symbol.Range.StartLine
	})
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		key := fmt.Sprintf(
			"%s\x00%s\x00%s\x00%d",
			candidate.path,
			candidate.symbol.Name,
			candidate.symbol.Kind,
			candidate.symbol.Range.StartLine,
		)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		if len(output.Suggestions) == limit {
			output.Truncated = true
			break
		}
		identity := contextSymbolCandidate{Path: candidate.path, Symbol: candidate.symbol}
		output.Suggestions = append(output.Suggestions, ContextSuggestion{
			Context: contextscope.Selector{
				Kind:         contextscope.KindSymbol,
				RepositoryID: repository.ID,
				Revision:     repository.IndexedCommit,
				Path:         candidate.path,
				Symbol:       candidate.symbol.Name,
				SymbolKind:   candidate.symbol.Kind,
				Line:         candidate.symbol.Range.StartLine,
			},
			Label: contextSymbolLabel(repositoryLabel, identity),
			Detail: fmt.Sprintf(
				"%s · %s:%d · %s",
				candidate.symbol.Kind,
				candidate.path,
				candidate.symbol.Range.StartLine,
				shortRevision(repository.IndexedCommit),
			),
		})
	}
	output.Truncated = output.Truncated || index.StructureTruncated || !index.Scope.Complete
	return output, nil
}
