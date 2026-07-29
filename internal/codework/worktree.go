// Package codework owns persistent, RepoKarta-controlled coding worktrees.
package codework

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spolnik/RepoKarta/internal/catalog"
)

const (
	DefaultContextLines         = 3
	MaximumContextLines         = 20
	MaximumDiffBytes            = 1 << 20
	MaximumPreviewBytes         = 1 << 20
	MaximumInventoryBytes       = 4 << 20
	MaximumChangedFiles         = 5000
	MaximumWorkspaceFiles       = 100000
	MaximumWorkspaceBytes int64 = 2 << 30
	gitTimeout                  = 30 * time.Second
)

var (
	sessionIDPattern = regexp.MustCompile(`^code-[a-f0-9]{16,64}$`)
	revisionPattern  = regexp.MustCompile(`^[a-fA-F0-9]{40,64}$`)
)

// RepositoryStore resolves a permission-filtered repository record.
type RepositoryStore interface {
	RepositoryByID(context.Context, int64) (catalog.Repository, error)
}

// Config selects the durable worktree root and Git executable.
type Config struct {
	DataDirectory string
	GitCommand    string
}

// Manager creates and inspects persistent worktrees without registering them
// against a user's source repository.
type Manager struct {
	root         string
	gitCommand   string
	repositories RepositoryStore
}

// CreateRequest identifies one exact repository baseline.
type CreateRequest struct {
	ID           string
	RepositoryID int64
	Baseline     string
}

// Workspace is the non-secret identity of one owned worktree.
type Workspace struct {
	ID           string `json:"id"`
	RepositoryID int64  `json:"repository_id"`
	Repository   string `json:"repository"`
	Baseline     string `json:"baseline"`
	Branch       string `json:"branch"`
	RootPath     string `json:"-"`
	GitPath      string `json:"-"`
	CheckoutPath string `json:"-"`
}

type marker struct {
	Version      int    `json:"version"`
	ID           string `json:"id"`
	RepositoryID int64  `json:"repository_id"`
	Repository   string `json:"repository"`
	Baseline     string `json:"baseline"`
	Branch       string `json:"branch"`
}

// FileChange is one path in a workspace diff.
type FileChange struct {
	Path       string `json:"path"`
	OldPath    string `json:"old_path,omitempty"`
	Status     string `json:"status"`
	Binary     bool   `json:"binary,omitempty"`
	Insertions int    `json:"insertions,omitempty"`
	Deletions  int    `json:"deletions,omitempty"`
}

// Diff is a bounded baseline-to-worktree patch with a complete path inventory.
type Diff struct {
	Baseline      string       `json:"baseline"`
	Head          string       `json:"head"`
	Version       string       `json:"version"`
	Files         []FileChange `json:"files"`
	FilesChanged  int          `json:"files_changed"`
	Insertions    int          `json:"insertions"`
	Deletions     int          `json:"deletions"`
	Patch         string       `json:"patch"`
	Truncated     bool         `json:"truncated"`
	ReturnedBytes int          `json:"returned_bytes"`
	MaximumBytes  int          `json:"maximum_bytes"`
	ContextLines  int          `json:"context_lines"`
}

// File is a bounded workspace preview.
type File struct {
	Path          string `json:"path"`
	Content       string `json:"content,omitempty"`
	Binary        bool   `json:"binary"`
	Truncated     bool   `json:"truncated"`
	ReturnedBytes int    `json:"returned_bytes"`
	MaximumBytes  int    `json:"maximum_bytes"`
}

// FinishResult identifies the isolated local commit produced by a session.
type FinishResult struct {
	Branch string `json:"branch"`
	Commit string `json:"commit"`
}

