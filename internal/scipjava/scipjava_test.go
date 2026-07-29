package scipjava

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
	runs    atomic.Int32
}

type failNextReadArtifacts struct {
	store *scipindex.Store
	err   error
}

func (artifacts *failNextReadArtifacts) Import(
	ctx context.Context,
	repositoryID int64,
	revision, sourceRoot string,
	reader io.Reader,
) (scipindex.ImportSummary, error) {
	return artifacts.store.Import(ctx, repositoryID, revision, sourceRoot, reader)
}

func (artifacts *failNextReadArtifacts) Read(
	ctx context.Context,
	repositoryID int64,
	revision string,
) (scipindex.Artifact, bool, error) {
	if artifacts.err != nil {
		err := artifacts.err
		artifacts.err = nil
		return scipindex.Artifact{}, false, err
	}
	return artifacts.store.Read(ctx, repositoryID, revision)
}

func (executor *fixtureExecutor) Verify(context.Context, string) (string, error) {
	return executor.version, nil
}

func (executor *fixtureExecutor) Run(_ context.Context, _ string, directory string, _ []string) ([]byte, error) {
	executor.runs.Add(1)
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
	var firstFinishedAt time.Time
	for {
		status := repositories.status()
		if status != nil && status.State == "ready" {
			if status.Revision != revision || status.Documents != 1 ||
				status.Symbols != 1 || status.Occurrences != 1 ||
				status.Configuration == "" {
				t.Fatalf("ready status = %#v", status)
			}
			firstFinishedAt = status.FinishedAt
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
	if executor.runs.Load() != 1 {
		t.Fatalf("indexer runs = %d, want 1", executor.runs.Load())
	}
	if _, err := os.Stat(filepath.Join(repositoryPath, "index.scip")); !os.IsNotExist(err) {
		t.Fatalf("source worktree was modified: %v", err)
	}
	service.Queue(7)
	time.Sleep(100 * time.Millisecond)
	if executor.runs.Load() != 1 {
		t.Fatalf("unchanged exact commit was regenerated %d times", executor.runs.Load())
	}

	service.artifacts = &failNextReadArtifacts{
		store: artifacts,
		err:   errors.New("SCIP artifact identity does not match its requested repository revision"),
	}
	service.Queue(7)
	deadline = time.Now().Add(10 * time.Second)
	for {
		status := repositories.status()
		if executor.runs.Load() == 2 && status != nil && status.State == "ready" &&
			status.FinishedAt.After(firstFinishedAt) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf(
				"stale ready artifact was not regenerated; indexer runs = %d, status = %#v",
				executor.runs.Load(),
				status,
			)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestNewRemovesAbandonedSCIPWorktreeDirectories(t *testing.T) {
	dataDirectory := t.TempDir()
	source := t.TempDir()
	runGitTest(t, source, "init", "-q")
	runGitTest(t, source, "config", "user.name", "RepoKarta Test")
	runGitTest(t, source, "config", "user.email", "test@repokarta.local")
	if err := os.WriteFile(filepath.Join(source, "build.gradle"), []byte("plugins { id 'java' }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, source, "add", ".")
	runGitTest(t, source, "commit", "-qm", "fixture")
	shadow := filepath.Join(dataDirectory, "git-shadow", "repository-7.git")
	if err := os.MkdirAll(shadow, 0o700); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, shadow, "init", "--bare", "-q")
	runGitTest(t, shadow, "fetch", "--force", source, "HEAD:refs/heads/main")
	orphan := filepath.Join(dataDirectory, "worktrees", "7-abandoned", "checkout")
	runGitTest(t, shadow, "worktree", "add", "--detach", orphan, "refs/heads/main")
	if err := os.WriteFile(filepath.Join(orphan, "index.scip"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	artifacts, err := scipindex.New(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(
		Config{Mode: ModeOff, DataDirectory: dataDirectory},
		&memoryRepositoryStore{},
		artifacts,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(orphan)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("abandoned worktree directory was retained: %v", err)
	}
	if listed := runGitTest(t, shadow, "worktree", "list", "--porcelain"); strings.Contains(listed, filepath.ToSlash(orphan)) ||
		strings.Contains(listed, orphan) {
		t.Fatalf("abandoned worktree registration was retained: %s", listed)
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

func TestInspectGradleRepositoryReadsExactWrapperAndToolchain(t *testing.T) {
	repositoryPath := t.TempDir()
	runGitTest(t, repositoryPath, "init", "-q")
	runGitTest(t, repositoryPath, "config", "user.name", "RepoKarta Test")
	runGitTest(t, repositoryPath, "config", "user.email", "test@repokarta.local")
	for _, directory := range []string{
		filepath.Join(repositoryPath, "gradle", "wrapper"),
		filepath.Join(repositoryPath, "src", "main", "java"),
	} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"settings.gradle.kts": `rootProject.name = "legacy"`,
		"build.gradle.kts": `
plugins { java }
java {
    toolchain {
        languageVersion = JavaLanguageVersion.of(17)
    }
}`,
		filepath.Join("gradle", "wrapper", "gradle-wrapper.properties"): "distributionUrl=https\\://services.gradle.org/distributions/gradle-6.9.4-bin.zip\n",
		filepath.Join("src", "main", "java", "Legacy.java"):             "class Legacy {}\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(repositoryPath, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGitTest(t, repositoryPath, "add", ".")
	runGitTest(t, repositoryPath, "commit", "-qm", "fixture")
	revision := strings.TrimSpace(runGitTest(t, repositoryPath, "rev-parse", "HEAD"))
	build, applicable, reason, err := inspectGradleRepository(
		context.Background(),
		catalog.Repository{Path: repositoryPath},
		revision,
	)
	if err != nil || !applicable || reason != "" ||
		build.Root != "" || build.GradleVersion != "6.9.4" ||
		build.ToolchainVersion != 17 {
		t.Fatalf("inspection = %#v, %v, %q, %v", build, applicable, reason, err)
	}

	if err := os.WriteFile(
		filepath.Join(repositoryPath, "gradle", "gradle-daemon-jvm.properties"),
		[]byte("toolchainVersion=11\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repositoryPath, "add", ".")
	runGitTest(t, repositoryPath, "commit", "-qm", "pin daemon JVM")
	revision = strings.TrimSpace(runGitTest(t, repositoryPath, "rev-parse", "HEAD"))
	build, applicable, reason, err = inspectGradleRepository(
		context.Background(),
		catalog.Repository{Path: repositoryPath},
		revision,
	)
	if err != nil || !applicable || reason != "" || build.ToolchainVersion != 11 {
		t.Fatalf("daemon JVM inspection = %#v, %v, %q, %v", build, applicable, reason, err)
	}
}

func TestSelectJDKUsesToolchainOrLegacyCompatibleLauncher(t *testing.T) {
	service := &Service{
		jdks: []jdkInstallation{
			{Home: "jdk-11", Major: 11, Source: "configured"},
			{Home: "jdk-17", Major: 17, Source: "configured"},
			{Home: "jdk-21", Major: 21, Source: "configured"},
		},
		inherited: &jdkInstallation{Home: "jdk-25", Major: 25, Source: "inherited"},
	}
	selection, err := service.selectJDK(gradleBuild{
		GradleVersion: "8.7", ToolchainVersion: 17,
	})
	if err != nil || selection.Major != 17 || selection.Source != "toolchain" {
		t.Fatalf("modern selection = %#v, %v", selection, err)
	}
	selection, err = service.selectJDK(gradleBuild{
		GradleVersion: "8.4", ToolchainVersion: 21,
	})
	if err != nil || selection.Major != 17 || selection.Source != "compatible-configured" {
		t.Fatalf("legacy selection = %#v, %v", selection, err)
	}

	service.jdks = []jdkInstallation{{Home: "jdk-25", Major: 25, Source: "override"}}
	service.inherited = nil
	selection, err = service.selectJDK(gradleBuild{GradleVersion: "6.9.4"})
	var classified *failure
	if !errors.As(err, &classified) ||
		classified.Category != FailureJDKIncompatibleWrapper ||
		selection.Major != 25 {
		t.Fatalf("incompatible override = %#v, %T %v", selection, err, err)
	}
}

func TestClassifyBuildFailureBucketsActionableCauses(t *testing.T) {
	tests := []struct {
		name     string
		output   string
		category string
	}{
		{
			name: "docker", output: "Cannot connect to the Docker daemon",
			category: FailureEnvironment,
		},
		{
			name: "wrapper", output: "Unsupported class file major version 65",
			category: FailureJDKIncompatibleWrapper,
		},
		{
			name: "compile", output: "Execution failed for task ':compileJava'. symbol not found",
			category: FailureCompileError,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := classifyBuildFailure(errors.New("exit status 1"), test.output)
			category, summary := classifyFailure(err)
			if category != test.category || summary == "" {
				t.Fatalf("classification = %q, %q, %v", category, summary, err)
			}
		})
	}
}

func TestParseJDKHomes(t *testing.T) {
	homes, err := ParseJDKHomes(`8=C:\Java\8,17=C:\Java\17`)
	if err != nil || homes[8] != `C:\Java\8` || homes[17] != `C:\Java\17` {
		t.Fatalf("JDK homes = %#v, %v", homes, err)
	}
	if _, err := ParseJDKHomes(`17=C:\one,17=C:\two`); err == nil {
		t.Fatal("duplicate Java version was accepted")
	}
}

func TestGradleJavaRuntimeCompatibilityBoundaries(t *testing.T) {
	tests := []struct {
		gradle string
		java   int
		want   bool
	}{
		{gradle: "6.9.4", java: 11, want: true},
		{gradle: "6.9.4", java: 17, want: false},
		{gradle: "8.5", java: 21, want: true},
		{gradle: "8.4", java: 21, want: false},
		{gradle: "9.1.0", java: 25, want: true},
		{gradle: "9.1.0", java: 11, want: false},
	}
	for _, test := range tests {
		if got := gradleSupportsJavaRuntime(test.gradle, test.java); got != test.want {
			t.Errorf("Gradle %s with Java %d = %v, want %v", test.gradle, test.java, got, test.want)
		}
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
	if !status.Enabled || status.Available || status.Error == "" ||
		status.FailureCategory != FailureEnvironment ||
		status.FailureSummary == "" {
		t.Fatalf("provider status = %#v", status)
	}
}

func TestBuildEnvironmentSelectsLauncherAndAdvertisesToolchains(t *testing.T) {
	service := &Service{jdks: []jdkInstallation{
		{Home: filepath.Join("C:", "Java", "11"), Major: 11, Source: "configured"},
		{Home: filepath.Join("C:", "Java", "17"), Major: 17, Source: "configured"},
	}}
	environment := service.buildEnvironment(jdkSelection{
		Home: service.jdks[0].Home, Major: 11, Source: "compatible-configured",
	})
	if got := environmentValue(environment, "JAVA_HOME"); got != service.jdks[0].Home {
		t.Fatalf("JAVA_HOME = %q", got)
	}
	if got := environmentValue(environment, "PATH"); !strings.HasPrefix(got, javaBin(service.jdks[0].Home)) {
		t.Fatalf("PATH = %q", got)
	}
	gradleOptions := environmentValue(environment, "GRADLE_OPTS")
	if !strings.Contains(gradleOptions, "org.gradle.java.installations.paths=") ||
		!strings.Contains(gradleOptions, service.jdks[0].Home) ||
		!strings.Contains(gradleOptions, service.jdks[1].Home) {
		t.Fatalf("GRADLE_OPTS = %q", gradleOptions)
	}
}

func TestRecordFailurePersistsClassificationAndRuntime(t *testing.T) {
	repositories := &memoryRepositoryStore{repository: catalog.Repository{
		ID: 7, Path: t.TempDir(),
	}}
	service := &Service{
		config: Config{DataDirectory: t.TempDir()},
		store:  repositories,
		provider: ProviderStatus{
			Configuration: "fixture", Version: "v-test",
		},
	}
	cause := classifyBuildFailure(
		errors.New("scip-java index failed"),
		"Unsupported class file major version 65",
	)
	err := service.recordFailure(
		context.Background(),
		repositories.repository,
		"abc123",
		gradleBuild{GradleVersion: "8.4", ToolchainVersion: 21},
		jdkSelection{Major: 17, Source: "compatible-configured"},
		true,
		cause,
	)
	status := repositories.status()
	if err == nil || status == nil ||
		status.FailureCategory != FailureJDKIncompatibleWrapper ||
		status.FailureSummary == "" ||
		status.GradleVersion != "8.4" ||
		status.RequestedJDKVersion != 21 ||
		status.JDKVersion != 17 ||
		status.JDKSource != "compatible-configured" {
		t.Fatalf("failure status = %#v, error = %v", status, err)
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
