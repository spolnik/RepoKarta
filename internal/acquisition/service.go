package acquisition

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spolnik/RepoKarta/internal/catalog"
)

const (
	defaultGitTimeout = 10 * time.Minute
	maximumErrorText  = 2048
)

// Registry persists repository acquisition provenance and source-free events.
type Registry interface {
	ListAcquisitions(context.Context) ([]Repository, error)
	ListRepositories(context.Context) ([]catalog.Repository, error)
	AcquisitionByID(context.Context, int64) (Repository, error)
	UpsertAcquisition(context.Context, Repository) (Repository, error)
	DeleteAcquisition(context.Context, int64) error
	RecordAcquisitionEvent(context.Context, Event) error
}

// Config controls repository acquisition without containing secret values.
type Config struct {
	DataDirectory string
	Version       string
	GitCommand    string
	HTTPClient    *http.Client
	GitHubAPI     string
	GitLabAPI     string
	GitTimeout    time.Duration
}

// Service discovers, acquires, synchronizes, and safely removes repositories.
type Service struct {
	registry       Registry
	repositoryRoot string
	trashRoot      string
	hooksRoot      string
	version        string
	gitCommand     string
	gitTimeout     time.Duration
	httpClient     *http.Client
	githubAPI      string
	gitlabAPI      string

	refreshMu   sync.RWMutex
	refresh     func(context.Context) error
	operation   sync.Mutex
	gitOverride func(context.Context, ...string) (string, error)
}

// New creates the RepoKarta-owned repository directories and service.
func New(config Config, registry Registry) (*Service, error) {
	if registry == nil {
		return nil, errors.New("acquisition registry is required")
	}
	dataDirectory, err := canonicalDirectory(config.DataDirectory)
	if err != nil {
		return nil, fmt.Errorf("prepare acquisition data directory: %w", err)
	}
	service := &Service{
		registry:       registry,
		repositoryRoot: filepath.Join(dataDirectory, "repositories"),
		trashRoot:      filepath.Join(dataDirectory, "repository-trash"),
		hooksRoot:      filepath.Join(dataDirectory, "no-git-hooks"),
		version:        strings.TrimSpace(config.Version),
		gitCommand:     strings.TrimSpace(config.GitCommand),
		gitTimeout:     config.GitTimeout,
		httpClient:     config.HTTPClient,
		githubAPI:      strings.TrimRight(strings.TrimSpace(config.GitHubAPI), "/"),
		gitlabAPI:      strings.TrimRight(strings.TrimSpace(config.GitLabAPI), "/"),
	}
	if service.version == "" {
		service.version = "dev"
	}
	if service.gitCommand == "" {
		service.gitCommand = "git"
	}
	if service.gitTimeout <= 0 {
		service.gitTimeout = defaultGitTimeout
	}
	if service.httpClient == nil {
		service.httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	if service.githubAPI == "" {
		service.githubAPI = "https://api.github.com"
	}
	if service.gitlabAPI == "" {
		service.gitlabAPI = "https://gitlab.com/api/v4"
	}
	for _, directory := range []string{service.repositoryRoot, service.trashRoot, service.hooksRoot} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, fmt.Errorf("create acquisition directory %q: %w", directory, err)
		}
	}
	return service, nil
}

