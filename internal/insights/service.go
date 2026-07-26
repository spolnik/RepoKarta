package insights

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spolnik/RepoKarta/internal/catalog"
)

const (
	maximumReportBytes = 32 << 20
	defaultRetention   = 50
)

// Service imports, reconciles, stores, compares, and exposes observations.
type Service struct {
	store      RepositoryStore
	httpClient *http.Client
	mu         sync.RWMutex
	baseURL    string
	pollSem    chan struct{}
}

func New(store RepositoryStore, baseURL string) *Service {
	return &Service{
		store: store, baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		httpClient: &http.Client{Timeout: 25 * time.Second},
		pollSem:    make(chan struct{}, 2),
	}
}

func (s *Service) SetBaseURL(baseURL string) {
	s.mu.Lock()
	s.baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	s.mu.Unlock()
}

func (s *Service) Import(ctx context.Context, request ImportRequest) (Run, error) {
	repository, err := s.store.RepositoryByID(ctx, request.RepositoryID)
	if err != nil {
		return Run{}, err
	}
	if len(request.Content) > maximumReportBytes {
		return Run{}, fmt.Errorf("report exceeds the %d MiB limit", maximumReportBytes>>20)
	}
	request.Revision = strings.TrimSpace(request.Revision)
	request.Branch = strings.TrimSpace(request.Branch)
	request.Format = strings.ToLower(strings.TrimSpace(request.Format))
	if request.Revision == "" {
		return Run{}, errors.New("the exact analyzed Git revision is required")
	}
	if request.ObservedAt.IsZero() {
		request.ObservedAt = time.Now().UTC()
	} else {
		request.ObservedAt = request.ObservedAt.UTC()
	}
	run := Run{
		ID: newRunID(), RepositoryID: repository.ID, Repository: repository.Name,
		Revision: request.Revision, Branch: request.Branch,
		Tool:        firstNonEmpty(request.Tool, defaultToolForFormat(request.Format)),
		ToolVersion: strings.TrimSpace(request.ToolVersion),
		SourceKind:  firstNonEmpty(request.SourceKind, "uploaded_report"),
		SourceRef:   strings.TrimSpace(request.SourceRef), RulePack: strings.TrimSpace(request.RulePack),
		Configuration: strings.TrimSpace(request.Configuration), License: strings.TrimSpace(request.License),
		Status: StatusCurrent, Confidence: "reported", ObservedAt: request.ObservedAt,
		IngestedAt: time.Now().UTC(), Metadata: map[string]string{"format": request.Format},
	}

	var observations []Observation
	var warnings []string
	var metadata map[string]string
	var parseErr error
	switch request.Format {
	case "lcov", "jacoco", "jacoco-xml", "cobertura", "cobertura-xml":
		observations, warnings, parseErr = parseCoverage(request.Format, request.Content)
	case "sarif", "sarif-2.1.0", "megalinter-sarif":
		observations, metadata, warnings, parseErr = parseSARIF(request.Content)
	case "semgrep", "semgrep-json":
		observations, metadata, warnings, parseErr = parseSemgrep(request.Content)
	default:
		parseErr = fmt.Errorf("unsupported insight report format %q", request.Format)
	}
	for key, value := range metadata {
		run.Metadata[key] = value
	}
	if parseErr != nil {
		run.Status = StatusQuarantined
		run.StatusMessage = parseErr.Error()
		observations = []Observation{{
			Kind: KindFinding, Key: "ingestion.parse_error", Severity: "error",
			Message: parseErr.Error(), State: StateParseError, Confidence: "reported",
		}}
	}
	if len(warnings) > 0 {
		run.StatusMessage = strings.Join(warnings, "; ")
		if run.Status == StatusCurrent {
			run.Status = StatusPartial
		}
	}
	if request.Revision != repository.IndexedCommit || repository.IndexedCommit == "" {
		run.Status = StatusQuarantined
		run.StatusMessage = appendStatus(run.StatusMessage, fmt.Sprintf(
			"report revision %s does not match indexed revision %s",
			request.Revision, firstNonEmpty(repository.IndexedCommit, "(none)"),
		))
	}

	var repositoryPaths map[string]string
	if run.Status != StatusQuarantined {
		repositoryPaths, err = committedPaths(ctx, repository, request.Revision)
		if err != nil {
			run.Status = StatusPartial
			run.StatusMessage = appendStatus(run.StatusMessage, "could not reconcile report paths: "+err.Error())
		}
	}
	unresolved := 0
	for index := range observations {
		observation := &observations[index]
		observation.RunID = run.ID
		observation.RepositoryID = repository.ID
		observation.Repository = repository.Name
		observation.Revision = request.Revision
		observation.Branch = request.Branch
		observation.Owner = firstNonEmpty(observation.Owner, request.Owner)
		observation.ObservedAt = request.ObservedAt
		if observation.Kind == "" {
			observation.Kind = KindFinding
		}
		if observation.State == "" {
			observation.State = StateMeasured
		}
		if observation.Confidence == "" {
			observation.Confidence = "reported"
		}
		if observation.Path != "" && repositoryPaths != nil {
			reconciled, ok := reconcilePath(observation.Path, request.PathPrefix, repositoryPaths)
			if !ok {
				observation.State = StateUnresolvedPath
				observation.SourceURL = ""
				unresolved++
			} else {
				observation.Path = reconciled
				observation.SourceURL = s.sourceURL(repository.ID, request.Revision, reconciled, observation.StartLine)
			}
		}
		if observation.Path == "" && observation.Metadata == nil {
			observation.Metadata = map[string]any{"scope": "aggregate"}
		}
	}
	if unresolved > 0 {
		if run.Status == StatusCurrent {
			run.Status = StatusPartial
		}
		run.StatusMessage = appendStatus(run.StatusMessage, fmt.Sprintf(
			"%d observation paths could not be reconciled with the indexed snapshot", unresolved,
		))
	}
	run.ObservationCount = len(observations)
	if err := s.store.SaveInsightRun(ctx, run, observations); err != nil {
		return Run{}, err
	}
	if err := s.store.DeleteOldInsightRuns(ctx, repository.ID, run.Tool, defaultRetention); err != nil {
		return Run{}, fmt.Errorf("apply insight retention: %w", err)
	}
	return run, nil
}

