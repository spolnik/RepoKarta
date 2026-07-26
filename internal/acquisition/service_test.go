package acquisition

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spolnik/RepoKarta/internal/catalog"
)

type memoryRegistry struct {
	mu           sync.Mutex
	nextID       int64
	repositories []Repository
	catalogue    []catalog.Repository
	events       []Event
}

func (r *memoryRegistry) ListAcquisitions(context.Context) ([]Repository, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Repository(nil), r.repositories...), nil
}

func (r *memoryRegistry) ListRepositories(context.Context) ([]catalog.Repository, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]catalog.Repository(nil), r.catalogue...), nil
}

func (r *memoryRegistry) AcquisitionByID(_ context.Context, id int64) (Repository, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, repository := range r.repositories {
		if repository.ID == id {
			return repository, nil
		}
	}
	return Repository{}, os.ErrNotExist
}

func (r *memoryRegistry) UpsertAcquisition(_ context.Context, repository Repository) (Repository, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if repository.ID == 0 {
		r.nextID++
		repository.ID = r.nextID
		r.repositories = append(r.repositories, repository)
		return repository, nil
	}
	for index := range r.repositories {
		if r.repositories[index].ID == repository.ID {
			r.repositories[index] = repository
			return repository, nil
		}
	}
	return Repository{}, os.ErrNotExist
}

func (r *memoryRegistry) DeleteAcquisition(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for index := range r.repositories {
		if r.repositories[index].ID == id {
			r.repositories = append(r.repositories[:index], r.repositories[index+1:]...)
			return nil
		}
	}
	return os.ErrNotExist
}

func (r *memoryRegistry) RecordAcquisitionEvent(_ context.Context, event Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
	return nil
}

func TestLocalDiscoveryAcquireSyncAndRemoveNeverMutatesSource(t *testing.T) {
	repositoryPath := createGitRepository(t)
	readmePath := filepath.Join(repositoryPath, "README.md")
	original, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatal(err)
	}
	registry := &memoryRegistry{}
	service, err := New(Config{DataDirectory: t.TempDir(), Version: "test"}, registry)
	if err != nil {
		t.Fatal(err)
	}
	refreshes := 0
	service.UseRefresher(func(context.Context) error {
		refreshes++
		return nil
	})

	candidates, err := service.Discover(context.Background(), DiscoverRequest{
		Provider: ProviderLocal,
		Location: filepath.Dir(repositoryPath),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].AlreadyManaged {
		t.Fatalf("local candidates = %#v", candidates)
	}
	acquired, err := service.Acquire(context.Background(), candidates[0], "")
	if err != nil {
		t.Fatal(err)
	}
	if acquired.Owned || acquired.State != StateReady || acquired.HeadCommit == "" || refreshes != 1 {
		t.Fatalf("acquired local repository = %#v, refreshes = %d", acquired, refreshes)
	}
	if _, err := service.Sync(context.Background(), acquired.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Remove(context.Background(), acquired.ID); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatal("local repository was removed:", err)
	}
	if string(after) != string(original) {
		t.Fatalf("local source changed: before %q, after %q", original, after)
	}
	if refreshes != 3 {
		t.Fatalf("refreshes = %d, want 3", refreshes)
	}
}