func canonicalDirectory(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("data directory is required")
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

// UseRefresher connects successful operations to catalogue reconciliation.
func (s *Service) UseRefresher(refresh func(context.Context) error) {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	s.refresh = refresh
}

// List returns all administrator-managed repository sources.
func (s *Service) List(ctx context.Context) ([]Repository, error) {
	return s.registry.ListAcquisitions(ctx)
}

// Discover returns a bounded preview without cloning or modifying repositories.
func (s *Service) Discover(ctx context.Context, request DiscoverRequest) ([]Candidate, error) {
	request.Provider = strings.ToLower(strings.TrimSpace(request.Provider))
	request.Location = strings.TrimSpace(request.Location)
	request.CredentialRef = strings.TrimSpace(request.CredentialRef)
	request.Team = strings.TrimSpace(request.Team)
	request.Topics = normalizePolicyValues(request.Topics)
	request.Allow = normalizePolicyValues(request.Allow)
	request.Deny = normalizePolicyValues(request.Deny)
	if err := validateCredentialRef(request.CredentialRef); err != nil {
		return nil, err
	}
	for _, pattern := range append(append([]string{}, request.Allow...), request.Deny...) {
		if _, err := path.Match(pattern, "owner/repository"); err != nil {
			return nil, fmt.Errorf("invalid repository policy pattern %q: %w", pattern, err)
		}
	}
	var candidates []Candidate
	var err error
	switch request.Provider {
	case ProviderLocal:
		candidates, err = s.discoverLocal(request)
	case ProviderGitHub:
		candidates, err = s.discoverGitHub(ctx, request)
	case ProviderGitLab:
		candidates, err = s.discoverGitLab(ctx, request)
	default:
		err = errors.New("repository provider must be local, github, or gitlab")
	}
	if err != nil {
		_ = s.audit(ctx, Event{
			Action:  "discover",
			Outcome: "error",
			Detail:  fmt.Sprintf("provider=%s error=%s", request.Provider, boundedError(err)),
		})
		return nil, err
	}
	managed, err := s.registry.ListAcquisitions(ctx)
	if err != nil {
		return nil, err
	}
	managedIDs := make(map[string]struct{}, len(managed))
	for _, repository := range managed {
		managedIDs[strings.ToLower(repository.CanonicalID)] = struct{}{}
		if repository.ProviderRepositoryID != "" {
			managedIDs[providerIdentityKey(repository.Provider, repository.ProviderRepositoryID)] = struct{}{}
		}
	}
	catalogue, err := s.registry.ListRepositories(ctx)
	if err != nil {
		return nil, err
	}
	for _, repository := range catalogue {
		managedIDs[strings.ToLower(localCanonicalID(repository.Path))] = struct{}{}
		if canonicalID := canonicalRemoteID(repository.OriginURL); canonicalID != "" {
			managedIDs[canonicalID] = struct{}{}
		}
	}
	seen := make(map[string]struct{}, len(candidates))
	output := make([]Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		key := strings.ToLower(strings.TrimSpace(candidate.CanonicalID))
		if key == "" {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		_, candidate.AlreadyManaged = managedIDs[key]
		if !candidate.AlreadyManaged && candidate.ProviderRepositoryID != "" {
			_, candidate.AlreadyManaged = managedIDs[providerIdentityKey(candidate.Provider, candidate.ProviderRepositoryID)]
		}
		candidate.InclusionPolicy = discoveryPolicy(request)
		if candidate.Excluded {
			_ = s.audit(ctx, Event{
				CanonicalID: candidate.CanonicalID,
				Action:      "discovery_policy",
				Outcome:     "skipped",
				Detail:      candidate.Exclusion,
			})
		}
		output = append(output, candidate)
	}
	sort.Slice(output, func(left, right int) bool {
		return strings.ToLower(output[left].CanonicalID) < strings.ToLower(output[right].CanonicalID)
	})
	_ = s.audit(ctx, Event{
		Action:  "discover",
		Outcome: "success",
		Detail:  fmt.Sprintf("provider=%s candidates=%d", request.Provider, len(output)),
	})
	return output, nil
}

// Acquire approves one preview candidate and makes its verified checkout
// available to the normal commit-pinned catalogue/indexing flow.
func (s *Service) Acquire(ctx context.Context, candidate Candidate, credentialRef string) (Repository, error) {
	s.operation.Lock()
	defer s.operation.Unlock()
	credentialRef = strings.TrimSpace(credentialRef)
	if err := validateCredentialRef(credentialRef); err != nil {
		return Repository{}, err
	}
	candidate.Provider = strings.ToLower(strings.TrimSpace(candidate.Provider))
	if candidate.Excluded {
		return Repository{}, errors.New("excluded discovery candidates cannot be acquired")
	}
	normalized, err := normalizeCandidate(candidate)
	if err != nil {
		return Repository{}, err
	}
	existing, err := s.registry.ListAcquisitions(ctx)
	if err != nil {
		return Repository{}, err
	}
	for _, repository := range existing {
		if strings.EqualFold(repository.CanonicalID, normalized.CanonicalID) ||
			(normalized.ProviderRepositoryID != "" &&
				repository.Provider == normalized.Provider &&
				repository.ProviderRepositoryID == normalized.ProviderRepositoryID) {
			return Repository{}, fmt.Errorf("%s is already managed", normalized.CanonicalID)
		}
	}
	now := time.Now().UTC()
	record := Repository{
		Provider:             normalized.Provider,
		ProviderRepositoryID: normalized.ProviderRepositoryID,
		CanonicalID:          normalized.CanonicalID,
		Name:                 normalized.Name,
		Namespace:            normalized.Namespace,
		RemoteURL:            normalized.RemoteURL,
		WebURL:               normalized.WebURL,
		DefaultBranch:        normalized.DefaultBranch,
		CredentialRef:        credentialRef,
		InclusionPolicy:      normalized.InclusionPolicy,
		Visibility:           normalized.Visibility,
		Archived:             normalized.Archived,
		Forked:               normalized.Forked,
		Owned:                normalized.Provider != ProviderLocal,
		State:                StateAcquiring,
		CreatedAt:            now,
		DiscoveredAt:         now,
		UpdatedAt:            now,
	}
	if normalized.Provider == ProviderLocal {
		record.CheckoutPath = normalized.LocalPath
	} else {
		record.CheckoutPath = s.checkoutPath(normalized)
	}
	record, err = s.registry.UpsertAcquisition(ctx, record)
	if err != nil {
		return Repository{}, err
	}
	_ = s.audit(ctx, Event{RepositoryID: record.ID, CanonicalID: record.CanonicalID, Action: "acquire", Outcome: "started"})

	if record.Provider == ProviderLocal {
		record, err = s.finishLocal(ctx, record)
	} else {
		record, err = s.clone(ctx, record)
	}
	if err != nil {
		return s.fail(ctx, record, "acquire", err)
	}
	if err := s.refreshCatalogue(ctx); err != nil {
		return s.fail(ctx, record, "acquire", fmt.Errorf("refresh catalogue: %w", err))
	}
	_ = s.audit(ctx, Event{
		RepositoryID: record.ID,
		CanonicalID:  record.CanonicalID,
		Action:       "acquire",
		Outcome:      "success",
		Revision:     record.HeadCommit,
	})
	return record, nil
}

func (s *Service) finishLocal(ctx context.Context, record Repository) (Repository, error) {
	inspected, err := catalog.Inspect(record.CheckoutPath)
	if err != nil {
		return record, err
	}
	record.Name = inspected.Name
	record.RemoteURL = inspected.OriginURL
	record.DefaultBranch = inspected.DefaultRevision
	record.HeadCommit = inspected.HeadCommit
	record.State = StateReady
	record.LastError = ""
	record.FailureCount = 0
	record.NextSyncAt = time.Time{}
	record.SyncedAt = time.Now().UTC()
	record.UpdatedAt = record.SyncedAt
	return s.registry.UpsertAcquisition(ctx, record)
}

func (s *Service) clone(ctx context.Context, record Repository) (Repository, error) {
	if err := s.validateOwnedTarget(record.CheckoutPath); err != nil {
		return record, err
	}
	if _, err := os.Stat(record.CheckoutPath); err == nil {
		return record, fmt.Errorf("checkout target already exists: %s", record.CheckoutPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return record, err
	}
	parent := filepath.Dir(record.CheckoutPath)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return record, err
	}
	temporary, err := os.MkdirTemp(parent, ".repokarta-clone-")
	if err != nil {
		return record, err
	}
	defer os.RemoveAll(temporary)
	arguments := []string{
		"-c", "protocol.file.allow=never",
		"-c", "protocol.ext.allow=never",
		"-c", "core.hooksPath=" + s.hooksRoot,
		"clone", "--no-recurse-submodules", "--origin", "origin",
	}
	if branch := strings.TrimSpace(record.DefaultBranch); branch != "" {
		arguments = append(arguments, "--branch", branch, "--single-branch")
	}
	arguments = append(arguments, "--", record.RemoteURL, temporary)
	if _, err := s.runGit(ctx, arguments...); err != nil {
		return record, err
	}
	if _, err := s.runGit(ctx, "-C", temporary, "config", "core.hooksPath", s.hooksRoot); err != nil {
		return record, err
	}
	if _, err := s.runGit(ctx, "-C", temporary, "config", "submodule.recurse", "false"); err != nil {
		return record, err
	}
	inspected, err := catalog.Inspect(temporary)
	if err != nil {
		return record, fmt.Errorf("verify cloned repository: %w", err)
	}
	if err := os.Rename(temporary, record.CheckoutPath); err != nil {
		return record, fmt.Errorf("activate cloned repository: %w", err)
	}
	record.HeadCommit = inspected.HeadCommit
	if record.DefaultBranch == "" {
		record.DefaultBranch = inspected.DefaultRevision
	}
	record.State = StateReady
	record.LastError = ""
	record.FailureCount = 0
	record.NextSyncAt = time.Time{}
	record.SyncedAt = time.Now().UTC()
	record.UpdatedAt = record.SyncedAt
	return s.registry.UpsertAcquisition(ctx, record)
}

// Sync fetches one owned checkout or reinspects one user-owned local repository.
func (s *Service) Sync(ctx context.Context, id int64) (Repository, error) {
	s.operation.Lock()
	defer s.operation.Unlock()
	record, err := s.registry.AcquisitionByID(ctx, id)
	if err != nil {
		return Repository{}, err
	}
	record.State = StateSyncing
	record.LastError = ""
	record.UpdatedAt = time.Now().UTC()
	record, err = s.registry.UpsertAcquisition(ctx, record)
	if err != nil {
		return Repository{}, err
	}
	_ = s.audit(ctx, Event{RepositoryID: record.ID, CanonicalID: record.CanonicalID, Action: "sync", Outcome: "started", Revision: record.HeadCommit})
	previousHead := record.HeadCommit
	if record.Owned {
		if _, statError := os.Stat(record.CheckoutPath); errors.Is(statError, os.ErrNotExist) {
			record, err = s.clone(ctx, record)
		} else if statError != nil {
			err = statError
		} else {
			record, err = s.syncOwned(ctx, record)
		}
	} else {
		record, err = s.finishLocal(ctx, record)
	}
	if err != nil {
		record.HeadCommit = previousHead
		return s.fail(ctx, record, "sync", err)
	}
	if err := s.refreshCatalogue(ctx); err != nil {
		return s.fail(ctx, record, "sync", fmt.Errorf("refresh catalogue: %w", err))
	}
	_ = s.audit(ctx, Event{RepositoryID: record.ID, CanonicalID: record.CanonicalID, Action: "sync", Outcome: "success", Revision: record.HeadCommit})
	return record, nil
}

func (s *Service) syncOwned(ctx context.Context, record Repository) (Repository, error) {
	if err := s.validateOwnedCheckout(record); err != nil {
		return record, err
	}
	configuredRemote, err := s.runGit(ctx, "-C", record.CheckoutPath, "remote", "get-url", "origin")
	if err != nil {
		return record, err
	}
	if canonicalRemoteID(configuredRemote) != strings.ToLower(record.CanonicalID) {
		return record, errors.New("owned checkout origin no longer matches its approved canonical repository identity")
	}
	if _, err := s.runGit(ctx,
		"-c", "protocol.file.allow=never",
		"-c", "protocol.ext.allow=never",
		"-c", "core.hooksPath="+s.hooksRoot,
		"-C", record.CheckoutPath,
		"fetch", "--prune", "--no-recurse-submodules", "origin"); err != nil {
		return record, err
	}
	branch := strings.TrimSpace(record.DefaultBranch)
	if branch == "" || branch == "HEAD" {
		reference, err := s.runGit(ctx, "-C", record.CheckoutPath, "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
		if err != nil {
			return record, errors.New("remote default branch is unavailable")
		}
		branch = strings.TrimPrefix(reference, "origin/")
	}
	if _, err := s.runGit(ctx, "-c", "core.hooksPath="+s.hooksRoot, "-C", record.CheckoutPath,
		"checkout", "--force", "-B", branch, "refs/remotes/origin/"+branch); err != nil {
		return record, err
	}
	inspected, err := catalog.Inspect(record.CheckoutPath)
	if err != nil {
		return record, err
	}
	record.DefaultBranch = branch
	record.HeadCommit = inspected.HeadCommit
	record.State = StateReady
	record.LastError = ""
	record.FailureCount = 0
	record.NextSyncAt = time.Time{}
	record.SyncedAt = time.Now().UTC()
	record.UpdatedAt = record.SyncedAt
	return s.registry.UpsertAcquisition(ctx, record)
}

// Remove unregisters a local repository without touching it. RepoKarta-owned
// checkouts are moved into RepoKarta's trash directory for recoverability.
func (s *Service) Remove(ctx context.Context, id int64) (string, error) {
	s.operation.Lock()
	defer s.operation.Unlock()
	record, err := s.registry.AcquisitionByID(ctx, id)
	if err != nil {
		return "", err
	}
	removedPath := ""
	if record.Owned {
		if _, statError := os.Stat(record.CheckoutPath); errors.Is(statError, os.ErrNotExist) {
			if err := s.validateOwnedTarget(record.CheckoutPath); err != nil {
				return "", err
			}
		} else if statError != nil {
			return "", statError
		} else {
			if err := s.validateOwnedCheckout(record); err != nil {
				return "", err
			}
			suffix, err := randomSuffix()
			if err != nil {
				return "", err
			}
			removedPath = filepath.Join(s.trashRoot, fmt.Sprintf("%d-%s-%s", record.ID, safeSegment(record.Name), suffix))
			if err := s.validateTrashTarget(removedPath); err != nil {
				return "", err
			}
			if err := os.Rename(record.CheckoutPath, removedPath); err != nil {
				return "", fmt.Errorf("move owned checkout to trash: %w", err)
			}
		}
	}
	if err := s.registry.DeleteAcquisition(ctx, record.ID); err != nil {
		if removedPath != "" {
			_ = os.Rename(removedPath, record.CheckoutPath)
		}
		return "", err
	}
	_ = s.audit(ctx, Event{
		RepositoryID: record.ID,
		CanonicalID:  record.CanonicalID,
		Action:       "remove",
		Outcome:      "success",
		Revision:     record.HeadCommit,
		Detail:       map[bool]string{true: "owned checkout moved to RepoKarta trash", false: "local registration removed"}[record.Owned],
	})
	if err := s.refreshCatalogue(ctx); err != nil {
		return removedPath, fmt.Errorf("repository removed but catalogue refresh failed: %w", err)
	}
	return removedPath, nil
}

// CatalogueRepositories returns verified approved repositories for the normal
// coordinator refresh. Error-state checkouts remain eligible so a failed sync
// never silently removes the last usable revision.
func (s *Service) CatalogueRepositories(ctx context.Context) ([]catalog.Repository, error) {
	records, err := s.registry.ListAcquisitions(ctx)
	if err != nil {
		return nil, err
	}
	repositories := make([]catalog.Repository, 0, len(records))
	for _, record := range records {
		if record.CheckoutPath == "" || record.State == StateAcquiring {
			continue
		}
		repository, err := catalog.Inspect(record.CheckoutPath)
		if err != nil {
			continue
		}
		repositories = append(repositories, repository)
	}
	return repositories, nil
}

// StartScheduledSync runs a single bounded synchronization worker. A zero
// interval disables scheduling while keeping manual synchronization available.
func (s *Service) StartScheduledSync(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				records, err := s.registry.ListAcquisitions(ctx)
				if err != nil {
					continue
				}
				for _, record := range records {
					if ctx.Err() != nil {
						return
					}
					if !record.NextSyncAt.IsZero() && record.NextSyncAt.After(time.Now().UTC()) {
						continue
					}
					_, _ = s.Sync(ctx, record.ID)
				}
			}
		}
	}()
}

