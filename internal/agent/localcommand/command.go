// Package localcommand resolves locally installed provider CLIs.
package localcommand

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

type cacheEntry struct {
	path string
	err  error
	at   time.Time
}

var (
	cacheMu sync.Mutex
	cache   = make(map[string]cacheEntry)
)

// Resolve finds an explicitly configured command or known local installation.
func Resolve(configured, name string) (string, error) {
	key := configured + "\x00" + name
	cacheMu.Lock()
	if entry, ok := cache[key]; ok && time.Since(entry.at) < 15*time.Second {
		cacheMu.Unlock()
		return entry.path, entry.err
	}
	cacheMu.Unlock()
	path, err := resolve(configured, name)
	cacheMu.Lock()
	cache[key] = cacheEntry{path: path, err: err, at: time.Now()}
	cacheMu.Unlock()
	return path, err
}

func resolve(configured, name string) (string, error) {
	if configured != "" && configured != name {
		if filepath.IsAbs(configured) {
			if _, err := os.Stat(configured); err != nil {
				return "", err
			}
			return configured, nil
		}
		return exec.LookPath(configured)
	}
	if command, err := exec.LookPath(name); err == nil {
		if canExecute(command) {
			return command, nil
		}
	}

	userDirectory, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	var candidates []string
	switch name {
	case "codex":
		candidates = []string{
			filepath.Join(userDirectory, ".codex", "plugins", ".plugin-appserver", executable(name)),
			filepath.Join(userDirectory, ".codex", ".sandbox-bin", executable(name)),
		}
	case "claude":
		candidates = []string{
			filepath.Join(userDirectory, ".local", "bin", executable(name)),
		}
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return exec.LookPath(name)
}

func executable(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func canExecute(path string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, path, "--version")
	return command.Run() == nil
}
