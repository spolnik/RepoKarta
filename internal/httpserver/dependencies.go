package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spolnik/RepoKarta/internal/dependencies"
	"github.com/spolnik/RepoKarta/internal/graph"
)

func (s *Server) dependencyPage(response http.ResponseWriter, request *http.Request) {
	data, err := s.pageData(request.Context())
	if err != nil {
		http.Error(response, "Could not load repositories", http.StatusInternalServerError)
		return
	}
	repositoryID, err := optionalRepositoryID(request.URL.Query().Get("repository"))
	if err != nil {
		http.Error(response, "Invalid repository", http.StatusBadRequest)
		return
	}
	view := strings.ToLower(strings.TrimSpace(request.URL.Query().Get("view")))
	if view == "" || view == "topology" {
		if s.dependencies == nil {
			http.Error(response, "Distributed topology service is unavailable", http.StatusServiceUnavailable)
			return
		}
		options, err := dependencyTopologyOptions(request)
		if err != nil {
			http.Error(response, err.Error(), http.StatusBadRequest)
			return
		}
		snapshot, progress, err := s.maps.ReadTopologySnapshot(request.Context(), repositoryID)
		if err != nil {
			slog.Error("compose distributed topology", "repository_id", repositoryID, "error", err)
			http.Error(response, "Distributed topology could not be built", http.StatusInternalServerError)
			return
		}
		topology, err := s.dependencies.Topology(request.Context(), snapshot, progress, options)
		if err != nil {
			slog.Error("join runtime topology observations", "repository_id", repositoryID, "error", err)
			http.Error(response, "Distributed topology could not be loaded", http.StatusInternalServerError)
			return
		}
		data.ActivePage = "dependencies"
		s.render(response, "dependencies", dependencyPageData{
			pageData: data, Topology: topology, TopologyView: true,
			SelectedRepositoryID: repositoryID,
			APIURL:               dependencyTopologyURL("/api/dependencies/topology", repositoryID, options),
		})
		return
	}
	options, err := dependencyOptions(request)
	if err != nil {
		http.Error(response, err.Error(), http.StatusBadRequest)
		return
	}
	snapshot, progress, err := s.maps.ReadDependencySnapshot(request.Context(), repositoryID)
	if err != nil {
		slog.Error("build dependency inventory", "repository_id", repositoryID, "error", err)
		http.Error(response, "Dependency inventory could not be built", http.StatusInternalServerError)
		return
	}
	data.ActivePage = "dependencies"
	inventory, err := s.dependencyInventory(request.Context(), snapshot, options)
	if err != nil {
		slog.Error("load dependency registry observations", "error", err)
		http.Error(response, "Dependency inventory could not be built", http.StatusInternalServerError)
		return
	}
	inventory.BuildProgress = progress
	findingsView := view == "findings"
	advisoryOptions, err := dependencyAdvisoryOptions(request)
	if err != nil {
		http.Error(response, err.Error(), http.StatusBadRequest)
		return
	}
	findings := dependencies.FindingResponse{}
	if findingsView {
		if s.dependencies == nil {
			http.Error(response, "Dependency advisory service is unavailable", http.StatusServiceUnavailable)
			return
		}
		findings, err = s.dependencies.Findings(request.Context(), snapshot, advisoryOptions)
		if err != nil {
			slog.Error("join dependency advisories", "repository_id", repositoryID, "error", err)
			http.Error(response, "Dependency findings could not be loaded", http.StatusInternalServerError)
			return
		}
	}
	previousURL := ""
	nextURL := ""
	firstRow, lastRow := 0, 0
	if findingsView {
		if findings.Offset > 0 {
			previousURL = dependencyAdvisoryURL(
				"/dependencies", repositoryID, advisoryOptions,
				max(0, findings.Offset-findings.Limit), true,
			)
		}
		if findings.HasMore {
			nextURL = dependencyAdvisoryURL(
				"/dependencies", repositoryID, advisoryOptions,
				findings.Offset+findings.ReturnedCount, true,
			)
		}
		if findings.ReturnedCount > 0 {
			firstRow = findings.Offset + 1
			lastRow = findings.Offset + findings.ReturnedCount
		}
	} else {
		if inventory.Offset > 0 {
			previousURL = dependencyURL("/dependencies", repositoryID, options, max(0, inventory.Offset-inventory.Limit))
		}
		if inventory.HasMore {
			nextURL = dependencyURL("/dependencies", repositoryID, options, inventory.Offset+inventory.ReturnedCount)
		}
		if inventory.ReturnedCount > 0 {
			firstRow = inventory.Offset + 1
			lastRow = inventory.Offset + inventory.ReturnedCount
		}
	}
	s.render(response, "dependencies", dependencyPageData{
		pageData:             data,
		Inventory:            inventory,
		Findings:             findings,
		AdvisoryOptions:      advisoryOptions,
		FindingsView:         findingsView,
		SelectedRepositoryID: repositoryID,
		PreviousURL:          previousURL,
		NextURL:              nextURL,
		APIURL: func() string {
			if findingsView {
				return dependencyAdvisoryURL(
					"/api/dependencies/findings", repositoryID, advisoryOptions, findings.Offset, false,
				)
			}
			return dependencyURL("/api/dependencies", repositoryID, options, inventory.Offset)
		}(),
		SARIFURL: dependencyAdvisoryURL(
			"/api/dependencies/findings.sarif", repositoryID, advisoryOptions, 0, false,
		),
		RefreshURL: dependencyURL("/api/dependencies/refresh", repositoryID, options, 0),
		AdvisoryRefreshURL: dependencyAdvisoryURL(
			"/api/dependencies/advisories/refresh", repositoryID, dependencies.AdvisoryOptions{}, 0, false,
		),
		FirstRow: firstRow,
		LastRow:  lastRow,
		RefreshProgress: func() dependencies.RefreshProgress {
			if s.dependencies == nil {
				return dependencies.RefreshProgress{State: "unavailable"}
			}
			return s.dependencies.Progress()
		}(),
		AdvisoryProgress: func() dependencies.AdvisoryRefreshProgress {
			if s.dependencies == nil {
				return dependencies.AdvisoryRefreshProgress{State: "unavailable"}
			}
			return s.dependencies.AdvisoryProgress()
		}(),
	})
}