// NewManager initializes the owned worktree directory.
func NewManager(config Config, repositories RepositoryStore) (*Manager, error) {
	if repositories == nil {
		return nil, errors.New("code worktree repository store is required")
	}
	dataDirectory := strings.TrimSpace(config.DataDirectory)
	if dataDirectory == "" {
		return nil, errors.New("code worktree data directory is required")
	}
	absolute, err := filepath.Abs(dataDirectory)
	if err != nil {
		return nil, fmt.Errorf("resolve code worktree data directory: %w", err)
	}
	root := filepath.Join(absolute, "code-worktrees")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create code worktree directory: %w", err)
	}
	command := strings.TrimSpace(config.GitCommand)
	if command == "" {
		command = "git"
	}
	return &Manager{root: filepath.Clean(root), gitCommand: command, repositories: repositories}, nil
}

// Create builds a branch-backed worktree from a per-session bare Git shadow.
func (m *Manager) Create(ctx context.Context, request CreateRequest) (workspace Workspace, resultErr error) {
	if !sessionIDPattern.MatchString(request.ID) {
		return Workspace{}, errors.New("invalid code session ID")
	}
	if request.RepositoryID <= 0 {
		return Workspace{}, errors.New("repository ID must be positive")
	}
	repository, err := m.repositories.RepositoryByID(ctx, request.RepositoryID)
	if err != nil {
		return Workspace{}, err
	}
	baseline := strings.TrimSpace(request.Baseline)
	if baseline == "" {
		baseline = strings.TrimSpace(repository.IndexedCommit)
	}
	if baseline == "" {
		baseline = strings.TrimSpace(repository.HeadCommit)
	}
	if !revisionPattern.MatchString(baseline) {
		return Workspace{}, errors.New("an exact hexadecimal baseline commit is required")
	}
	resolved, err := m.git(ctx, repository.Path, "rev-parse", "--verify", baseline+"^{commit}")
	if err != nil {
		return Workspace{}, fmt.Errorf("resolve code baseline: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(resolved), baseline) {
		return Workspace{}, errors.New("code baseline did not resolve to the requested commit")
	}

	root, err := m.sessionRoot(request.ID)
	if err != nil {
		return Workspace{}, err
	}
	if _, err := os.Lstat(root); err == nil {
		return Workspace{}, errors.New("code worktree already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return Workspace{}, err
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		return Workspace{}, fmt.Errorf("create code worktree root: %w", err)
	}
	defer func() {
		if resultErr != nil {
			_ = os.RemoveAll(root)
		}
	}()

	gitPath := filepath.Join(root, "repository.git")
	checkoutPath := filepath.Join(root, "checkout")
	hooksPath := filepath.Join(root, "hooks-disabled")
	if err := os.Mkdir(hooksPath, 0o700); err != nil {
		return Workspace{}, fmt.Errorf("create disabled hooks directory: %w", err)
	}
	if _, err := m.git(ctx, "", "init", "--bare", gitPath); err != nil {
		return Workspace{}, fmt.Errorf("initialize code Git shadow: %w", err)
	}
	refspec := "+" + baseline + ":refs/repokarta/baseline"
	if _, err := m.gitWithDirectory(ctx, "", gitPath,
		"fetch", "--no-tags", "--force", "--", repository.Path, refspec); err != nil {
		return Workspace{}, fmt.Errorf("copy code baseline into Git shadow: %w", err)
	}
	branch := "repokarta/code/" + strings.TrimPrefix(request.ID, "code-")
	if _, err := m.gitWithDirectory(ctx, "", gitPath,
		"worktree", "add", "-b", branch, checkoutPath, "refs/repokarta/baseline"); err != nil {
		return Workspace{}, fmt.Errorf("create code worktree: %w", err)
	}
	for _, setting := range [][2]string{
		{"user.name", "RepoKarta Code"},
		{"user.email", "repokarta-code@localhost"},
		{"core.hooksPath", hooksPath},
	} {
		if _, err := m.git(ctx, checkoutPath, "config", "--local", setting[0], setting[1]); err != nil {
			return Workspace{}, fmt.Errorf("configure code worktree: %w", err)
		}
	}
	workspace = Workspace{
		ID: request.ID, RepositoryID: repository.ID, Repository: repository.Name,
		Baseline: baseline, Branch: branch, RootPath: root,
		GitPath: gitPath, CheckoutPath: checkoutPath,
	}
	encoded, err := json.Marshal(marker{
		Version: 1, ID: workspace.ID, RepositoryID: workspace.RepositoryID,
		Repository: workspace.Repository, Baseline: workspace.Baseline, Branch: workspace.Branch,
	})
	if err != nil {
		return Workspace{}, err
	}
	if err := os.WriteFile(filepath.Join(root, "ownership.json"), encoded, 0o600); err != nil {
		return Workspace{}, fmt.Errorf("record code worktree ownership: %w", err)
	}
	return workspace, nil
}

// Workspace loads and validates an existing owned worktree.
func (m *Manager) Workspace(id string) (Workspace, error) {
	root, err := m.sessionRoot(id)
	if err != nil {
		return Workspace{}, err
	}
	info, err := os.Lstat(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Workspace{}, errors.New("code worktree was not found")
		}
		return Workspace{}, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return Workspace{}, errors.New("code worktree root is not an owned directory")
	}
	encoded, err := os.ReadFile(filepath.Join(root, "ownership.json"))
	if err != nil {
		return Workspace{}, errors.New("code worktree ownership proof is missing")
	}
	var proof marker
	if json.Unmarshal(encoded, &proof) != nil || proof.Version != 1 ||
		proof.ID != id || proof.RepositoryID <= 0 ||
		!revisionPattern.MatchString(proof.Baseline) ||
		!strings.HasPrefix(proof.Branch, "repokarta/code/") {
		return Workspace{}, errors.New("code worktree ownership proof is invalid")
	}
	gitPath := filepath.Join(root, "repository.git")
	checkoutPath := filepath.Join(root, "checkout")
	for _, target := range []string{gitPath, checkoutPath} {
		resolved, err := filepath.EvalSymlinks(target)
		if err != nil {
			return Workspace{}, errors.New("code worktree is incomplete")
		}
		if !pathWithin(root, resolved) {
			return Workspace{}, errors.New("code worktree escaped its owned root")
		}
	}
	return Workspace{
		ID: id, RepositoryID: proof.RepositoryID, Repository: proof.Repository,
		Baseline: proof.Baseline, Branch: proof.Branch, RootPath: root,
		GitPath: gitPath, CheckoutPath: checkoutPath,
	}, nil
}

// Diff returns the current baseline-to-working-tree diff.
func (m *Manager) Diff(ctx context.Context, id string, contextLines int) (Diff, error) {
	workspace, err := m.Workspace(id)
	if err != nil {
		return Diff{}, err
	}
	if err := validateWorkspaceQuota(
		workspace.CheckoutPath, MaximumWorkspaceBytes, MaximumWorkspaceFiles,
	); err != nil {
		return Diff{}, err
	}
	if contextLines <= 0 {
		contextLines = DefaultContextLines
	}
	if contextLines > MaximumContextLines {
		return Diff{}, fmt.Errorf("diff context must not exceed %d lines", MaximumContextLines)
	}
	changes, err := m.changes(ctx, workspace)
	if err != nil {
		return Diff{}, err
	}
	patch, truncated, err := m.gitBounded(
		ctx, workspace.CheckoutPath, MaximumDiffBytes,
		"diff", "--no-ext-diff", "--no-textconv", "--find-renames",
		fmt.Sprintf("--unified=%d", contextLines), workspace.Baseline, "--",
	)
	if err != nil {
		return Diff{}, fmt.Errorf("read code worktree diff: %w", err)
	}
	buffer := newBoundedText(MaximumDiffBytes)
	buffer.WriteString(patch)
	if len(patch) > 0 && !strings.HasSuffix(patch, "\n") {
		buffer.WriteString("\n")
	}
	for index := range changes {
		if changes[index].Status != "untracked" {
			continue
		}
		additions, binary, err := appendUntrackedPatch(
			buffer, workspace.CheckoutPath, changes[index].Path, contextLines,
		)
		if err != nil {
			return Diff{}, err
		}
		changes[index].Insertions = additions
		changes[index].Binary = binary
	}
	truncated = truncated || buffer.Truncated()
	insertions, deletions := countPatchLines(buffer.String())
	for index := range changes {
		if changes[index].Status == "untracked" {
			continue
		}
		changeInsertions, changeDeletions, _ := m.fileStats(ctx, workspace, changes[index].Path)
		changes[index].Insertions = changeInsertions
		changes[index].Deletions = changeDeletions
	}
	head, err := m.git(ctx, workspace.CheckoutPath, "rev-parse", "HEAD")
	if err != nil {
		return Diff{}, err
	}
	version, err := diffVersion(workspace, changes)
	if err != nil {
		return Diff{}, err
	}
	return Diff{
		Baseline: workspace.Baseline, Head: strings.TrimSpace(head), Version: version,
		Files: changes, FilesChanged: len(changes), Insertions: insertions, Deletions: deletions,
		Patch: buffer.String(), Truncated: truncated, ReturnedBytes: buffer.Len(),
		MaximumBytes: MaximumDiffBytes, ContextLines: contextLines,
	}, nil
}

// ReadFile returns a bounded workspace file after canonical containment checks.
func (m *Manager) ReadFile(_ context.Context, id, path string, maximumBytes int) (File, error) {
	workspace, err := m.Workspace(id)
	if err != nil {
		return File{}, err
	}
	cleaned, target, err := safeWorkspacePath(workspace.CheckoutPath, path)
	if err != nil {
		return File{}, err
	}
	if maximumBytes <= 0 || maximumBytes > MaximumPreviewBytes {
		maximumBytes = MaximumPreviewBytes
	}
	file, err := os.Open(target)
	if err != nil {
		return File{}, err
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, int64(maximumBytes+1)))
	if err != nil {
		return File{}, err
	}
	truncated := len(content) > maximumBytes
	if truncated {
		content = content[:maximumBytes]
	}
	binary := bytes.IndexByte(content, 0) >= 0 || !utf8.Valid(content)
	result := File{
		Path: cleaned, Binary: binary, Truncated: truncated,
		ReturnedBytes: len(content), MaximumBytes: maximumBytes,
	}
	if !binary {
		result.Content = string(content)
	}
	return result, nil
}

