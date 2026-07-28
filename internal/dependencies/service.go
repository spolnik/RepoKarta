package dependencies

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spolnik/RepoKarta/internal/graph"
)

var (
	registryTokenEnvironmentPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	prereleaseQualifierPattern      = regexp.MustCompile(
		`(?i)(?:^|[0-9._+-])(?:snapshot|alpha|beta|rc|dev|pre|preview|milestone|m)(?:[0-9]*)(?:$|[._+-])`,
	)
)

const (
	PublicNPMRegistry     = "https://registry.npmjs.org"
	PublicMavenRepository = "https://search.maven.org"
	PublicPyPIRegistry    = "https://pypi.org"
	PublicCargoRegistry   = "https://crates.io"
	PublicGoProxy         = "https://proxy.golang.org"
	PublicNuGetRegistry   = "https://api.nuget.org"

	defaultObservationTTL = 24 * time.Hour
	defaultErrorTTL       = 15 * time.Minute
	defaultWorkerCount    = 8
	maximumRegistryBody   = 8 << 20
)

// RegistryKey uniquely identifies one public package lookup. The registry is
// part of the key so a future private-registry adapter cannot contaminate
// public observations for a package with the same name.
type RegistryKey struct {
	Ecosystem string `json:"ecosystem"`
	Registry  string `json:"registry"`
	Package   string `json:"package"`
}

// RegistryConfig explicitly routes package prefixes to a private registry.
// TokenEnv names an environment variable; its secret value is never persisted.
type RegistryConfig struct {
	Ecosystem           string   `json:"ecosystem"`
	BaseURL             string   `json:"base_url"`
	MetadataURLTemplate string   `json:"metadata_url_template"`
	PackagePrefixes     []string `json:"package_prefixes"`
	TokenEnv            string   `json:"token_env,omitempty"`
}

type registryDecision struct {
	Key    RegistryKey
	Status string
	Detail string
}

// Observation is one durable, cacheable registry result.
type Observation struct {
	RegistryKey
	LatestStable string    `json:"latest_stable,omitempty"`
	Status       string    `json:"status"`
	Error        string    `json:"error,omitempty"`
	ETag         string    `json:"etag,omitempty"`
	LastModified string    `json:"last_modified,omitempty"`
	ObservedAt   time.Time `json:"observed_at"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// ObservationStore persists registry results independently from commit-pinned
// declarations.
type ObservationStore interface {
	ListDependencyObservations(context.Context) ([]Observation, error)
	UpsertDependencyObservation(context.Context, Observation) error
}

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// RefreshProgress is a bounded background refresh snapshot.
type RefreshProgress struct {
	State      string `json:"state"`
	Total      int    `json:"total"`
	Completed  int    `json:"completed"`
	Failed     int    `json:"failed"`
	Skipped    int    `json:"skipped"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
}

// Service joins cached registry observations onto declarations and owns the
// token-free background refresh worker pool.
type Service struct {
	ctx         context.Context
	store       ObservationStore
	client      httpDoer
	ttl         time.Duration
	workerCount int
	now         func() time.Time
	registries  []RegistryConfig

	startMu  sync.Mutex
	mu       sync.RWMutex
	progress RefreshProgress

	advisoryDirectory string
	advisoryBaseURL   string
	advisoryStartMu   sync.Mutex
	advisoryMu        sync.RWMutex
	advisoryFileMu    sync.RWMutex
	advisoryProgress  AdvisoryRefreshProgress
}

