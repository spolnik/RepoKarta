// Package scipjava schedules compiler-precise Java SCIP generation after
// ordinary repository indexing. It executes only when explicitly enabled and
// never turns a build-tool failure into a source-index failure.
package scipjava

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/scipindex"
	"github.com/spolnik/RepoKarta/internal/telemetry"
)

const (
	ModeOff      = "off"
	ModeAuto     = "auto"
	ModeRequired = "required"

	FailureEnvironment            = "environment"
	FailureJDKIncompatibleWrapper = "jdk_incompatible_wrapper"
	FailureCompileError           = "compile_error"

	DefaultTimeout             = 20 * time.Minute
	DefaultConcurrency         = 1
	MaximumConcurrency         = 4
	maximumGitOutput           = 16 << 20
	maximumBuildOutput         = 1 << 20
	maximumStatusError         = 4 << 10
	maximumGradleMetadataFiles = 256
	maximumGradleMetadataBytes = 1 << 20
)

// Config controls the optional external compiler indexer.
type Config struct {
	Mode          string
	Command       string
	DataDirectory string
	Timeout       time.Duration
	Concurrency   int
	JDKHome       string
	JDKHomes      map[int]string

	executor commandExecutor
}

// ProviderStatus describes whether the configured producer can run.
type ProviderStatus struct {
	Mode            string `json:"mode"`
	Enabled         bool   `json:"enabled"`
	Available       bool   `json:"available"`
	Command         string `json:"command,omitempty"`
	Version         string `json:"version,omitempty"`
	Configuration   string `json:"configuration,omitempty"`
	JDKVersions     []int  `json:"jdk_versions,omitempty"`
	FailureCategory string `json:"failure_category,omitempty"`
	FailureSummary  string `json:"failure_summary,omitempty"`
	Error           string `json:"error,omitempty"`
}

// RepositoryStore is the durable state surface required by the scheduler.
type RepositoryStore interface {
	RepositoryByID(context.Context, int64) (catalog.Repository, error)
	UpdateSCIPIndexStatus(context.Context, int64, catalog.SCIPIndexStatus) error
}

// importer matches scipindex.Store without weakening its io.Reader contract.
type importer interface {
	Import(context.Context, int64, string, string, io.Reader) (scipindex.ImportSummary, error)
	Read(context.Context, int64, string) (scipindex.Artifact, bool, error)
}

type commandExecutor interface {
	Verify(context.Context, string) (string, error)
	Run(context.Context, string, string, []string) ([]byte, error)
}

type osCommandExecutor struct{}

func (osCommandExecutor) Verify(ctx context.Context, command string) (string, error) {
	output, err := exec.CommandContext(ctx, command, "--version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("run --version: %w", err)
	}
	return boundedOneLine(output), nil
}

func (osCommandExecutor) Run(ctx context.Context, command, directory string, environment []string) ([]byte, error) {
	process := exec.CommandContext(ctx, command, "index")
	process.Dir = directory
	process.Env = environment
	var output cappedBuffer
	output.maximum = maximumBuildOutput
	process.Stdout = &output
	process.Stderr = &output
	err := process.Run()
	return output.Bytes(), err
}

type gradleBuild struct {
	Root             string
	GradleVersion    string
	ToolchainVersion int
}

type jdkInstallation struct {
	Home   string
	Major  int
	Source string
}

type jdkSelection struct {
	Home             string
	Major            int
	Source           string
	RequestedVersion int
	GradleVersion    string
}

type failure struct {
	Category string
	Summary  string
	Cause    error
}

func (value *failure) Error() string {
	return value.Cause.Error()
}

func (value *failure) Unwrap() error {
	return value.Cause
}

var (
	gradleDistributionPattern = regexp.MustCompile(`(?i)gradle-([0-9]+(?:\.[0-9]+){1,2})-(?:bin|all)\.zip`)
	javaToolchainPatterns     = []*regexp.Regexp{
		regexp.MustCompile(`(?i)JavaLanguageVersion\.of\(\s*["']?([0-9]+)["']?\s*\)`),
		regexp.MustCompile(`(?i)\bjvmToolchain\(\s*([0-9]+)\s*\)`),
		regexp.MustCompile(`(?m)^\s*toolchainVersion\s*=\s*([0-9]+)\s*$`),
	}
)

// Service owns a lossless, deduplicated queue independent of structural-map
// concurrency. One worker is the default because each item may compile a full
// Gradle build.
type Service struct {
	config    Config
	store     RepositoryStore
	artifacts importer
	provider  ProviderStatus
	executor  commandExecutor
	jdks      []jdkInstallation
	inherited *jdkInstallation

	startOnce sync.Once
	baseCtxMu sync.RWMutex
	baseCtx   context.Context
	signal    chan struct{}
	mu        sync.Mutex
	pending   []int64
	queued    map[int64]struct{}
	running   map[int64]struct{}
	rerun     map[int64]struct{}
}