// DiscardFile restores one changed path to the immutable baseline.
func (m *Manager) DiscardFile(ctx context.Context, id, path, expectedVersion string) (Diff, error) {
	workspace, err := m.Workspace(id)
	if err != nil {
		return Diff{}, err
	}
	current, err := m.Diff(ctx, id, DefaultContextLines)
	if err != nil {
		return Diff{}, err
	}
	if strings.TrimSpace(expectedVersion) == "" || current.Version != expectedVersion {
		return Diff{}, errors.New("code diff changed; refresh before discarding")
	}
	cleaned, target, err := safeWorkspacePath(workspace.CheckoutPath, path)
	if err != nil {
		return Diff{}, err
	}
	var change *FileChange
	for index := range current.Files {
		if current.Files[index].Path == cleaned {
			change = &current.Files[index]
			break
		}
	}
	if change == nil {
		return Diff{}, errors.New("changed code path was not found")
	}
	if change.Status == "untracked" || change.Status == "added" {
		if _, err := m.git(ctx, workspace.CheckoutPath, "rm", "--cached", "--ignore-unmatch", "--", cleaned); err != nil {
			return Diff{}, err
		}
		if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return Diff{}, err
		}
	} else {
		if _, err := m.git(ctx, workspace.CheckoutPath,
			"restore", "--source="+workspace.Baseline, "--staged", "--worktree", "--", cleaned); err != nil {
			return Diff{}, err
		}
	}
	return m.Diff(ctx, id, current.ContextLines)
}