func TestDiscoveryMarksRepositoriesAlreadyInTheCatalogue(t *testing.T) {
	repositoryPath := createGitRepository(t)
	inspected, err := catalog.Inspect(repositoryPath)
	if err != nil {
		t.Fatal(err)
	}
	registry := &memoryRegistry{catalogue: []catalog.Repository{inspected}}
	service, err := New(Config{DataDirectory: t.TempDir()}, registry)
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := service.Discover(context.Background(), DiscoverRequest{
		Provider: ProviderLocal,
		Location: filepath.Dir(repositoryPath),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || !candidates[0].AlreadyManaged {
		t.Fatalf("catalogue duplicate was not marked: %#v", candidates)
	}
}

func TestGitHubDiscoveryUsesCredentialReferenceAndAppliesPreviewPolicy(t *testing.T) {
	t.Setenv("REPOKARTA_TEST_GITHUB_TOKEN", "secret-token-that-must-not-be-persisted")
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/orgs/acme/teams/platform/repos" {
			t.Fatalf("GitHub path = %q", request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer secret-token-that-must-not-be-persisted" {
			t.Fatalf("GitHub authorization header = %q", request.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(response).Encode([]githubRepository{
			{ID: 42, Name: "public", FullName: "acme/public", CloneURL: "https://github.com/acme/public.git", Visibility: "public", Topics: []string{"go"}},
			{Name: "private-fork", FullName: "acme/private-fork", CloneURL: "https://github.com/acme/private-fork.git", Visibility: "private", Private: true, Fork: true},
			{Name: "archive", FullName: "acme/archive", CloneURL: "https://github.com/acme/archive.git", Visibility: "public", Archived: true},
		})
	}))
	defer server.Close()
	registry := &memoryRegistry{nextID: 1, repositories: []Repository{{
		ID:                   1,
		Provider:             ProviderGitHub,
		ProviderRepositoryID: "42",
		CanonicalID:          "github.com/acme/old-name",
		Name:                 "old-name",
		CheckoutPath:         filepath.Join(t.TempDir(), "old-name"),
		State:                StateReady,
	}}}
	service, err := New(Config{DataDirectory: t.TempDir(), GitHubAPI: server.URL}, registry)
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := service.Discover(context.Background(), DiscoverRequest{
		Provider:      ProviderGitHub,
		Location:      "acme",
		CredentialRef: "REPOKARTA_TEST_GITHUB_TOKEN",
		Team:          "platform",
		Topics:        []string{"go"},
		Allow:         []string{"acme/*"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 3 || candidates[0].CanonicalID != "github.com/acme/archive" {
		t.Fatalf("GitHub candidates = %#v", candidates)
	}
	exclusions := map[string]string{}
	alreadyManaged := map[string]bool{}
	for _, candidate := range candidates {
		if strings.Contains(strings.Join([]string{candidate.CanonicalID, candidate.RemoteURL, candidate.WebURL}, " "), "secret-token") {
			t.Fatal("credential value leaked into discovery candidate")
		}
		exclusions[candidate.Name] = candidate.Exclusion
		alreadyManaged[candidate.Name] = candidate.AlreadyManaged
	}
	if exclusions["public"] != "" ||
		!strings.Contains(exclusions["private-fork"], "fork") ||
		!strings.Contains(exclusions["archive"], "archived") {
		t.Fatalf("GitHub exclusions = %#v", exclusions)
	}
	if !alreadyManaged["public"] {
		t.Fatal("stable GitHub repository ID did not detect a renamed managed repository")
	}
	if !strings.Contains(candidates[0].InclusionPolicy, "team=platform") {
		t.Fatalf("inclusion policy = %q", candidates[0].InclusionPolicy)
	}
}

func TestGitLabProjectDiscoverySupportsNestedNamespaces(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.EscapedPath() != "/projects/platform%2Ftools%2Frepokarta" {
			t.Fatalf("GitLab escaped path = %q", request.URL.EscapedPath())
		}
		_ = json.NewEncoder(response).Encode(gitlabRepository{
			Name:                "repokarta",
			PathWithNamespace:   "platform/tools/repokarta",
			HTTPURLToRepository: "https://gitlab.com/platform/tools/repokarta.git",
			WebURL:              "https://gitlab.com/platform/tools/repokarta",
			DefaultBranch:       "main",
			Visibility:          "private",
		})
	}))
	defer server.Close()
	service, err := New(Config{DataDirectory: t.TempDir(), GitLabAPI: server.URL}, &memoryRegistry{})
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := service.Discover(context.Background(), DiscoverRequest{
		Provider:       ProviderGitLab,
		Location:       "https://gitlab.com/platform/tools/repokarta",
		IncludePrivate: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].CanonicalID != "gitlab.com/platform/tools/repokarta" || candidates[0].Excluded {
		t.Fatalf("GitLab candidates = %#v", candidates)
	}
}

func TestProviderRateLimitIsActionableWithoutPersistingResponseContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Retry-After", "60")
		response.WriteHeader(http.StatusTooManyRequests)
		_, _ = response.Write([]byte(`{"message":"internal provider detail"}`))
	}))
	defer server.Close()
	service, err := New(Config{DataDirectory: t.TempDir(), GitHubAPI: server.URL}, &memoryRegistry{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Discover(context.Background(), DiscoverRequest{
		Provider: ProviderGitHub,
		Location: "acme",
	})
	if err == nil || !strings.Contains(err.Error(), "rate limited") ||
		!strings.Contains(err.Error(), "60") || strings.Contains(err.Error(), "internal provider detail") {
		t.Fatalf("rate limit error = %v", err)
	}
}

func TestFailedLocalSyncPreservesLastVerifiedRevisionAndSchedulesBackoff(t *testing.T) {
	repositoryPath := createGitRepository(t)
	inspected, err := catalog.Inspect(repositoryPath)
	if err != nil {
		t.Fatal(err)
	}
	registry := &memoryRegistry{nextID: 1, repositories: []Repository{{
		ID:           1,
		Provider:     ProviderLocal,
		CanonicalID:  localCanonicalID(repositoryPath),
		Name:         inspected.Name,
		CheckoutPath: repositoryPath,
		State:        StateReady,
		HeadCommit:   inspected.HeadCommit,
	}}}
	service, err := New(Config{DataDirectory: t.TempDir()}, registry)
	if err != nil {
		t.Fatal(err)
	}
	service.UseRefresher(func(context.Context) error { return nil })
	missing := repositoryPath + "-missing"
	if err := os.Rename(repositoryPath, missing); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Rename(missing, repositoryPath) })
	failed, err := service.Sync(context.Background(), 1)
	if err == nil {
		t.Fatal("expected missing local repository sync to fail")
	}
	if failed.State != StateError || failed.HeadCommit != inspected.HeadCommit ||
		failed.FailureCount != 1 || !failed.NextSyncAt.After(time.Now().UTC()) {
		t.Fatalf("failed sync state = %#v", failed)
	}
}