// New resolves and verifies the configured scip-java producer. Auto mode keeps
// startup healthy when the command is absent; required mode fails closed.
func New(config Config, store RepositoryStore, artifacts importer) (*Service, error) {
	if store == nil {
		return nil, errors.New("Java SCIP repository store is required")
	}
	if artifacts == nil {
		return nil, errors.New("Java SCIP artifact store is required")
	}
	config.Mode = strings.ToLower(strings.TrimSpace(config.Mode))
	if config.Mode == "" {
		config.Mode = ModeOff
	}
	switch config.Mode {
	case ModeOff, ModeAuto, ModeRequired:
	default:
		return nil, fmt.Errorf("Java SCIP mode must be %s, %s, or %s", ModeOff, ModeAuto, ModeRequired)
	}
	if strings.TrimSpace(config.DataDirectory) == "" {
		return nil, errors.New("Java SCIP data directory is required")
	}
	if config.Timeout <= 0 {
		config.Timeout = DefaultTimeout
	}
	if config.Concurrency <= 0 {
		config.Concurrency = DefaultConcurrency
	}
	if config.Concurrency > MaximumConcurrency {
		return nil, fmt.Errorf("Java SCIP concurrency exceeds maximum %d", MaximumConcurrency)
	}
	var jdks []jdkInstallation
	var inherited *jdkInstallation
	var err error
	if config.Mode != ModeOff {
		jdks, err = configuredJDKInstallations(config)
		if err != nil {
			return nil, err
		}
		inherited = inheritedJDKInstallation()
	}
	if err := os.MkdirAll(filepath.Join(config.DataDirectory, "worktrees"), 0o700); err != nil {
		return nil, fmt.Errorf("create Java SCIP worktree directory: %w", err)
	}
	if cleanupErr := cleanupAbandonedWorktrees(config.DataDirectory); cleanupErr != nil {
		slog.Warn("clean abandoned Java SCIP worktrees", "error", cleanupErr)
	}
	executor := config.executor
	if executor == nil {
		executor = osCommandExecutor{}
	}
	service := &Service{
		config: config, store: store, artifacts: artifacts, executor: executor,
		jdks: jdks, inherited: inherited,
		signal:  make(chan struct{}, config.Concurrency),
		queued:  make(map[int64]struct{}),
		running: make(map[int64]struct{}),
		rerun:   make(map[int64]struct{}),
		provider: ProviderStatus{
			Mode:    config.Mode,
			Enabled: config.Mode != ModeOff,
		},
	}
	if config.Mode == ModeOff {
		return service, nil
	}

	command, err := resolveCommand(config.Command)
	if err != nil {
		service.provider.Error = "scip-java is not available"
		service.provider.FailureCategory = FailureEnvironment
		service.provider.FailureSummary = "The configured scip-java executable could not be found."
		if config.Mode == ModeRequired {
			return nil, fmt.Errorf("resolve required scip-java command: %w", err)
		}
		return service, nil
	}
	verifyContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	version, verifyErr := executor.Verify(verifyContext, command)
	cancel()
	if verifyErr != nil {
		service.provider.Error = "scip-java did not pass its version check"
		service.provider.FailureCategory = FailureEnvironment
		service.provider.FailureSummary = "The configured scip-java executable could not be started."
		if config.Mode == ModeRequired {
			return nil, fmt.Errorf("verify required scip-java command: %w", verifyErr)
		}
		return service, nil
	}
	if version == "" {
		version = "unknown"
	}
	configurationParts := []string{command, version, "index"}
	for _, jdk := range jdks {
		configurationParts = append(configurationParts, strconv.Itoa(jdk.Major), jdk.Home, jdk.Source)
		service.provider.JDKVersions = append(service.provider.JDKVersions, jdk.Major)
	}
	if inherited != nil {
		configurationParts = append(configurationParts, "inherited", strconv.Itoa(inherited.Major), inherited.Home)
	}
	digest := sha256.Sum256([]byte(strings.Join(configurationParts, "\x00")))
	service.provider.Available = true
	service.provider.Command = filepath.Base(command)
	service.provider.Version = version
	service.provider.Configuration = hex.EncodeToString(digest[:])
	service.config.Command = command
	return service, nil
}

func configuredJDKInstallations(config Config) ([]jdkInstallation, error) {
	installations := make([]jdkInstallation, 0, len(config.JDKHomes)+1)
	seen := make(map[string]struct{})
	if home := strings.TrimSpace(config.JDKHome); home != "" {
		home, err := validateJDKHome(home)
		if err != nil {
			return nil, fmt.Errorf("validate Java SCIP JDK override: %w", err)
		}
		major, err := detectJDKMajor(home)
		if err != nil {
			return nil, fmt.Errorf("inspect Java SCIP JDK override: %w", err)
		}
		installations = append(installations, jdkInstallation{
			Home: home, Major: major, Source: "override",
		})
		seen[filepath.Clean(home)] = struct{}{}
	}
	versions := make([]int, 0, len(config.JDKHomes))
	for major := range config.JDKHomes {
		if major <= 0 {
			return nil, errors.New("configured Java SCIP JDK versions must be positive")
		}
		versions = append(versions, major)
	}
	sort.Ints(versions)
	for _, major := range versions {
		home, err := validateJDKHome(config.JDKHomes[major])
		if err != nil {
			return nil, fmt.Errorf("validate configured Java %d home: %w", major, err)
		}
		detected, err := detectJDKMajor(home)
		if err != nil {
			return nil, fmt.Errorf("inspect configured Java %d home: %w", major, err)
		}
		if detected != major {
			return nil, fmt.Errorf(
				"configured Java %d home reports Java %d",
				major,
				detected,
			)
		}
		if _, duplicate := seen[filepath.Clean(home)]; duplicate {
			continue
		}
		seen[filepath.Clean(home)] = struct{}{}
		installations = append(installations, jdkInstallation{
			Home: home, Major: major, Source: "configured",
		})
	}
	return installations, nil
}

func validateJDKHome(home string) (string, error) {
	absolute, err := filepath.Abs(strings.TrimSpace(home))
	if err != nil {
		return "", err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("JDK home is not a directory")
	}
	if _, err := os.Stat(javaExecutable(absolute)); err != nil {
		return "", fmt.Errorf("find Java executable: %w", err)
	}
	return filepath.Clean(absolute), nil
}

func javaExecutable(home string) string {
	name := "java"
	if filepath.Separator == '\\' {
		name += ".exe"
	}
	return filepath.Join(home, "bin", name)
}

func detectJDKMajor(home string) (int, error) {
	command := "java"
	if strings.TrimSpace(home) != "" {
		command = javaExecutable(home)
	}
	output, err := exec.Command(command, "-version").CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("run java -version: %w", err)
	}
	return parseJavaMajor(string(output))
}