// ParseRegistryConfigs validates the optional JSON environment configuration.
func ParseRegistryConfigs(value string) ([]RegistryConfig, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	if len(value) > 64<<10 {
		return nil, errors.New("dependency registry configuration exceeds 64 KiB")
	}
	var configs []RegistryConfig
	if err := json.Unmarshal([]byte(value), &configs); err != nil {
		return nil, fmt.Errorf("decode dependency registry configuration: %w", err)
	}
	for index := range configs {
		configs[index].Ecosystem = strings.ToLower(strings.TrimSpace(configs[index].Ecosystem))
		configs[index].BaseURL = strings.TrimRight(strings.TrimSpace(configs[index].BaseURL), "/")
		configs[index].MetadataURLTemplate = strings.TrimSpace(configs[index].MetadataURLTemplate)
		configs[index].TokenEnv = strings.TrimSpace(configs[index].TokenEnv)
		if !slices.Contains([]string{"npm", "maven", "pypi", "cargo", "go", "nuget"}, configs[index].Ecosystem) {
			return nil, fmt.Errorf("dependency registry %d has unsupported ecosystem", index+1)
		}
		if err := validateRegistryURL(configs[index].BaseURL); err != nil {
			return nil, fmt.Errorf("dependency registry %d base URL: %w", index+1, err)
		}
		if err := validateRegistryURL(configs[index].MetadataURLTemplate); err != nil {
			return nil, fmt.Errorf("dependency registry %d metadata template: %w", index+1, err)
		}
		baseURL, _ := url.Parse(configs[index].BaseURL)
		metadataURL, _ := url.Parse(configs[index].MetadataURLTemplate)
		if !strings.EqualFold(baseURL.Scheme, metadataURL.Scheme) ||
			!strings.EqualFold(baseURL.Host, metadataURL.Host) {
			return nil, fmt.Errorf(
				"dependency registry %d metadata template must use the base URL origin",
				index+1,
			)
		}
		if !strings.Contains(configs[index].MetadataURLTemplate, "{package}") &&
			!strings.Contains(configs[index].MetadataURLTemplate, "{module}") &&
			!strings.Contains(configs[index].MetadataURLTemplate, "{group_path}") &&
			!strings.Contains(configs[index].MetadataURLTemplate, "{cargo_path}") {
			return nil, fmt.Errorf("dependency registry %d metadata template lacks a package placeholder", index+1)
		}
		if len(configs[index].PackagePrefixes) == 0 {
			return nil, fmt.Errorf("dependency registry %d needs at least one package prefix", index+1)
		}
		for prefixIndex := range configs[index].PackagePrefixes {
			configs[index].PackagePrefixes[prefixIndex] = strings.TrimSpace(configs[index].PackagePrefixes[prefixIndex])
			if configs[index].PackagePrefixes[prefixIndex] == "" {
				return nil, fmt.Errorf("dependency registry %d has an empty package prefix", index+1)
			}
		}
		if configs[index].TokenEnv != "" &&
			!registryTokenEnvironmentPattern.MatchString(configs[index].TokenEnv) {
			return nil, fmt.Errorf("dependency registry %d has an invalid token environment variable", index+1)
		}
	}
	return configs, nil
}

// UseRegistries installs validated explicit private-registry routing.
func (s *Service) UseRegistries(configs []RegistryConfig) {
	s.registries = append([]RegistryConfig(nil), configs...)
}

// NewService creates a cache-first dependency service. Registry calls are made
// only after StartRefresh; Inventory itself never performs network I/O.
func NewService(ctx context.Context, store ObservationStore, client *http.Client) *Service {
	if ctx == nil {
		ctx = context.Background()
	}
	if client == nil {
		client = newDependencyHTTPClient()
	}
	return &Service{
		ctx:              ctx,
		store:            store,
		client:           client,
		ttl:              defaultObservationTTL,
		workerCount:      defaultWorkerCount,
		now:              time.Now,
		progress:         RefreshProgress{State: "idle"},
		advisoryProgress: AdvisoryRefreshProgress{State: "idle"},
	}
}

