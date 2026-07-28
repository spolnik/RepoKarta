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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/scipindex"
)

const (
	ModeOff      = "off"
	ModeAuto     = "auto"
	ModeRequired = "required"

	DefaultTimeout     = 20 * time.Minute
	DefaultConcurrency = 1
	MaximumConcurrency = 4
	maximumGitOutput   = 16 << 20
	maximumBuildOutput = 1 << 20
	maximumStatusError = 4 << 10
)

// Config controls the optional external compiler indexer.
type Config struct {
	Mode          string
	Command       string
	DataDirectory string
	Timeout       time.Duration
	Concurrency   int

	executor commandExecutor
}

// ProviderStatus describes whether the configured producer can run.
type ProviderStatus struct {
	Mode          string `json:"mode"`
	Enabled       bool   `json:"enabled"`
	Available     bool   `json:"available"`
	Command       string `json:"command,omitempty"`
	Version       string `json:"version,omitempty"`
	Configuration string `json:"configuration,omitempty"`
	Error         string `json:"error,omitempty"`
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
	Run(context.Context, string, string) ([]byte, error)
}

type osCommandExecutor struct{}

func (osCommandExecutor) Verify(ctx context.Context, command string) (string, error) {
	output, err := exec.CommandContext(ctx, command, "--version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("run --version: %w", err)
	}
	return boundedOneLine(output), nil
}

func (osCommandExecutor) Run(ctx context.Context, command, directory string) ([]byte, error) {
	process := exec.CommandContext(ctx, command, "index")
	process.Dir = directory
	var output cappedBuffer
	output.maximum = maximumBuildOutput
	process.Stdout = &output
	process.Stderr = &output
	err := process.Run()
	return output.Bytes(), err
}

// Service owns a lossless, deduplicated queue independent of structural-map
// concurrency. One worker is the default because each item may compile a full
// Gradle build.
type Service struct {
	config    Config
	store     RepositoryStore
	artifacts importer
	provider  ProviderStatus
	executor  commandExecutor

	startOnce sync.Once
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
	if err := os.MkdirAll(filepath.Join(config.DataDirectory, "worktrees"), 0o700); err != nil {
		return nil, fmt.Errorf("create Java SCIP worktree directory: %w", err)
	}
	executor := config.executor
	if executor == nil {
		executor = osCommandExecutor{}
	}
	service := &Service{
		config: config, store: store, artifacts: artifacts, executor: executor,
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
		if config.Mode == ModeRequired {
			return nil, fmt.Errorf("verify required scip-java command: %w", verifyErr)
		}
		return service, nil
	}
	if version == "" {
		version = "unknown"
	}
	digest := sha256.Sum256([]byte(command + "\x00" + version + "\x00index"))
	service.provider.Available = true
	service.provider.Command = filepath.Base(command)
	service.provider.Version = version
	service.provider.Configuration = hex.EncodeToString(digest[:])
	service.config.Command = command
	return service, nil
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
		s.baseCtx = ctx
		for range s.config.Concurrency {
			go s.worker(ctx)
		}
	})
}

// Queue schedules one repository without dropping work or duplicating a
// currently pending ID.
func (s *Service) Queue(repositoryID int64) {
	if s == nil || !s.provider.Enabled || repositoryID <= 0 || s.baseCtx == nil {
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
	s.mu.Unlock()
	if repository, err := s.store.RepositoryByID(s.baseCtx, repositoryID); err == nil &&
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
		if statusErr := s.store.UpdateSCIPIndexStatus(s.baseCtx, repositoryID, catalog.SCIPIndexStatus{
			Provider: "scip-java", State: "pending", Applicable: applicable,
			Revision: repository.IndexedCommit, Configuration: s.provider.Configuration,
			Indexer: "scip-java", Version: s.provider.Version, BuildRoot: buildRoot,
			QueuedAt: time.Now().UTC(),
		}); statusErr != nil && s.baseCtx.Err() == nil {
			slog.Warn("record queued Java SCIP index", "repository_id", repositoryID, "error", statusErr)
		}
	}
	select {
	case s.signal <- struct{}{}:
	case <-s.baseCtx.Done():
	default:
	}
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
	s.mu.Lock()
	delete(s.running, repositoryID)
	if _, requested := s.rerun[repositoryID]; requested && s.baseCtx.Err() == nil {
		delete(s.rerun, repositoryID)
		s.queued[repositoryID] = struct{}{}
		s.pending = append(s.pending, repositoryID)
		s.mu.Unlock()
		select {
		case s.signal <- struct{}{}:
		case <-s.baseCtx.Done():
		default:
		}
		return
	}
	delete(s.rerun, repositoryID)
	s.mu.Unlock()
}

