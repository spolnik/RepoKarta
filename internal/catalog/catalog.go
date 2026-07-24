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

// Repository is a read-only description of a local Git repository.
type Repository struct {
	ID              int64
	Name            string
	Path            string
	OriginURL       string
	DefaultRevision string
	HeadCommit      string
	Bare            bool
	ScanState       string
	ScanError       string
	IndexState      string
	IndexError      string
	IndexedCommit   string
	DiscoveredAt    time.Time
	ScannedAt       time.Time
	IndexedAt       time.Time
}

// DiscoverOptions controls repository catalogue discovery.
type DiscoverOptions struct {
	Exclude []string
}

// Discover finds regular, linked-worktree, and bare Git repositories below root.
// It only uses read-only Git commands and never follows directory symlinks.
func Discover(root string) ([]Repository, error) {
	return DiscoverWithOptions(root, DiscoverOptions{})
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
		if !entry.IsDir() {
			return nil
		}
		if path != root && entry.Name() == ".git" {
			return filepath.SkipDir
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
		repository.ScanState = "error"
		repository.ScanError = err.Error()
		return repository
	}

	if revision, err := gitOutput(ctx, path, bare, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
		repository.DefaultRevision = revision
	} else {
		repository.DefaultRevision = "HEAD"
	}

	if origin, err := gitOutput(ctx, path, bare, "config", "--get", "remote.origin.url"); err == nil {
		repository.OriginURL = origin
	}

	return repository
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
