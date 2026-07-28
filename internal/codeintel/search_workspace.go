package codeintel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spolnik/RepoKarta/internal/access"
	"github.com/spolnik/RepoKarta/internal/querylang"
	"github.com/spolnik/RepoKarta/internal/searchworkspace"
)

type SearchWorkspaceStore interface {
	AddRecentSearch(context.Context, searchworkspace.RecentRecord) error
	ListRecentSearches(context.Context, int) ([]searchworkspace.RecentRecord, error)
	ListSavedSearchRecords(context.Context) ([]searchworkspace.SavedRecord, error)
	GetSavedSearchRecord(context.Context, string) (searchworkspace.SavedRecord, error)
	CreateSavedSearchRecord(context.Context, searchworkspace.SavedRecord) (searchworkspace.SavedRecord, error)
	UpdateSavedSearchRecord(context.Context, string, searchworkspace.SavedRecord) (searchworkspace.SavedRecord, error)
	DeleteSavedSearchRecord(context.Context, string) error
	UpsertSearchMonitorRecord(context.Context, searchworkspace.MonitorRecord) (searchworkspace.MonitorRecord, error)
	GetSearchMonitorRecord(context.Context, string) (searchworkspace.MonitorRecord, error)
	GetSearchMonitorBySavedSearch(context.Context, string) (searchworkspace.MonitorRecord, error)
	AddSearchMonitorRun(context.Context, searchworkspace.RunRecord, int) (searchworkspace.RunRecord, error)
	ListSearchMonitorRuns(context.Context, string, int) ([]searchworkspace.RunRecord, error)
}

type SavedSearchInput struct {
	Title          string        `json:"title"`
	Description    string        `json:"description,omitempty"`
	Visibility     string        `json:"visibility,omitempty"`
	RevisionPolicy string        `json:"revision_policy,omitempty"`
	Request        SearchRequest `json:"request"`
}

type SavedSearch struct {
	ID             string         `json:"id"`
	Title          string         `json:"title"`
	Description    string         `json:"description,omitempty"`
	Visibility     string         `json:"visibility"`
	Managed        bool           `json:"managed"`
	Editable       bool           `json:"editable"`
	RevisionPolicy string         `json:"revision_policy"`
	Request        SearchRequest  `json:"request"`
	Monitor        *SearchMonitor `json:"monitor,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

type RecentSearch struct {
	ID          int64         `json:"id"`
	Request     SearchRequest `json:"request"`
	ResultCount int           `json:"result_count"`
	ExecutedAt  time.Time     `json:"executed_at"`
}

type SearchWorkspace struct {
	Saved  []SavedSearch  `json:"saved"`
	Recent []RecentSearch `json:"recent"`
}

type SearchMonitorInput struct {
	Enabled      *bool `json:"enabled,omitempty"`
	HistoryLimit int   `json:"history_limit,omitempty"`
}

type SearchMonitor struct {
	ID            string             `json:"id"`
	SavedSearchID string             `json:"saved_search_id"`
	Enabled       bool               `json:"enabled"`
	HistoryLimit  int                `json:"history_limit"`
	Runs          []SearchMonitorRun `json:"runs,omitempty"`
	CreatedAt     time.Time          `json:"created_at"`
	UpdatedAt     time.Time          `json:"updated_at"`
}

type SearchMonitorRun struct {
	ID                 int64     `json:"id"`
	RevisionKey        string    `json:"revision_key"`
	Added              []string  `json:"added"`
	Removed            []string  `json:"removed"`
	MatchCount         int       `json:"match_count"`
	Status             string    `json:"status"`
	NotificationStatus string    `json:"notification_status"`
	Error              string    `json:"error,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
}

func (s *Service) UseSearchWorkspace(store SearchWorkspaceStore) *Service {
	s.searchWorkspace = store
	return s
}

func (s *Service) RecordRecentSearch(
	ctx context.Context,
	request SearchRequest,
	response SearchResponse,
) error {
	if s.searchWorkspace == nil {
		return nil
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encode recent search: %w", err)
	}
	return s.searchWorkspace.AddRecentSearch(ctx, searchworkspace.RecentRecord{
		RequestJSON: string(encoded),
		ResultCount: response.MatchCount + len(response.Items),
	})
}

