package catalog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const gitCommandTimeout = 10 * time.Second

const (
	// EmptyRepositoryReason is deliberately stable across the catalogue, API,
	// and UI so an unborn Git repository is reported as a terminal condition
	// instead of looking like indexing work that will eventually start.
	EmptyRepositoryReason = "Nothing to index: repository has no commits."
)

// Repository is a read-only description of a local Git repository.
type Repository struct {
	ID              int64
	AcquisitionID   int64
	Name            string
	Path            string
	OriginURL       string
	DefaultRevision string
	HeadCommit      string
	IndexRevision   string
	IndexCommit     string
	Bare            bool
	ScanState       string
	ScanError       string
	IndexState      string
	IndexError      string
	IndexedCommit   string
	DiscoveredAt    time.Time
	ScannedAt       time.Time
	IndexedAt       time.Time
	SCIPJava        *SCIPIndexStatus
}

// SCIPIndexStatus is the durable state of one optional compiler-precise
// repository artifact. It is kept separate from IndexState because a failed
// language build must never make ordinary source search unavailable.
type SCIPIndexStatus struct {
	Provider            string    `json:"provider"`
	State               string    `json:"state"`
	Applicable          bool      `json:"applicable"`
	Revision            string    `json:"revision,omitempty"`
	Configuration       string    `json:"-"`
	Indexer             string    `json:"indexer,omitempty"`
	Version             string    `json:"version,omitempty"`
	BuildRoot           string    `json:"build_root,omitempty"`
	GradleVersion       string    `json:"gradle_version,omitempty"`
	RequestedJDKVersion int       `json:"requested_jdk_version,omitempty"`
	JDKVersion          int       `json:"jdk_version,omitempty"`
	JDKSource           string    `json:"jdk_source,omitempty"`
	Documents           int       `json:"documents,omitempty"`
	Symbols             int       `json:"symbols,omitempty"`
	Occurrences         int       `json:"occurrences,omitempty"`
	FailureCategory     string    `json:"failure_category,omitempty"`
	FailureSummary      string    `json:"failure_summary,omitempty"`
	Error               string    `json:"error,omitempty"`
	QueuedAt            time.Time `json:"queued_at,omitempty"`
	StartedAt           time.Time `json:"started_at,omitempty"`
	FinishedAt          time.Time `json:"finished_at,omitempty"`
}

// DiscoverOptions controls repository catalogue discovery.
type DiscoverOptions struct {
	Exclude []string
}

// Discover finds regular and bare Git repositories below root. A linked
// worktree is included only when it is the explicit root; linked worktrees
// encountered during a broader parent scan are skipped to avoid cataloguing
// several checkouts of the same repository as independent repositories.
// Discovery only uses read-only Git commands and never follows directory
// symlinks.
func Discover(root string) ([]Repository, error) {
	return DiscoverWithOptions(root, DiscoverOptions{})
}

// Inspect returns the read-only Git metadata for exactly one repository path.
// It does not search nested directories and never mutates the worktree.
func Inspect(path string) (Repository, error) {
	path, err := canonicalDirectory(path)
	if err != nil {
		return Repository{}, err
	}
	repositoryPath, bare, found, err := repositoryAt(path)
	if err != nil {
		return Repository{}, fmt.Errorf("inspect Git repository %q: %w", path, err)
	}
	if !found || !samePath(repositoryPath, path) {
		return Repository{}, fmt.Errorf("%q is not a Git repository", path)
	}
	repository := inspectRepository(repositoryPath, bare)
	if repository.ScanState == "error" {
		return Repository{}, errors.New(repository.ScanError)
	}
	return repository, nil
}