func (s *Server) apiDependencies(response http.ResponseWriter, request *http.Request) {
	repositoryID, err := optionalRepositoryID(request.URL.Query().Get("repository"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	options, err := dependencyOptions(request)
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	snapshot, progress, err := s.maps.ReadDependencySnapshot(request.Context(), repositoryID)
	if err != nil {
		slog.Error("build dependency inventory", "repository_id", repositoryID, "error", err)
		writeAPIError(response, http.StatusInternalServerError, errors.New("dependency inventory could not be built"))
		return
	}
	inventory, err := s.dependencyInventory(request.Context(), snapshot, options)
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, errors.New("dependency observations could not be loaded"))
		return
	}
	inventory.BuildProgress = progress
	status := http.StatusOK
	if progress.State == "building" {
		status = http.StatusAccepted
		response.Header().Set("Retry-After", "2")
	}
	writeJSON(response, status, inventory)
}

func (s *Server) apiDependencyTopology(response http.ResponseWriter, request *http.Request) {
	if s.dependencies == nil {
		writeAPIError(response, http.StatusServiceUnavailable, errors.New("distributed topology service is unavailable"))
		return
	}
	repositoryID, err := optionalRepositoryID(request.URL.Query().Get("repository"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	options, err := dependencyTopologyOptions(request)
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	snapshot, progress, err := s.maps.ReadTopologySnapshot(request.Context(), repositoryID)
	if err != nil {
		slog.Error("compose distributed topology", "repository_id", repositoryID, "error", err)
		writeAPIError(response, http.StatusInternalServerError, errors.New("distributed topology could not be built"))
		return
	}
	topology, err := s.dependencies.Topology(request.Context(), snapshot, progress, options)
	if err != nil {
		slog.Error("join runtime topology observations", "repository_id", repositoryID, "error", err)
		writeAPIError(response, http.StatusInternalServerError, errors.New("runtime topology observations could not be loaded"))
		return
	}
	topology = dependencies.SanitizeTopology(topology)
	status := http.StatusOK
	if progress.State == "building" {
		status = http.StatusAccepted
		response.Header().Set("Retry-After", "2")
	}
	writeJSON(response, status, topology)
}

func (s *Server) importDependencyTopology(response http.ResponseWriter, request *http.Request) {
	if s.dependencies == nil {
		writeAPIError(response, http.StatusServiceUnavailable, errors.New("distributed topology service is unavailable"))
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 4<<20)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var input dependencies.TopologyImportRequest
	if err := decoder.Decode(&input); err != nil {
		writeAPIError(response, http.StatusBadRequest, fmt.Errorf("decode runtime topology observations: %w", err))
		return
	}
	result, err := s.dependencies.ImportTopologyObservations(request.Context(), input)
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	writeJSON(response, http.StatusCreated, result)
}

func (s *Server) dependencyInventory(
	ctx context.Context,
	snapshot graph.Snapshot,
	options dependencies.Options,
) (dependencies.Inventory, error) {
	if s.dependencies == nil {
		return dependencies.BuildPage(snapshot, options), nil
	}
	return s.dependencies.Inventory(ctx, snapshot, options)
}

func (s *Server) refreshDependencies(response http.ResponseWriter, request *http.Request) {
	repositoryID, err := optionalRepositoryID(request.URL.Query().Get("repository"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	options, err := dependencyOptions(request)
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	snapshot, _, err := s.maps.ReadDependencySnapshot(request.Context(), repositoryID)
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, errors.New("dependency inventory could not be built"))
		return
	}
	force, err := optionalBool(request.URL.Query().Get("force"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("force must be true or false"))
		return
	}
	progress, err := s.dependencies.StartRefresh(snapshot, options, force)
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, errors.New("dependency refresh could not be started"))
		return
	}
	writeJSON(response, http.StatusAccepted, progress)
}