// Finish creates a local commit on the isolated branch after a stale-diff check.
func (m *Manager) Finish(ctx context.Context, id, message, expectedVersion string) (FinishResult, error) {
	workspace, err := m.Workspace(id)
	if err != nil {
		return FinishResult{}, err
	}
	current, err := m.Diff(ctx, id, DefaultContextLines)
	if err != nil {
		return FinishResult{}, err
	}
	if strings.TrimSpace(expectedVersion) == "" || current.Version != expectedVersion {
		return FinishResult{}, errors.New("code diff changed; refresh before finishing")
	}
	if current.FilesChanged == 0 {
		return FinishResult{}, errors.New("code worktree has no changes to finish")
	}
	message = strings.Join(strings.Fields(strings.TrimSpace(message)), " ")
	if message == "" {
		message = "Apply RepoKarta Code changes"
	}
	if len(message) > 200 {
		message = message[:200]
	}
	if _, err := m.git(ctx, workspace.CheckoutPath, "add", "-A", "--", "."); err != nil {
		return FinishResult{}, fmt.Errorf("stage code worktree: %w", err)
	}
	if _, err := m.git(ctx, workspace.CheckoutPath,
		"-c", "commit.gpgSign=false", "commit", "--no-verify", "-m", message); err != nil {
		return FinishResult{}, fmt.Errorf("commit code worktree: %w", err)
	}
	commit, err := m.git(ctx, workspace.CheckoutPath, "rev-parse", "HEAD")
	if err != nil {
		return FinishResult{}, err
	}
	return FinishResult{Branch: workspace.Branch, Commit: strings.TrimSpace(commit)}, nil
}