// Inventory reads only SQLite-backed observations and joins them to the
// commit-pinned declaration page.
func (s *Service) Inventory(
	ctx context.Context,
	snapshot graph.Snapshot,
	options Options,
) (Inventory, error) {
	observations, err := s.store.ListDependencyObservations(ctx)
	if err != nil {
		return Inventory{}, fmt.Errorf("list dependency observations: %w", err)
	}
	byKey := make(map[string]Observation, len(observations))
	for _, observation := range observations {
		byKey[registryKeyString(observation.RegistryKey)] = observation
	}
	now := s.now()
	return buildPage(snapshot, options, func(declaration *Declaration) {
		decision := s.registryDecisionFor(*declaration)
		if decision.Status != "" {
			declaration.CheckStatus = decision.Status
			declaration.CheckDetail = decision.Detail
			declaration.VersionDistance = "unknown"
			return
		}
		key := decision.Key
		declaration.Registry = key.Registry
		observation, ok := byKey[registryKeyString(key)]
		if !ok {
			return
		}
		declaration.LatestStable = observation.LatestStable
		declaration.ObservedAt = observation.ObservedAt.UTC().Format(time.RFC3339)
		switch {
		case observation.Status == "error":
			declaration.CheckStatus = "registry_error"
			declaration.CheckDetail = observation.Error
			declaration.VersionDistance = "unknown"
		case !observation.ExpiresAt.IsZero() && !observation.ExpiresAt.After(now):
			declaration.CheckStatus = "stale"
			declaration.CheckDetail = "the cached registry observation has expired"
			declaration.VersionDistance = "unknown"
		default:
			declaration.CheckStatus = declarationStatus(*declaration, observation.LatestStable)
			declaration.VersionDistance = versionDistance(*declaration, observation.LatestStable)
		}
	}), nil
}

// Progress returns a race-free copy of the current refresh state.
func (s *Service) Progress() RefreshProgress {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.progress
}

// StartRefresh deduplicates matching declarations and asynchronously refreshes
// only stale observations unless force is true.
func (s *Service) StartRefresh(
	snapshot graph.Snapshot,
	options Options,
	force bool,
) (RefreshProgress, error) {
	s.startMu.Lock()
	defer s.startMu.Unlock()
	s.mu.Lock()
	if s.progress.State == "running" {
		progress := s.progress
		s.mu.Unlock()
		return progress, nil
	}
	s.mu.Unlock()

	declarations := filterDeclarations(normalizedDeclarations(snapshot), options)
	targets := make(map[string]RegistryKey)
	skipped := 0
	for _, declaration := range declarations {
		decision := s.registryDecisionFor(declaration)
		if decision.Status != "" {
			skipped++
			continue
		}
		key := decision.Key
		targets[registryKeyString(key)] = key
	}
	observations, err := s.store.ListDependencyObservations(s.ctx)
	if err != nil {
		return RefreshProgress{}, fmt.Errorf("list dependency observations: %w", err)
	}
	prior := make(map[string]Observation, len(observations))
	for _, observation := range observations {
		prior[registryKeyString(observation.RegistryKey)] = observation
	}
	now := s.now()
	keys := make([]RegistryKey, 0, len(targets))
	for keyString, key := range targets {
		observation, found := prior[keyString]
		if !force && found && observation.ExpiresAt.After(now) {
			skipped++
			continue
		}
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(left, right RegistryKey) int {
		return strings.Compare(registryKeyString(left), registryKeyString(right))
	})
	progress := RefreshProgress{
		State:     "running",
		Total:     len(keys),
		Skipped:   skipped,
		StartedAt: now.UTC().Format(time.RFC3339),
	}
	if len(keys) == 0 {
		progress.State = "complete"
		progress.FinishedAt = progress.StartedAt
	}
	s.mu.Lock()
	s.progress = progress
	s.mu.Unlock()
	if len(keys) > 0 {
		go s.runRefresh(keys, prior)
	}
	return progress, nil
}

func (s *Service) runRefresh(keys []RegistryKey, prior map[string]Observation) {
	jobs := make(chan RegistryKey)
	var workers sync.WaitGroup
	for range min(s.workerCount, len(keys)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for key := range jobs {
				previous := prior[registryKeyString(key)]
				observation := s.lookup(s.ctx, key, previous)
				failed := observation.Status == "error"
				if err := s.store.UpsertDependencyObservation(s.ctx, observation); err != nil {
					failed = true
				}
				s.mu.Lock()
				s.progress.Completed++
				if failed {
					s.progress.Failed++
				}
				s.mu.Unlock()
			}
		}()
	}
	for _, key := range keys {
		select {
		case <-s.ctx.Done():
			close(jobs)
			workers.Wait()
			s.finishRefresh()
			return
		case jobs <- key:
		}
	}
	close(jobs)
	workers.Wait()
	s.finishRefresh()
}