func parseJavaMajor(value string) (int, error) {
	versionPattern := regexp.MustCompile(`(?i)(?:java|openjdk) version "([^"]+)"`)
	match := versionPattern.FindStringSubmatch(value)
	if len(match) != 2 {
		return 0, errors.New("java -version did not report a recognizable version")
	}
	parts := strings.FieldsFunc(match[1], func(character rune) bool {
		return character == '.' || character == '-' || character == '_'
	})
	if len(parts) == 0 {
		return 0, errors.New("java -version reported an empty version")
	}
	index := 0
	if parts[0] == "1" && len(parts) > 1 {
		index = 1
	}
	major, err := strconv.Atoi(parts[index])
	if err != nil || major <= 0 {
		return 0, errors.New("java -version reported an invalid major version")
	}
	return major, nil
}

func inheritedJDKInstallation() *jdkInstallation {
	home := strings.TrimSpace(os.Getenv("JAVA_HOME"))
	major, err := detectJDKMajor(home)
	if err != nil {
		return nil
	}
	return &jdkInstallation{Home: home, Major: major, Source: "inherited"}
}

func resolveCommand(configured string) (string, error) {
	configured = strings.TrimSpace(configured)
	if configured == "" {
		return exec.LookPath("scip-java")
	}
	if filepath.IsAbs(configured) {
		info, err := os.Stat(configured)
		if err != nil {
			return "", err
		}
		if info.IsDir() {
			return "", errors.New("configured scip-java command is a directory")
		}
		return filepath.Clean(configured), nil
	}
	return exec.LookPath(configured)
}

// ParseJDKHomes parses a deterministic comma-separated major=home mapping used
// to select a Gradle launcher per repository.
func ParseJDKHomes(value string) (map[int]string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	homes := make(map[int]string)
	for _, entry := range strings.Split(value, ",") {
		version, home, found := strings.Cut(strings.TrimSpace(entry), "=")
		major, err := strconv.Atoi(strings.TrimSpace(version))
		if !found || err != nil || major <= 0 || strings.TrimSpace(home) == "" {
			return nil, fmt.Errorf("expected comma-separated major=JDK-home entries")
		}
		if _, duplicate := homes[major]; duplicate {
			return nil, fmt.Errorf("Java %d JDK home is configured more than once", major)
		}
		homes[major] = strings.TrimSpace(home)
	}
	return homes, nil
}

// ProviderStatus returns a copy safe for API and template presentation.
func (s *Service) ProviderStatus() ProviderStatus {
	if s == nil {
		return ProviderStatus{Mode: ModeOff}
	}
	return s.provider
}

// Start launches the bounded worker pool once.
func (s *Service) Start(ctx context.Context) {
	if s == nil || !s.provider.Enabled {
		return
	}
	s.startOnce.Do(func() {
		s.baseCtxMu.Lock()
		s.baseCtx = ctx
		s.baseCtxMu.Unlock()
		for range s.config.Concurrency {
			go s.worker(ctx)
		}
	})
}

// Queue schedules one repository without dropping work or duplicating a
// currently pending ID.
func (s *Service) Queue(repositoryID int64) {
	if s == nil || !s.provider.Enabled || repositoryID <= 0 {
		return
	}
	baseCtx := s.baseContext()
	if baseCtx == nil {
		return
	}
	s.mu.Lock()
	if _, exists := s.queued[repositoryID]; exists {
		s.mu.Unlock()
		return
	}
	if _, exists := s.running[repositoryID]; exists {
		s.rerun[repositoryID] = struct{}{}
		s.mu.Unlock()
		return
	}
	s.queued[repositoryID] = struct{}{}
	s.pending = append(s.pending, repositoryID)
	if repository, err := s.store.RepositoryByID(baseCtx, repositoryID); err == nil &&
		repository.IndexState == "ready" &&
		strings.TrimSpace(repository.IndexedCommit) != "" &&
		(repository.SCIPJava == nil ||
			repository.SCIPJava.State != "ready" ||
			repository.SCIPJava.Revision != repository.IndexedCommit ||
			repository.SCIPJava.Configuration != s.provider.Configuration) {
		applicable := true
		buildRoot := ""
		if repository.SCIPJava != nil {
			applicable = repository.SCIPJava.Applicable
			buildRoot = repository.SCIPJava.BuildRoot
		}
		if statusErr := s.store.UpdateSCIPIndexStatus(baseCtx, repositoryID, catalog.SCIPIndexStatus{
			Provider: "scip-java", State: "pending", Applicable: applicable,
			Revision: repository.IndexedCommit, Configuration: s.provider.Configuration,
			Indexer: "scip-java", Version: s.provider.Version, BuildRoot: buildRoot,
			QueuedAt: time.Now().UTC(),
		}); statusErr != nil && baseCtx.Err() == nil {
			slog.Warn("record queued Java SCIP index", "repository_id", repositoryID, "error", statusErr)
		}
	}
	s.mu.Unlock()
	select {
	case s.signal <- struct{}{}:
	case <-baseCtx.Done():
	default:
	}
}

func (s *Service) baseContext() context.Context {
	s.baseCtxMu.RLock()
	defer s.baseCtxMu.RUnlock()
	return s.baseCtx
}

// Retry clears a terminal state and queues the repository again.
func (s *Service) Retry(ctx context.Context, repositoryID int64) error {
	if s == nil || !s.provider.Enabled {
		return errors.New("Java SCIP generation is disabled")
	}
	repository, err := s.store.RepositoryByID(ctx, repositoryID)
	if err != nil {
		return err
	}
	if repository.IndexState != "ready" || strings.TrimSpace(repository.IndexedCommit) == "" {
		return errors.New("repository source index is not ready")
	}
	now := time.Now().UTC()
	if err := s.store.UpdateSCIPIndexStatus(ctx, repositoryID, catalog.SCIPIndexStatus{
		Provider: "scip-java", State: "pending", Applicable: true,
		Revision: repository.IndexedCommit, Configuration: s.provider.Configuration,
		QueuedAt: now,
	}); err != nil {
		return err
	}
	s.Queue(repositoryID)
	return nil
}

