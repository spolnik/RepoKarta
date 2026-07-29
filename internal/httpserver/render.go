package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/codeintel"
	"github.com/spolnik/RepoKarta/internal/contextscope"
	"github.com/spolnik/RepoKarta/internal/docs"
	"github.com/spolnik/RepoKarta/internal/graph"
	"github.com/spolnik/RepoKarta/internal/identity"
	"github.com/spolnik/RepoKarta/internal/mcpserver"
	"github.com/spolnik/RepoKarta/internal/scipjava"
	"github.com/spolnik/RepoKarta/internal/search"
	"github.com/spolnik/RepoKarta/internal/security"
)

func (s *Server) home(response http.ResponseWriter, request *http.Request) {
	data, err := s.pageData(request.Context())
	if err != nil {
		http.Error(response, "Could not load repositories", http.StatusInternalServerError)
		return
	}
	s.render(response, "index", data)
}

func (s *Server) chatPage(response http.ResponseWriter, request *http.Request) {
	data, err := s.pageData(request.Context())
	if err != nil {
		http.Error(response, "Could not load repositories", http.StatusInternalServerError)
		return
	}
	data.ActivePage = "chat"
	s.render(response, "chat", data)
}

func (s *Server) contextPage(response http.ResponseWriter, request *http.Request) {
	repositoryID, err := optionalRepositoryID(request.URL.Query().Get("repository"))
	if err != nil || repositoryID <= 0 {
		http.Error(response, "A positive repository context is required", http.StatusBadRequest)
		return
	}
	line, err := strconv.Atoi(request.URL.Query().Get("line"))
	if err != nil && request.URL.Query().Get("line") != "" {
		http.Error(response, "Context line must be a positive integer", http.StatusBadRequest)
		return
	}
	useDefaults := false
	effective, err := s.intelligence.ResolveEffectiveContexts(
		request.Context(),
		contextscope.EffectiveRequest{
			Contexts: []contextscope.Selector{{
				Kind:         request.URL.Query().Get("kind"),
				RepositoryID: repositoryID,
				Revision:     request.URL.Query().Get("revision"),
				Path:         request.URL.Query().Get("path"),
				Symbol:       request.URL.Query().Get("symbol"),
				SymbolKind:   request.URL.Query().Get("symbol_kind"),
				Line:         line,
			}},
			UseDefaults: &useDefaults,
		},
	)
	if err != nil {
		writeContextPageError(response, err)
		return
	}
	if len(effective.Contexts) == 0 {
		http.Error(response, "Context resolved without a usable target", http.StatusUnprocessableEntity)
		return
	}
	base, err := s.pageData(request.Context())
	if err != nil {
		http.Error(response, "Could not load context", http.StatusInternalServerError)
		return
	}
	base.ActivePage = "context"
	title := "Structured context"
	if len(effective.Contexts) > 0 {
		title = effective.Contexts[0].Label
	}
	s.render(response, "context", contextPageData{
		pageData: base,
		Title:    title,
		Contexts: effective.Contexts,
		ShareURL: effective.Contexts[0].URL,
		UseURL:   "/chat?context_url=" + url.QueryEscape(effective.Contexts[0].URL),
	})
}

func (s *Server) namedContextPage(response http.ResponseWriter, request *http.Request) {
	named, err := s.intelligence.GetNamedContext(request.Context(), request.PathValue("contextID"))
	if err != nil {
		if errors.Is(err, contextscope.ErrNamedContextNotFound) {
			http.NotFound(response, request)
			return
		}
		http.Error(response, "Could not load named context", http.StatusInternalServerError)
		return
	}
	base, err := s.pageData(request.Context())
	if err != nil {
		http.Error(response, "Could not load context", http.StatusInternalServerError)
		return
	}
	base.ActivePage = "context"
	s.render(response, "context", contextPageData{
		pageData:     base,
		Title:        named.Title,
		Description:  named.Description,
		Category:     named.Category,
		DefaultScope: named.DefaultScope,
		Contexts:     named.Contexts,
		Issues:       named.Issues,
		ShareURL:     named.URL,
		UseURL:       "/chat?context=" + url.QueryEscape(named.ID),
	})
}

func writeContextPageError(response http.ResponseWriter, err error) {
	var resolution *contextscope.ResolutionError
	if errors.As(err, &resolution) {
		http.Error(response, resolution.Error(), http.StatusUnprocessableEntity)
		return
	}
	http.Error(response, err.Error(), http.StatusBadRequest)
}

