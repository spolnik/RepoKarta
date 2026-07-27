package codeintel

import (
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/spolnik/RepoKarta/internal/contextscope"
	"github.com/spolnik/RepoKarta/internal/querylang"
)

func (s *Service) addSearchActions(response *SearchResponse, query querylang.Query) {
	if response.Compact {
		return
	}
	for index := range response.Matches {
		match := &response.Matches[index]
		if match.RepositoryID <= 0 {
			continue
		}
		line := 0
		symbol := ""
		symbolKind := ""
		if len(match.Lines) > 0 {
			line = match.Lines[0].Number
			symbol = strings.TrimSpace(match.Lines[0].ReferenceTarget)
		}
		if symbol == "" && resultIsProgrammingElement(match.ResultType) {
			symbol = strings.TrimSpace(query.Text)
		}
		if match.ResultType == "symbol_definition" {
			symbolKind = queryFilterValue(query, querylang.FieldSymbolKind)
		}
		contextSymbol := ""
		contextLine := 0
		if match.ResultType == "symbol_definition" {
			contextSymbol = symbol
			contextLine = line
		}
		contextURL := s.ContextURL(contextscope.Context{
			Kind:         contextKind(match.Path, contextSymbol),
			RepositoryID: match.RepositoryID,
			Revision:     match.Revision,
			Path:         match.Path,
			Symbol:       contextSymbol,
			SymbolKind:   symbolKind,
			Line:         contextLine,
		})
		match.Actions = s.resultActions(
			match.RepositoryID,
			match.Path,
			symbol,
			match.Repository,
			match.SourceURL,
			contextURL,
			match.ResultType,
		)
	}
	for index := range response.Items {
		item := &response.Items[index]
		if item.RepositoryID <= 0 {
			continue
		}
		symbol := ""
		symbolKind := ""
		if resultIsProgrammingElement(item.ResultType) {
			symbol = strings.TrimSpace(query.Text)
			symbolKind = queryFilterValue(query, querylang.FieldSymbolKind)
		}
		contextURL := s.ContextURL(contextscope.Context{
			Kind:         contextKind(item.Path, symbol),
			RepositoryID: item.RepositoryID,
			Revision:     item.Revision,
			Path:         item.Path,
			Symbol:       symbol,
			SymbolKind:   symbolKind,
		})
		item.Actions = s.resultActions(
			item.RepositoryID,
			item.Path,
			symbol,
			item.Title,
			item.SourceURL,
			contextURL,
			item.ResultType,
		)
	}
}

func (s *Service) resultActions(
	repositoryID int64,
	filePath, symbol, title, sourceURL, contextURL, resultType string,
) []SearchAction {
	actions := make([]SearchAction, 0, 7)
	if sourceURL != "" {
		label := "Open evidence"
		if strings.Contains(sourceURL, "/source/") {
			label = "Open source"
		}
		actions = append(actions, SearchAction{Kind: "source", Label: label, URL: sourceURL})
	}

	focus := strings.TrimSpace(symbol)
	if focus == "" {
		focus = strings.TrimSpace(filePath)
	}
	if (resultType == "dependency" || resultType == "route") && strings.TrimSpace(title) != "" {
		focus = strings.TrimSpace(title)
	}
	if focus == "" {
		focus = "repository:" + strconv.FormatInt(repositoryID, 10)
	}
	actions = append(actions, SearchAction{
		Kind:  "map",
		Label: "Focus in Maps",
		URL: s.entityURL("/maps", url.Values{
			"repository": {strconv.FormatInt(repositoryID, 10)},
			"focus":      {focus},
		}),
	})

	dependencyValues := url.Values{"repository": {strconv.FormatInt(repositoryID, 10)}}
	if resultType == "dependency" {
		dependencyValues.Set("query", strings.TrimSpace(title))
	} else if filePath != "" {
		dependencyValues.Set("query", strings.TrimSpace(path.Base(filePath)))
	}
	actions = append(actions, SearchAction{
		Kind:  "dependencies",
		Label: "Inspect dependencies",
		URL:   s.entityURL("/dependencies", dependencyValues),
	})

	if symbol != "" {
		actions = append(actions,
			SearchAction{
				Kind:  "references",
				Label: "Find references",
				URL:   s.resultSearchURL(repositoryID, symbol, "reference"),
			},
			SearchAction{
				Kind:  "implementations",
				Label: "Find implementations",
				URL:   s.resultSearchURL(repositoryID, symbol, "implementation"),
			},
		)
	}

	actions = append(actions,
		SearchAction{
			Kind:  "conversation",
			Label: "Start scoped conversation",
			URL:   s.chatContextURL(contextURL, false),
		},
		SearchAction{
			Kind:  "context",
			Label: "Add to current context",
			URL:   s.chatContextURL(contextURL, true),
		},
	)
	return actions
}

func (s *Service) resultSearchURL(repositoryID int64, symbol, resultType string) string {
	query := quoteQueryValue(symbol) + " result_type:" + resultType
	return s.entityURL("/search", url.Values{
		"q":    {query},
		"repo": {strconv.FormatInt(repositoryID, 10)},
	})
}

func (s *Service) chatContextURL(contextURL string, reuse bool) string {
	values := url.Values{"context_url": {contextURL}}
	if reuse {
		values.Set("reuse", "current")
	}
	return s.entityURL("/chat", values)
}

func contextKind(filePath, symbol string) string {
	if symbol != "" {
		return contextscope.KindSymbol
	}
	if strings.TrimSpace(filePath) != "" {
		return contextscope.KindFile
	}
	return contextscope.KindRepository
}

func resultIsProgrammingElement(resultType string) bool {
	switch resultType {
	case "symbol_definition", "reference", "implementation":
		return true
	default:
		return false
	}
}

func queryFilterValue(query querylang.Query, field string) string {
	for _, filter := range query.Filters {
		if filter.Field == field && !filter.Negative {
			return filter.Value
		}
	}
	return ""
}

func quoteQueryValue(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`
}
