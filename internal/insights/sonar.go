package insights

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const sonarMetricKeys = "coverage,new_coverage,ncloc,complexity,cognitive_complexity,duplicated_lines_density,reliability_rating,security_rating,sqale_rating,bugs,vulnerabilities,code_smells"

func (s *Service) ConfigureSonar(ctx context.Context, connection SonarConnection) (SonarConnection, error) {
	if _, err := s.store.RepositoryByID(ctx, connection.RepositoryID); err != nil {
		return SonarConnection{}, err
	}
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(connection.BaseURL), "/"))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return SonarConnection{}, errors.New("SonarQube base URL must be an absolute HTTP or HTTPS URL")
	}
	if parsed.Scheme == "http" && parsed.Hostname() != "localhost" && parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "::1" {
		return SonarConnection{}, errors.New("non-loopback SonarQube connections require HTTPS")
	}
	connection.BaseURL = strings.TrimRight(parsed.String(), "/")
	connection.ProjectKey = strings.TrimSpace(connection.ProjectKey)
	connection.TokenEnv = strings.TrimSpace(connection.TokenEnv)
	if connection.ProjectKey == "" || connection.TokenEnv == "" {
		return SonarConnection{}, errors.New("SonarQube project key and credential environment variable are required")
	}
	if connection.PollIntervalMinutes == 0 {
		connection.PollIntervalMinutes = 15
	}
	if connection.PollIntervalMinutes < 5 || connection.PollIntervalMinutes > 1440 {
		return SonarConnection{}, errors.New("SonarQube poll interval must be between 5 and 1440 minutes")
	}
	if connection.RetentionRuns == 0 {
		connection.RetentionRuns = defaultRetention
	}
	if connection.RetentionRuns < 1 || connection.RetentionRuns > 500 {
		return SonarConnection{}, errors.New("SonarQube retention must be between 1 and 500 runs")
	}
	connection.State = StatusStale
	connection.NextPollAt = time.Now().UTC()
	return s.store.UpsertSonarConnection(ctx, connection)
}

func (s *Service) SonarConnections(ctx context.Context) ([]SonarConnection, error) {
	return s.store.ListSonarConnections(ctx, false)
}

func (s *Service) SyncSonar(ctx context.Context, repositoryID int64) (Run, error) {
	connections, err := s.store.ListSonarConnections(ctx, false)
	if err != nil {
		return Run{}, err
	}
	for _, connection := range connections {
		if connection.RepositoryID == repositoryID {
			run, syncErr := s.syncSonarConnection(ctx, connection)
			now := time.Now().UTC()
			connection.LastPolledAt = now
			if syncErr == nil {
				connection.State = run.Status
				connection.StatusMessage = run.StatusMessage
				connection.FailureCount = 0
				connection.NextPollAt = now.Add(time.Duration(connection.PollIntervalMinutes) * time.Minute)
			} else {
				connection.FailureCount++
				connection.State = StatusUnavailable
				var httpErr *sonarHTTPError
				if errors.As(syncErr, &httpErr) {
					connection.State = httpErr.status
				}
				connection.StatusMessage = syncErr.Error()
				connection.NextPollAt = now.Add(sonarBackoff(connection))
			}
			if stateErr := s.store.UpdateSonarConnectionState(ctx, connection); stateErr != nil && syncErr == nil {
				return Run{}, stateErr
			}
			return run, syncErr
		}
	}
	return Run{}, fmt.Errorf("repository %d has no SonarQube connection", repositoryID)
}