func (s *Server) dependencyRefreshProgress(response http.ResponseWriter, _ *http.Request) {
	writeJSON(response, http.StatusOK, s.dependencies.Progress())
}

func (s *Server) apiDependencyFindings(response http.ResponseWriter, request *http.Request) {
	repositoryID, err := optionalRepositoryID(request.URL.Query().Get("repository"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	options, err := dependencyAdvisoryOptions(request)
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	snapshot, progress, err := s.maps.ReadDependencySnapshot(request.Context(), repositoryID)
	if err != nil {
		slog.Error("build dependency inventory for findings", "repository_id", repositoryID, "error", err)
		writeAPIError(response, http.StatusInternalServerError, errors.New("dependency inventory could not be built"))
		return
	}
	findings, err := s.dependencies.Findings(request.Context(), snapshot, options)
	if err != nil {
		slog.Error("join dependency advisories", "repository_id", repositoryID, "error", err)
		writeAPIError(response, http.StatusInternalServerError, errors.New("dependency findings could not be loaded"))
		return
	}
	status := http.StatusOK
	if progress.State == "building" {
		status = http.StatusAccepted
		response.Header().Set("Retry-After", "2")
	}
	writeJSON(response, status, findings)
}

func (s *Server) refreshDependencyAdvisories(response http.ResponseWriter, request *http.Request) {
	repositoryID, err := optionalRepositoryID(request.URL.Query().Get("repository"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	force, err := optionalBool(request.URL.Query().Get("force"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("force must be true or false"))
		return
	}
	snapshot, _, err := s.maps.ReadDependencySnapshot(request.Context(), repositoryID)
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, errors.New("dependency inventory could not be built"))
		return
	}
	progress, err := s.dependencies.StartAdvisoryRefresh(snapshot, force)
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, fmt.Errorf("advisory refresh could not be started: %w", err))
		return
	}
	writeJSON(response, http.StatusAccepted, progress)
}

func (s *Server) dependencyAdvisoryProgress(response http.ResponseWriter, _ *http.Request) {
	writeJSON(response, http.StatusOK, s.dependencies.AdvisoryProgress())
}