func (s *Service) indexRepository(ctx context.Context, repositoryID int64) error {
	repository, err := s.store.RepositoryByID(ctx, repositoryID)
	if err != nil {
		return err
	}
	revision := strings.TrimSpace(repository.IndexedCommit)
	if repository.IndexState != "ready" || revision == "" {
		return nil
	}
	buildRoot, applicable, reason, inspectErr := inspectGradleRepository(ctx, repository, revision)
	if inspectErr != nil {
		return s.recordFailure(ctx, repository, revision, "", true, inspectErr)
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
			BuildRoot: buildRoot, Error: s.provider.Error,
			FinishedAt: time.Now().UTC(),
		})
	}
	if current := repository.SCIPJava; current != nil &&
		current.State == "ready" &&
		current.Revision == revision &&
		current.Configuration == s.provider.Configuration {
		if _, ok, readErr := s.artifacts.Read(ctx, repository.ID, revision); readErr != nil {
			return readErr
		} else if ok {
			return nil
		}
	}

	now := time.Now().UTC()
	status := catalog.SCIPIndexStatus{
		Provider: "scip-java", State: "indexing", Applicable: true,
		Revision: revision, Configuration: s.provider.Configuration,
		Indexer: "scip-java", Version: s.provider.Version,
		BuildRoot: buildRoot, QueuedAt: now, StartedAt: now,
	}
	if err := s.store.UpdateSCIPIndexStatus(ctx, repository.ID, status); err != nil {
		return err
	}
	bounded, cancel := context.WithTimeout(ctx, s.config.Timeout)
	summary, buildErr := s.build(bounded, repository, revision, buildRoot)
	cancel()
	if buildErr != nil {
		return s.recordFailure(ctx, repository, revision, buildRoot, true, buildErr)
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
	revision, buildRoot string,
	applicable bool,
	cause error,
) error {
	message := sanitizeFailure(cause.Error(), repository.Path, s.config.DataDirectory)
	statusErr := s.store.UpdateSCIPIndexStatus(ctx, repository.ID, catalog.SCIPIndexStatus{
		Provider: "scip-java", State: "failed", Applicable: applicable,
		Revision: revision, Configuration: s.provider.Configuration,
		Indexer: "scip-java", Version: s.provider.Version, BuildRoot: buildRoot,
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
	revision, buildRoot string,
) (scipindex.ImportSummary, error) {
	parent, err := os.MkdirTemp(filepath.Join(s.config.DataDirectory, "worktrees"), fmt.Sprintf("%d-", repository.ID))
	if err != nil {
		return scipindex.ImportSummary{}, fmt.Errorf("create isolated worktree parent: %w", err)
	}
	worktree := filepath.Join(parent, "checkout")
	defer func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if cleanupErr := removeWorktree(cleanupContext, repository.Path, worktree); cleanupErr != nil {
			slog.Warn("remove Java SCIP worktree", "repository_id", repository.ID, "error", cleanupErr)
		}
		if removeErr := os.RemoveAll(parent); removeErr != nil {
			slog.Warn("remove Java SCIP temporary directory", "repository_id", repository.ID, "error", removeErr)
		}
	}()

	if output, addErr := runGit(ctx, repository.Path, "worktree", "add", "--detach", "--force", worktree, revision); addErr != nil {
		return scipindex.ImportSummary{}, fmt.Errorf("create exact-commit worktree: %w: %s", addErr, output)
	}
	indexDirectory := worktree
	sourceRoot := "."
	if buildRoot != "" {
		indexDirectory = filepath.Join(worktree, filepath.FromSlash(buildRoot))
		sourceRoot = buildRoot
	}
	indexPath := filepath.Join(indexDirectory, "index.scip")
	if removeErr := os.Remove(indexPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return scipindex.ImportSummary{}, fmt.Errorf("remove stale temporary SCIP output: %w", removeErr)
	}
	output, runErr := s.executor.Run(ctx, s.config.Command, indexDirectory)
	if runErr != nil {
		return scipindex.ImportSummary{}, fmt.Errorf(
			"scip-java index failed: %w: %s",
			runErr,
			sanitizeFailure(string(output), repository.Path, worktree, s.config.DataDirectory),
		)
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
) (buildRoot string, applicable bool, reason string, err error) {
	output, err := runGit(ctx, repository.Path, "ls-tree", "-r", "-z", "--name-only", revision)
	if err != nil {
		return "", false, "", fmt.Errorf("list committed files: %w", err)
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
		return "", false, "No committed Java sources.", nil
	}
	roots := settingsRoots
	if len(roots) == 0 {
		roots = buildRoots
	}
	if len(roots) == 0 {
		return "", false, "Java sources found, but no Gradle build was detected.", nil
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
			return candidate, true, "", nil
		}
	}
	return "", true, "", errors.New("multiple independent Gradle roots are not supported; index each build as a separate repository")
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