func (s *Service) fail(ctx context.Context, record Repository, action string, operationError error) (Repository, error) {
	record.State = StateError
	record.LastError = boundedError(operationError)
	record.FailureCount++
	record.UpdatedAt = time.Now().UTC()
	record.NextSyncAt = record.UpdatedAt.Add(backoff(record.FailureCount))
	updated, updateError := s.registry.UpsertAcquisition(ctx, record)
	if updateError == nil {
		record = updated
	}
	_ = s.audit(ctx, Event{
		RepositoryID: record.ID,
		CanonicalID:  record.CanonicalID,
		Action:       action,
		Outcome:      "error",
		Revision:     record.HeadCommit,
		Detail:       boundedError(operationError),
	})
	if updateError != nil {
		return record, fmt.Errorf("%v; persist acquisition failure: %w", operationError, updateError)
	}
	return record, operationError
}

func (s *Service) refreshCatalogue(ctx context.Context) error {
	s.refreshMu.RLock()
	refresh := s.refresh
	s.refreshMu.RUnlock()
	if refresh == nil {
		return errors.New("catalogue refresher is unavailable")
	}
	return refresh(ctx)
}

func (s *Service) audit(ctx context.Context, event Event) error {
	event.CreatedAt = time.Now().UTC()
	return s.registry.RecordAcquisitionEvent(ctx, event)
}