// DiscoverWithOptions finds repositories while pruning excluded directories.
func DiscoverWithOptions(root string, options DiscoverOptions) ([]Repository, error) {
	root, err := canonicalDirectory(root)
	if err != nil {
		return nil, err
	}

	excludes, err := canonicalExcludes(root, options.Exclude)
	if err != nil {
		return nil, err
	}

	var repositories []Repository
	// ignoreScopes holds the .gitignore rules of every directory that encloses
	// the entry being visited, so a directory a repository already ignores is
	// never searched for nested repositories.
	var ignoreScopes []ignoreScope
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrPermission) {
				return filepath.SkipDir
			}
			return walkErr
		}

		if entry.Type()&os.ModeSymlink != 0 && entry.IsDir() {
			return filepath.SkipDir
		}
		if entry.IsDir() && excluded(path, excludes) {
			return filepath.SkipDir
		}
		// Hidden directories, including .git internals and hidden worktrees
		// such as .jobguard-wt, are never repository roots RepoKarta manages.
		if entry.IsDir() && path != root && strings.HasPrefix(entry.Name(), ".") {
			return filepath.SkipDir
		}
		if !entry.IsDir() {
			return nil
		}
		if path != root && linkedWorktreeAt(path) {
			return filepath.SkipDir
		}

		ignoreScopes = activeIgnoreScopes(ignoreScopes, path)
		if path != root && ignoredDirectory(ignoreScopes, path) {
			return filepath.SkipDir
		}
		if scope, ok := loadIgnoreScope(path); ok {
			ignoreScopes = append(ignoreScopes, scope)
		}

		repositoryPath, bare, found, err := repositoryAt(path)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}

		repository := inspectRepository(repositoryPath, bare)
		repositories = append(repositories, repository)
		if samePath(repositoryPath, root) {
			return nil
		}
		return filepath.SkipDir
	})
	if err != nil {
		return nil, fmt.Errorf("discover Git repositories below %q: %w", root, err)
	}

	slices.SortFunc(repositories, func(left, right Repository) int {
		if compared := strings.Compare(strings.ToLower(left.Name), strings.ToLower(right.Name)); compared != 0 {
			return compared
		}
		return strings.Compare(strings.ToLower(left.Path), strings.ToLower(right.Path))
	})

	return repositories, nil
}

func linkedWorktreeAt(path string) bool {
	gitPath := filepath.Join(path, ".git")
	info, err := os.Lstat(gitPath)
	return err == nil && info.Mode().IsRegular() && validGitFile(gitPath)
}

func canonicalDirectory(root string) (string, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve repository root %q: %w", root, err)
	}
	absolute = filepath.Clean(absolute)

	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("inspect repository root %q: %w", absolute, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("repository root %q is not a directory", absolute)
	}

	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve repository root symlinks %q: %w", absolute, err)
	}
	return filepath.Clean(resolved), nil
}

func canonicalExcludes(root string, patterns []string) ([]string, error) {
	excludes := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		if !filepath.IsAbs(pattern) {
			pattern = filepath.Join(root, pattern)
		}
		absolute, err := filepath.Abs(pattern)
		if err != nil {
			return nil, fmt.Errorf("resolve excluded path %q: %w", pattern, err)
		}
		excludes = append(excludes, filepath.Clean(absolute))
	}
	return excludes, nil
}

func excluded(path string, excludes []string) bool {
	path = filepath.Clean(path)
	for _, exclude := range excludes {
		if samePath(path, exclude) {
			return true
		}
	}
	return false
}

func samePath(left, right string) bool {
	if strings.EqualFold(left, right) {
		return true
	}
	relative, err := filepath.Rel(right, left)
	return err == nil && relative == "."
}

func repositoryAt(path string) (repositoryPath string, bare, found bool, err error) {
	gitPath := filepath.Join(path, ".git")
	if info, statErr := os.Lstat(gitPath); statErr == nil {
		if info.IsDir() && validGitDirectory(gitPath) {
			return path, false, true, nil
		}
		if info.Mode().IsRegular() && validGitFile(gitPath) {
			return path, false, true, nil
		}
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return "", false, false, statErr
	}

	if !strings.HasSuffix(strings.ToLower(filepath.Base(path)), ".git") {
		return "", false, false, nil
	}
	head, headErr := os.Stat(filepath.Join(path, "HEAD"))
	objects, objectsErr := os.Stat(filepath.Join(path, "objects"))
	if headErr == nil && head.Mode().IsRegular() && objectsErr == nil && objects.IsDir() {
		return path, true, true, nil
	}
	return "", false, false, nil
}

func validGitDirectory(gitPath string) bool {
	head, headErr := os.Stat(filepath.Join(gitPath, "HEAD"))
	objects, objectsErr := os.Stat(filepath.Join(gitPath, "objects"))
	return headErr == nil && head.Mode().IsRegular() && objectsErr == nil && objects.IsDir()
}

func validGitFile(gitPath string) bool {
	content, err := os.ReadFile(gitPath)
	if err != nil || len(content) > 4096 {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(string(content)), "gitdir:")
}

