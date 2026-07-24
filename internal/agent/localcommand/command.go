// Package localcommand resolves locally installed provider CLIs.
package localcommand

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// Resolve finds an explicitly configured command or known local installation.
func Resolve(configured, name string) (string, error) {
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
	command := exec.Command(path, "--version")
	return command.Run() == nil
}