// Remove deletes only a worktree with a valid proof inside the owned root.
func (m *Manager) Remove(ctx context.Context, id string) error {
	workspace, err := m.Workspace(id)
	if err != nil {
		return err
	}
	if _, err := m.gitWithDirectory(ctx, "", workspace.GitPath,
		"worktree", "remove", "--force", workspace.CheckoutPath); err != nil {
		return fmt.Errorf("remove code worktree registration: %w", err)
	}
	if !pathWithin(m.root, workspace.RootPath) {
		return errors.New("refusing to remove code worktree outside owned root")
	}
	if err := os.RemoveAll(workspace.RootPath); err != nil {
		return fmt.Errorf("remove code worktree: %w", err)
	}
	return nil
}

func (m *Manager) changes(ctx context.Context, workspace Workspace) ([]FileChange, error) {
	output, truncated, err := m.gitBounded(ctx, workspace.CheckoutPath, MaximumInventoryBytes,
		"diff", "--name-status", "-z", "--find-renames", workspace.Baseline, "--")
	if err != nil {
		return nil, err
	}
	if truncated {
		return nil, errors.New("code change inventory exceeds its response limit")
	}
	fields := strings.Split(output, "\x00")
	var changes []FileChange
	for index := 0; index < len(fields) && fields[index] != ""; {
		statusCode := fields[index]
		index++
		if index >= len(fields) {
			break
		}
		change := FileChange{Status: changeStatus(statusCode)}
		if strings.HasPrefix(statusCode, "R") || strings.HasPrefix(statusCode, "C") {
			change.OldPath = filepath.ToSlash(fields[index])
			index++
			if index >= len(fields) {
				break
			}
		}
		change.Path = filepath.ToSlash(fields[index])
		index++
		changes = append(changes, change)
		if len(changes) > MaximumChangedFiles {
			return nil, fmt.Errorf("code workspace exceeds %d changed files", MaximumChangedFiles)
		}
	}
	untracked, truncated, err := m.gitBounded(ctx, workspace.CheckoutPath, MaximumInventoryBytes,
		"ls-files", "--others", "--exclude-standard", "-z", "--")
	if err != nil {
		return nil, err
	}
	if truncated {
		return nil, errors.New("untracked code inventory exceeds its response limit")
	}
	for _, path := range strings.Split(untracked, "\x00") {
		if path = strings.TrimSpace(path); path != "" {
			changes = append(changes, FileChange{Path: filepath.ToSlash(path), Status: "untracked"})
			if len(changes) > MaximumChangedFiles {
				return nil, fmt.Errorf("code workspace exceeds %d changed files", MaximumChangedFiles)
			}
		}
	}
	return changes, nil
}