func inspectRepository(path string, bare bool) Repository {
	now := time.Now().UTC()
	repository := Repository{
		Name:         strings.TrimSuffix(filepath.Base(path), ".git"),
		Path:         path,
		Bare:         bare,
		ScanState:    "ready",
		IndexState:   "pending",
		DiscoveredAt: now,
		ScannedAt:    now,
	}

	ctx, cancel := context.WithTimeout(context.Background(), gitCommandTimeout)
	defer cancel()

	if head, err := gitOutput(ctx, path, bare, "rev-parse", "--verify", "HEAD"); err == nil {
		repository.HeadCommit = head
	} else {
		commit, commitErr := gitOutput(ctx, path, bare, "rev-list", "--all", "--max-count=1")
		if commitErr == nil && commit == "" {
			repository.ScanState = "empty"
			repository.ScanError = EmptyRepositoryReason
			repository.IndexState = "empty"
			repository.IndexError = EmptyRepositoryReason
		} else {
			repository.ScanState = "error"
			repository.ScanError = err.Error()
			repository.IndexState = "error"
			repository.IndexError = repository.ScanError
		}
	}

	repository.DefaultRevision, repository.IndexCommit = configuredDefaultRevision(
		ctx,
		path,
		bare,
		repository.HeadCommit,
	)
	repository.IndexRevision = repository.DefaultRevision

	if origin, err := gitOutput(ctx, path, bare, "config", "--get", "remote.origin.url"); err == nil {
		repository.OriginURL = origin
	}

	return repository
}

// configuredDefaultRevision resolves the configured default branch without
// checking it out or otherwise mutating the repository. A configured
// origin/HEAD wins over the current worktree branch; repositories without a
// remote default safely fall back to their current HEAD.
func configuredDefaultRevision(
	ctx context.Context,
	repositoryPath string,
	bare bool,
	headCommit string,
) (string, string) {
	currentRevision, _ := gitOutput(
		ctx,
		repositoryPath,
		bare,
		"symbolic-ref",
		"--quiet",
		"--short",
		"HEAD",
	)
	if target, err := gitOutput(
		ctx,
		repositoryPath,
		bare,
		"symbolic-ref",
		"--quiet",
		"refs/remotes/origin/HEAD",
	); err == nil {
		target = strings.TrimSpace(target)
		name := strings.TrimPrefix(target, "refs/remotes/origin/")
		if name != "" {
			if commit, commitErr := gitOutput(
				ctx,
				repositoryPath,
				bare,
				"rev-parse",
				"--verify",
				"refs/heads/"+name+"^{commit}",
			); commitErr == nil {
				return name, commit
			}
		}
		if commit, commitErr := gitOutput(
			ctx,
			repositoryPath,
			bare,
			"rev-parse",
			"--verify",
			target+"^{commit}",
		); commitErr == nil {
			if name != "" {
				return name, commit
			}
		}
	}
	if currentRevision = strings.TrimSpace(currentRevision); currentRevision != "" {
		return currentRevision, headCommit
	}
	return "HEAD", headCommit
}

func gitOutput(ctx context.Context, path string, bare bool, arguments ...string) (string, error) {
	commandArguments := make([]string, 0, len(arguments)+2)
	if bare {
		commandArguments = append(commandArguments, "--git-dir", path)
	} else {
		commandArguments = append(commandArguments, "-C", path)
	}
	commandArguments = append(commandArguments, arguments...)

	command := exec.CommandContext(ctx, "git", commandArguments...)
	command.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(arguments, " "), message)
	}
	return strings.TrimSpace(string(output)), nil
}

// DisplayNames returns a stable, unique label for every repository keyed by
// repository ID. Repositories whose names collide are disambiguated with the
// shortest distinguishing parent-directory suffix, so pickers never show two
// identical entries. When paths cannot separate them the repository ID is
// appended as a final stable tiebreaker.
func DisplayNames(repositories []Repository) map[int64]string {
	counts := make(map[string]int, len(repositories))
	for _, repository := range repositories {
		counts[strings.ToLower(repository.Name)]++
	}
	labels := make(map[int64]string, len(repositories))
	used := make(map[string]int64, len(repositories))
	for _, repository := range repositories {
		label := repository.Name
		if counts[strings.ToLower(repository.Name)] > 1 {
			label = repository.Name + " · " + repositoryQualifier(repository)
		}
		if owner, taken := used[strings.ToLower(label)]; taken && owner != repository.ID {
			label = fmt.Sprintf("%s (#%d)", label, repository.ID)
		}
		used[strings.ToLower(label)] = repository.ID
		labels[repository.ID] = label
	}
	return labels
}

// repositoryQualifier describes where a repository lives well enough to tell it
// apart from a same-named sibling.
func repositoryQualifier(repository Repository) string {
	segments := strings.Split(filepath.ToSlash(repository.Path), "/")
	for index := len(segments) - 2; index >= 0; index-- {
		if segment := strings.TrimSpace(segments[index]); segment != "" && segment != "." {
			return segment
		}
	}
	if branch := strings.TrimSpace(repository.DefaultRevision); branch != "" {
		return branch
	}
	return filepath.ToSlash(repository.Path)
}
