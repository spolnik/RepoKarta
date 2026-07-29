// Package atomicfile publishes complete files through a same-directory
// temporary file and a Windows-compatible replacement fallback.
package atomicfile

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Options controls temporary-file naming, permissions, and durability.
type Options struct {
	Pattern string
	Mode    fs.FileMode
	Sync    bool
}

// Write publishes content at target without exposing a partially written file.
// Callers remain responsible for serializing readers when replacement on
// Windows must briefly remove an existing target.
func Write(target string, content []byte, options Options) error {
	if target == "" {
		return errors.New("atomic file target is required")
	}
	directory := filepath.Dir(target)
	pattern := options.Pattern
	if pattern == "" {
		pattern = filepath.Base(target) + ".*.tmp"
	}
	temporary, err := os.CreateTemp(directory, pattern)
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)

	closed := false
	defer func() {
		if !closed {
			_ = temporary.Close()
		}
	}()
	if options.Mode != 0 {
		if err := temporary.Chmod(options.Mode); err != nil {
			return fmt.Errorf("set temporary file mode: %w", err)
		}
	}
	written, err := temporary.Write(content)
	if err != nil {
		return fmt.Errorf("write temporary file: %w", err)
	}
	if written != len(content) {
		return fmt.Errorf("write temporary file: %w", io.ErrShortWrite)
	}
	if options.Sync {
		if err := temporary.Sync(); err != nil {
			return fmt.Errorf("sync temporary file: %w", err)
		}
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	closed = true
	if err := Replace(temporaryName, target); err != nil {
		return fmt.Errorf("publish temporary file: %w", err)
	}
	return nil
}

// Replace renames temporaryName to target. Windows does not replace an
// existing file through os.Rename, so an existing target is removed and the
// rename is retried.
func Replace(temporaryName, target string) error {
	renameErr := os.Rename(temporaryName, target)
	if renameErr == nil {
		return nil
	}
	if _, err := os.Stat(target); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return renameErr
		}
		return err
	}
	if err := os.Remove(target); err != nil {
		return err
	}
	return os.Rename(temporaryName, target)
}
