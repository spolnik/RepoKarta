package zoekt

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/search"
)

func TestDiscoverCTagsTreatsMissingConfiguredCommandAsUnavailable(t *testing.T) {
	t.Setenv("CTAGS_COMMAND", filepath.Join(t.TempDir(), "missing-ctags"))
	t.Setenv("PATH", t.TempDir())
	if found := discoverCTags(); found != "" {
		t.Fatalf("discoverCTags() = %q, want unavailable", found)
	}
}

func TestBuildQueryUsesORWithinFieldsAndNOTForNegativeFilters(t *testing.T) {
	compiled, err := buildQuery(search.Query{
		IncludeText:          []string{"first", "second"},
		ExcludeText:          []string{"generated"},
		RepositoryIDs:        []uint32{1, 2},
		ExcludeRepositoryIDs: []uint32{2},
		Languages:            []string{"Go", "TypeScript"},
		ExcludeLanguages:     []string{"Java"},
	})
	if err != nil {
		t.Fatal(err)
	}
	value := compiled.String()
	for _, fragment := range []string{
		"(or substr:\"first\" substr:\"second\")", "(not substr:\"generated\")",
		"(repoids count:2)", "(not (repoids repoid={2}))",
		"(or lang:Go lang:TypeScript)", "(not lang:Java)",
	} {
		if !strings.Contains(value, fragment) {
			t.Fatalf("compiled query %q does not contain %q", value, fragment)
		}
	}
}

func TestNormalizeLanguageFiltersCanonicalizesNamesAndWarnsOncePerUnknownValue(t *testing.T) {
	normalized, warnings := normalizeLanguageFilters(search.Query{
		Language:         "java",
		Languages:        []string{"go", "TypeScript", "not-a-language"},
		ExcludeLanguages: []string{"JAVA", " NOT-A-LANGUAGE "},
	})
	if normalized.Language != "Java" {
		t.Fatalf("language = %q, want Java", normalized.Language)
	}
	if got := strings.Join(normalized.Languages, ","); got != "Go,TypeScript,not-a-language" {
		t.Fatalf("languages = %q", got)
	}
	if got := strings.Join(normalized.ExcludeLanguages, ","); got != "Java,not-a-language" {
		t.Fatalf("excluded languages = %q", got)
	}
	if len(warnings) != 1 || warnings[0].Code != "unknown_language" ||
		!strings.Contains(warnings[0].Message, `"not-a-language"`) {
		t.Fatalf("warnings = %#v", warnings)
	}
}

func TestDiscoverCTagsAcceptsHomebrewNameOnlyWhenItIsUniversal(t *testing.T) {
	paths := map[string]string{
		"universal-ctags": "",
		"ctags":           "/opt/homebrew/bin/ctags",
	}
	found := discoverCTagsWith(
		"",
		func(candidate string) (string, error) {
			if path := paths[candidate]; path != "" {
				return path, nil
			}
			return "", exec.ErrNotFound
		},
		func(command string) bool { return command == "/opt/homebrew/bin/ctags" },
	)
	if found != "/opt/homebrew/bin/ctags" {
		t.Fatalf("discoverCTagsWith() = %q, want Homebrew ctags", found)
	}
}

func TestDiscoverCTagsRejectsBSDCTagsAndContinues(t *testing.T) {
	found := discoverCTagsWith(
		"/usr/bin/ctags",
		func(candidate string) (string, error) {
			switch candidate {
			case "/usr/bin/ctags", "ctags":
				return "/usr/bin/ctags", nil
			case "universal-ctags":
				return "/usr/local/bin/universal-ctags", nil
			default:
				return "", exec.ErrNotFound
			}
		},
		func(command string) bool { return command == "/usr/local/bin/universal-ctags" },
	)
	if found != "/usr/local/bin/universal-ctags" {
		t.Fatalf("discoverCTagsWith() = %q, want verified Universal Ctags", found)
	}
}