func (s *Service) ListSearchWorkspace(ctx context.Context) (SearchWorkspace, error) {
	output := SearchWorkspace{Saved: []SavedSearch{}, Recent: []RecentSearch{}}
	if s.searchWorkspace == nil {
		return output, nil
	}
	records, err := s.searchWorkspace.ListSavedSearchRecords(ctx)
	if err != nil {
		return output, err
	}
	for _, record := range records {
		view, err := s.savedSearchView(ctx, record)
		if err != nil {
			return output, err
		}
		output.Saved = append(output.Saved, view)
	}
	recent, err := s.searchWorkspace.ListRecentSearches(ctx, 20)
	if err != nil {
		return output, err
	}
	for _, record := range recent {
		var request SearchRequest
		if err := json.Unmarshal([]byte(record.RequestJSON), &request); err != nil {
			return output, fmt.Errorf("decode recent search: %w", err)
		}
		output.Recent = append(output.Recent, RecentSearch{
			ID: record.ID, Request: request, ResultCount: record.ResultCount,
			ExecutedAt: record.ExecutedAt,
		})
	}
	return output, nil
}

func (s *Service) CreateSavedSearch(
	ctx context.Context,
	input SavedSearchInput,
) (SavedSearch, error) {
	record, err := validSavedSearch(input)
	if err != nil {
		return SavedSearch{}, err
	}
	if s.searchWorkspace == nil {
		return SavedSearch{}, errors.New("saved searches are unavailable")
	}
	record, err = s.searchWorkspace.CreateSavedSearchRecord(ctx, record)
	if err != nil {
		return SavedSearch{}, err
	}
	return s.savedSearchView(ctx, record)
}

func (s *Service) UpdateSavedSearch(
	ctx context.Context,
	id string,
	input SavedSearchInput,
) (SavedSearch, error) {
	record, err := validSavedSearch(input)
	if err != nil {
		return SavedSearch{}, err
	}
	if s.searchWorkspace == nil {
		return SavedSearch{}, searchworkspace.ErrNotFound
	}
	record, err = s.searchWorkspace.UpdateSavedSearchRecord(ctx, strings.TrimSpace(id), record)
	if err != nil {
		return SavedSearch{}, err
	}
	return s.savedSearchView(ctx, record)
}

func (s *Service) DeleteSavedSearch(ctx context.Context, id string) error {
	if s.searchWorkspace == nil {
		return searchworkspace.ErrNotFound
	}
	return s.searchWorkspace.DeleteSavedSearchRecord(ctx, strings.TrimSpace(id))
}

func (s *Service) ConfigureSearchMonitor(
	ctx context.Context,
	savedSearchID string,
	input SearchMonitorInput,
) (SearchMonitor, error) {
	if s.searchWorkspace == nil {
		return SearchMonitor{}, searchworkspace.ErrNotFound
	}
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	historyLimit := input.HistoryLimit
	if historyLimit == 0 {
		historyLimit = 20
	}
	if historyLimit < 1 || historyLimit > 100 {
		return SearchMonitor{}, errors.New("history_limit must be between 1 and 100")
	}
	record, err := s.searchWorkspace.UpsertSearchMonitorRecord(ctx, searchworkspace.MonitorRecord{
		SavedSearchID: strings.TrimSpace(savedSearchID),
		Enabled:       enabled,
		HistoryLimit:  historyLimit,
	})
	if err != nil {
		return SearchMonitor{}, err
	}
	return s.monitorView(ctx, record)
}

