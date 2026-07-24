package source

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/spolnik/RepoKarta/internal/catalog"
)

func TestOpenFileReadsRecordedRevisionAndRejectsTraversal(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}

	root := t.TempDir()
	repositoryPath := filepath.Join(root, "example")
	runGit(t, root, "init", repositoryPath)
	runGit(t, repositoryPath, "config", "user.email", "repokarta@example.test")
	runGit(t, repositoryPath, "config", "user.name", "RepoKarta tests")
	if err := os.WriteFile(filepath.Join(repositoryPath, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repositoryPath, "add", "main.go")
	runGit(t, repositoryPath, "commit", "-m", "Initial source")

	repositories, err := catalog.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	repository := repositories[0]
	repository.IndexedCommit = repository.HeadCommit

	file, err := OpenFile(context.Background(), repository, repository.HeadCommit, "main.go", 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if file.TotalLines != 3 || len(file.Lines) != 2 || file.Lines[1].Number != 3 {
		t.Fatalf("unexpected source file: %#v", file)
	}

	if _, err := OpenFile(context.Background(), repository, repository.HeadCommit, "../secret", 1, 1); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("expected unsafe path error, got %v", err)
	}
	if _, err := OpenFile(context.Background(), repository, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "main.go", 1, 1); !errors.Is(err, ErrUnknownRevision) {
		t.Fatalf("expected unknown revision error, got %v", err)
	}
}

func runGit(t *testing.T, directory string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, arguments...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
}