func (s *Service) Query(ctx context.Context, filter Filter) (QueryResponse, error) {
	filter, err := s.authorizedFilter(ctx, filter)
	if err != nil {
		return QueryResponse{}, err
	}
	currentRevisions, err := s.currentRevisions(ctx, filter)
	if err != nil {
		return QueryResponse{}, err
	}
	if filter.Limit <= 0 {
		filter.Limit = 1000
	}
	requestedLimit := min(filter.Limit, 5000)
	filter.Limit = requestedLimit + 1
	observations, err := s.store.ListInsightObservations(ctx, filter)
	if err != nil {
		return QueryResponse{}, err
	}
	truncated := len(observations) > requestedLimit
	if truncated {
		observations = observations[:requestedLimit]
	}
	runFilter := filter
	runFilter.Limit = min(200, requestedLimit)
	runs, err := s.store.ListInsightRuns(ctx, runFilter)
	if err != nil {
		return QueryResponse{}, err
	}
	response := QueryResponse{
		Current: []Observation{}, History: []Observation{}, Runs: runs,
		Truncated: truncated, Facets: emptyFacets(), GeneratedAt: time.Now().UTC(),
	}
	for index := range response.Runs {
		run := &response.Runs[index]
		currentRevision := currentRevisions[run.RepositoryID]
		if currentRevision == "" && run.Status != StatusQuarantined {
			run.Status = StatusUnavailable
			run.StatusMessage = appendStatus(run.StatusMessage, "repository has no indexed revision")
		} else if run.Revision != currentRevision && run.Status != StatusQuarantined {
			run.Status = StatusStale
			run.StatusMessage = appendStatus(
				run.StatusMessage,
				fmt.Sprintf("indexed revision advanced to %s", shortRevision(currentRevision)),
			)
		}
	}
	seen := make(map[string]struct{})
	staleObservations := 0
	for _, observation := range observations {
		identity := observationIdentity(observation)
		selectedRevision := strings.TrimSpace(filter.Revision)
		if selectedRevision == "" {
			selectedRevision = currentRevisions[observation.RepositoryID]
		}
		if selectedRevision != "" && observation.Revision == selectedRevision {
			if _, ok := seen[identity]; !ok {
				seen[identity] = struct{}{}
				response.Current = append(response.Current, observation)
			} else {
				response.History = append(response.History, observation)
			}
		} else {
			staleObservations++
			response.History = append(response.History, observation)
		}
		addFacet(response.Facets.Repositories, observation.Repository)
		addFacet(response.Facets.Branches, observation.Branch)
		addFacet(response.Facets.Languages, observation.Language)
		addFacet(response.Facets.Rules, observation.Key)
		addFacet(response.Facets.Severities, observation.Severity)
		addFacet(response.Facets.Owners, observation.Owner)
		addFacet(response.Facets.States, observation.State)
		addFacet(response.Facets.Tools, observation.Tool)
	}
	if truncated {
		response.Warnings = append(response.Warnings, fmt.Sprintf("observation window truncated at %d entries", requestedLimit))
	}
	if staleObservations > 0 {
		response.Warnings = append(response.Warnings, fmt.Sprintf(
			"%d observations are historical because their revision is no longer indexed",
			staleObservations,
		))
	}
	return response, nil
}

