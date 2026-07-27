package httpserver

import (
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spolnik/RepoKarta/internal/identity"
	"github.com/spolnik/RepoKarta/internal/insights"
	"github.com/spolnik/RepoKarta/internal/security"
)

const maximumInsightUploadBytes = 32 << 20

type insightsPageData struct {
	pageData
	Response         insights.QueryResponse
	Filter           insights.Filter
	View             string
	Evaluations      []insights.ThresholdEvaluation
	Connections      []insights.SonarConnection
	Metrics          []insights.Observation
	Findings         []insights.Observation
	Comparison       *insights.Comparison
	SelectedID       int64
	SelectedLabel    string
	SelectedRevision string
	SelectedBranch   string
	Notice           string
	Error            string
	CanManage        bool
	CanAdminister    bool
	SinceValue       string
	UntilValue       string
}

func (s *Server) insightsPage(response http.ResponseWriter, request *http.Request) {
	base, err := s.pageData(request.Context())
	if err != nil {
		http.Error(response, "Could not load repositories", http.StatusInternalServerError)
		return
	}
	base.ActivePage = "insights"
	selected, _ := strconv.ParseInt(strings.TrimSpace(request.URL.Query().Get("repository")), 10, 64)
	filter, err := insightFilter(request, selected)
	if err != nil {
		http.Error(response, err.Error(), http.StatusBadRequest)
		return
	}
	result, err := s.insights.Query(request.Context(), filter)
	if err != nil {
		http.Error(response, err.Error(), http.StatusBadRequest)
		return
	}
	viewer := s.conversationViewer(request.Context())
	canManage := true
	if s.security != nil {
		principal, ok := security.PrincipalFromContext(request.Context())
		canManage = ok && identity.Allows(principal.Role, identity.PermissionManageArtifacts)
	}
	data := insightsPageData{
		pageData: base, Response: result, Filter: filter, SelectedID: selected,
		View:   normalizeInsightsView(request.URL.Query().Get("view")),
		Notice: request.URL.Query().Get("notice"), Error: request.URL.Query().Get("error"),
		CanManage:     canManage,
		CanAdminister: viewer.Admin,
		SinceValue:    request.URL.Query().Get("since"), UntilValue: request.URL.Query().Get("until"),
	}
	if selected > 0 {
		data.SelectedLabel = base.RepositoryLabels[selected]
		for _, repository := range base.Repositories {
			if repository.ID == selected {
				data.SelectedRevision = repository.IndexedCommit
				data.SelectedBranch = repository.DefaultRevision
				break
			}
		}
		data.Evaluations, _ = s.insights.EvaluateThresholds(request.Context(), selected)
	}
	for _, observation := range result.Current {
		if observation.Kind == insights.KindMetric && len(data.Metrics) < 16 {
			data.Metrics = append(data.Metrics, observation)
		}
		if observation.Kind == insights.KindFinding && len(data.Findings) < 100 {
			data.Findings = append(data.Findings, observation)
		}
	}
	fromRevision := strings.TrimSpace(request.URL.Query().Get("from_revision"))
	toRevision := strings.TrimSpace(request.URL.Query().Get("to_revision"))
	if selected > 0 && fromRevision != "" && toRevision != "" {
		comparison, compareErr := s.insights.Compare(request.Context(), selected, fromRevision, toRevision)
		if compareErr != nil {
			data.Error = compareErr.Error()
		} else {
			data.Comparison = &comparison
		}
	}
	if viewer.Admin {
		data.Connections, _ = s.insights.SonarConnections(request.Context())
	}
	s.render(response, "insights", data)
}

func normalizeInsightsView(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "overview", "trends", "ingestion", "integrations":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "overview"
	}
}

func (s *Server) apiInsights(response http.ResponseWriter, request *http.Request) {
	filter, err := insightFilter(request, 0)
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	result, err := s.insights.Query(request.Context(), filter)
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func insightFilter(request *http.Request, fallbackRepositoryID int64) (insights.Filter, error) {
	query := request.URL.Query()
	repositoryID := fallbackRepositoryID
	if value := strings.TrimSpace(query.Get("repository")); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed <= 0 {
			return insights.Filter{}, errors.New("repository must be a positive numeric ID")
		}
		repositoryID = parsed
	}
	limit := 1000
	if value := strings.TrimSpace(query.Get("limit")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 5000 {
			return insights.Filter{}, errors.New("limit must be between 1 and 5000")
		}
		limit = parsed
	}
	since, err := optionalRFC3339(query.Get("since"))
	if err != nil {
		return insights.Filter{}, errors.New("since must be RFC3339")
	}
	until, err := optionalRFC3339(query.Get("until"))
	if err != nil {
		return insights.Filter{}, errors.New("until must be RFC3339")
	}
	return insights.Filter{
		RepositoryID: repositoryID, Revision: query.Get("revision"),
		Branch: query.Get("branch"), Directory: query.Get("directory"),
		File: query.Get("file"), Language: query.Get("language"),
		Tool: query.Get("tool"), Rule: query.Get("rule"),
		Severity: query.Get("severity"), Owner: query.Get("owner"),
		Kind: query.Get("kind"), Since: since, Until: until, Limit: limit,
		IncludeQuarantined: strings.EqualFold(query.Get("include_quarantined"), "true"),
	}, nil
}