func validateWorkspaceQuota(root string, maximumBytes int64, maximumFiles int) error {
	var bytesUsed int64
	filesUsed := 0
	err := filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		filesUsed++
		bytesUsed += info.Size()
		if filesUsed > maximumFiles {
			return fmt.Errorf("code workspace exceeds %d files", maximumFiles)
		}
		if bytesUsed > maximumBytes {
			return fmt.Errorf("code workspace exceeds %d bytes", maximumBytes)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("validate code workspace quota: %w", err)
	}
	return nil
}

func (m *Manager) fileStats(ctx context.Context, workspace Workspace, path string) (int, int, error) {
	output, err := m.git(ctx, workspace.CheckoutPath,
		"diff", "--numstat", workspace.Baseline, "--", path)
	if err != nil {
		return 0, 0, err
	}
	fields := strings.Fields(output)
	if len(fields) < 2 || fields[0] == "-" || fields[1] == "-" {
		return 0, 0, nil
	}
	additions, _ := strconv.Atoi(fields[0])
	deletions, _ := strconv.Atoi(fields[1])
	return additions, deletions, nil
}

func (m *Manager) sessionRoot(id string) (string, error) {
	if !sessionIDPattern.MatchString(id) {
		return "", errors.New("invalid code session ID")
	}
	root := filepath.Join(m.root, id)
	if !pathWithin(m.root, root) {
		return "", errors.New("code worktree path escaped its root")
	}
	return root, nil
}

func (m *Manager) git(ctx context.Context, directory string, arguments ...string) (string, error) {
	return m.gitWithDirectory(ctx, directory, "", arguments...)
}

func (m *Manager) gitWithDirectory(
	ctx context.Context,
	directory, gitDirectory string,
	arguments ...string,
) (string, error) {
	bounded, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	if gitDirectory != "" {
		arguments = append([]string{"--git-dir", gitDirectory}, arguments...)
	}
	command := exec.CommandContext(bounded, m.gitCommand, arguments...)
	command.Dir = directory
	command.Env = gitEnvironment()
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s",
			strings.Join(arguments, " "), err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func (m *Manager) gitBounded(
	ctx context.Context,
	directory string,
	limit int,
	arguments ...string,
) (string, bool, error) {
	bounded, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	command := exec.CommandContext(bounded, m.gitCommand, arguments...)
	command.Dir = directory
	command.Env = gitEnvironment()
	output := newBoundedText(limit)
	var stderr bytes.Buffer
	command.Stdout = output
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return "", output.Truncated(), fmt.Errorf(
			"git %s: %w: %s", strings.Join(arguments, " "), err, strings.TrimSpace(stderr.String()),
		)
	}
	return output.String(), output.Truncated(), nil
}

type boundedText struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func newBoundedText(limit int) *boundedText {
	return &boundedText{limit: limit}
}

func (b *boundedText) Write(value []byte) (int, error) {
	original := len(value)
	remaining := b.limit - b.buffer.Len()
	if remaining <= 0 {
		b.truncated = b.truncated || original > 0
		return original, nil
	}
	if len(value) > remaining {
		value = value[:remaining]
		b.truncated = true
	}
	_, _ = b.buffer.Write(value)
	return original, nil
}

func (b *boundedText) WriteString(value string) {
	_, _ = b.Write([]byte(value))
}

func (b *boundedText) String() string { return b.buffer.String() }
func (b *boundedText) Len() int       { return b.buffer.Len() }
func (b *boundedText) Truncated() bool {
	return b.truncated
}