func (s *Service) currentRevisions(ctx context.Context, filter Filter) (map[int64]string, error) {
	output := make(map[int64]string)
	if filter.RepositoryID > 0 {
		repository, err := s.store.RepositoryByID(ctx, filter.RepositoryID)
		if err != nil {
			return nil, err
		}
		output[repository.ID] = strings.TrimSpace(repository.IndexedCommit)
		return output, nil
	}
	repositories, err := s.store.ListRepositories(ctx)
	if err != nil {
		return nil, err
	}
	for _, repository := range repositories {
		output[repository.ID] = strings.TrimSpace(repository.IndexedCommit)
	}
	return output, nil
}

func shortRevision(revision string) string {
	revision = strings.TrimSpace(revision)
	if len(revision) > 8 {
		return revision[:8]
	}
	return revision
}

func (s *Service) Compare(ctx context.Context, repositoryID int64, fromRevision, toRevision string) (Comparison, error) {
	if _, err := s.store.RepositoryByID(ctx, repositoryID); err != nil {
		return Comparison{}, err
	}
	fromRevision = strings.TrimSpace(fromRevision)
	toRevision = strings.TrimSpace(toRevision)
	if fromRevision == "" || toRevision == "" || fromRevision == toRevision {
		return Comparison{}, errors.New("two distinct exact revisions are required")
	}
	load := func(revision string) ([]Observation, error) {
		return s.store.ListInsightObservations(ctx, Filter{
			RepositoryID: repositoryID, Revision: revision,
			IncludeQuarantined: false, Limit: 5000,
		})
	}
	from, err := load(fromRevision)
	if err != nil {
		return Comparison{}, err
	}
	to, err := load(toRevision)
	if err != nil {
		return Comparison{}, err
	}
	result := Comparison{RepositoryID: repositoryID, FromRevision: fromRevision, ToRevision: toRevision}
	fromMetrics, toMetrics := latestMetrics(from), latestMetrics(to)
	keys := make(map[string]struct{}, len(fromMetrics)+len(toMetrics))
	for key := range fromMetrics {
		keys[key] = struct{}{}
	}
	for key := range toMetrics {
		keys[key] = struct{}{}
	}
	var ordered []string
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	for _, key := range ordered {
		left, leftOK := fromMetrics[key]
		right, rightOK := toMetrics[key]
		delta := MetricDelta{Key: firstNonEmpty(right.Key, left.Key), Path: firstNonEmpty(right.Path, left.Path), Unit: firstNonEmpty(right.Unit, left.Unit)}
		if leftOK {
			delta.FromValue = left.Value
		}
		if rightOK {
			delta.ToValue = right.Value
		}
		if leftOK && rightOK && left.Value != nil && right.Value != nil {
			value := *right.Value - *left.Value
			delta.Delta = &value
		}
		result.MetricDeltas = append(result.MetricDeltas, delta)
	}
	fromFindings, toFindings := findingSet(from), findingSet(to)
	for key, finding := range toFindings {
		if _, exists := fromFindings[key]; !exists {
			result.Introduced = append(result.Introduced, finding)
		}
	}
	for key, finding := range fromFindings {
		if _, exists := toFindings[key]; !exists {
			result.Resolved = append(result.Resolved, finding)
		}
	}
	sort.Slice(result.Introduced, func(i, j int) bool {
		return observationIdentity(result.Introduced[i]) < observationIdentity(result.Introduced[j])
	})
	sort.Slice(result.Resolved, func(i, j int) bool {
		return observationIdentity(result.Resolved[i]) < observationIdentity(result.Resolved[j])
	})
	if len(from) == 0 || len(to) == 0 {
		result.Warnings = append(result.Warnings, "one or both revisions have no comparable normalized observations")
	}
	return result, nil
}

