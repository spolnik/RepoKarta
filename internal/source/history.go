package source

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spolnik/RepoKarta/internal/catalog"
)

const historyCommandTimeout = 15 * time.Second

var (
	filesChangedPattern = regexp.MustCompile(`(\d+) files? changed`)
	insertionsPattern   = regexp.MustCompile(`(\d+) insertions?\(\+\)`)
	deletionsPattern    = regexp.MustCompile(`(\d+) deletions?\(-\)`)
)

// Commit is immutable metadata from one reachable Git commit.
type Commit struct {
	Revision    string
	Parents     []string
	AuthorName  string
	AuthorEmail string
	AuthoredAt  string
	Subject     string
	Body        string
}

// Diff is a bounded unified patch between two reachable Git commits.
type Diff struct {
	FromRevision  string
	ToRevision    string
	Patch         string
	FilesChanged  int
	Insertions    int
	Deletions     int
	Truncated     bool
	ReturnedBytes int
}

// ResolveCommit resolves a hexadecimal revision and permits it only when it is
// one of, or an ancestor of, RepoKarta's recorded indexed or HEAD commits.
func ResolveCommit(ctx context.Context, repository catalog.Repository, revision string) (string, error) {
	revision = strings.TrimSpace(revision)
	if revision == "" {
		revision = repository.IndexedCommit
	}
	if revision == "" {
		revision = repository.HeadCommit
	}
	if !isHex(revision) {
		return "", ErrUnknownRevision
	}
	output, err := gitOutput(ctx, repository, "rev-parse", "--verify", revision+"^{commit}")
	if err != nil {
		return "", ErrUnknownRevision
	}
	resolved := strings.TrimSpace(string(output))
	if !isHex(resolved) {
		return "", ErrUnknownRevision
	}
	return requireReachableCommit(ctx, repository, resolved)
}

// ResolveBranch resolves an exact local or remote-tracking branch and permits
// its tip only when it is reachable from RepoKarta's recorded indexed or HEAD
// commit. Branch names are passed as Git arguments, never shell text.
func ResolveBranch(ctx context.Context, repository catalog.Repository, branch string) (string, error) {
	branch = strings.TrimSpace(strings.ReplaceAll(branch, "\\", "/"))
	if !safeBranchName(branch) {
		return "", ErrUnknownRevision
	}
	var resolved string
	for _, prefix := range []string{"refs/heads/", "refs/remotes/"} {
		output, err := gitOutput(ctx, repository, "rev-parse", "--verify", prefix+branch+"^{commit}")
		if err != nil {
			continue
		}
		resolved = strings.TrimSpace(string(output))
		if isHex(resolved) {
			break
		}
	}
	if !isHex(resolved) {
		return "", ErrUnknownRevision
	}
	return requireReachableCommit(ctx, repository, resolved)
}

func requireReachableCommit(ctx context.Context, repository catalog.Repository, resolved string) (string, error) {
	for _, root := range []string{repository.IndexedCommit, repository.HeadCommit} {
		root = strings.TrimSpace(root)
		if !isHex(root) {
			continue
		}
		if strings.EqualFold(resolved, root) {
			return resolved, nil
		}
		if _, err := gitOutput(ctx, repository, "merge-base", "--is-ancestor", resolved, root); err == nil {
			return resolved, nil
		}
	}
	return "", ErrUnknownRevision
}

func safeBranchName(value string) bool {
	return value != "" &&
		len(value) <= 255 &&
		!strings.HasPrefix(value, "-") &&
		!strings.HasPrefix(value, "/") &&
		!strings.HasSuffix(value, "/") &&
		!strings.HasSuffix(value, ".") &&
		!strings.Contains(value, "..") &&
		!strings.Contains(value, "@{") &&
		!strings.ContainsAny(value, " \t\r\n~^:?*[\\\x00")
}

// Log returns newest-first commits reachable from a recorded revision. Both
// the commit count and serialized Git output are bounded.
func Log(ctx context.Context, repository catalog.Repository, revision, filePath string, limit, maximumBytes int) ([]Commit, bool, bool, int, error) {
	bounded, cancel := context.WithTimeout(ctx, historyCommandTimeout)
	defer cancel()
	resolved, err := ResolveCommit(bounded, repository, revision)
	if err != nil {
		return nil, false, false, 0, err
	}
	filePath, err = safeHistoryPath(filePath)
	if err != nil {
		return nil, false, false, 0, err
	}
	arguments := []string{
		"log",
		"--no-show-signature",
		"--format=%H%x1f%P%x1f%an%x1f%ae%x1f%aI%x1f%s%x1f%b%x1e",
		"-n", strconv.Itoa(limit + 1),
		resolved,
	}
	if filePath != "" {
		arguments = append(arguments, "--", filePath)
	}
	output, outputTruncated, err := gitOutputLimited(bounded, repository, maximumBytes, arguments...)
	if err != nil {
		return nil, false, false, 0, fmt.Errorf("read Git log: %w", err)
	}
	records := strings.Split(string(output), "\x1e")
	if outputTruncated && len(records) > 0 && records[len(records)-1] != "" {
		records = records[:len(records)-1]
	}
	commits := make([]Commit, 0, min(limit+1, len(records)))
	for _, record := range records {
		record = strings.TrimPrefix(record, "\n")
		record = strings.TrimSuffix(record, "\n")
		if record == "" {
			continue
		}
		fields := strings.SplitN(record, "\x1f", 7)
		if len(fields) != 7 {
			return nil, false, false, 0, fmt.Errorf("parse Git log record")
		}
		commits = append(commits, Commit{
			Revision:    fields[0],
			Parents:     strings.Fields(fields[1]),
			AuthorName:  fields[2],
			AuthorEmail: fields[3],
			AuthoredAt:  fields[4],
			Subject:     fields[5],
			Body:        strings.TrimSpace(fields[6]),
		})
	}
	truncated := len(commits) > limit
	if truncated {
		commits = commits[:limit]
	}
	return commits, truncated || outputTruncated, outputTruncated, len(output), nil
}