func (s *Service) RunSearchMonitor(
	ctx context.Context,
	monitorID string,
) (SearchMonitorRun, error) {
	if s.searchWorkspace == nil {
		return SearchMonitorRun{}, searchworkspace.ErrNotFound
	}
	monitor, err := s.searchWorkspace.GetSearchMonitorRecord(ctx, strings.TrimSpace(monitorID))
	if err != nil {
		return SearchMonitorRun{}, err
	}
	if !monitor.Enabled {
		return SearchMonitorRun{}, errors.New("search monitor is disabled")
	}
	saved, err := s.searchWorkspace.GetSavedSearchRecord(ctx, monitor.SavedSearchID)
	if err != nil {
		return SearchMonitorRun{}, err
	}
	var request SearchRequest
	if err := json.Unmarshal([]byte(saved.RequestJSON), &request); err != nil {
		return SearchMonitorRun{}, fmt.Errorf("decode monitored search: %w", err)
	}
	response, searchErr := s.Search(ctx, request)
	revisionKey, revisionErr := s.searchRequestRevisionKey(ctx, request)
	if searchErr == nil && revisionErr != nil {
		searchErr = revisionErr
	}
	keys := stableSearchResultKeys(response)
	previous, err := s.searchWorkspace.ListSearchMonitorRuns(ctx, monitor.ID, 1)
	if err != nil {
		return SearchMonitorRun{}, err
	}
	var previousKeys []string
	if len(previous) > 0 {
		_ = json.Unmarshal([]byte(previous[0].ResultKeysJSON), &previousKeys)
	}
	added, removed := setDifference(keys, previousKeys), setDifference(previousKeys, keys)
	status, errorText := "complete", ""
	if searchErr != nil {
		status, errorText = "failed", searchErr.Error()
	} else if response.Truncated || !response.TotalFilesExact {
		status = "incomplete"
	}
	if len(previous) > 0 && !revisionScopesComparable(previous[0].RevisionKey, revisionKey) {
		status = "incomplete"
		errorText = "the monitored repository scope changed; added and removed matches are not comparable"
		added, removed = []string{}, []string{}
	}
	keysJSON, _ := json.Marshal(keys)
	addedJSON, _ := json.Marshal(added)
	removedJSON, _ := json.Marshal(removed)
	record, err := s.searchWorkspace.AddSearchMonitorRun(ctx, searchworkspace.RunRecord{
		MonitorID:          monitor.ID,
		RevisionKey:        revisionKey,
		ResultKeysJSON:     string(keysJSON),
		AddedJSON:          string(addedJSON),
		RemovedJSON:        string(removedJSON),
		MatchCount:         response.MatchCount + len(response.Items),
		Status:             status,
		NotificationStatus: "not_configured",
		Error:              errorText,
	}, monitor.HistoryLimit)
	if err != nil {
		return SearchMonitorRun{}, err
	}
	view := monitorRunView(record)
	if searchErr != nil {
		return view, fmt.Errorf("monitored search failed: %w", searchErr)
	}
	return view, nil
}

func validSavedSearch(input SavedSearchInput) (searchworkspace.SavedRecord, error) {
	input.Title = strings.TrimSpace(input.Title)
	input.Description = strings.TrimSpace(input.Description)
	input.Visibility = strings.ToLower(strings.TrimSpace(input.Visibility))
	input.RevisionPolicy = strings.ToLower(strings.TrimSpace(input.RevisionPolicy))
	if input.Visibility == "" {
		input.Visibility = "personal"
	}
	if input.RevisionPolicy == "" {
		input.RevisionPolicy = "latest_indexed"
	}
	if len([]rune(input.Title)) < 1 || len([]rune(input.Title)) > 120 {
		return searchworkspace.SavedRecord{}, errors.New("saved search title must contain 1 to 120 characters")
	}
	if len([]rune(input.Description)) > 500 {
		return searchworkspace.SavedRecord{}, errors.New("saved search description cannot exceed 500 characters")
	}
	if input.Visibility != "personal" && input.Visibility != "shared" {
		return searchworkspace.SavedRecord{}, errors.New("visibility must be personal or shared")
	}
	if input.RevisionPolicy != "pinned" && input.RevisionPolicy != "latest_indexed" {
		return searchworkspace.SavedRecord{}, errors.New("revision_policy must be pinned or latest_indexed")
	}
	if strings.TrimSpace(input.Request.Query) == "" {
		return searchworkspace.SavedRecord{}, errors.New("saved search query is required")
	}
	parsed, err := querylang.Parse(input.Request.Query)
	if err != nil {
		return searchworkspace.SavedRecord{}, err
	}
	if input.RevisionPolicy == "pinned" {
		hasPinnedRevision := false
		for _, filter := range parsed.Filters {
			if filter.Field == querylang.FieldRevision && !filter.Negative {
				hasPinnedRevision = true
			}
		}
		for _, selector := range input.Request.Contexts {
			if strings.TrimSpace(selector.Revision) != "" {
				hasPinnedRevision = true
			}
		}
		if !hasPinnedRevision {
			return searchworkspace.SavedRecord{}, errors.New(
				"a pinned saved search requires revision:... or an exact-revision structured context",
			)
		}
	}
	encoded, err := json.Marshal(input.Request)
	if err != nil {
		return searchworkspace.SavedRecord{}, err
	}
	return searchworkspace.SavedRecord{
		Title: input.Title, Description: input.Description,
		Visibility: input.Visibility, RevisionPolicy: input.RevisionPolicy,
		RequestJSON: string(encoded),
	}, nil
}