func (s *Service) finishRefresh() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.progress.State = "complete"
	s.progress.FinishedAt = s.now().UTC().Format(time.RFC3339)
}

func (s *Service) lookup(ctx context.Context, key RegistryKey, previous Observation) Observation {
	now := s.now().UTC()
	observation := Observation{
		RegistryKey:  key,
		LatestStable: previous.LatestStable,
		Status:       "error",
		ETag:         previous.ETag,
		LastModified: previous.LastModified,
		ObservedAt:   now,
		ExpiresAt:    now.Add(defaultErrorTTL),
	}
	requestURL, headers, tokenEnv, err := s.registryRequest(key)
	if err != nil {
		observation.Error = err.Error()
		return observation
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		observation.Error = err.Error()
		return observation
	}
	request.Header.Set("Accept", headers)
	request.Header.Set("User-Agent", "RepoKarta dependency-checker (+https://github.com/spolnik/RepoKarta)")
	if tokenEnv != "" {
		token := strings.TrimSpace(os.Getenv(tokenEnv))
		if token == "" {
			observation.Error = "registry credential environment variable is not set"
			return observation
		}
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if previous.ETag != "" {
		request.Header.Set("If-None-Match", previous.ETag)
	}
	if previous.LastModified != "" {
		request.Header.Set("If-Modified-Since", previous.LastModified)
	}
	response, err := s.client.Do(request)
	if err != nil {
		observation.Error = err.Error()
		return observation
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotModified && previous.Status == "ok" {
		previous.ObservedAt = now
		previous.ExpiresAt = now.Add(s.ttl)
		return previous
	}
	if response.StatusCode != http.StatusOK {
		observation.Error = "registry returned " + response.Status
		if response.StatusCode == http.StatusNotFound {
			observation.ExpiresAt = now.Add(time.Hour)
		}
		if response.StatusCode == http.StatusTooManyRequests {
			observation.ExpiresAt = retryAfter(response.Header.Get("Retry-After"), now)
		}
		return observation
	}
	content, err := io.ReadAll(io.LimitReader(response.Body, maximumRegistryBody+1))
	if err != nil {
		observation.Error = err.Error()
		return observation
	}
	if len(content) > maximumRegistryBody {
		observation.Error = "registry response exceeded 8 MiB"
		return observation
	}
	switch key.Ecosystem {
	case "npm":
		observation.LatestStable, err = npmLatestStable(content)
	case "maven":
		observation.LatestStable, err = mavenLatestStable(content)
	case "pypi":
		observation.LatestStable, err = pypiLatestStable(content)
	case "cargo":
		observation.LatestStable, err = cargoLatestStable(content)
	case "go":
		observation.LatestStable, err = goLatestStable(content)
	case "nuget":
		observation.LatestStable, err = nugetLatestStable(content)
	default:
		err = errors.New("unsupported dependency registry")
	}
	if err != nil {
		observation.Error = err.Error()
		return observation
	}
	observation.Status = "ok"
	observation.ETag = response.Header.Get("ETag")
	observation.LastModified = response.Header.Get("Last-Modified")
	return observation
}

func registryRequest(key RegistryKey) (string, string, error) {
	switch key.Ecosystem {
	case "npm":
		return PublicNPMRegistry + "/" + url.PathEscape(key.Package),
			"application/vnd.npm.install-v1+json", nil
	case "maven":
		parts := strings.Split(key.Package, ":")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return "", "", errors.New("Maven package must be group:artifact")
		}
		query := url.Values{
			"q":    {`g:"` + parts[0] + `" AND a:"` + parts[1] + `"`},
			"core": {"gav"},
			"rows": {"200"},
			"sort": {"timestamp desc"},
			"wt":   {"json"},
		}
		return PublicMavenRepository + "/solrsearch/select?" + query.Encode(),
			"application/json", nil
	case "pypi":
		return PublicPyPIRegistry + "/pypi/" + url.PathEscape(key.Package) + "/json",
			"application/json", nil
	case "cargo":
		return PublicCargoRegistry + "/api/v1/crates/" + url.PathEscape(key.Package),
			"application/json", nil
	case "go":
		return PublicGoProxy + "/" + escapeGoModulePath(key.Package) + "/@v/list",
			"text/plain", nil
	case "nuget":
		return PublicNuGetRegistry + "/v3-flatcontainer/" +
				url.PathEscape(strings.ToLower(key.Package)) + "/index.json",
			"application/json", nil
	default:
		return "", "", errors.New("unsupported dependency ecosystem")
	}
}

func (s *Service) registryRequest(key RegistryKey) (string, string, string, error) {
	if isPublicRegistry(key.Registry) {
		requestURL, accept, err := registryRequest(key)
		return requestURL, accept, "", err
	}
	for _, config := range s.registries {
		if config.Ecosystem != key.Ecosystem ||
			!strings.EqualFold(strings.TrimRight(config.BaseURL, "/"), strings.TrimRight(key.Registry, "/")) {
			continue
		}
		requestURL := expandRegistryTemplate(config.MetadataURLTemplate, key)
		return requestURL, registryAccept(key.Ecosystem), config.TokenEnv, nil
	}
	return "", "", "", errors.New("dependency registry configuration is unavailable")
}

func npmLatestStable(content []byte) (string, error) {
	var document struct {
		DistTags map[string]string          `json:"dist-tags"`
		Versions map[string]json.RawMessage `json:"versions"`
	}
	if err := json.Unmarshal(content, &document); err != nil {
		return "", fmt.Errorf("decode npm metadata: %w", err)
	}
	latest := strings.TrimSpace(document.DistTags["latest"])
	if latest != "" && stableVersion(latest) {
		return latest, nil
	}
	latest = ""
	for version := range document.Versions {
		if stableVersion(version) && (latest == "" || compareLooseVersions(version, latest) > 0) {
			latest = version
		}
	}
	if latest == "" {
		return "", errors.New("npm package has no stable version")
	}
	return latest, nil
}

func mavenLatestStable(content []byte) (string, error) {
	var document struct {
		Response struct {
			Docs []struct {
				LatestVersion string `json:"latestVersion"`
				Version       string `json:"v"`
			} `json:"docs"`
		} `json:"response"`
	}
	if err := json.Unmarshal(content, &document); err == nil {
		for _, document := range document.Response.Docs {
			version := firstNonEmpty(document.Version, document.LatestVersion)
			if mavenStableVersion(version) {
				return strings.TrimSpace(version), nil
			}
		}
	}
	var metadata struct {
		Versioning struct {
			Release  string   `xml:"release"`
			Versions []string `xml:"versions>version"`
		} `xml:"versioning"`
	}
	if err := xml.Unmarshal(content, &metadata); err == nil {
		if mavenStableVersion(metadata.Versioning.Release) {
			return strings.TrimSpace(metadata.Versioning.Release), nil
		}
		for index := len(metadata.Versioning.Versions) - 1; index >= 0; index-- {
			if mavenStableVersion(metadata.Versioning.Versions[index]) {
				return strings.TrimSpace(metadata.Versioning.Versions[index]), nil
			}
		}
	}
	return "", errors.New("Maven artifact has no stable version")
}

func pypiLatestStable(content []byte) (string, error) {
	var document struct {
		Info struct {
			Version string `json:"version"`
		} `json:"info"`
		Releases map[string]json.RawMessage `json:"releases"`
	}
	if err := json.Unmarshal(content, &document); err != nil {
		return "", fmt.Errorf("decode PyPI metadata: %w", err)
	}
	// PyPI's info.version is the index-selected latest release. Prefer it over
	// locally reimplementing the full PEP 440 ordering rules.
	if genericStableVersion(document.Info.Version) {
		return document.Info.Version, nil
	}
	latest := ""
	for version := range document.Releases {
		if genericStableVersion(version) &&
			(latest == "" || compareLooseVersions(version, latest) > 0) {
			latest = version
		}
	}
	if latest == "" {
		return "", errors.New("PyPI project has no stable version")
	}
	return latest, nil
}

func cargoLatestStable(content []byte) (string, error) {
	var document struct {
		Crate struct {
			MaxStableVersion string `json:"max_stable_version"`
			MaxVersion       string `json:"max_version"`
			NewestVersion    string `json:"newest_version"`
		} `json:"crate"`
	}
	if err := json.Unmarshal(content, &document); err == nil {
		version := firstNonEmpty(
			document.Crate.MaxStableVersion,
			document.Crate.MaxVersion,
			document.Crate.NewestVersion,
		)
		if genericStableVersion(version) {
			return version, nil
		}
	}
	latest := ""
	for _, line := range strings.Split(string(content), "\n") {
		var entry struct {
			Version string `json:"vers"`
			Yanked  bool   `json:"yanked"`
		}
		if json.Unmarshal([]byte(line), &entry) == nil && !entry.Yanked &&
			genericStableVersion(entry.Version) &&
			(latest == "" || compareLooseVersions(entry.Version, latest) > 0) {
			latest = entry.Version
		}
	}
	if latest == "" {
		return "", errors.New("crate has no stable version")
	}
	return latest, nil
}

func goLatestStable(content []byte) (string, error) {
	latest := ""
	for _, version := range strings.Fields(string(content)) {
		if stableVersion(strings.TrimPrefix(version, "v")) &&
			(latest == "" || compareLooseVersions(version, latest) > 0) {
			latest = version
		}
	}
	if latest == "" {
		return "", errors.New("Go module has no stable version")
	}
	return latest, nil
}

func nugetLatestStable(content []byte) (string, error) {
	var document struct {
		Versions []string `json:"versions"`
	}
	if err := json.Unmarshal(content, &document); err != nil {
		return "", fmt.Errorf("decode NuGet metadata: %w", err)
	}
	latest := ""
	for _, version := range document.Versions {
		if genericStableVersion(version) &&
			(latest == "" || compareLooseVersions(version, latest) > 0) {
			latest = version
		}
	}
	if latest == "" {
		return "", errors.New("NuGet package has no stable version")
	}
	return latest, nil
}

func (s *Service) registryDecisionFor(declaration Declaration) registryDecision {
	key := RegistryKey{
		Ecosystem: strings.ToLower(strings.TrimSpace(declaration.Ecosystem)),
		Package:   strings.TrimSpace(declaration.Package),
	}
	if key.Package == "" {
		return registryDecision{Status: "unavailable", Detail: "the declaration has no package coordinate"}
	}
	for _, prefix := range []string{"workspace:", "file:", "link:", "git+", "http:", "https:"} {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(declaration.Declared)), prefix) {
			return registryDecision{
				Status: "unavailable",
				Detail: "local, linked, Git, and URL dependencies are not sent to a package registry",
			}
		}
	}
	if strings.HasPrefix(strings.TrimSpace(declaration.Declared), "@ ") {
		return registryDecision{
			Status: "unresolved",
			Detail: "the declared version is an unresolved build indirection",
		}
	}
	longestPrefix := -1
	for _, config := range s.registries {
		if config.Ecosystem != key.Ecosystem {
			continue
		}
		for _, prefix := range config.PackagePrefixes {
			if strings.HasPrefix(strings.ToLower(key.Package), strings.ToLower(prefix)) &&
				len(prefix) > longestPrefix {
				key.Registry = config.BaseURL
				longestPrefix = len(prefix)
			}
		}
	}
	if key.Registry != "" {
		return registryDecision{Key: key}
	}
	if looksPrivatePackage(key.Ecosystem, key.Package) {
		return registryDecision{
			Status: "private_internal",
			Detail: "not checked publicly; configure an explicit package-prefix route to a safe registry",
		}
	}
	switch key.Ecosystem {
	case "npm":
		key.Registry = PublicNPMRegistry
	case "maven":
		if strings.HasPrefix(key.Package, "project:") || strings.Count(key.Package, ":") != 1 {
			return registryDecision{
				Status: "unavailable",
				Detail: "the Maven coordinate is unresolved or malformed",
			}
		}
		key.Registry = PublicMavenRepository
	case "pypi":
		key.Registry = PublicPyPIRegistry
	case "cargo":
		key.Registry = PublicCargoRegistry
	case "go":
		key.Registry = PublicGoProxy
	case "nuget":
		key.Registry = PublicNuGetRegistry
	default:
		return registryDecision{
			Status: "unavailable",
			Detail: "no package registry adapter is available for this ecosystem",
		}
	}
	return registryDecision{Key: key}
}

