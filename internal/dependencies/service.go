package dependencies

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spolnik/RepoKarta/internal/graph"
)

const (
	PublicNPMRegistry     = "https://registry.npmjs.org"
	PublicMavenRepository = "https://search.maven.org"

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

	startMu  sync.Mutex
	mu       sync.RWMutex
	progress RefreshProgress
}

// NewService creates a cache-first dependency service. Registry calls are made
// only after StartRefresh; Inventory itself never performs network I/O.
func NewService(ctx context.Context, store ObservationStore, client *http.Client) *Service {
	if ctx == nil {
		ctx = context.Background()
	}
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	return &Service{
		ctx:         ctx,
		store:       store,
		client:      client,
		ttl:         defaultObservationTTL,
		workerCount: defaultWorkerCount,
		now:         time.Now,
		progress:    RefreshProgress{State: "idle"},
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
		key, ok := registryKeyFor(*declaration)
		if !ok {
			declaration.CheckStatus = "not_comparable"
			return
		}
		declaration.Registry = key.Registry
		observation, ok := byKey[registryKeyString(key)]
		if !ok {
			return
		}
		declaration.LatestStable = observation.LatestStable
		declaration.ObservedAt = observation.ObservedAt.UTC().Format(time.RFC3339)
		switch {
		case observation.Status == "error":
			declaration.CheckStatus = "error"
		case !observation.ExpiresAt.IsZero() && !observation.ExpiresAt.After(now):
			declaration.CheckStatus = "stale"
		default:
			declaration.CheckStatus = declarationStatus(*declaration, observation.LatestStable)
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
		key, ok := registryKeyFor(declaration)
		if !ok {
			skipped++
			continue
		}
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
	requestURL, headers, err := registryRequest(key)
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
	request.Header.Set("User-Agent", "RepoKarta dependency-checker")
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
	default:
		return "", "", errors.New("unsupported dependency ecosystem")
	}
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
	if err := json.Unmarshal(content, &document); err != nil {
		return "", fmt.Errorf("decode Maven metadata: %w", err)
	}
	for _, document := range document.Response.Docs {
		version := firstNonEmpty(document.Version, document.LatestVersion)
		if mavenStableVersion(version) {
			return strings.TrimSpace(version), nil
		}
	}
	return "", errors.New("Maven artifact has no stable version")
}

func registryKeyFor(declaration Declaration) (RegistryKey, bool) {
	key := RegistryKey{
		Ecosystem: strings.ToLower(strings.TrimSpace(declaration.Ecosystem)),
		Package:   strings.TrimSpace(declaration.Package),
	}
	switch key.Ecosystem {
	case "npm":
		key.Registry = PublicNPMRegistry
	case "maven":
		if strings.HasPrefix(key.Package, "project:") || strings.Count(key.Package, ":") != 1 {
			return RegistryKey{}, false
		}
		key.Registry = PublicMavenRepository
	default:
		return RegistryKey{}, false
	}
	return key, key.Package != ""
}

func registryKeyString(key RegistryKey) string {
	return strings.ToLower(strings.TrimSpace(key.Ecosystem)) + "\x00" +
		strings.TrimRight(strings.TrimSpace(key.Registry), "/") + "\x00" +
		strings.TrimSpace(key.Package)
}

func declarationStatus(declaration Declaration, latest string) string {
	declared := strings.TrimSpace(strings.TrimPrefix(declaration.Declared, "v"))
	latest = strings.TrimSpace(strings.TrimPrefix(latest, "v"))
	if latest == "" {
		return "unchecked"
	}
	if declaration.Resolution != "exact" {
		return "latest_known"
	}
	if strings.EqualFold(declared, latest) {
		return "current"
	}
	return "update_available"
}

func stableVersion(version string) bool {
	version = strings.TrimSpace(strings.TrimPrefix(version, "v"))
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
	leftParts := strings.Split(strings.TrimPrefix(left, "v"), ".")
	rightParts := strings.Split(strings.TrimPrefix(right, "v"), ".")
	for index := range max(len(leftParts), len(rightParts)) {
		leftValue, rightValue := 0, 0
		if index < len(leftParts) {
			leftValue, _ = strconv.Atoi(leftParts[index])
		}
		if index < len(rightParts) {
			rightValue, _ = strconv.Atoi(rightParts[index])
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