func (s *Service) worker(ctx context.Context) {
	for {
		repositoryID, ok := s.next()
		if ok {
			if err := s.indexRepository(ctx, repositoryID); err != nil && ctx.Err() == nil {
				slog.Warn("build Java SCIP index", "repository_id", repositoryID, "error", err)
			}
			s.finish(repositoryID)
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-s.signal:
		}
	}
}

func (s *Service) next() (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		return 0, false
	}
	repositoryID := s.pending[0]
	s.pending = s.pending[1:]
	delete(s.queued, repositoryID)
	s.running[repositoryID] = struct{}{}
	return repositoryID, true
}

func (s *Service) finish(repositoryID int64) {
	baseCtx := s.baseContext()
	s.mu.Lock()
	delete(s.running, repositoryID)
	if _, requested := s.rerun[repositoryID]; requested && baseCtx != nil && baseCtx.Err() == nil {
		delete(s.rerun, repositoryID)
		s.queued[repositoryID] = struct{}{}
		s.pending = append(s.pending, repositoryID)
		s.mu.Unlock()
		select {
		case s.signal <- struct{}{}:
		case <-baseCtx.Done():
		default:
		}
		return
	}
	delete(s.rerun, repositoryID)
	s.mu.Unlock()
}

func (s *Service) indexRepository(ctx context.Context, repositoryID int64) (resultErr error) {
	ctx, finish := telemetry.StartOperation(ctx, telemetry.OperationSCIPBuild, telemetry.Labels{
		Provider: "scip-java",
		Trigger:  "background",
	})
	defer func() { finish(resultErr) }()
	repository, err := s.store.RepositoryByID(ctx, repositoryID)
	if err != nil {
		return err
	}
	revision := strings.TrimSpace(repository.IndexedCommit)
	if repository.IndexState != "ready" || revision == "" {
		return nil
	}
	build, applicable, reason, inspectErr := inspectGradleRepository(ctx, repository, revision)
	if inspectErr != nil {
		return s.recordFailure(ctx, repository, revision, gradleBuild{}, jdkSelection{}, true, &failure{
			Category: FailureEnvironment,
			Summary:  "RepoKarta could not inspect the exact-commit Gradle build.",
			Cause:    inspectErr,
		})
	}
	if !applicable {
		return s.store.UpdateSCIPIndexStatus(ctx, repository.ID, catalog.SCIPIndexStatus{
			Provider: "scip-java", State: "skipped", Applicable: false,
			Revision: revision, Configuration: s.provider.Configuration,
			Error: reason, FinishedAt: time.Now().UTC(),
		})
	}
	if !s.provider.Available {
		return s.store.UpdateSCIPIndexStatus(ctx, repository.ID, catalog.SCIPIndexStatus{
			Provider: "scip-java", State: "unavailable", Applicable: true,
			Revision: revision, Configuration: s.provider.Configuration,
			BuildRoot: build.Root, GradleVersion: build.GradleVersion,
			RequestedJDKVersion: build.ToolchainVersion,
			FailureCategory:     s.provider.FailureCategory,
			FailureSummary:      s.provider.FailureSummary,
			Error:               s.provider.Error,
			FinishedAt:          time.Now().UTC(),
		})
	}
	if current := repository.SCIPJava; current != nil &&
		current.State == "ready" &&
		current.Revision == revision &&
		current.Configuration == s.provider.Configuration {
		if _, ok, readErr := s.artifacts.Read(ctx, repository.ID, revision); readErr != nil {
			slog.Warn(
				"rebuild unusable Java SCIP artifact",
				"repository_id", repository.ID,
				"error", readErr,
			)
		} else if ok {
			return nil
		}
	}
	selection, selectionErr := s.selectJDK(build)
	if selectionErr != nil {
		return s.recordFailure(ctx, repository, revision, build, selection, true, selectionErr)
	}

	now := time.Now().UTC()
	status := catalog.SCIPIndexStatus{
		Provider: "scip-java", State: "indexing", Applicable: true,
		Revision: revision, Configuration: s.provider.Configuration,
		Indexer: "scip-java", Version: s.provider.Version,
		BuildRoot: build.Root, GradleVersion: build.GradleVersion,
		RequestedJDKVersion: build.ToolchainVersion,
		JDKVersion:          selection.Major, JDKSource: selection.Source,
		QueuedAt: now, StartedAt: now,
	}
	if err := s.store.UpdateSCIPIndexStatus(ctx, repository.ID, status); err != nil {
		return err
	}
	bounded, cancel := context.WithTimeout(ctx, s.config.Timeout)
	summary, buildErr := s.build(bounded, repository, revision, build, selection)
	cancel()
	if buildErr != nil {
		return s.recordFailure(ctx, repository, revision, build, selection, true, buildErr)
	}
	status.State = "ready"
	status.Indexer = summary.Indexer.Name
	status.Version = summary.Indexer.Version
	status.Documents = summary.Documents
	status.Symbols = summary.Symbols
	status.Occurrences = summary.Occurrences
	status.FinishedAt = time.Now().UTC()
	return s.store.UpdateSCIPIndexStatus(ctx, repository.ID, status)
}

