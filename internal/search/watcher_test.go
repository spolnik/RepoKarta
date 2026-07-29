package search

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/spolnik/RepoKarta/internal/catalog"
)

func TestRepositoryWatcherRefreshesAfterCommittedRefChange(t *testing.T) {
	repositoryPath := t.TempDir()
	runWatcherGit(t, repositoryPath, "init")
	runWatcherGit(t, repositoryPath, "config", "user.email", "repokarta@example.test")
	runWatcherGit(t, repositoryPath, "config", "user.name", "RepoKarta tests")
	writeWatcherFile(t, repositoryPath, "first")
	runWatcherGit(t, repositoryPath, "add", "watched.txt")
	runWatcherGit(t, repositoryPath, "commit", "-m", "First")

	repository, err := catalog.Inspect(repositoryPath)
	if err != nil {
		t.Fatal(err)
	}
	watcher, err := newRepositoryWatcher()
	if err != nil {
		t.Fatal(err)
	}
	if err := watcher.Update([]catalog.Repository{repository}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	refreshed := make(chan struct{}, 1)
	go watcher.Run(ctx, func() {
		select {
		case refreshed <- struct{}{}:
		default:
		}
	})

	writeWatcherFile(t, repositoryPath, "second")
	runWatcherGit(t, repositoryPath, "add", "watched.txt")
	runWatcherGit(t, repositoryPath, "commit", "-m", "Second")
	select {
	case <-refreshed:
	case <-time.After(5 * time.Second):
		t.Fatal("committed ref change did not trigger a filesystem refresh")
	}
}

func writeWatcherFile(t *testing.T, root, value string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "watched.txt"), []byte(value+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runWatcherGit(t *testing.T, root string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, arguments...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
}