func (s *Service) runGit(ctx context.Context, arguments ...string) (string, error) {
	if s.gitOverride != nil {
		return s.gitOverride(ctx, arguments...)
	}
	gitContext, cancel := context.WithTimeout(ctx, s.gitTimeout)
	defer cancel()
	command := exec.CommandContext(gitContext, s.gitCommand, arguments...)
	command.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_CONFIG_NOSYSTEM=0",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		message := boundedError(errors.New(strings.TrimSpace(string(output))))
		if message == "" {
			message = err.Error()
		}
		if errors.Is(gitContext.Err(), context.DeadlineExceeded) {
			message = "Git operation timed out"
		}
		return "", fmt.Errorf("git %s: %s", safeGitAction(arguments), message)
	}
	return strings.TrimSpace(string(output)), nil
}

func safeGitAction(arguments []string) string {
	for _, argument := range arguments {
		switch argument {
		case "clone", "fetch", "checkout", "config", "symbolic-ref":
			return argument
		}
	}
	return "operation"
}

func normalizeCandidate(candidate Candidate) (Candidate, error) {
	candidate.Provider = strings.ToLower(strings.TrimSpace(candidate.Provider))
	candidate.Name = strings.TrimSpace(candidate.Name)
	candidate.Namespace = strings.Trim(strings.TrimSpace(candidate.Namespace), "/")
	candidate.DefaultBranch = strings.TrimSpace(candidate.DefaultBranch)
	candidate.Visibility = strings.TrimSpace(candidate.Visibility)
	switch candidate.Provider {
	case ProviderLocal:
		repository, err := catalog.Inspect(candidate.LocalPath)
		if err != nil {
			return Candidate{}, err
		}
		candidate.LocalPath = repository.Path
		candidate.CanonicalID = localCanonicalID(repository.Path)
		candidate.Name = repository.Name
		candidate.RemoteURL = repository.OriginURL
		candidate.DefaultBranch = repository.DefaultRevision
		candidate.Visibility = "local"
	case ProviderGitHub, ProviderGitLab:
		expectedHost := candidate.Provider + ".com"
		parsed, err := urlForRemote(candidate.RemoteURL)
		if err != nil || !strings.EqualFold(parsed.Hostname(), expectedHost) {
			return Candidate{}, fmt.Errorf("%s acquisition requires an HTTPS %s remote", candidate.Provider, expectedHost)
		}
		identity := strings.TrimSuffix(strings.Trim(parsed.Path, "/"), ".git")
		if strings.Count(identity, "/") < 1 || strings.Contains(identity, "..") {
			return Candidate{}, errors.New("repository remote identity is invalid")
		}
		parts := strings.Split(identity, "/")
		candidate.CanonicalID = strings.ToLower(expectedHost + "/" + identity)
		candidate.Name = parts[len(parts)-1]
		candidate.Namespace = strings.Join(parts[:len(parts)-1], "/")
		candidate.RemoteURL = "https://" + expectedHost + "/" + identity + ".git"
	default:
		return Candidate{}, errors.New("repository provider must be local, github, or gitlab")
	}
	if candidate.Name == "" {
		return Candidate{}, errors.New("repository name is required")
	}
	return candidate, nil
}