func (s *Service) Thresholds(ctx context.Context, repositoryID int64) ([]Threshold, error) {
	if repositoryID > 0 {
		if _, err := s.store.RepositoryByID(ctx, repositoryID); err != nil {
			return nil, err
		}
	}
	return s.store.ListInsightThresholds(ctx, repositoryID)
}

func (s *Service) SetThreshold(ctx context.Context, threshold Threshold) (Threshold, error) {
	threshold.Key = strings.TrimSpace(threshold.Key)
	threshold.Operator = strings.ToLower(strings.TrimSpace(threshold.Operator))
	threshold.Severity = firstNonEmpty(threshold.Severity, "warning")
	if threshold.Key == "" {
		return Threshold{}, errors.New("threshold metric key is required")
	}
	switch threshold.Operator {
	case "lt", "lte", "gt", "gte":
	default:
		return Threshold{}, errors.New("threshold operator must be lt, lte, gt, or gte")
	}
	if threshold.RepositoryID > 0 {
		if _, err := s.store.RepositoryByID(ctx, threshold.RepositoryID); err != nil {
			return Threshold{}, err
		}
	}
	return s.store.UpsertInsightThreshold(ctx, threshold)
}

func (s *Service) EvaluateThresholds(ctx context.Context, repositoryID int64) ([]ThresholdEvaluation, error) {
	thresholds, err := s.Thresholds(ctx, repositoryID)
	if err != nil {
		return nil, err
	}
	response, err := s.Query(ctx, Filter{RepositoryID: repositoryID, Kind: KindMetric, Limit: 5000})
	if err != nil {
		return nil, err
	}
	latest := make(map[string]Observation)
	for _, observation := range response.Current {
		if observation.Path == "" && observation.Value != nil {
			if _, exists := latest[observation.Key]; !exists {
				latest[observation.Key] = observation
			}
		}
	}
	var output []ThresholdEvaluation
	for _, threshold := range thresholds {
		if !threshold.Enabled {
			continue
		}
		observation, ok := latest[threshold.Key]
		if !ok || observation.Value == nil {
			continue
		}
		value := *observation.Value
		violated := map[string]bool{
			"lt": value < threshold.Value, "lte": value <= threshold.Value,
			"gt": value > threshold.Value, "gte": value >= threshold.Value,
		}[threshold.Operator]
		output = append(output, ThresholdEvaluation{
			Threshold: threshold, Observed: observation, Violated: violated, Advisory: true,
		})
	}
	return output, nil
}

func (s *Service) authorizedFilter(ctx context.Context, filter Filter) (Filter, error) {
	if filter.RepositoryID > 0 {
		if _, err := s.store.RepositoryByID(ctx, filter.RepositoryID); err != nil {
			return filter, err
		}
		filter.RepositoryIDs = nil
		return filter, nil
	}
	repositories, err := s.store.ListRepositories(ctx)
	if err != nil {
		return filter, err
	}
	filter.RepositoryIDs = make([]int64, 0, len(repositories))
	for _, repository := range repositories {
		filter.RepositoryIDs = append(filter.RepositoryIDs, repository.ID)
	}
	if len(filter.RepositoryIDs) == 0 {
		// An empty IN-list must mean "no authorized repositories", never
		// accidentally remove the permission boundary from a fleet query.
		filter.RepositoryIDs = []int64{-1}
	}
	return filter, nil
}

func (s *Service) sourceURL(repositoryID int64, revision, file string, line int) string {
	s.mu.RLock()
	baseURL := s.baseURL
	s.mu.RUnlock()
	if baseURL == "" || file == "" {
		return ""
	}
	query := url.Values{"rev": {revision}, "path": {file}, "lines": {"1-200"}}
	fragment := ""
	if line > 0 {
		focus := strconv.Itoa(line) + "-" + strconv.Itoa(line)
		query.Set("focus", focus)
		fragment = "#L" + strconv.Itoa(line)
	}
	return fmt.Sprintf("%s/source/%d?%s%s", baseURL, repositoryID, query.Encode(), fragment)
}