func TestUniversalCTagsVersionDetectionRejectsBSDAndExuberant(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{name: "universal", output: "Universal Ctags 6.2.0, Copyright (C) Universal Ctags Team", want: true},
		{name: "homebrew universal", output: "Universal Ctags 6.1.0(p6.1.20240630.0)", want: true},
		{name: "bsd", output: "usage: ctags [-BFadtuwvx] [-f tagsfile] file ...", want: false},
		{name: "exuberant", output: "Exuberant Ctags 5.8, Copyright (C) 1996-2009 Darren Hiebert", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isUniversalCTagsVersion([]byte(test.output)); got != test.want {
				t.Fatalf("isUniversalCTagsVersion(%q) = %v, want %v", test.output, got, test.want)
			}
		})
	}
}

func TestAdapterIndexesAndSearchesRepositoryOnNativePlatform(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}

	root := t.TempDir()
	repositoryPath := filepath.Join(root, "example")
	runGit(t, root, "init", repositoryPath)
	internalPath := filepath.Join(repositoryPath, "internal")
	if err := os.MkdirAll(internalPath, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, repositoryPath, "config", "user.email", "repokarta@example.test")
	runGit(t, repositoryPath, "config", "user.name", "RepoKarta tests")
	source := "package greeting\n\nfunc HelloRepoKarta() string {\n\treturn \"needle from local code\"\n}\n"
	if err := os.WriteFile(filepath.Join(internalPath, "hello.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repositoryPath, "other.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repositoryPath, "add", "internal/hello.go", "other.go")
	runGit(t, repositoryPath, "commit", "-m", "Add searchable source")
	runGit(t, repositoryPath, "remote", "add", "origin", "git@github.com:example/example.git")

	repositories, err := catalog.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	repository := repositories[0]
	repository.ID = 42

	adapter, err := New(filepath.Join(t.TempDir(), "indexes"))
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()

	updated, err := adapter.Index(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	if !updated {
		t.Fatal("expected the first index to create a shard")
	}

	result, err := adapter.Search(context.Background(), search.Query{
		Text:       "needle from local code",
		Repository: "example",
		Language:   "Go",
		Path:       "hello",
		File:       ".go",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Matches) != 1 {
		t.Fatalf("expected one matching file, got %#v", result)
	}
	match := result.Matches[0]
	if match.RepositoryID != repository.ID ||
		match.Repository != filepath.ToSlash(repository.Path) ||
		match.Path != "internal/hello.go" ||
		match.Revision != repository.HeadCommit {
		t.Fatalf("unexpected search match: %#v", match)
	}
	if len(match.Lines) != 1 || match.Lines[0].Number != 4 {
		t.Fatalf("expected a cited line 4, got %#v", match.Lines)
	}
	lowercaseLanguage, err := adapter.Search(context.Background(), search.Query{
		Text:     "needle from local code",
		Language: "go",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(lowercaseLanguage.Matches) != 2 || len(lowercaseLanguage.Warnings) != 0 {
		t.Fatalf("lowercase language search = %#v", lowercaseLanguage)
	}
	unknownLanguage, err := adapter.Search(context.Background(), search.Query{
		Text:     "needle from local code",
		Language: "not-a-language",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(unknownLanguage.Matches) != 0 ||
		len(unknownLanguage.Warnings) != 1 ||
		unknownLanguage.Warnings[0].Code != "unknown_language" {
		t.Fatalf("unknown language search = %#v", unknownLanguage)
	}
	scoped, err := adapter.Search(context.Background(), search.Query{
		Text: "needle from local code",
		Scopes: []search.Scope{{
			RepositoryID: uint32(repository.ID),
			Repository:   filepath.ToSlash(repositoryPath),
			Path:         "internal/hello.go",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped.Matches) != 1 || scoped.Matches[0].Path != "internal/hello.go" {
		t.Fatalf("structured file scope = %#v; indexed repository identity = %q", scoped.Matches, match.Repository)
	}
	directoryScoped, err := adapter.Search(context.Background(), search.Query{
		Text: "needle from local code",
		Scopes: []search.Scope{{
			RepositoryID: uint32(repository.ID),
			Repository:   filepath.ToSlash(repositoryPath),
			Kind:         search.ScopeKindDirectory,
			Path:         "internal",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(directoryScoped.Matches) != 1 ||
		directoryScoped.Matches[0].Path != "internal/hello.go" {
		t.Fatalf("structured directory scope = %#v", directoryScoped.Matches)
	}

	if err := os.WriteFile(filepath.Join(internalPath, "hello.go"), []byte(source+"\nconst UpdatedNeedle = \"fresh index\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repositoryPath, "add", "internal/hello.go")
	runGit(t, repositoryPath, "commit", "-m", "Update searchable source")
	repositories, err = catalog.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	updatedRepository := repositories[0]
	updatedRepository.ID = repository.ID

	updated, err = adapter.Index(context.Background(), updatedRepository)
	if err != nil {
		t.Fatal(err)
	}
	if !updated {
		t.Fatal("expected changed HEAD to update the shard")
	}
	result, err = adapter.Search(context.Background(), search.Query{Text: "fresh index"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Matches) != 1 || result.Matches[0].Revision != updatedRepository.HeadCommit {
		t.Fatalf("expected search to use updated commit, got %#v", result)
	}
}

func TestSearchReportsExactFileLimitTruncation(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	repositoryPath := filepath.Join(root, "many-files")
	runGit(t, root, "init", repositoryPath)
	runGit(t, repositoryPath, "config", "user.email", "repokarta@example.test")
	runGit(t, repositoryPath, "config", "user.name", "RepoKarta tests")
	for _, name := range []string{"one.txt", "two.txt", "three.txt"} {
		if err := os.WriteFile(filepath.Join(repositoryPath, name), []byte("fleet needle\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, repositoryPath, "add", ".")
	runGit(t, repositoryPath, "commit", "-m", "Add fleet files")
	repositories, err := catalog.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	repository := repositories[0]
	repository.ID = 43
	adapter, err := New(filepath.Join(t.TempDir(), "indexes"))
	if err != nil {
		t.Fatal(err)
	}
	adapter.symbolsEnabled = false
	adapter.ctagsPath = ""
	defer adapter.Close()
	if _, err := adapter.Index(context.Background(), repository); err != nil {
		t.Fatal(err)
	}

	result, err := adapter.Search(context.Background(), search.Query{Text: "fleet needle", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Truncated || result.ReturnedFiles != 2 || result.FileCount != 3 {
		t.Fatalf("unexpected completeness metadata: %#v", result)
	}
	if !result.TotalFilesExact || result.EstimatedFiles != 3 || result.Limit != 2 {
		t.Fatalf("unexpected total metadata: %#v", result)
	}
}

func TestSymbolQueryWarnsWhenUniversalCTagsIsUnavailable(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	repositoryPath := filepath.Join(root, "symbols")
	runGit(t, root, "init", repositoryPath)
	runGit(t, repositoryPath, "config", "user.email", "repokarta@example.test")
	runGit(t, repositoryPath, "config", "user.name", "RepoKarta tests")
	if err := os.WriteFile(filepath.Join(repositoryPath, "main.go"), []byte("package main\n\nfunc FleetSymbol() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repositoryPath, "add", ".")
	runGit(t, repositoryPath, "commit", "-m", "Add symbol")
	repositories, err := catalog.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	repository := repositories[0]
	repository.ID = 44
	adapter, err := New(filepath.Join(t.TempDir(), "indexes"))
	if err != nil {
		t.Fatal(err)
	}
	adapter.symbolsEnabled = false
	adapter.ctagsPath = ""
	defer adapter.Close()
	if _, err := adapter.Index(context.Background(), repository); err != nil {
		t.Fatal(err)
	}

	result, err := adapter.Search(context.Background(), search.Query{
		Text:  "sym:FleetSymbol",
		Mode:  "zoekt",
		Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Warnings) != 1 || result.Warnings[0].Code != "symbol_index_disabled" {
		t.Fatalf("warnings = %#v", result.Warnings)
	}
}

func TestIndexUsesGitShadowForWorktreeConfigRepositories(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	repositoryPath := filepath.Join(root, "worktree-config")
	runGit(t, root, "init", repositoryPath)
	runGit(t, repositoryPath, "config", "user.email", "repokarta@example.test")
	runGit(t, repositoryPath, "config", "user.name", "RepoKarta tests")
	if err := os.WriteFile(filepath.Join(repositoryPath, "fallback.txt"), []byte("worktree fallback needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repositoryPath, "add", ".")
	runGit(t, repositoryPath, "commit", "-m", "Add fallback source")
	runGit(t, repositoryPath, "config", "core.repositoryFormatVersion", "1")
	runGit(t, repositoryPath, "config", "extensions.worktreeConfig", "true")

	repositories, err := catalog.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	repository := repositories[0]
	repository.ID = 45
	indexDirectory := filepath.Join(t.TempDir(), "indexes")
	adapter, err := New(indexDirectory)
	if err != nil {
		t.Fatal(err)
	}
	adapter.symbolsEnabled = false
	adapter.ctagsPath = ""
	defer adapter.Close()
	if _, err := adapter.Index(context.Background(), repository); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(indexDirectory, "git-shadow", "repo-45.git", "HEAD")); err != nil {
		t.Fatalf("Git shadow was not created: %v", err)
	}
	result, err := adapter.Search(context.Background(), search.Query{Text: "worktree fallback needle"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Matches) != 1 || result.Matches[0].Revision != repository.HeadCommit {
		t.Fatalf("Git shadow search result = %#v", result)
	}
}

func TestIndexUsesConfiguredCommitWithoutChangingCheckout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	repositoryPath := filepath.Join(root, "configured-default")
	runGit(t, root, "init", repositoryPath)
	runGit(t, repositoryPath, "config", "user.email", "repokarta@example.test")
	runGit(t, repositoryPath, "config", "user.name", "RepoKarta tests")
	if err := os.WriteFile(
		filepath.Join(repositoryPath, "branch.txt"),
		[]byte("default branch needle\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	runGit(t, repositoryPath, "add", "branch.txt")
	runGit(t, repositoryPath, "commit", "-m", "Default branch")
	defaultCommit := runGitOutput(t, repositoryPath, "rev-parse", "HEAD")
	defaultBranch := runGitOutput(t, repositoryPath, "branch", "--show-current")
	runGit(t, repositoryPath, "checkout", "-b", "feature")
	if err := os.WriteFile(
		filepath.Join(repositoryPath, "branch.txt"),
		[]byte("feature branch needle\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	runGit(t, repositoryPath, "add", "branch.txt")
	runGit(t, repositoryPath, "commit", "-m", "Feature branch")

	repository, err := catalog.Inspect(repositoryPath)
	if err != nil {
		t.Fatal(err)
	}
	repository.ID = 46
	repository.IndexRevision = defaultBranch
	repository.IndexCommit = defaultCommit
	adapter, err := New(filepath.Join(t.TempDir(), "indexes"))
	if err != nil {
		t.Fatal(err)
	}
	adapter.symbolsEnabled = false
	defer adapter.Close()
	if _, err := adapter.Index(context.Background(), repository); err != nil {
		t.Fatal(err)
	}
	result, err := adapter.Search(context.Background(), search.Query{Text: "default branch needle"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Matches) != 1 || result.Matches[0].Revision != defaultCommit {
		t.Fatalf("configured-commit result = %#v", result)
	}
	if branch := runGitOutput(t, repositoryPath, "branch", "--show-current"); branch != "feature" {
		t.Fatalf("checkout changed to %q while indexing configured commit", branch)
	}
}

func runGit(t *testing.T, directory string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, arguments...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
}

func runGitOutput(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, arguments...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
	return strings.TrimSpace(string(output))
}