func (s *Service) recordFailure(
	ctx context.Context,
	repository catalog.Repository,
	revision string,
	build gradleBuild,
	selection jdkSelection,
	applicable bool,
	cause error,
) error {
	sensitivePaths := []string{repository.Path, s.config.DataDirectory}
	for _, installation := range s.jdks {
		sensitivePaths = append(sensitivePaths, installation.Home)
	}
	message := sanitizeFailure(cause.Error(), sensitivePaths...)
	category, summary := classifyFailure(cause)
	statusErr := s.store.UpdateSCIPIndexStatus(ctx, repository.ID, catalog.SCIPIndexStatus{
		Provider: "scip-java", State: "failed", Applicable: applicable,
		Revision: revision, Configuration: s.provider.Configuration,
		Indexer: "scip-java", Version: s.provider.Version,
		BuildRoot: build.Root, GradleVersion: build.GradleVersion,
		RequestedJDKVersion: build.ToolchainVersion,
		JDKVersion:          selection.Major, JDKSource: selection.Source,
		FailureCategory: category, FailureSummary: summary,
		Error: message, FinishedAt: time.Now().UTC(),
	})
	if statusErr != nil {
		return errors.Join(cause, statusErr)
	}
	return cause
}

func (s *Service) build(
	ctx context.Context,
	repository catalog.Repository,
	revision string,
	build gradleBuild,
	selection jdkSelection,
) (scipindex.ImportSummary, error) {
	shadowPath, err := s.prepareGitShadow(ctx, repository, revision)
	if err != nil {
		return scipindex.ImportSummary{}, fmt.Errorf("prepare Java SCIP Git shadow: %w", err)
	}
	parent, err := os.MkdirTemp(filepath.Join(s.config.DataDirectory, "worktrees"), fmt.Sprintf("%d-", repository.ID))
	if err != nil {
		return scipindex.ImportSummary{}, fmt.Errorf("create isolated worktree parent: %w", err)
	}
	worktree := filepath.Join(parent, "checkout")
	defer func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if cleanupErr := removeWorktree(cleanupContext, shadowPath, worktree); cleanupErr != nil {
			slog.Warn("remove Java SCIP worktree", "repository_id", repository.ID, "error", cleanupErr)
		}
		if removeErr := os.RemoveAll(parent); removeErr != nil {
			slog.Warn("remove Java SCIP temporary directory", "repository_id", repository.ID, "error", removeErr)
		}
	}()

	if output, addErr := runGit(ctx, shadowPath, "worktree", "add", "--detach", "--force", worktree, revision); addErr != nil {
		return scipindex.ImportSummary{}, fmt.Errorf("create exact-commit worktree: %w: %s", addErr, output)
	}
	indexDirectory := worktree
	sourceRoot := "."
	if build.Root != "" {
		indexDirectory = filepath.Join(worktree, filepath.FromSlash(build.Root))
		sourceRoot = build.Root
	}
	indexPath := filepath.Join(indexDirectory, "index.scip")
	if removeErr := os.Remove(indexPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return scipindex.ImportSummary{}, fmt.Errorf("remove stale temporary SCIP output: %w", removeErr)
	}
	output, runErr := s.executor.Run(ctx, s.config.Command, indexDirectory, s.buildEnvironment(selection))
	if runErr != nil {
		detail := fmt.Errorf(
			"scip-java index failed: %w: %s", runErr,
			sanitizeFailure(string(output), repository.Path, worktree, s.config.DataDirectory),
		)
		if ctx.Err() != nil {
			return scipindex.ImportSummary{}, &failure{
				Category: FailureEnvironment,
				Summary:  "Java SCIP generation exceeded its configured time limit.",
				Cause:    errors.Join(ctx.Err(), detail),
			}
		}
		return scipindex.ImportSummary{}, classifyBuildFailure(detail, string(output))
	}
	input, err := os.Open(indexPath)
	if err != nil {
		return scipindex.ImportSummary{}, fmt.Errorf("open generated index.scip: %w", err)
	}
	defer input.Close()
	summary, err := s.artifacts.Import(ctx, repository.ID, revision, sourceRoot, input)
	if err != nil {
		return scipindex.ImportSummary{}, fmt.Errorf("import generated index.scip: %w", err)
	}
	return summary, nil
}

func (s *Service) prepareGitShadow(
	ctx context.Context,
	repository catalog.Repository,
	revision string,
) (string, error) {
	root := filepath.Join(s.config.DataDirectory, "git-shadow")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	shadowPath := filepath.Join(root, fmt.Sprintf("repository-%d.git", repository.ID))
	if _, err := os.Stat(filepath.Join(shadowPath, "HEAD")); errors.Is(err, os.ErrNotExist) {
		command := exec.CommandContext(ctx, "git", "init", "--bare", shadowPath)
		if output, commandErr := command.CombinedOutput(); commandErr != nil {
			return "", fmt.Errorf("initialize shadow: %w: %s", commandErr, boundedOneLine(output))
		}
	} else if err != nil {
		return "", err
	}
	ref := "refs/repokarta/scip"
	if output, err := runGit(
		ctx,
		shadowPath,
		"fetch", "--force", "--no-tags", repository.Path, "+"+revision+":"+ref,
	); err != nil {
		return "", fmt.Errorf("fetch exact revision: %w: %s", err, output)
	}
	return shadowPath, nil
}

func cleanupAbandonedWorktrees(dataDirectory string) error {
	worktreeRoot := filepath.Join(dataDirectory, "worktrees")
	entries, err := os.ReadDir(worktreeRoot)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var cleanupErr error
	for _, entry := range entries {
		if !ownedWorktreeDirectory(entry.Name()) {
			continue
		}
		target := filepath.Join(worktreeRoot, entry.Name())
		info, err := os.Lstat(target)
		if err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			err = os.Remove(target)
		} else if info.IsDir() {
			err = os.RemoveAll(target)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove %s: %w", entry.Name(), err))
		}
	}

	shadowRoot := filepath.Join(dataDirectory, "git-shadow")
	shadows, err := os.ReadDir(shadowRoot)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.Join(cleanupErr, err)
	}
	for _, entry := range shadows {
		if !entry.IsDir() || !ownedShadowDirectory(entry.Name()) {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		output, err := runGit(
			ctx,
			filepath.Join(shadowRoot, entry.Name()),
			"worktree", "prune", "--expire", "now",
		)
		cancel()
		if err != nil {
			cleanupErr = errors.Join(
				cleanupErr,
				fmt.Errorf("prune %s worktree registrations: %w: %s", entry.Name(), err, output),
			)
		}
	}
	return cleanupErr
}