func (s *Service) syncSonarConnection(ctx context.Context, connection SonarConnection) (Run, error) {
	repository, err := s.store.RepositoryByID(ctx, connection.RepositoryID)
	if err != nil {
		return Run{}, err
	}
	token := strings.TrimSpace(os.Getenv(connection.TokenEnv))
	if token == "" {
		return Run{}, fmt.Errorf("SonarQube credential environment variable %s is empty", connection.TokenEnv)
	}
	var analyses sonarAnalyses
	if err := s.sonarGET(ctx, connection, token, "/api/project_analyses/search", url.Values{
		"project": {connection.ProjectKey}, "ps": {"1"},
	}, &analyses); err != nil {
		return Run{}, err
	}
	if len(analyses.Analyses) == 0 || strings.TrimSpace(analyses.Analyses[0].Revision) == "" {
		return Run{}, errors.New("SonarQube did not return an exact analysis revision")
	}
	analysis := analyses.Analyses[0]
	observedAt := parseSonarTime(analysis.Date)
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	var measures sonarMeasures
	if err := s.sonarGET(ctx, connection, token, "/api/measures/component", url.Values{
		"component": {connection.ProjectKey}, "metricKeys": {sonarMetricKeys},
	}, &measures); err != nil {
		return Run{}, err
	}
	var issues sonarIssues
	issuesErr := s.sonarGET(ctx, connection, token, "/api/issues/search", url.Values{
		"componentKeys": {connection.ProjectKey}, "ps": {"500"}, "resolved": {"false"},
	}, &issues)
	var gate sonarQualityGate
	gateErr := s.sonarGET(ctx, connection, token, "/api/qualitygates/project_status", url.Values{
		"projectKey": {connection.ProjectKey},
	}, &gate)

	run := Run{
		ID: newRunID(), RepositoryID: repository.ID, Repository: repository.Name,
		Revision: analysis.Revision, Branch: repository.DefaultRevision,
		Tool: "SonarQube Community Build", SourceKind: "sonarqube_web_api",
		SourceRef: connection.BaseURL + "/dashboard?id=" + url.QueryEscape(connection.ProjectKey),
		Status:    StatusCurrent, Confidence: "reported", ObservedAt: observedAt,
		IngestedAt: time.Now().UTC(), Metadata: map[string]string{
			"project_key":          connection.ProjectKey,
			"analysis_key":         analysis.Key,
			"credential_reference": connection.TokenEnv,
		},
	}
	if analysis.Revision != repository.IndexedCommit || repository.IndexedCommit == "" {
		run.Status = StatusQuarantined
		run.StatusMessage = fmt.Sprintf(
			"SonarQube analysis revision %s does not match indexed revision %s",
			analysis.Revision, firstNonEmpty(repository.IndexedCommit, "(none)"),
		)
	}
	var observations []Observation
	for _, measure := range measures.Component.Measures {
		value, parseErr := strconv.ParseFloat(measure.Value, 64)
		if parseErr != nil {
			continue
		}
		unit := sonarUnit(measure.Metric)
		observations = append(observations, Observation{
			Kind: KindMetric, Key: "sonar." + measure.Metric, Value: number(value),
			Unit: unit, State: StateMeasured, Confidence: "reported",
			Metadata: map[string]any{"scope": "repository"},
		})
	}
	if issuesErr != nil {
		if run.Status == StatusCurrent {
			run.Status = StatusPartial
		}
		run.StatusMessage = appendStatus(run.StatusMessage, "issues unavailable: "+issuesErr.Error())
	} else {
		for _, issue := range issues.Issues {
			file := strings.TrimPrefix(issue.Component, connection.ProjectKey+":")
			observations = append(observations, Observation{
				Kind: KindFinding, Key: issue.Rule, Severity: sonarSeverity(issue.Severity),
				Message: issue.Message, Path: file, StartLine: issue.TextRange.StartLine,
				EndLine: issue.TextRange.EndLine, Fingerprint: firstNonEmpty(issue.Hash, issue.Key),
				Suppressed: issue.Resolution != "", State: StateMeasured, Confidence: "reported",
				Metadata: map[string]any{
					"issue_key": issue.Key, "type": issue.Type, "status": issue.Status,
					"resolution": issue.Resolution, "effort": issue.Effort,
					"flows": issue.Flows,
				},
			})
		}
	}
	if gateErr != nil {
		if run.Status == StatusCurrent {
			run.Status = StatusPartial
		}
		run.StatusMessage = appendStatus(run.StatusMessage, "quality gate unavailable: "+gateErr.Error())
	} else {
		gateValue := 1.0
		if !strings.EqualFold(gate.ProjectStatus.Status, "OK") {
			gateValue = 0
		}
		observations = append(observations, Observation{
			Kind: KindMetric, Key: "sonar.quality_gate", Value: number(gateValue),
			Unit: "boolean", State: StateMeasured, Confidence: "reported",
			Message: gate.ProjectStatus.Status,
			Metadata: map[string]any{
				"scope": "repository", "originating_gate": true,
				"conditions": gate.ProjectStatus.Conditions,
			},
		})
	}
	paths, pathErr := committedPaths(ctx, repository, analysis.Revision)
	if pathErr != nil && run.Status != StatusQuarantined {
		run.Status = StatusPartial
		run.StatusMessage = appendStatus(run.StatusMessage, "source reconciliation unavailable: "+pathErr.Error())
	}
	unresolved := 0
	for index := range observations {
		observation := &observations[index]
		observation.RunID = run.ID
		observation.RepositoryID = repository.ID
		observation.Repository = repository.Name
		observation.Revision = run.Revision
		observation.Branch = run.Branch
		observation.ObservedAt = observedAt
		if observation.Path != "" && paths != nil {
			if reconciled, ok := reconcilePath(observation.Path, "", paths); ok {
				observation.Path = reconciled
				observation.SourceURL = s.sourceURL(repository.ID, run.Revision, reconciled, observation.StartLine)
			} else {
				observation.State = StateUnresolvedPath
				unresolved++
			}
		}
	}
	if unresolved > 0 {
		if run.Status == StatusCurrent {
			run.Status = StatusPartial
		}
		run.StatusMessage = appendStatus(run.StatusMessage, fmt.Sprintf("%d SonarQube issue paths are unresolved", unresolved))
	}
	run.ObservationCount = len(observations)
	if err := s.store.SaveInsightRun(ctx, run, observations); err != nil {
		return Run{}, err
	}
	if err := s.store.DeleteOldInsightRuns(ctx, repository.ID, run.Tool, connection.RetentionRuns); err != nil {
		return Run{}, err
	}
	return run, nil
}