func TestHostedCheckoutLifecycleUsesOwnedStorageAndRecoverableRemoval(t *testing.T) {
	source := createGitRepository(t)
	t.Setenv("GITHUB_TOKEN", "hosted-lifecycle-token")
	registry := &memoryRegistry{}
	dataDirectory := t.TempDir()
	service, err := New(Config{DataDirectory: dataDirectory}, registry)
	if err != nil {
		t.Fatal(err)
	}
	service.UseRefresher(func(context.Context) error { return nil })
	service.gitOverride = hostedGitOverride(source, "https://github.com/acme/example.git")
	acquired, err := service.Acquire(context.Background(), Candidate{
		Provider:      ProviderGitHub,
		RemoteURL:     "https://github.com/acme/example.git",
		DefaultBranch: "main",
		Visibility:    "private",
	}, "GITHUB_TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	if !acquired.Owned || acquired.State != StateReady {
		t.Fatalf("hosted acquisition = %#v", acquired)
	}
	if err := service.validateOwnedTarget(acquired.CheckoutPath); err != nil {
		t.Fatalf("hosted checkout path = %q: %v", acquired.CheckoutPath, err)
	}
	if _, err := os.Stat(filepath.Join(acquired.CheckoutPath, "README.md")); err != nil {
		t.Fatal(err)
	}
	runGit(t, acquired.CheckoutPath, "remote", "set-url", "origin", "https://github.com/other/repository.git")
	failedSync, err := service.Sync(context.Background(), acquired.ID)
	if err == nil || !strings.Contains(err.Error(), "approved canonical") ||
		failedSync.HeadCommit != acquired.HeadCommit {
		t.Fatalf("tampered remote sync = %#v, error = %v", failedSync, err)
	}
	runGit(t, acquired.CheckoutPath, "remote", "set-url", "origin", "https://github.com/acme/example.git")

	if err := os.WriteFile(filepath.Join(source, "SECOND.md"), []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "add", "SECOND.md")
	runGit(t, source, "commit", "-m", "Second commit")
	synced, err := service.Sync(context.Background(), acquired.ID)
	if err != nil {
		t.Fatal(err)
	}
	if synced.HeadCommit == acquired.HeadCommit {
		t.Fatal("hosted sync did not advance the verified revision")
	}
	removedPath, err := service.Remove(context.Background(), acquired.ID)
	if err != nil {
		t.Fatal(err)
	}
	if removedPath == "" {
		t.Fatal("owned checkout was not moved to trash")
	}
	if _, err := os.Stat(acquired.CheckoutPath); !os.IsNotExist(err) {
		t.Fatalf("owned checkout still exists at original path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(removedPath, "SECOND.md")); err != nil {
		t.Fatalf("recoverable checkout is unavailable: %v", err)
	}
	if _, err := os.Stat(filepath.Join(source, "SECOND.md")); err != nil {
		t.Fatalf("upstream source was changed or removed: %v", err)
	}
}

func TestFailedHostedCloneCanBeRetriedBySynchronization(t *testing.T) {
	source := createGitRepository(t)
	t.Setenv("GITLAB_TOKEN", "hosted-retry-token")
	service, err := New(Config{DataDirectory: t.TempDir()}, &memoryRegistry{})
	if err != nil {
		t.Fatal(err)
	}
	service.UseRefresher(func(context.Context) error { return nil })
	service.gitOverride = func(context.Context, ...string) (string, error) {
		return "", errors.New("credential helper unavailable")
	}
	failed, err := service.Acquire(context.Background(), Candidate{
		Provider:      ProviderGitLab,
		RemoteURL:     "https://gitlab.com/acme/example.git",
		DefaultBranch: "main",
	}, "GITLAB_TOKEN")
	if err == nil || failed.ID <= 0 || failed.State != StateError {
		t.Fatalf("failed acquisition = %#v, error = %v", failed, err)
	}
	service.gitOverride = hostedGitOverride(source, "https://gitlab.com/acme/example.git")
	retried, err := service.Sync(context.Background(), failed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retried.State != StateReady || retried.HeadCommit == "" || retried.FailureCount != 0 {
		t.Fatalf("retried acquisition = %#v", retried)
	}
}

func TestHostedGitOperationsReceiveCredentialWithoutCommandArgument(t *testing.T) {
	source := createGitRepository(t)
	t.Setenv("REPOKARTA_PRIVATE_TOKEN", "private-token-value")
	service, err := New(Config{DataDirectory: t.TempDir()}, &memoryRegistry{})
	if err != nil {
		t.Fatal(err)
	}
	service.UseRefresher(func(context.Context) error { return nil })
	runner := hostedGitOverride(source, "https://github.com/acme/private.git")
	var cloneEnvironment map[string]string
	var cloneArguments []string
	service.gitEnvironmentOverride = func(ctx context.Context, environment map[string]string, arguments ...string) (string, error) {
		if safeGitAction(arguments) == "clone" {
			cloneEnvironment = make(map[string]string, len(environment))
			for key, value := range environment {
				cloneEnvironment[key] = value
			}
			cloneArguments = append([]string(nil), arguments...)
		}
		return runner(ctx, arguments...)
	}
	if _, err := service.Acquire(context.Background(), Candidate{
		Provider:      ProviderGitHub,
		RemoteURL:     "https://github.com/acme/private.git",
		DefaultBranch: "main",
		Visibility:    "private",
	}, "REPOKARTA_PRIVATE_TOKEN"); err != nil {
		t.Fatal(err)
	}
	expected := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:private-token-value"))
	if cloneEnvironment["GIT_CONFIG_VALUE_0"] != expected ||
		cloneEnvironment["GIT_CONFIG_KEY_0"] != "http.https://github.com/.extraHeader" {
		t.Fatalf("clone credential environment = %#v", cloneEnvironment)
	}
	for _, argument := range cloneArguments {
		if strings.Contains(argument, "private-token-value") || strings.Contains(argument, expected) {
			t.Fatalf("credential leaked into Git argument %q", argument)
		}
	}
}

func TestConfiguredHostedOriginSupportsEnterpriseGitServers(t *testing.T) {
	source := createGitRepository(t)
	service, err := New(Config{
		DataDirectory: t.TempDir(),
		GitHubHost:    "https://git.example.com",
	}, &memoryRegistry{})
	if err != nil {
		t.Fatal(err)
	}
	service.UseRefresher(func(context.Context) error { return nil })
	service.gitOverride = hostedGitOverride(source, "https://git.example.com/acme/service.git")
	acquired, err := service.Acquire(context.Background(), Candidate{
		Provider:      ProviderGitHub,
		RemoteURL:     "https://git.example.com/acme/service.git",
		DefaultBranch: "main",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if acquired.CanonicalID != "git.example.com/acme/service" ||
		acquired.RemoteURL != "https://git.example.com/acme/service.git" {
		t.Fatalf("enterprise acquisition = %#v", acquired)
	}
	if _, err := normalizeCandidateForHosts(Candidate{
		Provider:  ProviderGitHub,
		RemoteURL: "https://github.com/acme/service.git",
	}, service.githubHost, service.gitlabHost); err == nil {
		t.Fatal("public GitHub remote was accepted for an enterprise-only GitHub host")
	}
}

func TestOwnedCheckoutRejectsSymlinkOrJunctionReplacement(t *testing.T) {
	source := createGitRepository(t)
	service, err := New(Config{DataDirectory: t.TempDir()}, &memoryRegistry{})
	if err != nil {
		t.Fatal(err)
	}
	service.UseRefresher(func(context.Context) error { return nil })
	service.gitOverride = hostedGitOverride(source, "https://github.com/acme/example.git")
	acquired, err := service.Acquire(context.Background(), Candidate{
		Provider:      ProviderGitHub,
		RemoteURL:     "https://github.com/acme/example.git",
		DefaultBranch: "main",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(acquired.CheckoutPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(source, acquired.CheckoutPath); err != nil {
		t.Skipf("directory symlinks are unavailable: %v", err)
	}
	if _, err := service.Sync(context.Background(), acquired.ID); err == nil ||
		!strings.Contains(err.Error(), "symbolic link or junction") {
		t.Fatalf("symlinked checkout sync error = %v", err)
	}
}

func TestNormalizeHostedCandidateRejectsCredentialBearingAndForeignRemotes(t *testing.T) {
	for _, remote := range []string{
		"https://token@github.com/acme/repo.git",
		"https://gitlab.com/acme/repo.git",
		"file:///tmp/repo.git",
	} {
		if _, err := normalizeCandidate(Candidate{
			Provider:  ProviderGitHub,
			RemoteURL: remote,
		}); err == nil {
			t.Fatalf("remote %q was accepted", remote)
		}
	}
}

func createGitRepository(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repositoryPath := filepath.Join(t.TempDir(), "example")
	runGit(t, "", "init", repositoryPath)
	runGit(t, repositoryPath, "branch", "-M", "main")
	runGit(t, repositoryPath, "config", "user.email", "repokarta@example.test")
	runGit(t, repositoryPath, "config", "user.name", "RepoKarta tests")
	if err := os.WriteFile(filepath.Join(repositoryPath, "README.md"), []byte("read only\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repositoryPath, "add", "README.md")
	runGit(t, repositoryPath, "commit", "-m", "Initial commit")
	return repositoryPath
}

func hostedGitOverride(source, remote string) func(context.Context, ...string) (string, error) {
	return func(ctx context.Context, arguments ...string) (string, error) {
		action := safeGitAction(arguments)
		if action == "clone" {
			destination := arguments[len(arguments)-1]
			command := exec.CommandContext(ctx, "git", "clone", "--branch", "main", "--", source, destination)
			output, err := command.CombinedOutput()
			if err != nil {
				return strings.TrimSpace(string(output)), err
			}
			command = exec.CommandContext(ctx, "git", "-C", destination, "remote", "set-url", "origin", remote)
			output, err = command.CombinedOutput()
			return strings.TrimSpace(string(output)), err
		}
		if action == "fetch" {
			checkout := ""
			for index := 0; index+1 < len(arguments); index++ {
				if arguments[index] == "-C" {
					checkout = arguments[index+1]
					break
				}
			}
			command := exec.CommandContext(ctx, "git", "-C", checkout, "fetch", "--prune", source,
				"+refs/heads/main:refs/remotes/origin/main")
			output, err := command.CombinedOutput()
			return strings.TrimSpace(string(output)), err
		}
		command := exec.CommandContext(ctx, "git", arguments...)
		output, err := command.CombinedOutput()
		return strings.TrimSpace(string(output)), err
	}
}

func runGit(t *testing.T, directory string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", arguments...)
	if directory != "" {
		command.Dir = directory
	}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
}
