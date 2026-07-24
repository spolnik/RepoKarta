package source

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spolnik/RepoKarta/internal/catalog"
)

func TestHistoryAndDiffStayBoundedToRecordedAncestry(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}

	root := t.TempDir()
	repositoryPath := filepath.Join(root, "example")
	runGit(t, root, "init", repositoryPath)
	runGit(t, repositoryPath, "config", "user.email", "repokarta@example.test")
	runGit(t, repositoryPath, "config", "user.name", "RepoKarta tests")
	writeHistoryFile(t, repositoryPath, "first\n")
	runGit(t, repositoryPath, "add", "history.txt")
	runGit(t, repositoryPath, "commit", "-m", "Initial history")
	first := currentRevision(t, repositoryPath)

	writeHistoryFile(t, repositoryPath, "second\n")
	runGit(t, repositoryPath, "commit", "-am", "Update history")
	second := currentRevision(t, repositoryPath)

	repositories, err := catalog.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	repository := repositories[0]
	repository.IndexedCommit = second

	commits, truncated, outputTruncated, _, err := Log(context.Background(), repository, "", "", 1, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || outputTruncated || len(commits) != 1 || commits[0].Revision != second || commits[0].Subject != "Update history" {
		t.Fatalf("history = %#v, truncated = %v, output truncated = %v", commits, truncated, outputTruncated)
	}

	_, truncated, outputTruncated, returnedBytes, err := Log(context.Background(), repository, "", "", 2, 32)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || !outputTruncated || returnedBytes != 32 {
		t.Fatalf("bounded log: truncated = %v, output truncated = %v, bytes = %d", truncated, outputTruncated, returnedBytes)
	}

	diff, err := DiffCommits(context.Background(), repository, first, second, "history.txt", 3, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if diff.FromRevision != first || diff.ToRevision != second || diff.Truncated {
		t.Fatalf("diff metadata = %#v", diff)
	}
	if diff.FilesChanged != 1 || diff.Insertions != 1 || diff.Deletions != 1 {
		t.Fatalf("diff stats = %#v", diff)
	}
	if !strings.Contains(diff.Patch, "-first") || !strings.Contains(diff.Patch, "+second") {
		t.Fatalf("patch = %q", diff.Patch)
	}

	rootDiff, err := DiffCommits(context.Background(), repository, "", first, "", 3, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if rootDiff.FromRevision != "" || rootDiff.ToRevision != first ||
		rootDiff.FilesChanged != 1 || !strings.Contains(rootDiff.Patch, "+first") {
		t.Fatalf("root diff = %#v", rootDiff)
	}

	boundedDiff, err := DiffCommits(context.Background(), repository, first, second, "", 3, 32)
	if err != nil {
		t.Fatal(err)
	}
	if !boundedDiff.Truncated || boundedDiff.ReturnedBytes != 32 {
		t.Fatalf("bounded diff = %#v", boundedDiff)
	}

	historical, err := OpenFile(context.Background(), repository, first, "history.txt", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if historical.Revision != first || len(historical.Lines) != 1 || historical.Lines[0].Text != "first" {
		t.Fatalf("historical file = %#v", historical)
	}

	if _, _, _, _, err := Log(context.Background(), repository, "", "../escape", 2, 1<<20); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("expected unsafe log path, got %v", err)
	}
	if _, err := DiffCommits(context.Background(), repository, first, second, "../escape", 3, 1<<20); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("expected unsafe diff path, got %v", err)
	}

	runGit(t, repositoryPath, "checkout", "--orphan", "unrelated")
	runGit(t, repositoryPath, "rm", "-rf", ".")
	writeHistoryFile(t, repositoryPath, "unrelated\n")
	runGit(t, repositoryPath, "add", "history.txt")
	runGit(t, repositoryPath, "commit", "-m", "Unrelated history")
	unrelated := currentRevision(t, repositoryPath)
	if _, err := ResolveCommit(context.Background(), repository, unrelated); !errors.Is(err, ErrUnknownRevision) {
		t.Fatalf("expected unrelated revision to be rejected, got %v", err)
	}

	if _, err := ResolveCommit(context.Background(), repository, strings.Repeat("a", 40)); !errors.Is(err, ErrUnknownRevision) {
		t.Fatalf("expected unknown revision, got %v", err)
	}
}

func writeHistoryFile(t *testing.T, repositoryPath, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repositoryPath, "history.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func currentRevision(t *testing.T, repositoryPath string) string {
	t.Helper()
	command := exec.Command("git", "-C", repositoryPath, "rev-parse", "HEAD")
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(output))
}
