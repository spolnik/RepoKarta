// Package source provides bounded, read-only access to repository contents.
package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spolnik/RepoKarta/internal/catalog"
)

const (
	maximumFileSize = 2 << 20
	commandTimeout  = 10 * time.Second
)

var (
	// ErrUnsafePath means the requested path was not a safe repository-relative path.
	ErrUnsafePath = errors.New("unsafe repository path")
	// ErrUnsupportedFile means the blob is binary, too large, or not UTF-8.
	ErrUnsupportedFile = errors.New("unsupported source file")
	// ErrUnknownRevision means the revision is not reachable from a commit
	// recorded by RepoKarta.
	ErrUnknownRevision = errors.New("unknown repository revision")
)

// File is a bounded source view tied to an exact Git revision.
type File struct {
	Repository catalog.Repository
	Revision   string
	Path       string
	Language   string
	Lines      []Line
	TotalLines int
	StartLine  int
	EndLine    int
}

// Line is one source line with a stable one-based number.
type Line struct {
	Number int
	Text   string
}

// Content is one complete, bounded UTF-8 source blob normalized to LF.
type Content struct {
	Revision string
	Path     string
	Language string
	Bytes    []byte
}

// OpenFile reads a blob through Git without touching the worktree.
func OpenFile(ctx context.Context, repository catalog.Repository, revision, filePath string, startLine, endLine int) (File, error) {
	content, err := ReadFileContent(ctx, repository, revision, filePath)
	if err != nil {
		return File{}, err
	}
	allLines := strings.Split(string(content.Bytes), "\n")
	if len(allLines) > 0 && allLines[len(allLines)-1] == "" {
		allLines = allLines[:len(allLines)-1]
	}
	if startLine <= 0 {
		startLine = 1
	}
	if endLine <= 0 {
		endLine = startLine + 199
	}
	if endLine < startLine {
		endLine = startLine
	}
	if startLine > len(allLines) && len(allLines) > 0 {
		startLine = len(allLines)
	}
	if endLine > len(allLines) {
		endLine = len(allLines)
	}

	file := File{
		Repository: repository,
		Revision:   content.Revision,
		Path:       content.Path,
		Language:   content.Language,
		TotalLines: len(allLines),
		StartLine:  startLine,
		EndLine:    endLine,
	}
	if len(allLines) == 0 {
		return file, nil
	}
	for index := startLine - 1; index < endLine; index++ {
		file.Lines = append(file.Lines, Line{Number: index + 1, Text: allLines[index]})
	}
	return file, nil
}

// ReadFileContent reads a complete source blob through Git without touching
// the worktree. Binary, non-UTF-8, and files larger than two MiB are rejected.
func ReadFileContent(ctx context.Context, repository catalog.Repository, revision, filePath string) (Content, error) {
	if revision == "" {
		revision = repository.IndexedCommit
	}
	if revision == "" {
		revision = repository.HeadCommit
	}

	filePath = strings.TrimSpace(strings.ReplaceAll(filePath, "\\", "/"))
	cleanPath := path.Clean(filePath)
	if filePath == "" || cleanPath == "." || cleanPath != filePath || path.IsAbs(cleanPath) ||
		cleanPath == ".." || strings.HasPrefix(cleanPath, "../") || strings.ContainsRune(cleanPath, 0) {
		return Content{}, ErrUnsafePath
	}

	boundedContext, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	revision, err := ResolveCommit(boundedContext, repository, revision)
	if err != nil {
		return Content{}, err
	}

	object := revision + ":" + cleanPath
	sizeText, err := gitOutput(boundedContext, repository, "cat-file", "-s", object)
	if err != nil {
		return Content{}, fmt.Errorf("inspect source blob: %w", err)
	}
	size, err := strconv.ParseInt(strings.TrimSpace(string(sizeText)), 10, 64)
	if err != nil {
		return Content{}, fmt.Errorf("parse source blob size: %w", err)
	}
	if size < 0 || size > maximumFileSize {
		return Content{}, fmt.Errorf("%w: file size %d exceeds %d bytes", ErrUnsupportedFile, size, maximumFileSize)
	}

	content, err := gitOutput(boundedContext, repository, "cat-file", "blob", object)
	if err != nil {
		return Content{}, fmt.Errorf("read source blob: %w", err)
	}
	if bytes.IndexByte(content, 0) >= 0 || !utf8.Valid(content) {
		return Content{}, fmt.Errorf("%w: binary or non-UTF-8 content", ErrUnsupportedFile)
	}

	text := strings.ReplaceAll(string(content), "\r\n", "\n")
	return Content{
		Revision: revision,
		Path:     cleanPath,
		Language: languageForPath(cleanPath),
		Bytes:    []byte(text),
	}, nil
}

func gitOutput(ctx context.Context, repository catalog.Repository, arguments ...string) ([]byte, error) {
	commandArguments := make([]string, 0, len(arguments)+2)
	if repository.Bare {
		commandArguments = append(commandArguments, "--git-dir", repository.Path)
	} else {
		commandArguments = append(commandArguments, "-C", repository.Path)
	}
	commandArguments = append(commandArguments, arguments...)

	command := exec.CommandContext(ctx, "git", commandArguments...)
	command.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_EXTERNAL_DIFF=", "LC_ALL=C")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, errors.New(message)
	}
	return output, nil
}

func isHex(value string) bool {
	if len(value) < 7 || len(value) > 64 {
		return false
	}
	for _, character := range value {
		if !((character >= '0' && character <= '9') ||
			(character >= 'a' && character <= 'f') ||
			(character >= 'A' && character <= 'F')) {
			return false
		}
	}
	return true
}

func languageForPath(filePath string) string {
	switch path.Ext(filePath) {
	case ".go":
		return "go"
	case ".ts", ".tsx":
		return "typescript"
	case ".js", ".jsx", ".mjs", ".cjs":
		return "javascript"
	case ".py":
		return "python"
	case ".rs":
		return "rust"
	case ".java":
		return "java"
	case ".c", ".h":
		return "c"
	case ".cc", ".cpp", ".cxx", ".hpp":
		return "cpp"
	case ".cs":
		return "csharp"
	case ".rb":
		return "ruby"
	case ".php":
		return "php"
	case ".html", ".htm":
		return "html"
	case ".css":
		return "css"
	case ".json":
		return "json"
	case ".yaml", ".yml":
		return "yaml"
	case ".md", ".markdown":
		return "markdown"
	case ".sh", ".bash", ".zsh":
		return "bash"
	case ".sql":
		return "sql"
	default:
		return "plaintext"
	}
}