// registryKeyFor is retained for focused tests and callers that only need the
// selected destination. New code should use registryDecisionFor so a
// fail-closed row remains distinguishable from an unsupported declaration.
func (s *Service) registryKeyFor(declaration Declaration) (RegistryKey, bool) {
	decision := s.registryDecisionFor(declaration)
	return decision.Key, decision.Status == ""
}

func looksPrivatePackage(ecosystem, packageName string) bool {
	ecosystem = strings.ToLower(strings.TrimSpace(ecosystem))
	lower := strings.ToLower(strings.TrimSpace(packageName))
	if lower == "" {
		return false
	}
	segments := strings.FieldsFunc(lower, func(character rune) bool {
		switch character {
		case '/', '\\', ':', '.', '-', '_', '@':
			return true
		default:
			return false
		}
	})
	for _, segment := range segments {
		switch segment {
		case "internal", "private", "corp", "corporate", "intranet", "local":
			return true
		}
	}
	switch ecosystem {
	case "npm":
		// Scoped npm coordinates can be public, but they can also disclose an
		// organization name. Require an explicit prefix route before any scoped
		// coordinate crosses the network.
		return strings.HasPrefix(lower, "@")
	case "go":
		host, _, _ := strings.Cut(lower, "/")
		return host == "localhost" || strings.HasSuffix(host, ".local") || !strings.Contains(host, ".")
	case "maven":
		group, _, found := strings.Cut(lower, ":")
		return found && (group == "com.example" || strings.HasSuffix(group, ".local"))
	default:
		return false
	}
}

func validateRegistryURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Hostname() == "" {
		return errors.New("must be an absolute HTTP URL")
	}
	if parsed.Scheme == "https" {
		return nil
	}
	host := strings.ToLower(parsed.Hostname())
	if parsed.Scheme == "http" && (host == "localhost" || host == "127.0.0.1" || host == "::1") {
		return nil
	}
	return errors.New("must use HTTPS unless it targets loopback")
}

func isPublicRegistry(registry string) bool {
	for _, public := range []string{
		PublicNPMRegistry,
		PublicMavenRepository,
		PublicPyPIRegistry,
		PublicCargoRegistry,
		PublicGoProxy,
		PublicNuGetRegistry,
	} {
		if strings.EqualFold(strings.TrimRight(registry, "/"), public) {
			return true
		}
	}
	return false
}

func expandRegistryTemplate(template string, key RegistryKey) string {
	group, artifact, _ := strings.Cut(key.Package, ":")
	replacements := map[string]string{
		"{package}":    url.PathEscape(key.Package),
		"{module}":     escapeGoModulePath(key.Package),
		"{group}":      url.PathEscape(group),
		"{artifact}":   url.PathEscape(artifact),
		"{group_path}": strings.ReplaceAll(url.PathEscape(strings.ReplaceAll(group, ".", "/")), "%2F", "/"),
		"{cargo_path}": cargoSparsePath(key.Package),
	}
	output := template
	for placeholder, value := range replacements {
		output = strings.ReplaceAll(output, placeholder, value)
	}
	return output
}