func (s *Service) savedSearchView(
	ctx context.Context,
	record searchworkspace.SavedRecord,
) (SavedSearch, error) {
	var request SearchRequest
	if err := json.Unmarshal([]byte(record.RequestJSON), &request); err != nil {
		return SavedSearch{}, fmt.Errorf("decode saved search: %w", err)
	}
	viewer, restricted := access.ViewerFromContext(ctx)
	editable := !restricted || (record.Managed && viewer.Admin) ||
		(!record.Managed && record.AuthorID == viewer.ID)
	view := SavedSearch{
		ID: record.ID, Title: record.Title, Description: record.Description,
		Visibility: record.Visibility, Managed: record.Managed, Editable: editable,
		RevisionPolicy: record.RevisionPolicy, Request: request,
		CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
	}
	if monitor, err := s.searchWorkspace.GetSearchMonitorBySavedSearch(ctx, record.ID); err == nil {
		monitorView, viewErr := s.monitorView(ctx, monitor)
		if viewErr != nil {
			return view, viewErr
		}
		view.Monitor = &monitorView
	} else if !errors.Is(err, searchworkspace.ErrNotFound) {
		return view, err
	}
	return view, nil
}

func (s *Service) monitorView(
	ctx context.Context,
	record searchworkspace.MonitorRecord,
) (SearchMonitor, error) {
	runs, err := s.searchWorkspace.ListSearchMonitorRuns(ctx, record.ID, record.HistoryLimit)
	if err != nil {
		return SearchMonitor{}, err
	}
	view := SearchMonitor{
		ID: record.ID, SavedSearchID: record.SavedSearchID,
		Enabled: record.Enabled, HistoryLimit: record.HistoryLimit,
		Runs: []SearchMonitorRun{}, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
	}
	for _, run := range runs {
		view.Runs = append(view.Runs, monitorRunView(run))
	}
	return view, nil
}

func monitorRunView(record searchworkspace.RunRecord) SearchMonitorRun {
	var added, removed []string
	_ = json.Unmarshal([]byte(record.AddedJSON), &added)
	_ = json.Unmarshal([]byte(record.RemovedJSON), &removed)
	if added == nil {
		added = []string{}
	}
	if removed == nil {
		removed = []string{}
	}
	return SearchMonitorRun{
		ID: record.ID, RevisionKey: record.RevisionKey,
		Added: added, Removed: removed, MatchCount: record.MatchCount,
		Status: record.Status, NotificationStatus: record.NotificationStatus,
		Error: record.Error, CreatedAt: record.CreatedAt,
	}
}

func stableSearchResultKeys(response SearchResponse) []string {
	keys := make([]string, 0, len(response.Matches)+len(response.Items))
	for _, match := range response.Matches {
		lines := make([]string, 0, len(match.Lines))
		for _, line := range match.Lines {
			lines = append(lines, strconv.Itoa(line.Number))
		}
		keys = append(keys, fmt.Sprintf(
			"source:%d:%s:%s:%s",
			match.RepositoryID, match.Revision, match.Path, strings.Join(lines, ","),
		))
	}
	for _, item := range response.Items {
		keys = append(keys, fmt.Sprintf(
			"%s:%d:%s:%s:%s",
			item.ResultType, item.RepositoryID, item.Revision, item.Path, item.Title,
		))
	}
	sort.Strings(keys)
	return compactStrings(keys)
}

func (s *Service) searchRequestRevisionKey(
	ctx context.Context,
	request SearchRequest,
) (string, error) {
	parsed, err := querylang.Parse(request.Query)
	if err != nil {
		return "", err
	}
	repositories, _, err := s.selectDerivedRepositories(ctx, request, parsed)
	if err != nil {
		return "", err
	}
	values := make([]string, 0, len(repositories))
	for _, repository := range repositories {
		values = append(values, fmt.Sprintf("%d:%s", repository.ID, repository.Revision))
	}
	sort.Strings(values)
	return strings.Join(compactStrings(values), ","), nil
}

func revisionScopesComparable(previous, current string) bool {
	repositories := func(value string) []string {
		var output []string
		for _, item := range strings.Split(value, ",") {
			id, _, found := strings.Cut(item, ":")
			if found && id != "" {
				output = append(output, id)
			}
		}
		sort.Strings(output)
		return output
	}
	return strings.Join(repositories(previous), ",") == strings.Join(repositories(current), ",")
}

func setDifference(left, right []string) []string {
	seen := make(map[string]struct{}, len(right))
	for _, value := range right {
		seen[value] = struct{}{}
	}
	output := make([]string, 0)
	for _, value := range left {
		if _, ok := seen[value]; !ok {
			output = append(output, value)
		}
	}
	return output
}

func compactStrings(values []string) []string {
	output := values[:0]
	for _, value := range values {
		if len(output) == 0 || output[len(output)-1] != value {
			output = append(output, value)
		}
	}
	return output
}
