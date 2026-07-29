package httpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/spolnik/RepoKarta/internal/apicontract"
	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/codeintel"
	"github.com/spolnik/RepoKarta/internal/contextscope"
	"github.com/spolnik/RepoKarta/internal/identity"
	"github.com/spolnik/RepoKarta/internal/search"
	"github.com/spolnik/RepoKarta/internal/searchworkspace"
	"github.com/spolnik/RepoKarta/internal/security"
	"github.com/spolnik/RepoKarta/internal/source"
)

func (s *Server) apiSearch(response http.ResponseWriter, request *http.Request) {
	limit, err := apiSearchLimit(request.URL.Query().Get("limit"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	compact, err := optionalBool(request.URL.Query().Get("compact"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("compact must be a boolean"))
		return
	}
	repositoryID, repositoryName := repositorySelector(request.URL.Query().Get("repo"))
	mode := strings.TrimSpace(request.URL.Query().Get("mode"))
	if mode == "" {
		mode = "zoekt"
	}
	input := codeintel.SearchRequest{
		Query:        request.URL.Query().Get("q"),
		RepositoryID: repositoryID,
		Repository:   repositoryName,
		Language:     request.URL.Query().Get("lang"),
		Path:         request.URL.Query().Get("path"),
		File:         request.URL.Query().Get("file"),
		Mode:         mode,
		Limit:        limit,
		Compact:      compact,
	}
	result, err := s.intelligence.Search(request.Context(), input)
	if err != nil {
		writeContextOrAPIError(response, err)
		return
	}
	if err := s.intelligence.RecordRecentSearch(request.Context(), input, result); err != nil {
		slog.Warn("record recent search", "error", err)
	}
	writeSearchJSON(response, result)
}

func (s *Server) apiSearchJSON(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, 1<<20)
	var input codeintel.SearchRequest
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("invalid structured search request"))
		return
	}
	result, err := s.intelligence.Search(request.Context(), input)
	if err != nil {
		writeContextOrAPIError(response, err)
		return
	}
	if err := s.intelligence.RecordRecentSearch(request.Context(), input, result); err != nil {
		slog.Warn("record recent structured search", "error", err)
	}
	writeSearchJSON(response, result)
}

func (s *Server) apiSearchWorkspace(response http.ResponseWriter, request *http.Request) {
	workspace, err := s.intelligence.ListSearchWorkspace(request.Context())
	if err != nil {
		writeSearchWorkspaceError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, workspace)
}

func (s *Server) createSavedSearch(response http.ResponseWriter, request *http.Request) {
	var input codeintel.SavedSearchInput
	if err := decodeBoundedJSON(response, request, &input); err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("invalid saved search"))
		return
	}
	saved, err := s.intelligence.CreateSavedSearch(request.Context(), input)
	if err != nil {
		writeSearchWorkspaceError(response, err)
		return
	}
	writeJSON(response, http.StatusCreated, saved)
}

func (s *Server) updateSavedSearch(response http.ResponseWriter, request *http.Request) {
	var input codeintel.SavedSearchInput
	if err := decodeBoundedJSON(response, request, &input); err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("invalid saved search"))
		return
	}
	saved, err := s.intelligence.UpdateSavedSearch(
		request.Context(), request.PathValue("searchID"), input,
	)
	if err != nil {
		writeSearchWorkspaceError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, saved)
}

func (s *Server) deleteSavedSearch(response http.ResponseWriter, request *http.Request) {
	if err := s.intelligence.DeleteSavedSearch(
		request.Context(), request.PathValue("searchID"),
	); err != nil {
		writeSearchWorkspaceError(response, err)
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func (s *Server) configureSearchMonitor(response http.ResponseWriter, request *http.Request) {
	var input codeintel.SearchMonitorInput
	if err := decodeBoundedJSON(response, request, &input); err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("invalid search monitor"))
		return
	}
	monitor, err := s.intelligence.ConfigureSearchMonitor(
		request.Context(), request.PathValue("searchID"), input,
	)
	if err != nil {
		writeSearchWorkspaceError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, monitor)
}

func (s *Server) runSearchMonitor(response http.ResponseWriter, request *http.Request) {
	run, err := s.intelligence.RunSearchMonitor(
		request.Context(), request.PathValue("monitorID"),
	)
	if err != nil {
		writeSearchWorkspaceError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, run)
}

func decodeBoundedJSON(response http.ResponseWriter, request *http.Request, value any) error {
	request.Body = http.MaxBytesReader(response, request.Body, 1<<20)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	return decoder.Decode(value)
}