func (s *Server) exportDependencyFindingsSARIF(response http.ResponseWriter, request *http.Request) {
	repositoryID, err := optionalRepositoryID(request.URL.Query().Get("repository"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	options, err := dependencyAdvisoryOptions(request)
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	options.Offset = 0
	options.Limit = dependencies.MaximumFindingLimit
	snapshot, progress, err := s.maps.ReadDependencySnapshot(request.Context(), repositoryID)
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, errors.New("dependency inventory could not be built"))
		return
	}
	if progress.State == "building" {
		response.Header().Set("Retry-After", "2")
		writeAPIError(response, http.StatusConflict, errors.New("dependency inventory is still building"))
		return
	}
	all, err := s.dependencies.Findings(request.Context(), snapshot, options)
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, errors.New("dependency findings could not be loaded"))
		return
	}
	for all.HasMore {
		options.Offset = all.Offset + all.ReturnedCount
		page, err := s.dependencies.Findings(request.Context(), snapshot, options)
		if err != nil {
			writeAPIError(response, http.StatusInternalServerError, errors.New("dependency findings could not be loaded"))
			return
		}
		all.Findings = append(all.Findings, page.Findings...)
		all.ReturnedCount = len(all.Findings)
		all.HasMore = page.HasMore
		all.Offset = 0
		if len(all.Findings) > 50_000 {
			writeAPIError(response, http.StatusUnprocessableEntity, errors.New("SARIF export exceeds 50000 findings; narrow the filters"))
			return
		}
	}
	content, err := dependencies.FindingsSARIF(all, s.config.Version)
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, errors.New("dependency findings SARIF could not be generated"))
		return
	}
	response.Header().Set("Content-Type", "application/sarif+json")
	response.Header().Set("Content-Disposition", `attachment; filename="repokarta-dependency-findings.sarif"`)
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(content)
}

func (s *Server) apiArtifactProgress(response http.ResponseWriter, request *http.Request) {
	progress, err := s.maps.StructureProgress(request.Context(), 0)
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, errors.New("artifact progress could not be loaded"))
		return
	}
	writeJSON(response, http.StatusOK, progress)
}

func dependencyOptions(request *http.Request) (dependencies.Options, error) {
	query := request.URL.Query()
	options := dependencies.Options{
		Query:        query.Get("query"),
		Package:      query.Get("package"),
		Ecosystem:    query.Get("ecosystem"),
		Usage:        query.Get("usage"),
		Relationship: query.Get("relationship"),
		Resolution:   query.Get("resolution"),
		CheckStatus:  query.Get("check_status"),
		Distance:     query.Get("distance"),
		Limit:        dependencies.DefaultPageLimit,
	}
	if len(options.Query) > 200 || len(options.Package) > 200 ||
		len(options.Ecosystem) > 50 || len(options.Usage) > 50 ||
		len(options.Relationship) > 50 || len(options.Resolution) > 50 ||
		len(options.CheckStatus) > 50 || len(options.Distance) > 50 {
		return dependencies.Options{}, errors.New("dependency filters are too long")
	}
	for _, check := range []struct {
		name    string
		value   string
		allowed []string
	}{
		{"check_status", options.CheckStatus, []string{
			"current", "behind", "ahead", "prerelease", "unavailable",
			"private_internal", "unresolved", "registry_error", "stale", "unchecked",
		}},
		{"distance", options.Distance, []string{"major", "minor", "patch", "none", "unknown"}},
	} {
		value := strings.ToLower(strings.TrimSpace(check.value))
		if value != "" && !slices.Contains(check.allowed, value) {
			return dependencies.Options{}, fmt.Errorf(
				"%s must be one of %s", check.name, strings.Join(check.allowed, ", "),
			)
		}
	}
	if value := strings.TrimSpace(query.Get("offset")); value != "" {
		offset, err := strconv.Atoi(value)
		if err != nil || offset < 0 {
			return dependencies.Options{}, errors.New("offset must be a non-negative integer")
		}
		options.Offset = offset
	}
	if value := strings.TrimSpace(query.Get("limit")); value != "" {
		limit, err := strconv.Atoi(value)
		if err != nil || limit < 1 || limit > dependencies.MaximumPageLimit {
			return dependencies.Options{}, fmt.Errorf("limit must be between 1 and %d", dependencies.MaximumPageLimit)
		}
		options.Limit = limit
	}
	options.Query = strings.TrimSpace(options.Query)
	options.Package = strings.TrimSpace(options.Package)
	options.Ecosystem = strings.ToLower(strings.TrimSpace(options.Ecosystem))
	options.Usage = strings.ToLower(strings.TrimSpace(options.Usage))
	options.CheckStatus = strings.ToLower(strings.TrimSpace(options.CheckStatus))
	options.Distance = strings.ToLower(strings.TrimSpace(options.Distance))
	return options, nil
}