func appendUntrackedPatch(
	output *boundedText,
	checkout, path string,
	_ int,
) (int, bool, error) {
	_, target, err := safeWorkspacePath(checkout, path)
	if err != nil {
		return 0, false, err
	}
	file, err := os.Open(target)
	if err != nil {
		return 0, false, err
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, MaximumDiffBytes+1))
	if err != nil {
		return 0, false, err
	}
	output.WriteString("diff --git a/" + path + " b/" + path + "\n")
	output.WriteString("new file mode 100644\n--- /dev/null\n+++ b/" + path + "\n")
	if len(content) > MaximumDiffBytes || bytes.IndexByte(content, 0) >= 0 || !utf8.Valid(content) {
		output.WriteString("Binary files /dev/null and b/" + path + " differ\n")
		return 0, true, nil
	}
	lines := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
	if len(content) == 0 {
		lines = nil
	}
	output.WriteString(fmt.Sprintf("@@ -0,0 +1,%d @@\n", len(lines)))
	for _, line := range lines {
		output.WriteString("+" + line + "\n")
	}
	return len(lines), false, nil
}

func countPatchLines(patch string) (int, int) {
	var additions, deletions int
	for line := range strings.SplitSeq(patch, "\n") {
		switch {
		case strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---"):
		case strings.HasPrefix(line, "+"):
			additions++
		case strings.HasPrefix(line, "-"):
			deletions++
		}
	}
	return additions, deletions
}

func changeStatus(value string) string {
	switch value[0] {
	case 'A':
		return "added"
	case 'D':
		return "deleted"
	case 'R':
		return "renamed"
	case 'C':
		return "copied"
	case 'T':
		return "type_changed"
	default:
		return "modified"
	}
}

func safeWorkspacePath(checkout, value string) (string, string, error) {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if value == "" {
		return "", "", errors.New("workspace path is required")
	}
	cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(value)))
	if cleaned != value || filepath.IsAbs(value) || cleaned == ".." ||
		strings.HasPrefix(cleaned, "../") || strings.ContainsRune(cleaned, 0) ||
		cleaned == ".git" || strings.HasPrefix(cleaned, ".git/") {
		return "", "", errors.New("unsafe workspace path")
	}
	target := filepath.Join(checkout, filepath.FromSlash(cleaned))
	resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(target))
	if err != nil {
		return "", "", errors.New("workspace path parent was not found")
	}
	if !pathWithin(checkout, resolvedParent) {
		return "", "", errors.New("workspace path escaped its checkout")
	}
	if _, err := os.Lstat(target); err == nil {
		resolvedTarget, resolveErr := filepath.EvalSymlinks(target)
		if resolveErr != nil || !pathWithin(checkout, resolvedTarget) {
			return "", "", errors.New("workspace path escaped its checkout")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", errors.New("workspace path could not be inspected")
	}
	return cleaned, target, nil
}

func pathWithin(root, target string) bool {
	rootAbsolute, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	targetAbsolute, err := filepath.Abs(target)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(filepath.Clean(rootAbsolute), filepath.Clean(targetAbsolute))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))
}

func diffVersion(workspace Workspace, changes []FileChange) (string, error) {
	hash := sha256.New()
	_, _ = io.WriteString(hash, workspace.Baseline+"\x00")
	for _, change := range changes {
		_, _ = io.WriteString(hash, change.Status+"\x00"+change.OldPath+"\x00"+change.Path+"\x00")
		if change.Status == "deleted" {
			continue
		}
		_, target, err := safeWorkspacePath(workspace.CheckoutPath, change.Path)
		if err != nil {
			return "", err
		}
		file, err := os.Open(target)
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(hash, file); err != nil {
			file.Close()
			return "", err
		}
		file.Close()
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func gitEnvironment() []string {
	environment := os.Environ()
	environment = append(environment,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_OPTIONAL_LOCKS=1",
	)
	if runtime.GOOS == "windows" {
		environment = append(environment, "GCM_INTERACTIVE=Never")
	}
	return environment
}