func (s *Server) apiQueryCompletions(response http.ResponseWriter, request *http.Request) {
	raw := request.URL.Query().Get("q")
	if len(raw) > 8192 {
		writeAPIError(response, http.StatusRequestURITooLong, errors.New("query is too long"))
		return
	}
	cursor, err := strconv.Atoi(request.URL.Query().Get("cursor"))
	if err != nil || cursor < 0 {
		writeAPIError(response, http.StatusBadRequest, errors.New("cursor must be a non-negative integer"))
		return
	}
	result, err := s.intelligence.CompleteQuery(request.Context(), raw, cursor)
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, errors.New("query completions are unavailable"))
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func (s *Server) apiContextSuggestions(response http.ResponseWriter, request *http.Request) {
	repositoryID, err := optionalRepositoryID(request.URL.Query().Get("repository_id"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	limit, err := apiSearchLimit(request.URL.Query().Get("limit"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	result, err := s.intelligence.SuggestContexts(request.Context(), codeintel.ContextSuggestionRequest{
		Kind:         request.URL.Query().Get("kind"),
		Query:        request.URL.Query().Get("q"),
		RepositoryID: repositoryID,
		Limit:        limit,
	})
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func (s *Server) apiContextResolution(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, 64<<10)
	var input contextscope.EffectiveRequest
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("invalid structured context request"))
		return
	}
	effective, err := s.intelligence.ResolveEffectiveContexts(request.Context(), input)
	if err != nil {
		writeContextOrAPIError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, effective)
}

func (s *Server) apiNamedContexts(response http.ResponseWriter, request *http.Request) {
	contexts, err := s.intelligence.ListNamedContexts(request.Context())
	if err != nil {
		writeNamedContextError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, contexts)
}

func (s *Server) apiNamedContext(response http.ResponseWriter, request *http.Request) {
	context, err := s.intelligence.GetNamedContext(request.Context(), request.PathValue("contextID"))
	if err != nil {
		writeNamedContextError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, context)
}

func (s *Server) createNamedContext(response http.ResponseWriter, request *http.Request) {
	input, err := decodeNamedContextInput(response, request)
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	context, err := s.intelligence.CreateNamedContext(request.Context(), input)
	if err != nil {
		writeNamedContextError(response, err)
		return
	}
	writeJSON(response, http.StatusCreated, context)
}

func (s *Server) updateNamedContext(response http.ResponseWriter, request *http.Request) {
	input, err := decodeNamedContextInput(response, request)
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	context, err := s.intelligence.UpdateNamedContext(
		request.Context(),
		request.PathValue("contextID"),
		input,
	)
	if err != nil {
		writeNamedContextError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, context)
}

func (s *Server) deleteNamedContext(response http.ResponseWriter, request *http.Request) {
	if err := s.intelligence.DeleteNamedContext(request.Context(), request.PathValue("contextID")); err != nil {
		writeNamedContextError(response, err)
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func decodeNamedContextInput(
	response http.ResponseWriter,
	request *http.Request,
) (contextscope.NamedContextInput, error) {
	request.Body = http.MaxBytesReader(response, request.Body, 128<<10)
	var input contextscope.NamedContextInput
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return input, errors.New("invalid named context request")
	}
	return input, nil
}

func writeSearchJSON(response http.ResponseWriter, result codeintel.SearchResponse) {
	status := http.StatusOK
	if result.ReferenceIndex != nil && result.ReferenceIndex.State == "building" {
		status = http.StatusAccepted
		response.Header().Set("Retry-After", "2")
	}
	writeJSON(response, status, result)
}

func (s *Server) apiSymbol(response http.ResponseWriter, request *http.Request) {
	limit, err := apiSearchLimit(request.URL.Query().Get("limit"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	compact, err := optionalBool(request.URL.Query().Get("compact"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("compact must be a boolean"))
		return
	}
	repositoryID, repositoryName := repositorySelector(request.URL.Query().Get("repo"))
	result, err := s.intelligence.FindSymbol(request.Context(), codeintel.SymbolRequest{
		Symbol:       request.URL.Query().Get("symbol"),
		RepositoryID: repositoryID,
		Repository:   repositoryName,
		Language:     request.URL.Query().Get("lang"),
		Limit:        limit,
		Compact:      compact,
	})
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func (s *Server) apiSymbolJSON(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, 1<<20)
	var input codeintel.SymbolRequest
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("invalid symbol request"))
		return
	}
	result, err := s.intelligence.FindSymbol(request.Context(), input)
	if err != nil {
		writeContextError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func (s *Server) apiASTSearch(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, 64<<10)
	var input codeintel.ASTSearchRequest
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("invalid AST search request"))
		return
	}
	result, err := s.intelligence.SearchAST(request.Context(), input)
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	status := http.StatusOK
	if result.Index.State == "building" {
		response.Header().Set("Retry-After", "2")
		status = http.StatusAccepted
	}
	writeJSON(response, status, result)
}

func (s *Server) apiRepositories(response http.ResponseWriter, request *http.Request) {
	repositories, err := s.intelligence.Repositories(request.Context())
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, repositories)
}

func (s *Server) apiSCIPJava(response http.ResponseWriter, _ *http.Request) {
	writeJSON(response, http.StatusOK, s.scipJava.ProviderStatus())
}

func (s *Server) retrySCIPJava(response http.ResponseWriter, request *http.Request) {
	repositoryID, err := strconv.ParseInt(strings.TrimSpace(request.PathValue("repositoryID")), 10, 64)
	if err != nil || repositoryID <= 0 {
		writeAPIError(response, http.StatusBadRequest, errors.New("positive repository ID is required"))
		return
	}
	if err := s.scipJava.Retry(request.Context(), repositoryID); err != nil {
		writeAPIError(response, http.StatusConflict, err)
		return
	}
	if strings.HasPrefix(request.URL.Path, "/api/") {
		writeJSON(response, http.StatusAccepted, map[string]any{
			"repository_id": repositoryID,
			"state":         "pending",
		})
		return
	}
	if strings.EqualFold(strings.TrimSpace(request.Header.Get("HX-Request")), "true") {
		s.repositoryList(response, request)
		return
	}
	http.Redirect(response, request, "/", http.StatusSeeOther)
}

func (s *Server) apiWhoAmI(response http.ResponseWriter, request *http.Request) {
	principal, ok := security.PrincipalFromContext(request.Context())
	if !ok {
		writeAPIError(response, http.StatusUnauthorized, errors.New("authenticated identity is unavailable"))
		return
	}
	viewer := s.conversationViewer(request.Context())
	groups := append([]string{}, principal.Groups...)
	writeJSON(response, http.StatusOK, map[string]any{
		"id":          viewer.Author.ID,
		"name":        viewer.Author.Name,
		"email":       viewer.Author.Email,
		"provider":    viewer.Author.Provider,
		"groups":      groups,
		"admin":       viewer.Admin,
		"role":        identity.NormalizeRole(principal.Role),
		"permissions": identity.Permissions(principal.Role),
	})
}

func (s *Server) apiFile(response http.ResponseWriter, request *http.Request) {
	start, end := parseLineRange(request.URL.Query().Get("lines"))
	repositoryID, repositoryName := repositorySelector(request.PathValue("repository"))
	file, err := s.intelligence.GetFile(request.Context(), codeintel.FileRequest{
		RepositoryID: repositoryID,
		Repository:   repositoryName,
		Revision:     request.URL.Query().Get("rev"),
		Path:         request.URL.Query().Get("path"),
		StartLine:    start,
		EndLine:      end,
	})
	if err != nil {
		writeCodeIntelligenceError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, file)
}

func (s *Server) apiTree(response http.ResponseWriter, request *http.Request) {
	offset, err := nonNegativeInteger(request.URL.Query().Get("offset"), "offset")
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	repositoryID, repositoryName := repositorySelector(request.PathValue("repository"))
	tree, err := s.intelligence.ListTree(request.Context(), codeintel.TreeRequest{
		RepositoryID: repositoryID,
		Repository:   repositoryName,
		Revision:     request.URL.Query().Get("rev"),
		Path:         request.URL.Query().Get("path"),
		Offset:       offset,
	})
	if err != nil {
		writeCodeIntelligenceError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, tree)
}

func nonNegativeInteger(value, name string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", name)
	}
	return parsed, nil
}