func (s *Server) importInsights(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, maximumInsightUploadBytes+(1<<20))
	if err := request.ParseMultipartForm(maximumInsightUploadBytes + (1 << 20)); err != nil {
		s.writeInsightMutationError(response, request, errors.New("invalid or oversized multipart report"))
		return
	}
	file, _, err := request.FormFile("report")
	if err != nil {
		s.writeInsightMutationError(response, request, errors.New("report file is required"))
		return
	}
	defer file.Close()
	content, err := readBoundedReport(file)
	if err != nil {
		s.writeInsightMutationError(response, request, err)
		return
	}
	repositoryID, err := strconv.ParseInt(strings.TrimSpace(request.FormValue("repository_id")), 10, 64)
	if err != nil || repositoryID <= 0 {
		s.writeInsightMutationError(response, request, errors.New("repository_id must be a positive numeric ID"))
		return
	}
	observedAt, err := optionalRFC3339(request.FormValue("observed_at"))
	if err != nil {
		s.writeInsightMutationError(response, request, errors.New("observed_at must be RFC3339"))
		return
	}
	run, err := s.insights.Import(request.Context(), insights.ImportRequest{
		RepositoryID: repositoryID, Revision: request.FormValue("revision"),
		Branch: request.FormValue("branch"), Format: request.FormValue("format"),
		Tool: request.FormValue("tool"), ToolVersion: request.FormValue("tool_version"),
		SourceKind: request.FormValue("source_kind"), SourceRef: request.FormValue("source_ref"),
		RulePack: request.FormValue("rule_pack"), Configuration: request.FormValue("configuration"),
		License: request.FormValue("license"), Owner: request.FormValue("owner"),
		PathPrefix: request.FormValue("path_prefix"), ObservedAt: observedAt, Content: content,
	})
	if err != nil {
		s.writeInsightMutationError(response, request, err)
		return
	}
	s.writeInsightMutation(response, request, repositoryID, run)
}

func readBoundedReport(file multipart.File) ([]byte, error) {
	content, err := io.ReadAll(io.LimitReader(file, maximumInsightUploadBytes+1))
	if err != nil {
		return nil, err
	}
	if len(content) > maximumInsightUploadBytes {
		return nil, errors.New("report exceeds the 32 MiB limit")
	}
	return content, nil
}

func (s *Server) deriveInsights(response http.ResponseWriter, request *http.Request) {
	repositoryID, err := mutationRepositoryID(request)
	if err != nil {
		s.writeInsightMutationError(response, request, err)
		return
	}
	run, err := s.insights.Derive(request.Context(), repositoryID)
	if err != nil {
		s.writeInsightMutationError(response, request, err)
		return
	}
	s.writeInsightMutation(response, request, repositoryID, run)
}

func (s *Server) compareInsights(response http.ResponseWriter, request *http.Request) {
	repositoryID, err := strconv.ParseInt(strings.TrimSpace(request.URL.Query().Get("repository")), 10, 64)
	if err != nil || repositoryID <= 0 {
		writeAPIError(response, http.StatusBadRequest, errors.New("repository must be a positive numeric ID"))
		return
	}
	comparison, err := s.insights.Compare(
		request.Context(), repositoryID,
		request.URL.Query().Get("from_revision"), request.URL.Query().Get("to_revision"),
	)
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	writeJSON(response, http.StatusOK, comparison)
}

func (s *Server) insightThresholds(response http.ResponseWriter, request *http.Request) {
	repositoryID, _ := strconv.ParseInt(strings.TrimSpace(request.URL.Query().Get("repository")), 10, 64)
	thresholds, err := s.insights.Thresholds(request.Context(), repositoryID)
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"thresholds": thresholds,
		"advisory":   true,
		"notice":     "RepoKarta evaluates advisory thresholds; it does not enforce a CI gate.",
	})
}

func (s *Server) setInsightThreshold(response http.ResponseWriter, request *http.Request) {
	if !s.requireInsightAdmin(response, request) {
		return
	}
	var threshold insights.Threshold
	if strings.HasPrefix(request.URL.Path, "/api/") {
		request.Body = http.MaxBytesReader(response, request.Body, 32<<10)
		if err := json.NewDecoder(request.Body).Decode(&threshold); err != nil {
			writeAPIError(response, http.StatusBadRequest, errors.New("invalid threshold JSON"))
			return
		}
	} else {
		if err := request.ParseForm(); err != nil {
			s.writeInsightMutationError(response, request, errors.New("invalid threshold form"))
			return
		}
		threshold.RepositoryID, _ = strconv.ParseInt(request.FormValue("repository_id"), 10, 64)
		threshold.Key = request.FormValue("key")
		threshold.Operator = request.FormValue("operator")
		threshold.Value, _ = strconv.ParseFloat(request.FormValue("value"), 64)
		threshold.Severity = request.FormValue("severity")
		threshold.Enabled = true
	}
	updated, err := s.insights.SetThreshold(request.Context(), threshold)
	if err != nil {
		s.writeInsightMutationError(response, request, err)
		return
	}
	if !strings.HasPrefix(request.URL.Path, "/api/") {
		http.Redirect(response, request, "/insights?repository="+strconv.FormatInt(updated.RepositoryID, 10)+"&notice="+urlQueryEscape("Advisory threshold saved"), http.StatusSeeOther)
		return
	}
	writeJSON(response, http.StatusOK, updated)
}