func (s *Server) mapPage(response http.ResponseWriter, request *http.Request) {
	data, err := s.pageData(request.Context())
	if err != nil {
		http.Error(response, "Could not load repositories", http.StatusInternalServerError)
		return
	}
	data.ActivePage = "maps"
	s.render(response, "maps", data)
}

func (s *Server) wikiPage(response http.ResponseWriter, request *http.Request) {
	data, err := s.pageData(request.Context())
	if err != nil {
		http.Error(response, "Could not load repositories", http.StatusInternalServerError)
		return
	}
	data.ActivePage = "wiki"
	s.render(response, "wiki", data)
}

func (s *Server) mcpPage(response http.ResponseWriter, request *http.Request) {
	data, err := s.pageData(request.Context())
	if err != nil {
		http.Error(response, "Could not load MCP configuration", http.StatusInternalServerError)
		return
	}
	data.ActivePage = "mcp"
	endpointBaseURL := strings.TrimRight(strings.TrimSpace(s.config.MCPBaseURL), "/")
	if s.security != nil {
		if publicURL := s.security.Settings().PublicURL; publicURL != "" {
			endpointBaseURL = publicURL
		}
	}
	if endpointBaseURL == "" {
		endpointBaseURL = "http://" + request.Host
	}
	data.MCP = buildMCPPageData(
		endpointBaseURL+"/mcp",
		s.config.MCPToken,
		s.config.MCPCommand,
		strings.TrimRight(strings.TrimSpace(s.config.MCPBaseURL), "/"),
	)
	data.MCP.Shared = data.AuthMode != "" && data.AuthMode != string(security.ModeLocal)
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("Referrer-Policy", "no-referrer")
	response.Header().Set("X-Robots-Tag", "noindex, nofollow")
	s.render(response, "mcp-setup", data)
}