func dependencyTopologyOptions(request *http.Request) (dependencies.TopologyOptions, error) {
	query := request.URL.Query()
	options := dependencies.TopologyOptions{
		Query:       strings.TrimSpace(query.Get("query")),
		Protocol:    strings.ToLower(strings.TrimSpace(query.Get("protocol"))),
		Origin:      strings.ToLower(strings.TrimSpace(query.Get("origin"))),
		Environment: strings.TrimSpace(query.Get("environment")),
		Provider:    strings.TrimSpace(query.Get("provider")),
		Direction:   strings.ToLower(strings.TrimSpace(query.Get("direction"))),
		Depth:       1,
	}
	if options.Direction == "" {
		options.Direction = "both"
	}
	if len(options.Query) > 200 || len(options.Protocol) > 30 ||
		len(options.Origin) > 30 || len(options.Environment) > 80 ||
		len(options.Provider) > 80 || len(options.Direction) > 10 {
		return dependencies.TopologyOptions{}, errors.New("topology filters are too long")
	}
	if options.Protocol != "" && !slices.Contains(
		[]string{"http", "grpc", "kafka", "database", "mcp", "amqp", "unknown"},
		options.Protocol,
	) {
		return dependencies.TopologyOptions{}, errors.New("protocol filter is unsupported")
	}
	if options.Origin != "" && !slices.Contains(
		[]string{"static", "runtime", "confirmed"}, options.Origin,
	) {
		return dependencies.TopologyOptions{}, errors.New("origin must be static, runtime, or confirmed")
	}
	if !slices.Contains([]string{"both", "inbound", "outbound"}, options.Direction) {
		return dependencies.TopologyOptions{}, errors.New("direction must be both, inbound, or outbound")
	}
	if value := strings.TrimSpace(query.Get("depth")); value != "" {
		depth, err := strconv.Atoi(value)
		if err != nil || depth < 1 || depth > 2 {
			return dependencies.TopologyOptions{}, errors.New("depth must be 1 or 2")
		}
		options.Depth = depth
	}
	for key, target := range map[string]*time.Time{
		"observed_from": &options.ObservedFrom,
		"observed_to":   &options.ObservedTo,
	} {
		value := strings.TrimSpace(query.Get(key))
		if value == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return dependencies.TopologyOptions{}, fmt.Errorf("%s must be RFC3339", key)
		}
		*target = parsed.UTC()
	}
	if !options.ObservedFrom.IsZero() && !options.ObservedTo.IsZero() &&
		options.ObservedFrom.After(options.ObservedTo) {
		return dependencies.TopologyOptions{}, errors.New("observed_from must not be after observed_to")
	}
	return options, nil
}

func dependencyAdvisoryOptions(request *http.Request) (dependencies.AdvisoryOptions, error) {
	query := request.URL.Query()
	options := dependencies.AdvisoryOptions{
		Query: query.Get("query"), Ecosystem: query.Get("ecosystem"),
		Severity: query.Get("severity"), Usage: query.Get("usage"),
		Package: query.Get("package"), Limit: dependencies.DefaultFindingLimit,
	}
	if len(options.Query) > 200 || len(options.Ecosystem) > 50 ||
		len(options.Severity) > 50 || len(options.Usage) > 50 || len(options.Package) > 200 {
		return dependencies.AdvisoryOptions{}, errors.New("dependency finding filters are too long")
	}
	for _, check := range []struct {
		name    string
		value   string
		allowed []string
	}{
		{"ecosystem", options.Ecosystem, []string{"maven", "npm", "pypi", "go", "cargo", "nuget"}},
		{"severity", options.Severity, []string{"critical", "high", "medium", "low", "unknown"}},
		{"usage", options.Usage, []string{"production", "implementation", "test", "development", "build", "unknown"}},
	} {
		value := strings.ToLower(strings.TrimSpace(check.value))
		if value != "" && !slices.Contains(check.allowed, value) {
			return dependencies.AdvisoryOptions{}, fmt.Errorf(
				"%s must be one of %s", check.name, strings.Join(check.allowed, ", "),
			)
		}
	}
	if value := strings.TrimSpace(query.Get("offset")); value != "" {
		offset, err := strconv.Atoi(value)
		if err != nil || offset < 0 {
			return dependencies.AdvisoryOptions{}, errors.New("offset must be a non-negative integer")
		}
		options.Offset = offset
	}
	if value := strings.TrimSpace(query.Get("limit")); value != "" {
		limit, err := strconv.Atoi(value)
		if err != nil || limit < 1 || limit > dependencies.MaximumFindingLimit {
			return dependencies.AdvisoryOptions{}, fmt.Errorf(
				"limit must be between 1 and %d", dependencies.MaximumFindingLimit,
			)
		}
		options.Limit = limit
	}
	options.Query = strings.TrimSpace(options.Query)
	options.Ecosystem = strings.ToLower(strings.TrimSpace(options.Ecosystem))
	options.Severity = strings.ToLower(strings.TrimSpace(options.Severity))
	options.Usage = strings.ToLower(strings.TrimSpace(options.Usage))
	options.Package = strings.TrimSpace(options.Package)
	return options, nil
}