func urlForRemote(value string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return nil, errors.New("repository remote must be an HTTPS URL without embedded credentials")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("repository remote must not contain a query or fragment")
	}
	return parsed, nil
}

func (s *Service) checkoutPath(candidate Candidate) string {
	segments := strings.Split(strings.TrimPrefix(candidate.CanonicalID, candidate.Provider+".com/"), "/")
	clean := make([]string, 0, len(segments)+1)
	clean = append(clean, candidate.Provider)
	for _, segment := range segments {
		clean = append(clean, safeSegment(segment))
	}
	return filepath.Join(append([]string{s.repositoryRoot}, clean...)...)
}

func safeSegment(value string) string {
	value = strings.TrimSpace(value)
	var builder strings.Builder
	for _, character := range value {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '-' || character == '_' || character == '.' {
			builder.WriteRune(character)
		} else {
			builder.WriteRune('-')
		}
	}
	output := strings.Trim(builder.String(), ".-")
	if output == "" {
		return "repository"
	}
	return output
}

func localCanonicalID(path string) string {
	path = filepath.ToSlash(filepath.Clean(path))
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
	}
	return "local:" + path
}

func canonicalRemoteID(remote string) string {
	parsed, err := urlForRemote(remote)
	if err != nil {
		return ""
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "github.com" && host != "gitlab.com" {
		return ""
	}
	identity := strings.TrimSuffix(strings.Trim(parsed.Path, "/"), ".git")
	if strings.Count(identity, "/") < 1 {
		return ""
	}
	return strings.ToLower(host + "/" + identity)
}

func providerIdentityKey(provider, id string) string {
	return strings.ToLower(strings.TrimSpace(provider)) + ":id:" + strings.TrimSpace(id)
}

func discoveryPolicy(request DiscoverRequest) string {
	return fmt.Sprintf(
		"approved; include_private=%t; include_forks=%t; include_archived=%t; team=%s; topics=%s; allow=%s; deny=%s",
		request.IncludePrivate,
		request.IncludeForks,
		request.IncludeArchived,
		request.Team,
		strings.Join(request.Topics, ","),
		strings.Join(request.Allow, ","),
		strings.Join(request.Deny, ","),
	)
}

func normalizePolicyValues(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	output := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		output = append(output, value)
	}
	return output
}