func (s *Service) sonarGET(ctx context.Context, connection SonarConnection, token, endpoint string, query url.Values, output any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, connection.BaseURL+endpoint+"?"+query.Encode(), nil)
	if err != nil {
		return err
	}
	request.SetBasicAuth(token, "")
	request.Header.Set("Accept", "application/json")
	response, err := s.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("SonarQube request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusTooManyRequests {
		return &sonarHTTPError{status: StatusRateLimited, message: "SonarQube rate limited the request"}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return &sonarHTTPError{
			status:  StatusUnavailable,
			message: fmt.Sprintf("SonarQube returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body))),
		}
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(output); err != nil {
		return fmt.Errorf("decode SonarQube response: %w", err)
	}
	return nil
}

// StartPolling runs bounded external polling. It performs no local scans.
func (s *Service) StartPolling(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		s.pollDue(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.pollDue(ctx)
			}
		}
	}()
}

func (s *Service) pollDue(ctx context.Context) {
	connections, err := s.store.ListSonarConnections(ctx, true)
	if err != nil {
		return
	}
	for _, connection := range connections {
		connection := connection
		select {
		case s.pollSem <- struct{}{}:
			go func() {
				defer func() { <-s.pollSem }()
				pollContext, cancel := context.WithTimeout(ctx, 45*time.Second)
				defer cancel()
				run, pollErr := s.syncSonarConnection(pollContext, connection)
				now := time.Now().UTC()
				connection.LastPolledAt = now
				if pollErr == nil {
					connection.State = run.Status
					connection.StatusMessage = run.StatusMessage
					connection.FailureCount = 0
					connection.NextPollAt = now.Add(time.Duration(connection.PollIntervalMinutes) * time.Minute)
				} else {
					connection.FailureCount++
					connection.State = StatusUnavailable
					var httpErr *sonarHTTPError
					if errors.As(pollErr, &httpErr) {
						connection.State = httpErr.status
					}
					connection.StatusMessage = pollErr.Error()
					connection.NextPollAt = now.Add(sonarBackoff(connection))
				}
				_ = s.store.UpdateSonarConnectionState(context.WithoutCancel(ctx), connection)
			}()
		default:
			return
		}
	}
}

func sonarBackoff(connection SonarConnection) time.Duration {
	backoff := time.Duration(connection.PollIntervalMinutes) * time.Minute
	for index := 1; index < connection.FailureCount && backoff < 6*time.Hour; index++ {
		backoff *= 2
	}
	if backoff > 6*time.Hour {
		backoff = 6 * time.Hour
	}
	return backoff
}

type sonarHTTPError struct {
	status  string
	message string
}

func (e *sonarHTTPError) Error() string { return e.message }

type sonarAnalyses struct {
	Analyses []struct {
		Key      string `json:"key"`
		Date     string `json:"date"`
		Revision string `json:"revision"`
	} `json:"analyses"`
}

type sonarMeasures struct {
	Component struct {
		Measures []struct {
			Metric string `json:"metric"`
			Value  string `json:"value"`
		} `json:"measures"`
	} `json:"component"`
}

type sonarIssues struct {
	Issues []struct {
		Key        string `json:"key"`
		Rule       string `json:"rule"`
		Severity   string `json:"severity"`
		Component  string `json:"component"`
		Message    string `json:"message"`
		Hash       string `json:"hash"`
		Status     string `json:"status"`
		Resolution string `json:"resolution"`
		Type       string `json:"type"`
		Effort     string `json:"effort"`
		TextRange  struct {
			StartLine int `json:"startLine"`
			EndLine   int `json:"endLine"`
		} `json:"textRange"`
		Flows []any `json:"flows"`
	} `json:"issues"`
}

type sonarQualityGate struct {
	ProjectStatus struct {
		Status     string `json:"status"`
		Conditions []any  `json:"conditions"`
	} `json:"projectStatus"`
}

func parseSonarTime(value string) time.Time {
	parsed, _ := time.Parse(time.RFC3339, value)
	if parsed.IsZero() {
		parsed, _ = time.Parse("2006-01-02T15:04:05-0700", value)
	}
	return parsed.UTC()
}

func sonarSeverity(value string) string {
	switch strings.ToUpper(value) {
	case "BLOCKER", "CRITICAL":
		return "error"
	case "MAJOR":
		return "warning"
	case "MINOR", "INFO":
		return "note"
	default:
		return normalizeSeverity(value)
	}
}

func sonarUnit(metric string) string {
	switch metric {
	case "coverage", "new_coverage", "duplicated_lines_density":
		return "percent"
	case "ncloc":
		return "lines"
	case "complexity", "cognitive_complexity":
		return "points"
	case "reliability_rating", "security_rating", "sqale_rating":
		return "rating"
	default:
		return "count"
	}
}
