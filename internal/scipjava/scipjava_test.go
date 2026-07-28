package scipjava

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scip-code/scip/bindings/go/scip"
	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/scipindex"
	"google.golang.org/protobuf/proto"
)

type memoryRepositoryStore struct {
	mu         sync.Mutex
	repository catalog.Repository
}

func (store *memoryRepositoryStore) RepositoryByID(_ context.Context, id int64) (catalog.Repository, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.repository, nil
}

func (store *memoryRepositoryStore) UpdateSCIPIndexStatus(
	_ context.Context,
	id int64,
	status catalog.SCIPIndexStatus,
) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	copy := status
	store.repository.SCIPJava = &copy
	return nil
}

func (store *memoryRepositoryStore) status() *catalog.SCIPIndexStatus {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.repository.SCIPJava == nil {
		return nil
	}
	copy := *store.repository.SCIPJava
	return &copy
}

type fixtureExecutor struct {
	version string
	runs    int
}

func (executor *fixtureExecutor) Verify(context.Context, string) (string, error) {
	return executor.version, nil
}

func (executor *fixtureExecutor) Run(_ context.Context, _ string, directory string) ([]byte, error) {
	executor.runs++
	symbol := "scip-java maven com.acme:payments 1.0.0 com/acme/PaymentService#run()."
	occurrence := &scip.Occurrence{Symbol: symbol, SymbolRoles: int32(scip.SymbolRole_ReadAccess)}
	occurrence.SetSourceRange(scip.Range{
		Start: scip.Position{Line: 4, Character: 2},
		End:   scip.Position{Line: 4, Character: 5},
	})
	index := &scip.Index{
		Metadata: &scip.Metadata{ToolInfo: &scip.ToolInfo{Name: "scip-java", Version: executor.version}},
		Documents: []*scip.Document{{
			RelativePath: "src/main/java/com/acme/PaymentService.java",
			Language:     "java",
			Symbols: []*scip.SymbolInformation{{
				Symbol: symbol, DisplayName: "run", Kind: scip.SymbolInformation_Method,
			}},
			Occurrences: []*scip.Occurrence{occurrence},
		}},
	}
	content, err := proto.MarshalOptions{Deterministic: true}.Marshal(index)
	if err != nil {
		return nil, err
	}
	return nil, os.WriteFile(filepath.Join(directory, "index.scip"), content, 0o600)
}

func TestServiceGeneratesAndImportsExactCommit(t *testing.T) {
	repositoryPath := t.TempDir()
	runGitTest(t, repositoryPath, "init", "-q")
	runGitTest(t, repositoryPath, "config", "user.name", "RepoKarta Test")
	runGitTest(t, repositoryPath, "config", "user.email", "test@repokarta.local")
	if err := os.MkdirAll(filepath.Join(repositoryPath, "src", "main", "java", "com", "acme"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repositoryPath, "settings.gradle.kts"), []byte(`rootProject.name = "payments"`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repositoryPath, "build.gradle.kts"), []byte("plugins { java }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(repositoryPath, "src", "main", "java", "com", "acme", "PaymentService.java"),
		[]byte("package com.acme;\nclass PaymentService { void run() {} }\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repositoryPath, "add", ".")
	runGitTest(t, repositoryPath, "commit", "-qm", "fixture")
	revision := strings.TrimSpace(runGitTest(t, repositoryPath, "rev-parse", "HEAD"))

	command := filepath.Join(t.TempDir(), "scip-java-fixture")
	if err := os.WriteFile(command, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	repositories := &memoryRepositoryStore{repository: catalog.Repository{
		ID: 7, Name: "payments", Path: repositoryPath,
		HeadCommit: revision, IndexedCommit: revision,
		ScanState: "ready", IndexState: "ready",
	}}
	artifacts, err := scipindex.New(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	executor := &fixtureExecutor{version: "v-test"}
	service, err := New(Config{
		Mode: ModeRequired, Command: command, DataDirectory: t.TempDir(),
		Timeout: time.Minute, Concurrency: 1, executor: executor,
	}, repositories, artifacts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service.Start(ctx)
	service.Queue(7)

	deadline := time.Now().Add(10 * time.Second)
	for {
		status := repositories.status()
		if status != nil && status.State == "ready" {
			if status.Revision != revision || status.Documents != 1 ||
				status.Symbols != 1 || status.Occurrences != 1 ||
				status.Configuration == "" {
				t.Fatalf("ready status = %#v", status)
			}
			break
		}
		if status != nil && status.State == "failed" {
			t.Fatalf("Java SCIP failed: %#v", status)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out with status %#v", status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	artifact, ok, err := artifacts.Read(context.Background(), 7, revision)
	if err != nil || !ok || len(artifact.Documents) != 1 ||
		artifact.Documents[0].Path != "src/main/java/com/acme/PaymentService.java" {
		t.Fatalf("artifact = %#v, %v, %v", artifact, ok, err)
	}
	if executor.runs != 1 {
		t.Fatalf("indexer runs = %d, want 1", executor.runs)
	}
	if _, err := os.Stat(filepath.Join(repositoryPath, "index.scip")); !os.IsNotExist(err) {
		t.Fatalf("source worktree was modified: %v", err)
	}
	service.Queue(7)
	time.Sleep(100 * time.Millisecond)
	if executor.runs != 1 {
		t.Fatalf("unchanged exact commit was regenerated %d times", executor.runs)
	}
}

func TestInspectGradleRepositoryExplainsIneligibleRepositories(t *testing.T) {
	repositoryPath := t.TempDir()
	runGitTest(t, repositoryPath, "init", "-q")
	runGitTest(t, repositoryPath, "config", "user.name", "RepoKarta Test")
	runGitTest(t, repositoryPath, "config", "user.email", "test@repokarta.local")
	if err := os.WriteFile(filepath.Join(repositoryPath, "README.md"), []byte("# fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repositoryPath, "add", ".")
	runGitTest(t, repositoryPath, "commit", "-qm", "fixture")
	revision := strings.TrimSpace(runGitTest(t, repositoryPath, "rev-parse", "HEAD"))
	_, applicable, reason, err := inspectGradleRepository(
		context.Background(),
		catalog.Repository{Path: repositoryPath},
		revision,
	)
	if err != nil || applicable || reason != "No committed Java sources." {
		t.Fatalf("inspection = %v, %q, %v", applicable, reason, err)
	}
}

func TestAutoModeReportsMissingCommandWithoutFailingStartup(t *testing.T) {
	command := filepath.Join(t.TempDir(), "missing-scip-java")
	artifacts, err := scipindex.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(Config{
		Mode: ModeAuto, Command: command, DataDirectory: t.TempDir(),
	}, &memoryRepositoryStore{}, artifacts)
	if err != nil {
		t.Fatal(err)
	}
	status := service.ProviderStatus()
	if !status.Enabled || status.Available || status.Error == "" {
		t.Fatalf("provider status = %#v", status)
	}
}

func TestRequiredModeRejectsMissingCommand(t *testing.T) {
	command := filepath.Join(t.TempDir(), "missing-scip-java")
	artifacts, err := scipindex.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = New(Config{
		Mode: ModeRequired, Command: command, DataDirectory: t.TempDir(),
	}, &memoryRepositoryStore{}, artifacts)
	if err == nil || !strings.Contains(err.Error(), "resolve required scip-java command") {
		t.Fatalf("required missing command error = %v", err)
	}
}

func runGitTest(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, arguments...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
	return string(output)
}