// DiffCommits returns an exact, bounded unified diff. When fromRevision is
// empty, the first parent of toRevision is used; a root commit is compared
// against the empty tree.
func DiffCommits(ctx context.Context, repository catalog.Repository, fromRevision, toRevision, filePath string, contextLines, maximumBytes int) (Diff, error) {
	bounded, cancel := context.WithTimeout(ctx, historyCommandTimeout)
	defer cancel()
	toRevision, err := ResolveCommit(bounded, repository, toRevision)
	if err != nil {
		return Diff{}, err
	}
	if strings.TrimSpace(fromRevision) == "" {
		parents, err := gitOutput(bounded, repository, "rev-list", "--parents", "-n", "1", toRevision)
		if err != nil {
			return Diff{}, fmt.Errorf("resolve commit parent: %w", err)
		}
		fields := strings.Fields(string(parents))
		if len(fields) > 1 {
			fromRevision = fields[1]
		}
	} else {
		fromRevision, err = ResolveCommit(bounded, repository, fromRevision)
		if err != nil {
			return Diff{}, err
		}
	}
	filePath, err = safeHistoryPath(filePath)
	if err != nil {
		return Diff{}, err
	}

	patchArguments, statArguments := diffArguments(fromRevision, toRevision, filePath, contextLines)
	patch, truncated, err := gitOutputLimited(bounded, repository, maximumBytes, patchArguments...)
	if err != nil {
		return Diff{}, fmt.Errorf("read Git diff: %w", err)
	}
	statOutput, err := gitOutput(bounded, repository, statArguments...)
	if err != nil {
		return Diff{}, fmt.Errorf("read Git diff stats: %w", err)
	}
	files, insertions, deletions := parseShortStat(string(statOutput))
	return Diff{
		FromRevision:  fromRevision,
		ToRevision:    toRevision,
		Patch:         validUTF8Prefix(patch),
		FilesChanged:  files,
		Insertions:    insertions,
		Deletions:     deletions,
		Truncated:     truncated,
		ReturnedBytes: len(patch),
	}, nil
}

func diffArguments(fromRevision, toRevision, filePath string, contextLines int) ([]string, []string) {
	contextFlag := fmt.Sprintf("--unified=%d", contextLines)
	var patchArguments, statArguments []string
	if fromRevision == "" {
		patchArguments = []string{"show", "--format=", "--no-ext-diff", "--no-textconv", "--no-renames", contextFlag, toRevision}
		statArguments = []string{"show", "--format=", "--shortstat", "--no-ext-diff", "--no-textconv", "--no-renames", toRevision}
	} else {
		patchArguments = []string{"diff", "--no-ext-diff", "--no-textconv", "--no-renames", contextFlag, fromRevision, toRevision}
		statArguments = []string{"diff", "--shortstat", "--no-ext-diff", "--no-textconv", "--no-renames", fromRevision, toRevision}
	}
	if filePath != "" {
		patchArguments = append(patchArguments, "--", filePath)
		statArguments = append(statArguments, "--", filePath)
	}
	return patchArguments, statArguments
}

func safeHistoryPath(value string) (string, error) {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if value == "" {
		return "", nil
	}
	cleaned := path.Clean(value)
	if cleaned != value || path.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.ContainsRune(cleaned, 0) {
		return "", ErrUnsafePath
	}
	return cleaned, nil
}

func parseShortStat(output string) (int, int, int) {
	return firstStatValue(filesChangedPattern, output),
		firstStatValue(insertionsPattern, output),
		firstStatValue(deletionsPattern, output)
}

func firstStatValue(pattern *regexp.Regexp, output string) int {
	match := pattern.FindStringSubmatch(output)
	if len(match) != 2 {
		return 0
	}
	value, _ := strconv.Atoi(match[1])
	return value
}

type boundedOutput struct {
	buffer bytes.Buffer
	limit  int
	total  int
}

func (w *boundedOutput) Write(value []byte) (int, error) {
	w.total += len(value)
	if remaining := w.limit - w.buffer.Len(); remaining > 0 {
		w.buffer.Write(value[:min(remaining, len(value))])
	}
	return len(value), nil
}

func gitOutputLimited(ctx context.Context, repository catalog.Repository, limit int, arguments ...string) ([]byte, bool, error) {
	commandArguments := make([]string, 0, len(arguments)+2)
	if repository.Bare {
		commandArguments = append(commandArguments, "--git-dir", repository.Path)
	} else {
		commandArguments = append(commandArguments, "-C", repository.Path)
	}
	commandArguments = append(commandArguments, arguments...)
	command := exec.CommandContext(ctx, "git", commandArguments...)
	command.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_EXTERNAL_DIFF=", "LC_ALL=C")
	var stdout boundedOutput
	stdout.limit = limit
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, false, fmt.Errorf("%s", message)
	}
	return stdout.buffer.Bytes(), stdout.total > limit, nil
}

func validUTF8Prefix(value []byte) string {
	if utf8.Valid(value) {
		return string(value)
	}
	return strings.ToValidUTF8(string(value), "\uFFFD")
}
