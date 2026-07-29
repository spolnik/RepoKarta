package codework

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spolnik/RepoKarta/internal/catalog"
)

type fixtureRepositories struct {
	repository catalog.Repository
}

func (f fixtureRepositories) RepositoryByID(_ context.Context, id int64) (catalog.Repository, error) {
	repository := f.repository
	repository.ID = id
	return repository, nil
}

func TestManagerCreatesOwnedWorktreeAndLeavesSourceUntouched(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	runGit(t, "", "init", source)
	runGit(t, source, "config", "user.email", "fixture@example.test")
	runGit(t, source, "config", "user.name", "Fixture")
	if err := os.WriteFile(filepath.Join(source, "main.go"), []byte("package fixture\n\nconst Value = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "add", "main.go")
	runGit(t, source, "commit", "-m", "baseline")
	baseline := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))
	sourceWorktrees := runGit(t, source, "worktree", "list", "--porcelain")
	sourceStatus := runGit(t, source, "status", "--porcelain=v1")

	dataDirectory := filepath.Join(t.TempDir(), "data")
	manager, err := NewManager(Config{
		DataDirectory: dataDirectory,
		GitCommand:    "git",
	}, fixtureRepositories{repository: catalog.Repository{
		ID:            7,
		Name:          "fixture",
		Path:          source,
		IndexedCommit: baseline,
	}})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := manager.Create(t.Context(), CreateRequest{
		ID:           "code-0123456789abcdef",
		RepositoryID: 7,
		Baseline:     baseline,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Remove(context.Background(), workspace.ID) })

	if !strings.HasPrefix(
		strings.ToLower(filepath.Clean(workspace.CheckoutPath)),
		strings.ToLower(filepath.Clean(dataDirectory))+string(os.PathSeparator),
	) {
		t.Fatalf("checkout escaped data directory: %s", workspace.CheckoutPath)
	}
	if workspace.Baseline != baseline || !strings.HasPrefix(workspace.Branch, "repokarta/code/") {
		t.Fatalf("workspace identity = %#v", workspace)
	}
	if got := runGit(t, source, "worktree", "list", "--porcelain"); got != sourceWorktrees {
		t.Fatalf("source worktree registrations changed:\n%s\nwant:\n%s", got, sourceWorktrees)
	}

	if err := os.WriteFile(filepath.Join(workspace.CheckoutPath, "main.go"), []byte("package fixture\n\nconst Value = 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace.CheckoutPath, "new.go"), []byte("package fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	diff, err := manager.Diff(t.Context(), workspace.ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if diff.Version == "" || diff.FilesChanged != 2 ||
		!strings.Contains(diff.Patch, "const Value = 2") ||
		!containsChange(diff.Files, "main.go", "modified") ||
		!containsChange(diff.Files, "new.go", "untracked") {
		t.Fatalf("working diff = %#v", diff)
	}
	if _, err := manager.ReadFile(t.Context(), workspace.ID, "../outside", 1024); err == nil {
		t.Fatal("unsafe workspace path was accepted")
	}
	outside := filepath.Join(t.TempDir(), "outside-secret.txt")
	if err := os.WriteFile(outside, []byte("must-not-be-readable"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(workspace.CheckoutPath, "escaped-link.txt")
	if err := os.Symlink(outside, link); err == nil {
		if _, err := manager.ReadFile(t.Context(), workspace.ID, "escaped-link.txt", 1024); err == nil {
			t.Fatal("workspace file symlink escaped the checkout")
		}
		if _, err := manager.Diff(t.Context(), workspace.ID, 3); err == nil {
			t.Fatal("diff followed a workspace symlink outside the checkout")
		}
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
	}

	result, err := manager.Finish(t.Context(), workspace.ID, "Implement fixture change", diff.Version)
	if err != nil {
		t.Fatal(err)
	}
	if result.Commit == "" || result.Branch != workspace.Branch {
		t.Fatalf("finish result = %#v", result)
	}
	if got := runGit(t, source, "status", "--porcelain=v1"); got != sourceStatus {
		t.Fatalf("source status changed: %q, want %q", got, sourceStatus)
	}
	content, err := os.ReadFile(filepath.Join(source, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "Value = 2") {
		t.Fatal("source checkout content was modified")
	}

	if err := manager.Remove(t.Context(), workspace.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(workspace.RootPath); !os.IsNotExist(err) {
		t.Fatalf("owned workspace was not removed: %v", err)
	}
}

func TestManagerRejectsUnknownOrUnownedCleanup(t *testing.T) {
	dataDirectory := filepath.Join(t.TempDir(), "data")
	manager, err := NewManager(Config{DataDirectory: dataDirectory}, fixtureRepositories{})
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "keep.txt")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.Remove(t.Context(), "code-ffffffffffffffff"); err == nil {
		t.Fatal("unknown workspace cleanup succeeded")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("unrelated path changed: %v", err)
	}
}

func TestWorkspaceQuotaStopsAtFileAndByteLimits(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "one"), []byte("1234"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "two"), []byte("5678"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateWorkspaceQuota(root, 1024, 1); err == nil ||
		!strings.Contains(err.Error(), "files") {
		t.Fatalf("file quota error = %v", err)
	}
	if err := validateWorkspaceQuota(root, 7, 10); err == nil ||
		!strings.Contains(err.Error(), "bytes") {
		t.Fatalf("byte quota error = %v", err)
	}
	if err := validateWorkspaceQuota(root, 8, 2); err != nil {
		t.Fatalf("exact quota boundary failed: %v", err)
	}
}

func containsChange(changes []FileChange, path, status string) bool {
	for _, change := range changes {
		if change.Path == path && change.Status == status {
			return true
		}
	}
	return false
}

func runGit(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
	return string(output)
}