func cargoSparsePath(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	switch len(name) {
	case 0:
		return ""
	case 1:
		return "1/" + name
	case 2:
		return "2/" + name
	case 3:
		return "3/" + name[:1] + "/" + name
	default:
		return name[:2] + "/" + name[2:4] + "/" + name
	}
}

func registryAccept(ecosystem string) string {
	switch ecosystem {
	case "go":
		return "text/plain"
	default:
		return "application/json, application/xml, text/plain"
	}
}

func registryKeyString(key RegistryKey) string {
	return strings.ToLower(strings.TrimSpace(key.Ecosystem)) + "\x00" +
		strings.TrimRight(strings.TrimSpace(key.Registry), "/") + "\x00" +
		strings.TrimSpace(key.Package)
}

func declarationStatus(declaration Declaration, latest string) string {
	current := firstNonEmpty(declaration.Resolved, declaration.Declared)
	declared := strings.TrimSpace(strings.TrimPrefix(current, "v"))
	latest = strings.TrimSpace(strings.TrimPrefix(latest, "v"))
	if latest == "" {
		return "unavailable"
	}
	if declaration.Resolved == "" && declaration.Resolution != "exact" {
		return "unresolved"
	}
	if strings.EqualFold(declared, latest) {
		return "current"
	}
	if !genericStableVersion(declared) {
		return "prerelease"
	}
	if compareLooseVersions(declared, latest) > 0 {
		return "ahead"
	}
	return "behind"
}