func validateCredentialRef(value string) error {
	if value == "" {
		return nil
	}
	for index, character := range value {
		if !((character >= 'A' && character <= 'Z') ||
			(character >= 'a' && character <= 'z') ||
			(character >= '0' && character <= '9' && index > 0) ||
			character == '_') {
			return errors.New("credential reference must be an environment variable name")
		}
	}
	return nil
}

func (s *Service) validateOwnedCheckout(record Repository) error {
	if !record.Owned {
		return errors.New("repository is not a RepoKarta-owned checkout")
	}
	if err := s.validateOwnedTarget(record.CheckoutPath); err != nil {
		return err
	}
	expected := s.checkoutPath(Candidate{
		Provider:    record.Provider,
		CanonicalID: record.CanonicalID,
	})
	if !samePath(expected, record.CheckoutPath) {
		return errors.New("owned checkout path does not match its canonical repository identity")
	}
	info, err := os.Stat(record.CheckoutPath)
	if err != nil {
		return fmt.Errorf("inspect owned checkout: %w", err)
	}
	if !info.IsDir() {
		return errors.New("owned checkout path is not a directory")
	}
	return nil
}

func (s *Service) validateOwnedTarget(target string) error {
	return validateContainedPath(s.repositoryRoot, target, false)
}

func (s *Service) validateTrashTarget(target string) error {
	return validateContainedPath(s.trashRoot, target, false)
}

func validateContainedPath(root, target string, allowRoot bool) error {
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return errors.New("repository path escapes RepoKarta-owned storage")
	}
	if !allowRoot && relative == "." {
		return errors.New("repository path cannot be the storage root")
	}
	return nil
}

func samePath(left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func boundedError(err error) string {
	if err == nil {
		return ""
	}
	value := strings.TrimSpace(err.Error())
	if len(value) > maximumErrorText {
		value = value[:maximumErrorText]
	}
	return value
}

func backoff(failures int) time.Duration {
	if failures < 1 {
		return 0
	}
	if failures > 6 {
		failures = 6
	}
	return time.Duration(1<<(failures-1)) * time.Minute
}

func randomSuffix() (string, error) {
	var bytes [6]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}