func ownedWorktreeDirectory(name string) bool {
	separator := strings.IndexByte(name, '-')
	if separator <= 0 || separator == len(name)-1 {
		return false
	}
	repositoryID, err := strconv.ParseInt(name[:separator], 10, 64)
	return err == nil && repositoryID > 0
}

func ownedShadowDirectory(name string) bool {
	if !strings.HasPrefix(name, "repository-") || !strings.HasSuffix(name, ".git") {
		return false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(name, "repository-"), ".git")
	repositoryID, err := strconv.ParseInt(id, 10, 64)
	return err == nil && repositoryID > 0
}

func classifyFailure(cause error) (string, string) {
	var classified *failure
	if errors.As(cause, &classified) {
		return classified.Category, classified.Summary
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		return FailureEnvironment, "Java SCIP generation exceeded its configured time limit."
	}
	return FailureEnvironment, "The Java SCIP build environment failed before compilation completed."
}

func classifyBuildFailure(cause error, output string) error {
	lower := strings.ToLower(output + "\n" + cause.Error())
	jdkPatterns := []string{
		"unsupported class file major version",
		"could not determine java version from",
		"gradle requires jvm",
		"requires java 17 or later",
		"incompatible java version",
		"incompatible because this component declares a component compatible with java",
		"your build is currently configured to use incompatible java",
	}
	for _, pattern := range jdkPatterns {
		if strings.Contains(lower, pattern) {
			return &failure{
				Category: FailureJDKIncompatibleWrapper,
				Summary:  "The selected JDK cannot run this repository's Gradle wrapper.",
				Cause:    cause,
			}
		}
	}
	environmentPatterns := []string{
		"cannot connect to the docker daemon",
		"error during connect: this error may indicate that the docker daemon is not running",
		"docker: command not found",
		"'docker' is not recognized",
		"cannot run program \"docker\"",
		"no such file or directory",
		"the system cannot find the file specified",
		"permission denied",
		"access is denied",
		"could not resolve host",
		"connection timed out",
	}
	for _, pattern := range environmentPatterns {
		if strings.Contains(lower, pattern) {
			return &failure{
				Category: FailureEnvironment,
				Summary:  "The build environment or a required external service is unavailable.",
				Cause:    cause,
			}
		}
	}
	return &failure{
		Category: FailureCompileError,
		Summary:  "Gradle reached compilation, but the repository build did not compile successfully.",
		Cause:    cause,
	}
}

func (s *Service) selectJDK(build gradleBuild) (jdkSelection, error) {
	selection := jdkSelection{
		RequestedVersion: build.ToolchainVersion,
		GradleVersion:    build.GradleVersion,
	}
	if len(s.jdks) > 0 && s.jdks[0].Source == "override" {
		selection.Home = s.jdks[0].Home
		selection.Major = s.jdks[0].Major
		selection.Source = "override"
		if !gradleSupportsJavaRuntime(build.GradleVersion, selection.Major) {
			return selection, incompatibleJDKFailure(build, selection)
		}
		return selection, nil
	}

	candidates := append([]jdkInstallation(nil), s.jdks...)
	if s.inherited != nil {
		candidates = append(candidates, *s.inherited)
	}
	if build.ToolchainVersion > 0 {
		for _, candidate := range candidates {
			if candidate.Major == build.ToolchainVersion &&
				gradleSupportsJavaRuntime(build.GradleVersion, candidate.Major) {
				selection.Home = candidate.Home
				selection.Major = candidate.Major
				selection.Source = "toolchain"
				return selection, nil
			}
		}
	}
	if s.inherited != nil && gradleSupportsJavaRuntime(build.GradleVersion, s.inherited.Major) {
		selection.Home = s.inherited.Home
		selection.Major = s.inherited.Major
		selection.Source = "inherited"
		return selection, nil
	}
	sort.SliceStable(candidates, func(left, right int) bool {
		return candidates[left].Major > candidates[right].Major
	})
	for _, candidate := range candidates {
		if gradleSupportsJavaRuntime(build.GradleVersion, candidate.Major) {
			selection.Home = candidate.Home
			selection.Major = candidate.Major
			selection.Source = "compatible-configured"
			return selection, nil
		}
	}
	if build.GradleVersion == "" && len(candidates) == 0 {
		selection.Source = "inherited"
		return selection, nil
	}
	if len(candidates) > 0 {
		selection.Home = candidates[0].Home
		selection.Major = candidates[0].Major
		selection.Source = candidates[0].Source
	}
	return selection, incompatibleJDKFailure(build, selection)
}

func incompatibleJDKFailure(build gradleBuild, selection jdkSelection) error {
	detail := "no compatible configured JDK is available"
	if selection.Major > 0 {
		detail = fmt.Sprintf("Java %d cannot run Gradle %s", selection.Major, build.GradleVersion)
	}
	return &failure{
		Category: FailureJDKIncompatibleWrapper,
		Summary:  "No configured JDK can run this repository's Gradle wrapper.",
		Cause:    errors.New(detail),
	}
}