func committedPaths(ctx context.Context, repository catalog.Repository, revision string) (map[string]string, error) {
	commandContext, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	command := exec.CommandContext(commandContext, "git", "-C", repository.Path, "ls-tree", "-r", "-z", "--name-only", revision)
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("list committed paths: %w", err)
	}
	paths := strings.Split(string(output), "\x00")
	result := make(map[string]string, len(paths)*2)
	for _, file := range paths {
		file = strings.Trim(strings.ReplaceAll(file, "\\", "/"), "/")
		if file == "" {
			continue
		}
		result[strings.ToLower(file)] = file
	}
	return result, nil
}

func reconcilePath(value, prefix string, paths map[string]string) (string, bool) {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if decoded, err := url.PathUnescape(value); err == nil {
		value = decoded
	}
	value = strings.TrimPrefix(value, "file://")
	value = strings.TrimPrefix(value, "./")
	prefix = strings.Trim(strings.ReplaceAll(strings.TrimSpace(prefix), "\\", "/"), "/")
	if prefix != "" {
		lowerValue, lowerPrefix := strings.ToLower(value), strings.ToLower(prefix)
		if index := strings.Index(lowerValue, lowerPrefix+"/"); index >= 0 {
			value = value[index+len(prefix)+1:]
		}
	}
	value = strings.TrimPrefix(filepath.ToSlash(filepath.Clean(value)), "./")
	value = strings.TrimPrefix(value, "/")
	if match, ok := paths[strings.ToLower(value)]; ok {
		return match, true
	}
	parts := strings.Split(value, "/")
	for index := 1; index < len(parts); index++ {
		suffix := strings.Join(parts[index:], "/")
		if match, ok := paths[strings.ToLower(suffix)]; ok {
			return match, true
		}
	}
	return value, false
}

func appendStatus(current, next string) string {
	if strings.TrimSpace(current) == "" {
		return strings.TrimSpace(next)
	}
	if strings.TrimSpace(next) == "" {
		return strings.TrimSpace(current)
	}
	return strings.TrimSpace(current) + "; " + strings.TrimSpace(next)
}

func newRunID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err == nil {
		return hex.EncodeToString(value[:])
	}
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

func defaultToolForFormat(format string) string {
	switch format {
	case "lcov", "jacoco", "jacoco-xml", "cobertura", "cobertura-xml":
		return "coverage-import"
	case "sarif", "sarif-2.1.0":
		return "sarif-import"
	case "megalinter-sarif":
		return "MegaLinter"
	case "semgrep", "semgrep-json":
		return "Semgrep"
	default:
		return "report-import"
	}
}

func emptyFacets() Facets {
	return Facets{
		Repositories: map[string]int{}, Branches: map[string]int{},
		Languages: map[string]int{}, Tools: map[string]int{}, Rules: map[string]int{},
		Severities: map[string]int{}, Owners: map[string]int{}, States: map[string]int{},
	}
}

func addFacet(facet map[string]int, value string) {
	value = strings.TrimSpace(value)
	if value != "" {
		facet[value]++
	}
}

func observationIdentity(observation Observation) string {
	fingerprint := observation.Fingerprint
	if fingerprint == "" && observation.Kind == KindFinding {
		fingerprint = fmt.Sprintf("%s:%d:%s", observation.Path, observation.StartLine, observation.Message)
	}
	return fmt.Sprintf(
		"%d\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s",
		observation.RepositoryID, observation.Tool, observation.Language,
		observation.Kind, observation.Key, observation.Path, fingerprint,
	)
}

func latestMetrics(observations []Observation) map[string]Observation {
	output := make(map[string]Observation)
	for _, observation := range observations {
		if observation.Kind != KindMetric {
			continue
		}
		key := observation.Tool + "\x00" + observation.Language + "\x00" + observation.Key + "\x00" + observation.Path
		if _, exists := output[key]; !exists {
			output[key] = observation
		}
	}
	return output
}

func findingSet(observations []Observation) map[string]Observation {
	output := make(map[string]Observation)
	for _, observation := range observations {
		if observation.Kind != KindFinding || observation.Suppressed {
			continue
		}
		key := observation.Fingerprint
		if key == "" {
			key = observation.Tool + "\x00" + observation.Key + "\x00" + observation.Path + "\x00" + strconv.Itoa(observation.StartLine) + "\x00" + observation.Message
		} else {
			key = observation.Tool + "\x00" + key
		}
		if _, exists := output[key]; !exists {
			output[key] = observation
		}
	}
	return output
}