func (s *Server) apiWiki(response http.ResponseWriter, request *http.Request) {
	repositoryID, err := requiredRepositoryID(request.URL.Query().Get("repository"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	site, err := s.docs.Plan(request.Context(), repositoryID)
	if err != nil {
		writeDocumentationError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, site)
}

func (s *Server) apiWikiPage(response http.ResponseWriter, request *http.Request) {
	repositoryID, err := requiredRepositoryID(request.PathValue("repositoryID"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	page, err := s.docs.Page(request.Context(), repositoryID, strings.TrimSpace(request.PathValue("page")))
	if err != nil {
		writeDocumentationError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, page)
}

func (s *Server) generateWiki(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, 16<<10)
	var input struct {
		RepositoryID int64  `json:"repository_id"`
		Page         string `json:"page"`
		Refresh      bool   `json:"refresh"`
		SurveyOnly   bool   `json:"survey_only"`
		PlanOnly     bool   `json:"plan_only"`
		Preset       string `json:"preset"`
		Provider     string `json:"provider"`
		Model        string `json:"model"`
		Effort       string `json:"effort"`
		Timeout      int    `json:"timeout_seconds"`
		TokenBudget  int64  `json:"token_budget"`
	}
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("invalid documentation generation request"))
		return
	}
	if input.RepositoryID <= 0 {
		writeAPIError(response, http.StatusBadRequest, errors.New("repository_id must be a positive integer"))
		return
	}
	site, err := s.docs.Generate(request.Context(), docs.GenerateRequest{
		RepositoryID: input.RepositoryID,
		Page:         strings.TrimSpace(input.Page),
		Refresh:      input.Refresh,
		SurveyOnly:   input.SurveyOnly,
		PlanOnly:     input.PlanOnly,
		Preset:       strings.TrimSpace(input.Preset),
		Provider:     strings.TrimSpace(input.Provider),
		Model:        strings.TrimSpace(input.Model),
		Effort:       strings.TrimSpace(input.Effort),
		Timeout:      input.Timeout,
		TokenBudget:  input.TokenBudget,
	})
	if err != nil {
		writeDocumentationError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, site)
}

func (s *Server) exportWiki(response http.ResponseWriter, request *http.Request) {
	repositoryID, err := requiredRepositoryID(request.URL.Query().Get("repository"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	content, fileName, err := s.docs.Export(request.Context(), repositoryID)
	if err != nil {
		writeDocumentationError(response, err)
		return
	}
	response.Header().Set("Content-Type", "application/zip")
	response.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, fileName))
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(content)
}

func (s *Server) apiMap(response http.ResponseWriter, request *http.Request) {
	repositoryID, err := optionalRepositoryID(request.URL.Query().Get("repository"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	refresh := request.URL.Query().Get("refresh") == "true"
	principal, allowed := s.requirePermission(response, request, func() identity.Permission {
		if refresh {
			return identity.PermissionManageArtifacts
		}
		return identity.PermissionReadRepositories
	}())
	if !allowed {
		return
	}
	snapshot, err := s.maps.Snapshot(request.Context(), repositoryID, refresh)
	if err != nil {
		slog.Error("build repository map", "repository_id", repositoryID, "error", err)
		writeAPIError(response, http.StatusInternalServerError, errors.New("repository map could not be built"))
		return
	}
	if refresh {
		s.recordApplicationEvent(request, principal, "generation.map", "map", strconv.FormatInt(repositoryID, 10), "success", nil)
	}
	writeJSON(response, http.StatusOK, snapshot)
}

func (s *Server) apiGraphQuery(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, 64<<10)
	var input struct {
		RepositoryID int64 `json:"repository_id,omitempty"`
		graph.QueryRequest
	}
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("invalid graph query request"))
		return
	}
	if input.RepositoryID < 0 {
		writeAPIError(response, http.StatusBadRequest, errors.New("repository_id must be positive when provided"))
		return
	}
	snapshot, err := s.maps.Snapshot(request.Context(), input.RepositoryID, false)
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, errors.New("graph snapshot could not be loaded"))
		return
	}
	result, err := graph.QueryGraph(snapshot, input.QueryRequest)
	if err != nil {
		writeJSON(response, http.StatusUnprocessableEntity, map[string]any{
			"error":  err.Error(),
			"result": result,
		})
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func (s *Server) exportMap(response http.ResponseWriter, request *http.Request) {
	repositoryID, err := optionalRepositoryID(request.URL.Query().Get("repository"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	snapshot, err := s.maps.Snapshot(request.Context(), repositoryID, false)
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, errors.New("repository map could not be exported"))
		return
	}
	content, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, errors.New("repository map could not be encoded"))
		return
	}
	fileName := "repokarta-map-all.json"
	if repositoryID > 0 && len(snapshot.Repositories) == 1 {
		fileName = "repokarta-map-" + safeDownloadName(snapshot.Repositories[0].Name) + ".json"
	}
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, fileName))
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(content)
}

func (s *Server) repositoryList(response http.ResponseWriter, request *http.Request) {
	data, err := s.pageData(request.Context())
	if err != nil {
		http.Error(response, "Could not load repositories", http.StatusInternalServerError)
		return
	}
	// The fragment carries out-of-band copies of the header health chip and the
	// drawer metric tiles so a catalogue change updates all three together.
	s.render(response, "repository-list-fragment", data)
}

func (s *Server) refreshRepositories(response http.ResponseWriter, request *http.Request) {
	if err := s.refresher.Refresh(request.Context()); err != nil {
		slog.Error("refresh repository catalogue", "error", err)
		http.Error(response, "Could not refresh repositories", http.StatusInternalServerError)
		return
	}
	s.repositoryList(response, request)
}

func (s *Server) pageData(ctx context.Context) (pageData, error) {
	repositories, err := s.intelligence.CatalogRepositories(ctx)
	if err != nil {
		return pageData{}, err
	}
	data := pageData{
		Version:             s.config.Version,
		RepositoryRoot:      s.config.RepositoryRoot,
		Repositories:        repositories,
		RepositoryLabels:    catalog.DisplayNames(repositories),
		ActivePage:          "search",
		ChatEnabled:         s.agents != nil,
		WikiEnabled:         s.docs != nil,
		DependenciesEnabled: s.maps != nil,
		MCPEnabled:          s.config.MCPHandler != nil,
		InsightsEnabled:     s.insights != nil,
		CanManageArtifacts:  s.security == nil,
		Search: searchData{
			Query: search.Query{Limit: codeintel.DefaultSearchLimit},
		},
	}
	if s.scipJava != nil {
		data.SCIPJava = s.scipJava.ProviderStatus()
		data.SCIPJavaEnabled = data.SCIPJava.Enabled
	}
	if s.security != nil {
		data.AuthMode = string(s.security.Mode())
		data.AdminEnabled = s.security.AdminEnabled()
		if principal, ok := security.PrincipalFromContext(ctx); ok {
			data.CanAdminister = principal.Admin
			data.CanManageArtifacts = identity.Allows(principal.Role, identity.PermissionManageArtifacts)
			data.UserLabel = principal.Name
			if data.UserLabel == "" {
				data.UserLabel = principal.Email
			}
			if data.UserLabel == "" {
				data.UserLabel = principal.ID
			}
		}
	}
	for _, repository := range repositories {
		switch repository.IndexState {
		case "ready":
			data.ReadyCount++
		case "error":
			data.ErrorCount++
		case "empty":
			data.EmptyCount++
		default:
			data.PendingCount++
		}
		if repository.IndexState != "empty" {
			data.IndexableCount++
		}
	}
	if s.maps != nil {
		progress, progressErr := s.maps.StructureProgress(ctx, 0)
		if progressErr == nil {
			data.ArtifactProgress = progress
		}
	}
	return data, nil
}