func gradleSupportsJavaRuntime(gradleVersion string, javaMajor int) bool {
	if gradleVersion == "" || javaMajor == 0 {
		return true
	}
	if javaMajor < 8 {
		return false
	}
	gradle := parseNumericVersion(gradleVersion)
	if len(gradle) == 0 {
		return true
	}
	if gradle[0] >= 9 && javaMajor < 17 {
		return false
	}
	minimum, known := minimumGradleForJava(javaMajor)
	if !known {
		return false
	}
	return compareNumericVersions(gradle, minimum) >= 0
}

func minimumGradleForJava(javaMajor int) ([]int, bool) {
	minimum := map[int][]int{
		8: {2, 0}, 9: {4, 3}, 10: {4, 7}, 11: {5, 0},
		12: {5, 4}, 13: {6, 0}, 14: {6, 3}, 15: {6, 7},
		16: {7, 0}, 17: {7, 3}, 18: {7, 5}, 19: {7, 6},
		20: {8, 3}, 21: {8, 5}, 22: {8, 8}, 23: {8, 10},
		24: {8, 14}, 25: {9, 1, 0}, 26: {9, 4, 0},
	}
	version, ok := minimum[javaMajor]
	return version, ok
}

func parseNumericVersion(value string) []int {
	parts := strings.Split(strings.TrimSpace(value), ".")
	version := make([]int, 0, len(parts))
	for _, part := range parts {
		digits := strings.TrimLeftFunc(part, func(character rune) bool {
			return character < '0' || character > '9'
		})
		end := strings.IndexFunc(digits, func(character rune) bool {
			return character < '0' || character > '9'
		})
		if end >= 0 {
			digits = digits[:end]
		}
		if digits == "" {
			break
		}
		number, err := strconv.Atoi(digits)
		if err != nil {
			return nil
		}
		version = append(version, number)
	}
	return version
}

func compareNumericVersions(left, right []int) int {
	length := max(len(left), len(right))
	for index := 0; index < length; index++ {
		leftPart, rightPart := 0, 0
		if index < len(left) {
			leftPart = left[index]
		}
		if index < len(right) {
			rightPart = right[index]
		}
		if leftPart < rightPart {
			return -1
		}
		if leftPart > rightPart {
			return 1
		}
	}
	return 0
}

func (s *Service) buildEnvironment(selection jdkSelection) []string {
	environment := append([]string(nil), os.Environ()...)
	if selection.Home != "" {
		environment = setEnvironment(environment, "JAVA_HOME", selection.Home)
		currentPath := environmentValue(environment, "PATH")
		environment = setEnvironment(
			environment,
			"PATH",
			javaBin(selection.Home)+string(os.PathListSeparator)+currentPath,
		)
	}
	homes := make([]string, 0, len(s.jdks))
	for _, installation := range s.jdks {
		if !strings.Contains(installation.Home, ",") {
			homes = append(homes, installation.Home)
		}
	}
	if len(homes) > 0 {
		option := "-Dorg.gradle.java.installations.paths=" + strings.Join(homes, ",")
		if strings.ContainsAny(option, " \t") {
			option = `"` + strings.ReplaceAll(option, `"`, `\"`) + `"`
		}
		current := strings.TrimSpace(environmentValue(environment, "GRADLE_OPTS"))
		if current != "" {
			option = current + " " + option
		}
		environment = setEnvironment(environment, "GRADLE_OPTS", option)
	}
	return environment
}

func javaBin(home string) string {
	return filepath.Join(home, "bin")
}

func environmentValue(environment []string, key string) string {
	for _, entry := range environment {
		name, value, found := strings.Cut(entry, "=")
		if found && strings.EqualFold(name, key) {
			return value
		}
	}
	return ""
}

func setEnvironment(environment []string, key, value string) []string {
	for index, entry := range environment {
		name, _, found := strings.Cut(entry, "=")
		if found && strings.EqualFold(name, key) {
			environment[index] = key + "=" + value
			return environment
		}
	}
	return append(environment, key+"="+value)
}

func removeWorktree(ctx context.Context, repositoryPath, worktree string) error {
	if _, err := os.Stat(worktree); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	_, err := runGit(ctx, repositoryPath, "worktree", "remove", "--force", worktree)
	return err
}

func inspectGradleRepository(
	ctx context.Context,
	repository catalog.Repository,
	revision string,
) (build gradleBuild, applicable bool, reason string, err error) {
	output, err := runGit(ctx, repository.Path, "ls-tree", "-r", "-z", "--name-only", revision)
	if err != nil {
		return gradleBuild{}, false, "", fmt.Errorf("list committed files: %w", err)
	}
	files := strings.Split(strings.TrimSuffix(output, "\x00"), "\x00")
	javaFiles := make([]string, 0)
	settingsRoots := make(map[string]struct{})
	buildRoots := make(map[string]struct{})
	for _, file := range files {
		file = path.Clean(strings.TrimSpace(file))
		if file == "." || strings.HasPrefix(file, "../") || path.IsAbs(file) {
			continue
		}
		base := path.Base(file)
		switch {
		case strings.HasSuffix(strings.ToLower(base), ".java"):
			javaFiles = append(javaFiles, file)
		case base == "settings.gradle" || base == "settings.gradle.kts":
			settingsRoots[pathDirectory(file)] = struct{}{}
		case base == "build.gradle" || base == "build.gradle.kts":
			buildRoots[pathDirectory(file)] = struct{}{}
		}
	}
	if len(javaFiles) == 0 {
		return gradleBuild{}, false, "No committed Java sources.", nil
	}
	roots := settingsRoots
	if len(roots) == 0 {
		roots = buildRoots
	}
	if len(roots) == 0 {
		return gradleBuild{}, false, "Java sources found, but no Gradle build was detected.", nil
	}
	candidates := make([]string, 0, len(roots))
	for root := range roots {
		if containsAnyPath(root, javaFiles) {
			candidates = append(candidates, root)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		leftDepth := strings.Count(candidates[i], "/")
		rightDepth := strings.Count(candidates[j], "/")
		if leftDepth != rightDepth {
			return leftDepth < rightDepth
		}
		return candidates[i] < candidates[j]
	})
	for _, candidate := range candidates {
		if containsEveryPath(candidate, javaFiles) {
			build, err := inspectGradleMetadata(ctx, repository.Path, revision, candidate, files)
			if err != nil {
				return gradleBuild{}, true, "", err
			}
			return build, true, "", nil
		}
	}
	return gradleBuild{}, true, "", errors.New("multiple independent Gradle roots are not supported; index each build as a separate repository")
}