func dependencyURL(base string, repositoryID int64, options dependencies.Options, offset int) string {
	query := url.Values{}
	if base == "/dependencies" {
		query.Set("view", "inventory")
	}
	if repositoryID > 0 {
		query.Set("repository", strconv.FormatInt(repositoryID, 10))
	}
	if value := strings.TrimSpace(options.Query); value != "" {
		query.Set("query", value)
	}
	if value := strings.TrimSpace(options.Package); value != "" {
		query.Set("package", value)
	}
	if value := strings.TrimSpace(options.Ecosystem); value != "" {
		query.Set("ecosystem", value)
	}
	if value := strings.TrimSpace(options.Usage); value != "" {
		query.Set("usage", value)
	}
	if value := strings.TrimSpace(options.Relationship); value != "" {
		query.Set("relationship", value)
	}
	if value := strings.TrimSpace(options.Resolution); value != "" {
		query.Set("resolution", value)
	}
	if value := strings.TrimSpace(options.CheckStatus); value != "" {
		query.Set("check_status", value)
	}
	if value := strings.TrimSpace(options.Distance); value != "" {
		query.Set("distance", value)
	}
	query.Set("limit", strconv.Itoa(options.Limit))
	if offset > 0 {
		query.Set("offset", strconv.Itoa(offset))
	}
	return base + "?" + query.Encode()
}

func dependencyTopologyURL(
	base string,
	repositoryID int64,
	options dependencies.TopologyOptions,
) string {
	query := url.Values{}
	if repositoryID > 0 {
		query.Set("repository", strconv.FormatInt(repositoryID, 10))
	}
	for key, value := range map[string]string{
		"query": options.Query, "protocol": options.Protocol, "origin": options.Origin,
		"environment": options.Environment, "provider": options.Provider,
		"direction": options.Direction,
	} {
		if value = strings.TrimSpace(value); value != "" {
			query.Set(key, value)
		}
	}
	if !options.ObservedFrom.IsZero() {
		query.Set("observed_from", options.ObservedFrom.UTC().Format(time.RFC3339))
	}
	if !options.ObservedTo.IsZero() {
		query.Set("observed_to", options.ObservedTo.UTC().Format(time.RFC3339))
	}
	if options.Depth > 0 {
		query.Set("depth", strconv.Itoa(options.Depth))
	}
	if encoded := query.Encode(); encoded != "" {
		return base + "?" + encoded
	}
	return base
}

func dependencyAdvisoryURL(
	base string,
	repositoryID int64,
	options dependencies.AdvisoryOptions,
	offset int,
	view bool,
) string {
	query := url.Values{}
	if repositoryID > 0 {
		query.Set("repository", strconv.FormatInt(repositoryID, 10))
	}
	if view {
		query.Set("view", "findings")
	}
	if value := strings.TrimSpace(options.Query); value != "" {
		query.Set("query", value)
	}
	if value := strings.TrimSpace(options.Ecosystem); value != "" {
		query.Set("ecosystem", value)
	}
	if value := strings.TrimSpace(options.Severity); value != "" {
		query.Set("severity", value)
	}
	if value := strings.TrimSpace(options.Usage); value != "" {
		query.Set("usage", value)
	}
	if value := strings.TrimSpace(options.Package); value != "" {
		query.Set("package", value)
	}
	if options.Limit > 0 && options.Limit != dependencies.DefaultFindingLimit {
		query.Set("limit", strconv.Itoa(options.Limit))
	}
	if offset > 0 {
		query.Set("offset", strconv.Itoa(offset))
	}
	if encoded := query.Encode(); encoded != "" {
		return base + "?" + encoded
	}
	return base
}