func (s *Server) apiGitLog(response http.ResponseWriter, request *http.Request) {
	limit, err := apiBoundedInteger(
		request.URL.Query().Get("limit"),
		"limit",
		codeintel.DefaultGitLogLimit,
		codeintel.MaximumGitLogLimit,
	)
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	repositoryID, repositoryName := repositorySelector(request.PathValue("repository"))
	result, err := s.intelligence.GitLog(request.Context(), codeintel.GitLogRequest{
		RepositoryID: repositoryID,
		Repository:   repositoryName,
		Revision:     request.URL.Query().Get("rev"),
		Path:         request.URL.Query().Get("path"),
		Limit:        limit,
	})
	if err != nil {
		writeCodeIntelligenceError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func (s *Server) apiGitDiff(response http.ResponseWriter, request *http.Request) {
	contextLines, err := apiBoundedInteger(
		request.URL.Query().Get("context"),
		"context",
		codeintel.DefaultDiffContext,
		codeintel.MaximumDiffContext,
	)
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	repositoryID, repositoryName := repositorySelector(request.PathValue("repository"))
	result, err := s.intelligence.GitDiff(request.Context(), codeintel.GitDiffRequest{
		RepositoryID: repositoryID,
		Repository:   repositoryName,
		FromRevision: request.URL.Query().Get("from"),
		ToRevision:   request.URL.Query().Get("to"),
		Path:         request.URL.Query().Get("path"),
		ContextLines: contextLines,
	})
	if err != nil {
		writeCodeIntelligenceError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func (s *Server) search(response http.ResponseWriter, request *http.Request) {
	data, err := s.pageData(request.Context())
	if err != nil {
		http.Error(response, "Could not load repositories", http.StatusInternalServerError)
		return
	}

	repositoryID, repositoryName := repositorySelector(request.URL.Query().Get("repo"))
	for _, repository := range data.Repositories {
		if repository.ID == repositoryID {
			repositoryName = repository.Name
			break
		}
	}
	mode := strings.TrimSpace(request.URL.Query().Get("mode"))
	if mode == "" {
		mode = "zoekt"
	}
	query := search.Query{
		Text:       strings.TrimSpace(request.URL.Query().Get("q")),
		Repository: repositoryName,
		Language:   strings.TrimSpace(request.URL.Query().Get("lang")),
		Path:       strings.TrimSpace(request.URL.Query().Get("path")),
		File:       strings.TrimSpace(request.URL.Query().Get("file")),
		Mode:       mode,
		Limit:      parseSearchLimit(request.URL.Query().Get("limit")),
	}
	data.Search.Query = query
	data.Search.SelectedRepositoryID = repositoryID
	data.Search.Performed = query.Text != ""
	if data.Search.Performed {
		input := codeintel.SearchRequest{
			Query:        query.Text,
			RepositoryID: repositoryID,
			Repository:   query.Repository,
			Language:     query.Language,
			Path:         query.Path,
			File:         query.File,
			Mode:         query.Mode,
			Limit:        query.Limit,
		}
		result, searchError := s.intelligence.Search(request.Context(), input)
		if searchError != nil {
			data.Search.Error = searchError.Error()
		} else {
			if err := s.intelligence.RecordRecentSearch(request.Context(), input, result); err != nil {
				slog.Warn("record recent HTML search", "error", err)
			}
			data.Search.Duration = formatMilliseconds(result.DurationMS)
			data.Search.MatchCount = result.MatchCount
			data.Search.FileCount = result.MatchingFiles
			data.Search.EstimatedFiles = result.EstimatedTotalFiles
			data.Search.ReturnedFiles = result.ReturnedFiles
			data.Search.ReturnedItems = result.ReturnedItems
			data.Search.Limit = result.Limit
			data.Search.TotalFilesExact = result.TotalFilesExact
			data.Search.FilesSkipped = result.FilesSkipped
			data.Search.ShardsSkipped = result.ShardsSkipped
			data.Search.Warnings = result.Warnings
			data.Search.Truncated = result.Truncated
			data.Search.ResultType = result.ResultType
			data.Search.Matches = resolveSearchViews(result.Matches, data.Repositories)
			data.Search.Items = result.Items
			data.Search.Facets = result.Facets
			data.Search.FacetCoverage = result.FacetCoverage
		}
	}

	if request.Header.Get("HX-Request") == "true" {
		s.render(response, "search-results", data)
		return
	}
	s.render(response, "index", data)
}

func resolveSearchViews(matches []codeintel.SearchMatch, repositories []catalog.Repository) []searchMatchView {
	views := make([]searchMatchView, 0, len(matches))
	for _, match := range matches {
		view := searchMatchView{
			ResultType: match.ResultType,
			Repository: match.Repository,
			Revision:   match.Revision,
			Path:       match.Path,
			Language:   match.Language,
			Ranking:    match.Ranking,
			Actions:    match.Actions,
			Lines:      make([]search.LineMatch, 0, len(match.Lines)),
		}
		if len(match.Lines) > 0 {
			view.FocusLine = match.Lines[0].Number
		}
		for _, line := range match.Lines {
			view.Lines = append(view.Lines, search.LineMatch{
				Number:              line.Number,
				Text:                line.Text,
				Before:              line.Before,
				After:               line.After,
				Fragments:           line.Fragments,
				ReferenceKind:       line.ReferenceKind,
				ReferenceTarget:     line.ReferenceTarget,
				ReferenceReceiver:   line.ReferenceReceiver,
				ReferenceConfidence: line.ReferenceConfidence,
			})
		}
		for _, repository := range repositories {
			if repository.IndexedCommit != match.Revision && repository.HeadCommit != match.Revision {
				continue
			}
			normalizedSearchName := strings.ReplaceAll(match.Repository, "\\", "/")
			if normalizedSearchName == repository.Name ||
				strings.HasSuffix(strings.ToLower(normalizedSearchName), "/"+strings.ToLower(repository.Name)) {
				view.RepositoryID = repository.ID
				view.Repository = repository.Name
				break
			}
		}
		views = append(views, view)
	}
	return views
}

func parseSearchLimit(value string) int {
	limit, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || limit <= 0 {
		return codeintel.DefaultSearchLimit
	}
	return min(limit, codeintel.MaximumSearchLimit)
}

func apiSearchLimit(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return codeintel.DefaultSearchLimit, nil
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit <= 0 || limit > codeintel.MaximumSearchLimit {
		return 0, fmt.Errorf("limit must be an integer from 1 to %d", codeintel.MaximumSearchLimit)
	}
	return limit, nil
}

func apiBoundedInteger(value, name string, fallback, maximum int) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 || parsed > maximum {
		return 0, fmt.Errorf("%s must be an integer from 1 to %d", name, maximum)
	}
	return parsed, nil
}

func writeCodeIntelligenceError(response http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, source.ErrUnsafePath), errors.Is(err, source.ErrUnknownRevision):
		writeAPIError(response, http.StatusBadRequest, err)
	case errors.Is(err, source.ErrUnsupportedFile):
		writeAPIError(response, http.StatusUnsupportedMediaType, err)
	default:
		writeAPIError(response, http.StatusNotFound, err)
	}
}

func writeAPIError(response http.ResponseWriter, status int, err error) {
	writeJSON(response, status, apicontract.ErrorResponse{
		Error: apicontract.ErrorDetail{
			Message: err.Error(),
		},
	})
}

func writeContextOrAPIError(response http.ResponseWriter, err error) {
	var resolutionError *contextscope.ResolutionError
	if errors.As(err, &resolutionError) {
		writeContextError(response, resolutionError)
		return
	}
	if errors.Is(err, contextscope.ErrNamedContextNotFound) ||
		errors.Is(err, contextscope.ErrNamedContextForbidden) ||
		errors.Is(err, contextscope.ErrNamedContextConflict) {
		writeNamedContextError(response, err)
		return
	}
	writeAPIError(response, http.StatusBadRequest, err)
}

func writeNamedContextError(response http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, contextscope.ErrNamedContextNotFound):
		writeAPIError(response, http.StatusNotFound, err)
	case errors.Is(err, contextscope.ErrNamedContextForbidden):
		writeAPIError(response, http.StatusForbidden, err)
	case errors.Is(err, contextscope.ErrNamedContextConflict):
		writeAPIError(response, http.StatusConflict, err)
	default:
		writeContextOrAPIErrorWithoutNamedContext(response, err)
	}
}

func writeSearchWorkspaceError(response http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, searchworkspace.ErrNotFound):
		writeAPIError(response, http.StatusNotFound, err)
	case errors.Is(err, searchworkspace.ErrForbidden):
		writeAPIError(response, http.StatusForbidden, err)
	case errors.Is(err, searchworkspace.ErrConflict):
		writeAPIError(response, http.StatusConflict, err)
	default:
		writeContextOrAPIError(response, err)
	}
}

func writeContextOrAPIErrorWithoutNamedContext(response http.ResponseWriter, err error) {
	var resolutionError *contextscope.ResolutionError
	if errors.As(err, &resolutionError) {
		writeContextError(response, resolutionError)
		return
	}
	writeAPIError(response, http.StatusBadRequest, err)
}

func writeContextError(response http.ResponseWriter, err error) {
	var resolutionError *contextscope.ResolutionError
	if !errors.As(err, &resolutionError) {
		writeAPIError(response, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(response, http.StatusUnprocessableEntity, map[string]any{
		"error": map[string]any{
			"message": resolutionError.Error(),
			"code":    "context_resolution_failed",
			"issues":  resolutionError.Issues,
		},
	})
}
