package catalog

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type Repository struct {
	Name string
	Path string
}

func Discover(root string) ([]Repository, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve repository root %q: %w", root, err)
	}

	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("inspect repository root %q: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("repository root %q is not a directory", root)
	}

	var repositories []Repository
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() || entry.Name() != ".git" {
			return nil
		}

		repositoryPath := filepath.Dir(path)
		repositories = append(repositories, Repository{
			Name: filepath.Base(repositoryPath),
			Path: repositoryPath,
		})

		return filepath.SkipDir
	})
	if err != nil {
		return nil, fmt.Errorf("discover Git repositories below %q: %w", root, err)
	}

	slices.SortFunc(repositories, func(left, right Repository) int {
		return strings.Compare(strings.ToLower(left.Name), strings.ToLower(right.Name))
	})

	return repositories, nil
}