func buildMCPPageData(endpoint, token, command, stdioBaseURL string) mcpPageData {
	command = strings.TrimSpace(command)
	if command == "" {
		command = "repokarta"
	}
	if stdioBaseURL == "" {
		stdioBaseURL = strings.TrimSuffix(endpoint, "/mcp")
	}
	httpConfiguration, _ := json.MarshalIndent(map[string]any{
		"mcpServers": map[string]any{
			"repokarta": struct {
				Type    string            `json:"type"`
				URL     string            `json:"url"`
				Headers map[string]string `json:"headers"`
			}{
				Type: "http",
				URL:  endpoint,
				Headers: map[string]string{
					"Authorization": "Bearer " + token,
				},
			},
		},
	}, "", "  ")
	stdioConfiguration, _ := json.MarshalIndent(map[string]any{
		"mcpServers": map[string]any{
			"repokarta": struct {
				Command string   `json:"command"`
				Args    []string `json:"args"`
			}{
				Command: command,
				Args:    []string{"mcp", "-url", stdioBaseURL},
			},
		},
	}, "", "  ")
	tokenPreview := token
	if len(token) > 20 {
		tokenPreview = token[:8] + "••••••••" + token[len(token)-8:]
	}
	catalog := mcpserver.ToolCatalog()
	tools := make([]mcpToolView, len(catalog))
	for index, tool := range catalog {
		tools[index] = mcpToolView{Name: tool.Name, Description: tool.Description}
	}
	return mcpPageData{
		Endpoint:       endpoint,
		Token:          token,
		TokenPreview:   tokenPreview,
		HTTPConfig:     string(httpConfiguration),
		HTTPConfigView: strings.ReplaceAll(string(httpConfiguration), "Bearer "+token, "Bearer <current-token>"),
		StdioConfig:    string(stdioConfiguration),
		Tools:          tools,
	}
}

func (s *Server) render(response http.ResponseWriter, name string, data any) {
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(response, name, data); err != nil {
		slog.Error("render template", "template", name, "error", err)
	}
}

func optionalRepositoryID(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	repositoryID, err := strconv.ParseInt(value, 10, 64)
	if err != nil || repositoryID <= 0 {
		return 0, errors.New("repository must be a positive integer")
	}
	return repositoryID, nil
}

func optionalBool(value string) (bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return false, nil
	}
	return strconv.ParseBool(value)
}

// repositorySelector accepts either the stable numeric repository ID returned
// by /api/repositories or a repository name. Numeric selectors are preferred
// because repository names are not unique across roots.
func repositorySelector(value string) (int64, string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, ""
	}
	if repositoryID, err := strconv.ParseInt(value, 10, 64); err == nil && repositoryID > 0 {
		return repositoryID, ""
	}
	return 0, value
}

func requiredRepositoryID(value string) (int64, error) {
	repositoryID, err := optionalRepositoryID(value)
	if err != nil {
		return 0, err
	}
	if repositoryID == 0 {
		return 0, errors.New("repository is required")
	}
	return repositoryID, nil
}