func (s *Server) sonarConnections(response http.ResponseWriter, request *http.Request) {
	if !s.requireInsightAdmin(response, request) {
		return
	}
	connections, err := s.insights.SonarConnections(request.Context())
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"connections": connections})
}

func (s *Server) configureSonar(response http.ResponseWriter, request *http.Request) {
	if !s.requireInsightAdmin(response, request) {
		return
	}
	var connection insights.SonarConnection
	if strings.HasPrefix(request.URL.Path, "/api/") {
		request.Body = http.MaxBytesReader(response, request.Body, 32<<10)
		if err := json.NewDecoder(request.Body).Decode(&connection); err != nil {
			writeAPIError(response, http.StatusBadRequest, errors.New("invalid SonarQube connection JSON"))
			return
		}
	} else {
		if err := request.ParseForm(); err != nil {
			s.writeInsightMutationError(response, request, errors.New("invalid SonarQube form"))
			return
		}
		connection.RepositoryID, _ = strconv.ParseInt(request.FormValue("repository_id"), 10, 64)
		connection.BaseURL = request.FormValue("base_url")
		connection.ProjectKey = request.FormValue("project_key")
		connection.TokenEnv = request.FormValue("token_env")
		connection.PollIntervalMinutes, _ = strconv.Atoi(request.FormValue("poll_interval_minutes"))
		connection.RetentionRuns, _ = strconv.Atoi(request.FormValue("retention_runs"))
		connection.Enabled = true
	}
	updated, err := s.insights.ConfigureSonar(request.Context(), connection)
	if err != nil {
		s.writeInsightMutationError(response, request, err)
		return
	}
	if !strings.HasPrefix(request.URL.Path, "/api/") {
		http.Redirect(response, request, "/insights?repository="+strconv.FormatInt(updated.RepositoryID, 10)+"&notice="+urlQueryEscape("SonarQube connection saved; credential value remains in the environment"), http.StatusSeeOther)
		return
	}
	writeJSON(response, http.StatusOK, updated)
}

func (s *Server) syncSonar(response http.ResponseWriter, request *http.Request) {
	if !s.requireInsightAdmin(response, request) {
		return
	}
	repositoryID, err := mutationRepositoryID(request)
	if err != nil {
		s.writeInsightMutationError(response, request, err)
		return
	}
	run, err := s.insights.SyncSonar(request.Context(), repositoryID)
	if err != nil {
		s.writeInsightMutationError(response, request, err)
		return
	}
	s.writeInsightMutation(response, request, repositoryID, run)
}

func mutationRepositoryID(request *http.Request) (int64, error) {
	contentType := request.Header.Get("Content-Type")
	if strings.Contains(contentType, "application/json") {
		var input struct {
			RepositoryID int64 `json:"repository_id"`
		}
		if err := json.NewDecoder(io.LimitReader(request.Body, 32<<10)).Decode(&input); err != nil {
			return 0, errors.New("invalid request JSON")
		}
		if input.RepositoryID <= 0 {
			return 0, errors.New("repository_id must be a positive numeric ID")
		}
		return input.RepositoryID, nil
	}
	if err := request.ParseForm(); err != nil {
		return 0, errors.New("invalid request form")
	}
	value, err := strconv.ParseInt(strings.TrimSpace(request.FormValue("repository_id")), 10, 64)
	if err != nil || value <= 0 {
		return 0, errors.New("repository_id must be a positive numeric ID")
	}
	return value, nil
}

func (s *Server) writeInsightMutation(response http.ResponseWriter, request *http.Request, repositoryID int64, run insights.Run) {
	if strings.HasPrefix(request.URL.Path, "/api/") {
		writeJSON(response, http.StatusCreated, run)
		return
	}
	location := "/insights?repository=" + strconv.FormatInt(repositoryID, 10) +
		"&notice=" + urlQueryEscape("Stored "+run.Tool+" run as "+run.Status)
	http.Redirect(response, request, location, http.StatusSeeOther)
}

func (s *Server) writeInsightMutationError(response http.ResponseWriter, request *http.Request, err error) {
	if strings.HasPrefix(request.URL.Path, "/api/") {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	http.Redirect(response, request, "/insights?error="+urlQueryEscape(err.Error()), http.StatusSeeOther)
}

func (s *Server) requireInsightAdmin(response http.ResponseWriter, request *http.Request) bool {
	if s.conversationViewer(request.Context()).Admin {
		return true
	}
	writeAPIError(response, http.StatusForbidden, errors.New("administrator access is required"))
	return false
}

func optionalRFC3339(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, value)
}

func urlQueryEscape(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(url.QueryEscape(value), "+", "%20"), "%2F", "/")
}
