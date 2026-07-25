package catalog

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestDiscoverFindsWorktreeRepositories(t *testing.T) {
	root := t.TempDir()
	repositoryPath := filepath.Join(root, "owner", "example")
	runGit(t, root, "init", repositoryPath)
	runGit(t, repositoryPath, "config", "user.email", "repokarta@example.test")
	runGit(t, repositoryPath, "config", "user.name", "RepoKarta tests")
	if err := os.WriteFile(filepath.Join(repositoryPath, "README.md"), []byte("example\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repositoryPath, "add", "README.md")
	runGit(t, repositoryPath, "commit", "-m", "Initial commit")

	repositories, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}

	if len(repositories) != 1 {
		t.Fatalf("expected one repository, got %d", len(repositories))
	}
	if repositories[0].Name != "example" {
		t.Fatalf("expected repository name example, got %q", repositories[0].Name)
	}
	expectedPath := mustCanonicalDirectory(t, repositoryPath)
	if repositories[0].Path != expectedPath {
		t.Fatalf("expected repository path %q, got %q", expectedPath, repositories[0].Path)
	}
}

func TestDiscoverFindsWorktreesAndBareRepositoriesAndHonorsExclusions(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}

	root := t.TempDir()
	worktree := filepath.Join(root, "owner", "worktree")
	runGit(t, root, "init", worktree)
	runGit(t, worktree, "config", "user.email", "repokarta@example.test")
	runGit(t, worktree, "config", "user.name", "RepoKarta tests")
	if err := os.WriteFile(filepath.Join(worktree, "README.md"), []byte("searchable\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, worktree, "add", "README.md")
	runGit(t, worktree, "commit", "-m", "Initial commit")

	bare := filepath.Join(root, "owner", "archive.git")
	runGit(t, root, "clone", "--bare", worktree, bare)

	excludedRepository := filepath.Join(root, "excluded", "ignored")
	runGit(t, root, "init", excludedRepository)

	repositories, err := DiscoverWithOptions(root, DiscoverOptions{Exclude: []string{"excluded"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(repositories) != 2 {
		t.Fatalf("expected two repositories, got %d: %#v", len(repositories), repositories)
	}
	if repositories[0].Name != "archive" || !repositories[0].Bare {
		t.Fatalf("expected first repository to be bare archive, got %#v", repositories[0])
	}
	if repositories[1].Name != "worktree" || repositories[1].Bare {
		t.Fatalf("expected second repository to be regular worktree, got %#v", repositories[1])
	}
	for _, repository := range repositories {
		if repository.HeadCommit == "" || repository.DefaultRevision == "" || repository.ScanState != "ready" {
			t.Fatalf("expected inspected repository metadata, got %#v", repository)
		}
	}
}

func TestDiscoverContinuesBelowRepositoryRoot(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}

	root := t.TempDir()
	runGit(t, root, "init")
	runGit(t, root, "config", "user.email", "repokarta@example.test")
	runGit(t, root, "config", "user.name", "RepoKarta tests")
	if err := os.WriteFile(filepath.Join(root, "ROOT.md"), []byte("root\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "ROOT.md")
	runGit(t, root, "commit", "-m", "Root repository")

	child := filepath.Join(root, "child")
	runGit(t, root, "init", child)
	runGit(t, child, "config", "user.email", "repokarta@example.test")
	runGit(t, child, "config", "user.name", "RepoKarta tests")
	if err := os.WriteFile(filepath.Join(child, "CHILD.md"), []byte("child\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, child, "add", "CHILD.md")
	runGit(t, child, "commit", "-m", "Child repository")

	repositories, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(repositories) != 2 {
		t.Fatalf("expected root and child repositories, got %d: %#v", len(repositories), repositories)
	}
	paths := map[string]bool{}
	for _, repository := range repositories {
		paths[repository.Path] = true
	}
	expectedRoot := mustCanonicalDirectory(t, root)
	expectedChild := mustCanonicalDirectory(t, child)
	if !paths[expectedRoot] || !paths[expectedChild] {
		t.Fatalf("expected repositories at %q and %q, got %#v", expectedRoot, expectedChild, repositories)
	}
}

func TestDiscoverSkipsHiddenWorktreeDirectoriesByDefault(t *testing.T) {
	root := t.TempDir()
	visible := filepath.Join(root, "owner", "service")
	hidden := filepath.Join(root, ".jobguard-wt", "service")
	runGit(t, root, "init", visible)
	runGit(t, root, "init", hidden)

	repositories, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(repositories) != 1 {
		t.Fatalf("expected only visible repository, got %#v", repositories)
	}
	if repositories[0].Path != mustCanonicalDirectory(t, visible) {
		t.Fatalf("visible repository path = %q", repositories[0].Path)
	}
}

func TestDiscoverIgnoresInvalidGitMarkerAndContinues(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}

	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(root, "child")
	runGit(t, root, "init", child)
	runGit(t, child, "config", "user.email", "repokarta@example.test")
	runGit(t, child, "config", "user.name", "RepoKarta tests")
	if err := os.WriteFile(filepath.Join(child, "README.md"), []byte("child\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, child, "add", "README.md")
	runGit(t, child, "commit", "-m", "Child repository")

	repositories, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	expectedChild := mustCanonicalDirectory(t, child)
	if len(repositories) != 1 || repositories[0].Path != expectedChild {
		t.Fatalf("expected only the valid child repository, got %#v", repositories)
	}
}

func mustCanonicalDirectory(t *testing.T, path string) string {
	t.Helper()
	canonical, err := canonicalDirectory(path)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func runGit(t *testing.T, directory string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, arguments...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
}

func TestDisplayNamesDisambiguateDuplicateRepositoryNames(t *testing.T) {
	repositories := []Repository{
		{ID: 1, Name: "service", Path: filepath.FromSlash("/roots/ghorg/team-a/service")},
		{ID: 2, Name: "service", Path: filepath.FromSlash("/roots/ghorg/team-b/service")},
		{ID: 3, Name: "unique", Path: filepath.FromSlash("/roots/ghorg/team-a/unique")},
		{ID: 4, Name: "service", Path: filepath.FromSlash("/roots/ghorg/team-a/service")},
	}
	labels := DisplayNames(repositories)
	if labels[3] != "unique" {
		t.Fatalf("unique repository label = %q", labels[3])
	}
	if labels[1] != "service · team-a" || labels[2] != "service · team-b" {
		t.Fatalf("duplicate labels = %q and %q", labels[1], labels[2])
	}
	if labels[4] != "service · team-a (#4)" {
		t.Fatalf("identical path label = %q", labels[4])
	}
	seen := make(map[string]bool, len(labels))
	for id, label := range labels {
		if label == "" {
			t.Fatalf("repository %d has no label", id)
		}
		if seen[label] {
			t.Fatalf("duplicate label %q", label)
		}
		seen[label] = true
	}
}