func writeDocumentationError(response http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, docs.ErrPageNotFound), errors.Is(err, sql.ErrNoRows):
		writeAPIError(response, http.StatusNotFound, errors.New("documentation page was not found"))
	case errors.Is(err, docs.ErrInvalidKnowledgePreset):
		writeAPIError(response, http.StatusUnprocessableEntity, err)
	case errors.Is(err, docs.ErrNothingToExport):
		// An empty Wiki is a normal state, not a server failure, and the
		// caller needs the actual reason.
		writeAPIError(response, http.StatusConflict, err)
	case strings.Contains(err.Error(), ".repokarta.yml"):
		writeAPIError(response, http.StatusUnprocessableEntity, err)
	case errors.Is(err, docs.ErrGenerationRejected):
		// A quality gate rejected the provider result. The reason is the whole
		// value of the message and is safe to return: it names sections, counts,
		// and page slugs, never filesystem paths or credentials. It is also
		// logged so a completed run can be diagnosed after the fact.
		slog.Warn("documentation generation rejected", "error", err)
		writeAPIError(response, http.StatusUnprocessableEntity, err)
	default:
		slog.Error("documentation request", "error", err)
		writeAPIError(response, http.StatusInternalServerError, errors.New("documentation request could not be completed"))
	}
}

func safeDownloadName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var output strings.Builder
	lastDash := false
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') {
			output.WriteRune(character)
			lastDash = false
		} else if !lastDash && output.Len() > 0 {
			output.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(output.String(), "-")
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(status)
	if err := json.NewEncoder(response).Encode(value); err != nil {
		slog.Warn("encode JSON response", "error", err)
	}
}

func formatDuration(duration time.Duration) string {
	if duration < time.Millisecond {
		return "<1 ms"
	}
	return fmt.Sprintf("%d ms", duration.Milliseconds())
}

func formatMilliseconds(milliseconds float64) string {
	if milliseconds < 1 {
		return "<1 ms"
	}
	return fmt.Sprintf("%.0f ms", milliseconds)
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return "Not yet"
	}
	return value.Local().Format("Jan 2, 15:04")
}

func shortCommit(commit string) string {
	if len(commit) <= 8 {
		return commit
	}
	return commit[:8]
}

func statusLabel(state string) string {
	switch state {
	case "ready":
		return "Indexed"
	case "indexing":
		return "Indexing"
	case "error":
		return "Needs attention"
	case "empty":
		return "Empty"
	default:
		return "Queued"
	}
}

func scipStatusLabel(state string) string {
	switch state {
	case "ready":
		return "Precise"
	case "indexing":
		return "Compiling"
	case "pending":
		return "Queued"
	case "failed":
		return "Failed"
	case "unavailable":
		return "Unavailable"
	case "skipped":
		return "Not applicable"
	default:
		return "Not generated"
	}
}

func scipFailureLabel(category string) string {
	switch category {
	case scipjava.FailureEnvironment:
		return "Environment"
	case scipjava.FailureJDKIncompatibleWrapper:
		return "JDK / Gradle compatibility"
	case scipjava.FailureCompileError:
		return "Compilation"
	default:
		return "Build"
	}
}

// nextSearchLimit is the file limit a "Show more" control should request after
// a truncated result set. It roughly doubles the current limit and stops at the
// service ceiling so the control never offers a limit the API would reject.
func nextSearchLimit(limit int) int {
	if limit < 1 {
		limit = codeintel.DefaultSearchLimit
	}
	next := limit * 2
	if next > codeintel.MaximumSearchLimit {
		next = codeintel.MaximumSearchLimit
	}
	return next
}

// indexProgress reports how far first-run indexing has advanced, as a
// percentage suitable for a progress bar width. It is clamped so a catalogue
// that changes size mid-scan cannot produce an out-of-range bar.
func indexProgress(ready, total int) int {
	if total <= 0 {
		return 0
	}
	percent := ready * 100 / total
	return min(100, max(0, percent))
}

func repositorySignature(repositories []catalog.Repository) string {
	var builder strings.Builder
	for _, repository := range repositories {
		fmt.Fprintf(
			&builder,
			"%d:%s:%s:%s:%s:%s:%s;",
			repository.ID,
			repository.HeadCommit,
			repository.ScanState,
			repository.ScanError,
			repository.IndexState,
			repository.IndexError,
			func() string {
				if repository.SCIPJava == nil {
					return ""
				}
				return repository.SCIPJava.State + ":" +
					repository.SCIPJava.Revision + ":" +
					repository.SCIPJava.FailureCategory + ":" +
					repository.SCIPJava.FailureSummary + ":" +
					repository.SCIPJava.Error
			}(),
		)
	}
	return builder.String()
}
