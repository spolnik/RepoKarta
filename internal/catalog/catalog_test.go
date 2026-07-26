package catalog

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

func TestDiscoverClassifiesRepositoryWithoutCommitsAsEmpty(t *testing.T) {
	root := t.TempDir()
	repositoryPath := filepath.Join(root, "owner", "empty")
	runGit(t, root, "init", repositoryPath)

	repositories, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(repositories) != 1 {
		t.Fatalf("empty repository discovery = %#v", repositories)
	}
	repository := repositories[0]
	if repository.ScanState != "empty" ||
		repository.IndexState != "empty" ||
		repository.ScanError != EmptyRepositoryReason ||
		repository.IndexError != EmptyRepositoryReason ||
		repository.HeadCommit != "" {
		t.Fatalf("empty repository metadata = %#v", repository)
	}

	inspected, err := Inspect(repositoryPath)
	if err != nil {
		t.Fatalf("inspect empty repository: %v", err)
	}
	if inspected.IndexState != "empty" {
		t.Fatalf("inspected empty repository = %#v", inspected)
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

func TestDiscoverSkipsLinkedWorktreesBelowScanRoot(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}

	root := t.TempDir()
	primary := filepath.Join(root, "RepoKarta")
	linked := filepath.Join(root, "RepoKarta-m9")
	runGit(t, root, "init", primary)
	runGit(t, primary, "config", "user.email", "repokarta@example.test")
	runGit(t, primary, "config", "user.name", "RepoKarta tests")
	if err := os.WriteFile(filepath.Join(primary, "README.md"), []byte("primary\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, primary, "add", "README.md")
	runGit(t, primary, "commit", "-m", "Initial commit")
	runGit(t, primary, "worktree", "add", "--detach", linked)

	repositories, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(repositories) != 1 || repositories[0].Path != mustCanonicalDirectory(t, primary) {
		t.Fatalf("parent scan discovered linked worktree: %#v", repositories)
	}

	repositories, err = Discover(linked)
	if err != nil {
		t.Fatal(err)
	}
	if len(repositories) != 1 || repositories[0].Path != mustCanonicalDirectory(t, linked) {
		t.Fatalf("explicit linked-worktree scan = %#v", repositories)
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

func TestDiscoverRespectsGitignoreRules(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(`
# build output and vendored copies are not repositories we manage
node_modules/
/build
vendor/**/generated
sandbox/*
!sandbox/keep
excluded-parent
!excluded-parent/rescued
`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{
		"owner/service",                     // discovered
		"node_modules/some-package",         // ignored by name at any depth
		"owner/node_modules/nested-package", // ignored at depth too
		"build/tool",                        // ignored by anchored rule
		"vendor/acme/generated/lib",         // ignored through **
		"sandbox/dropped",                   // ignored directory
		"sandbox/keep/restored",             // re-included by negation
		"excluded-parent/rescued/service",   // stays hidden: Git cannot re-include
		//                                      anything below an excluded parent
	} {
		runGit(t, root, "init", filepath.Join(root, filepath.FromSlash(relative)))
	}

	repositories, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	discovered := make([]string, 0, len(repositories))
	for _, repository := range repositories {
		relative, relErr := filepath.Rel(mustCanonicalDirectory(t, root), repository.Path)
		if relErr != nil {
			t.Fatal(relErr)
		}
		discovered = append(discovered, filepath.ToSlash(relative))
	}
	slices.Sort(discovered)
	expected := []string{"owner/service", "sandbox/keep/restored"}
	if !slices.Equal(discovered, expected) {
		t.Fatalf("discovered %v, want %v", discovered, expected)
	}
}

func TestDiscoverAppliesGitignoreOnlyBelowItsOwnDirectory(t *testing.T) {
	root := t.TempDir()
	// A nested .gitignore must not hide anything outside its own subtree.
	nested := filepath.Join(root, "owner")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, ".gitignore"), []byte("tmp\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "init", filepath.Join(root, "owner", "service"))
	runGit(t, root, "init", filepath.Join(root, "owner", "tmp", "hidden"))
	runGit(t, root, "init", filepath.Join(root, "tmp", "visible"))

	repositories, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(repositories))
	for _, repository := range repositories {
		names = append(names, repository.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"service", "visible"}) {
		t.Fatalf("discovered %v, want [service visible]", names)
	}
}

func TestCompileIgnorePatternHandlesGitignoreSyntax(t *testing.T) {
	for _, testCase := range []struct {
		rule    string
		path    string
		matches bool
	}{
		{rule: "node_modules", path: "a/b/node_modules", matches: true},
		{rule: "/build", path: "build", matches: true},
		{rule: "/build", path: "sub/build", matches: false},
		{rule: "*.log", path: "logs/app.log", matches: true},
		{rule: "**/generated", path: "vendor/acme/generated", matches: true},
		{rule: "doc?", path: "docs", matches: true},
		{rule: "doc?", path: "documents", matches: false},
		{rule: "target/", path: "target", matches: true},
		{rule: "# comment", path: "anything", matches: false},
		{rule: "", path: "anything", matches: false},
	} {
		pattern, ok := compileIgnorePattern(testCase.rule)
		if !ok {
			if testCase.matches {
				t.Fatalf("rule %q was rejected", testCase.rule)
			}
			continue
		}
		if pattern.matcher.MatchString(testCase.path) != testCase.matches {
			t.Fatalf("rule %q against %q = %v, want %v",
				testCase.rule, testCase.path, !testCase.matches, testCase.matches)
		}
	}
}
