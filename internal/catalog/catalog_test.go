package catalog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverFindsWorktreeRepositories(t *testing.T) {
	root := t.TempDir()
	repositoryPath := filepath.Join(root, "owner", "example")
	if err := os.MkdirAll(filepath.Join(repositoryPath, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

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
	if repositories[0].Path != repositoryPath {
		t.Fatalf("expected repository path %q, got %q", repositoryPath, repositories[0].Path)
	}
}
