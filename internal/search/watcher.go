package search

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/spolnik/RepoKarta/internal/catalog"
)

const (
	maximumRepositoryWatchDirectories = 4096
	repositoryWatchDebounce           = 400 * time.Millisecond
)

// repositoryWatcher watches Git metadata rather than source trees. RepoKarta
// indexes committed revisions, so worktree edits alone are not an indexing
// event; HEAD, refs, and packed-refs changes are.
type repositoryWatcher struct {
	watcher *fsnotify.Watcher
	mu      sync.Mutex
	paths   map[string]struct{}
}

func newRepositoryWatcher() (*repositoryWatcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return &repositoryWatcher{
		watcher: watcher,
		paths:   make(map[string]struct{}),
	}, nil
}

func (w *repositoryWatcher) Close() error {
	return w.watcher.Close()
}

func (w *repositoryWatcher) Update(repositories []catalog.Repository) error {
	next := make(map[string]struct{})
	var updateErr error
	for _, repository := range repositories {
		paths, err := repositoryGitMetadataDirectories(repository)
		if err != nil {
			updateErr = errors.Join(updateErr, fmt.Errorf("watch %s: %w", repository.Name, err))
			continue
		}
		for _, candidate := range paths {
			if len(next) >= maximumRepositoryWatchDirectories {
				updateErr = errors.Join(updateErr, fmt.Errorf(
					"repository watch directory limit %d reached",
					maximumRepositoryWatchDirectories,
				))
				break
			}
			next[candidate] = struct{}{}
		}
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	for existing := range w.paths {
		if _, retained := next[existing]; retained {
			continue
		}
		if err := w.watcher.Remove(existing); err != nil &&
			!errors.Is(err, fsnotify.ErrNonExistentWatch) {
			updateErr = errors.Join(updateErr, err)
		}
		delete(w.paths, existing)
	}
	for candidate := range next {
		if _, exists := w.paths[candidate]; exists {
			continue
		}
		if err := w.watcher.Add(candidate); err != nil {
			updateErr = errors.Join(updateErr, err)
			continue
		}
		w.paths[candidate] = struct{}{}
	}
	return updateErr
}

func (w *repositoryWatcher) Run(ctx context.Context, refresh func()) {
	defer w.Close()
	var (
		timer   *time.Timer
		timerCh <-chan time.Time
	)
	schedule := func() {
		if timer == nil {
			timer = time.NewTimer(repositoryWatchDebounce)
		} else {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(repositoryWatchDebounce)
		}
		timerCh = timer.C
	}
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-w.watcher.Events:
			if !ok {
				return
			}
			if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) != 0 {
				schedule()
			}
		case err, ok := <-w.watcher.Errors:
			if !ok {
				return
			}
			slog.Warn("repository filesystem watcher", "error", err)
		case <-timerCh:
			timerCh = nil
			refresh()
		}
	}
}

func repositoryGitMetadataDirectories(repository catalog.Repository) ([]string, error) {
	gitDirectory := repository.Path
	if !repository.Bare {
		marker := filepath.Join(repository.Path, ".git")
		info, err := os.Stat(marker)
		if err != nil {
			return nil, err
		}
		if info.IsDir() {
			gitDirectory = marker
		} else {
			content, err := os.ReadFile(marker)
			if err != nil {
				return nil, err
			}
			value := strings.TrimSpace(string(content))
			value = strings.TrimSpace(strings.TrimPrefix(value, "gitdir:"))
			if value == "" {
				return nil, errors.New("linked worktree gitdir is empty")
			}
			if !filepath.IsAbs(value) {
				value = filepath.Join(repository.Path, value)
			}
			gitDirectory = filepath.Clean(value)
		}
	}
	directories := map[string]struct{}{gitDirectory: {}}
	if content, err := os.ReadFile(filepath.Join(gitDirectory, "commondir")); err == nil {
		common := strings.TrimSpace(string(content))
		if !filepath.IsAbs(common) {
			common = filepath.Join(gitDirectory, common)
		}
		directories[filepath.Clean(common)] = struct{}{}
	}
	for root := range directories {
		refs := filepath.Join(root, "refs")
		_ = filepath.WalkDir(refs, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if entry.IsDir() && len(directories) < maximumRepositoryWatchDirectories {
				directories[path] = struct{}{}
			}
			return nil
		})
	}
	output := make([]string, 0, len(directories))
	for directory := range directories {
		if info, err := os.Stat(directory); err == nil && info.IsDir() {
			output = append(output, directory)
		}
	}
	return output, nil
}