func inspectGradleMetadata(
	ctx context.Context,
	repositoryPath, revision, root string,
	files []string,
) (gradleBuild, error) {
	build := gradleBuild{Root: root}
	wrapperPath := path.Join(root, "gradle/wrapper/gradle-wrapper.properties")
	daemonJVMPath := path.Join(root, "gradle/gradle-daemon-jvm.properties")
	toolchainVersions := make(map[int]struct{})
	daemonJVMVersion := 0
	metadataFiles := make([]string, 0)
	for _, file := range files {
		if !withinRoot(root, file) {
			continue
		}
		base := path.Base(file)
		if file == wrapperPath || file == daemonJVMPath ||
			base == "build.gradle" || base == "build.gradle.kts" {
			metadataFiles = append(metadataFiles, file)
		}
	}
	sort.Strings(metadataFiles)
	if len(metadataFiles) > maximumGradleMetadataFiles {
		return gradleBuild{}, fmt.Errorf(
			"Gradle metadata inspection exceeds the %d-file limit",
			maximumGradleMetadataFiles,
		)
	}
	for _, file := range metadataFiles {
		content, readErr := runGit(ctx, repositoryPath, "show", revision+":"+file)
		if readErr != nil {
			return gradleBuild{}, fmt.Errorf("read committed Gradle metadata %q: %w", file, readErr)
		}
		if len(content) > maximumGradleMetadataBytes {
			return gradleBuild{}, fmt.Errorf(
				"committed Gradle metadata %q exceeds the %d-byte limit",
				file,
				maximumGradleMetadataBytes,
			)
		}
		if file == wrapperPath {
			if match := gradleDistributionPattern.FindStringSubmatch(content); len(match) == 2 {
				build.GradleVersion = match[1]
			}
		}
		for _, pattern := range javaToolchainPatterns {
			for _, match := range pattern.FindAllStringSubmatch(content, -1) {
				if len(match) != 2 {
					continue
				}
				major, parseErr := strconv.Atoi(match[1])
				if parseErr == nil && major > 0 {
					if file == daemonJVMPath {
						daemonJVMVersion = major
					} else {
						toolchainVersions[major] = struct{}{}
					}
				}
			}
		}
	}
	if daemonJVMVersion > 0 {
		build.ToolchainVersion = daemonJVMVersion
	} else if len(toolchainVersions) == 1 {
		for major := range toolchainVersions {
			build.ToolchainVersion = major
		}
	}
	return build, nil
}

func pathDirectory(file string) string {
	directory := path.Dir(file)
	if directory == "." {
		return ""
	}
	return directory
}

func containsAnyPath(root string, files []string) bool {
	for _, file := range files {
		if withinRoot(root, file) {
			return true
		}
	}
	return false
}

func containsEveryPath(root string, files []string) bool {
	for _, file := range files {
		if !withinRoot(root, file) {
			return false
		}
	}
	return true
}

func withinRoot(root, file string) bool {
	return root == "" || file == root || strings.HasPrefix(file, root+"/")
}

func runGit(ctx context.Context, repositoryPath string, arguments ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", repositoryPath}, arguments...)...)
	var output cappedBuffer
	output.maximum = maximumGitOutput
	command.Stdout = &output
	command.Stderr = &output
	err := command.Run()
	return string(output.Bytes()), err
}

type cappedBuffer struct {
	bytes.Buffer
	maximum   int
	truncated bool
}

func (buffer *cappedBuffer) Write(content []byte) (int, error) {
	original := len(content)
	remaining := buffer.maximum - buffer.Len()
	if remaining <= 0 {
		buffer.truncated = true
		return original, nil
	}
	if len(content) > remaining {
		content = content[:remaining]
		buffer.truncated = true
	}
	_, _ = buffer.Buffer.Write(content)
	return original, nil
}

func boundedOneLine(content []byte) string {
	line := strings.TrimSpace(string(content))
	if index := strings.IndexAny(line, "\r\n"); index >= 0 {
		line = line[:index]
	}
	if len(line) > 200 {
		line = line[:200]
	}
	return line
}

func sanitizeFailure(value string, paths ...string) string {
	value = strings.ReplaceAll(value, "\x00", "")
	for _, sensitivePath := range paths {
		sensitivePath = strings.TrimSpace(sensitivePath)
		if sensitivePath == "" {
			continue
		}
		value = strings.ReplaceAll(value, sensitivePath, "<path>")
		value = strings.ReplaceAll(value, filepath.ToSlash(sensitivePath), "<path>")
	}
	lines := strings.Split(value, "\n")
	output := lines[:0]
	for _, line := range lines {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "authorization:") ||
			strings.Contains(lower, "password=") ||
			strings.Contains(lower, "token=") ||
			strings.Contains(lower, "secret=") {
			output = append(output, "<redacted sensitive build output>")
			continue
		}
		output = append(output, strings.TrimRight(line, "\r"))
	}
	value = strings.TrimSpace(strings.Join(output, "\n"))
	if len(value) > maximumStatusError {
		value = value[len(value)-maximumStatusError:]
		value = "… " + value
	}
	if value == "" {
		return "Java SCIP generation failed without diagnostic output."
	}
	return value
}