func versionDistance(declaration Declaration, latest string) string {
	if declarationStatus(declaration, latest) == "current" {
		return "none"
	}
	if declarationStatus(declaration, latest) != "behind" {
		return "unknown"
	}
	current := firstNonEmpty(declaration.Resolved, declaration.Declared)
	currentParts := numericVersionParts(current)
	latestParts := numericVersionParts(latest)
	if len(currentParts) == 0 || len(latestParts) == 0 {
		return "unknown"
	}
	value := func(parts []int, index int) int {
		if index >= len(parts) {
			return 0
		}
		return parts[index]
	}
	switch {
	case value(currentParts, 0) != value(latestParts, 0):
		return "major"
	case value(currentParts, 1) != value(latestParts, 1):
		return "minor"
	case value(currentParts, 2) != value(latestParts, 2):
		return "patch"
	default:
		return "unknown"
	}
}

func stableVersion(version string) bool {
	version = strings.TrimSpace(strings.TrimPrefix(version, "v"))
	version, _, _ = strings.Cut(version, "+")
	if version == "" || strings.Contains(version, "-") {
		return false
	}
	for _, part := range strings.Split(version, ".") {
		if part == "" {
			return false
		}
		if _, err := strconv.Atoi(part); err != nil {
			return false
		}
	}
	return true
}

func compareLooseVersions(left, right string) int {
	leftParts := numericVersionParts(left)
	rightParts := numericVersionParts(right)
	for index := range max(len(leftParts), len(rightParts)) {
		leftValue, rightValue := 0, 0
		if index < len(leftParts) {
			leftValue = leftParts[index]
		}
		if index < len(rightParts) {
			rightValue = rightParts[index]
		}
		if leftValue < rightValue {
			return -1
		}
		if leftValue > rightValue {
			return 1
		}
	}
	return 0
}

func numericVersionParts(version string) []int {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	parts := make([]int, 0, 4)
	current := -1
	for _, character := range version {
		if character >= '0' && character <= '9' {
			if current < 0 {
				current = 0
			}
			current = current*10 + int(character-'0')
			continue
		}
		if current >= 0 {
			parts = append(parts, current)
			current = -1
		}
	}
	if current >= 0 {
		parts = append(parts, current)
	}
	return parts
}

func genericStableVersion(version string) bool {
	lower := strings.ToLower(strings.TrimSpace(version))
	if lower == "" {
		return false
	}
	return !prereleaseQualifierPattern.MatchString(lower) &&
		len(numericVersionParts(version)) > 0
}

func escapeGoModulePath(module string) string {
	var escaped strings.Builder
	for _, character := range module {
		if character >= 'A' && character <= 'Z' {
			escaped.WriteByte('!')
			escaped.WriteRune(character - 'A' + 'a')
			continue
		}
		escaped.WriteRune(character)
	}
	return escaped.String()
}

func mavenStableVersion(version string) bool {
	lower := strings.ToLower(strings.TrimSpace(version))
	if lower == "" {
		return false
	}
	for _, marker := range []string{
		"snapshot", "alpha", "beta", "-rc", ".rc", "-m", ".m",
		"milestone", "preview", "-ea", ".ea",
	} {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	return true
}

func retryAfter(value string, now time.Time) time.Time {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return now.Add(min(time.Duration(seconds)*time.Second, defaultObservationTTL))
	}
	if parsed, err := http.ParseTime(value); err == nil && parsed.After(now) {
		if parsed.After(now.Add(defaultObservationTTL)) {
			return now.Add(defaultObservationTTL)
		}
		return parsed
	}
	return now.Add(defaultErrorTTL)
}
