package insights

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/gitexec"
)

const (
	maximumDerivedFiles     = 500
	maximumDerivedBytes     = 50 << 20
	maximumDerivedFileBytes = 1 << 20
)

// Derive computes bounded, deterministic indicators from committed text. It
// invokes only read-only Git plumbing and never repository-owned programs.
func (s *Service) Derive(ctx context.Context, repositoryID int64) (Run, error) {
	repository, err := s.store.RepositoryByID(ctx, repositoryID)
	if err != nil {
		return Run{}, err
	}
	if repository.IndexedCommit == "" {
		return Run{}, errorsNew("repository has no indexed revision")
	}
	files, truncated, err := listDerivedFiles(ctx, repository, repository.IndexedCommit)
	if err != nil {
		return Run{}, err
	}
	now := time.Now().UTC()
	run := Run{
		ID: newRunID(), RepositoryID: repository.ID, Repository: repository.Name,
		Revision: repository.IndexedCommit, Branch: repository.DefaultRevision,
		Tool: "RepoKarta deterministic syntax indicators", ToolVersion: "1",
		SourceKind: "derived_committed_source", Status: StatusCurrent,
		Confidence: "syntax_approximation", ObservedAt: now, IngestedAt: now,
		Metadata: map[string]string{
			"execution_policy": "read-only Git plumbing; no repository code executed",
			"file_limit":       strconv.Itoa(maximumDerivedFiles),
			"byte_limit":       strconv.Itoa(maximumDerivedBytes),
		},
	}
	if truncated {
		run.Status = StatusPartial
		run.StatusMessage = fmt.Sprintf("derived analysis bounded to %d files and %d MiB", maximumDerivedFiles, maximumDerivedBytes>>20)
	}
	var observations []Observation
	type totals struct{ files, lines, nonblank, branches float64 }
	byLanguage := map[string]totals{}
	all := totals{}
	for _, file := range files {
		content, readErr := committedFile(ctx, repository, repository.IndexedCommit, file.path)
		if readErr != nil {
			run.Status = StatusPartial
			run.StatusMessage = appendStatus(run.StatusMessage, "some committed files could not be read")
			continue
		}
		lines, nonblank, branches := deterministicCounts(content, file.language)
		metadata := map[string]any{
			"scope": "file", "language": file.language,
			"indicator": "bounded lexical syntax approximation",
		}
		observations = append(observations,
			derivedMetric("code.lines", float64(lines), "lines", file.path, file.language, metadata),
			derivedMetric("code.nonblank_lines", float64(nonblank), "lines", file.path, file.language, metadata),
			derivedMetric("complexity.branch_points", float64(branches), "points", file.path, file.language, metadata),
		)
		value := byLanguage[file.language]
		value.files++
		value.lines += float64(lines)
		value.nonblank += float64(nonblank)
		value.branches += float64(branches)
		byLanguage[file.language] = value
		all.files++
		all.lines += float64(lines)
		all.nonblank += float64(nonblank)
		all.branches += float64(branches)
	}
	for language, value := range byLanguage {
		metadata := map[string]any{"scope": "language", "language": language}
		observations = append(observations,
			derivedMetric("code.files", value.files, "files", "", language, metadata),
			derivedMetric("code.lines", value.lines, "lines", "", language, metadata),
			derivedMetric("code.nonblank_lines", value.nonblank, "lines", "", language, metadata),
			derivedMetric("complexity.branch_points", value.branches, "points", "", language, metadata),
		)
	}
	aggregate := map[string]any{"scope": "repository"}
	observations = append(observations,
		derivedMetric("code.files", all.files, "files", "", "", aggregate),
		derivedMetric("code.lines", all.lines, "lines", "", "", aggregate),
		derivedMetric("code.nonblank_lines", all.nonblank, "lines", "", "", aggregate),
		derivedMetric("complexity.branch_points", all.branches, "points", "", "", aggregate),
	)
	for index := range observations {
		observation := &observations[index]
		observation.RunID = run.ID
		observation.RepositoryID = repository.ID
		observation.Repository = repository.Name
		observation.Revision = repository.IndexedCommit
		observation.Branch = repository.DefaultRevision
		observation.ObservedAt = now
		if observation.Path != "" {
			observation.SourceURL = s.sourceURL(repository.ID, repository.IndexedCommit, observation.Path, 1)
		}
	}
	run.ObservationCount = len(observations)
	if err := s.store.SaveInsightRun(ctx, run, observations); err != nil {
		return Run{}, err
	}
	if err := s.store.DeleteOldInsightRuns(ctx, repository.ID, run.Tool, defaultRetention); err != nil {
		return Run{}, err
	}
	return run, nil
}

type derivedFile struct {
	path     string
	language string
	size     int64
}

func listDerivedFiles(ctx context.Context, repository catalog.Repository, revision string) ([]derivedFile, bool, error) {
	result, err := gitexec.Run(ctx, gitexec.Options{
		Repository: gitexec.Repository{Directory: repository.Path},
		Timeout:    20 * time.Second,
	}, "ls-tree", "-r", "-l", revision)
	if err != nil {
		return nil, false, fmt.Errorf("list committed files for derived insights: %w", err)
	}
	var files []derivedFile
	var total int64
	truncated := false
	scanner := bufio.NewScanner(bytes.NewReader(result.Stdout))
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	for scanner.Scan() {
		line := scanner.Text()
		tab := strings.IndexByte(line, '\t')
		if tab < 0 {
			continue
		}
		fields := strings.Fields(line[:tab])
		if len(fields) < 4 || fields[1] != "blob" {
			continue
		}
		size, parseErr := strconv.ParseInt(fields[3], 10, 64)
		if parseErr != nil || size < 0 || size > maximumDerivedFileBytes {
			continue
		}
		file := strings.TrimSpace(line[tab+1:])
		language := languageForPath(file)
		if language == "" {
			continue
		}
		if len(files) >= maximumDerivedFiles || total+size > maximumDerivedBytes {
			truncated = true
			continue
		}
		files = append(files, derivedFile{path: filepath.ToSlash(file), language: language, size: size})
		total += size
	}
	if err := scanner.Err(); err != nil {
		return nil, false, err
	}
	return files, truncated, nil
}

func committedFile(ctx context.Context, repository catalog.Repository, revision, file string) ([]byte, error) {
	result, err := gitexec.Run(ctx, gitexec.Options{
		Repository: gitexec.Repository{Directory: repository.Path},
		Timeout:    5 * time.Second,
	}, "show", revision+":"+file)
	if err != nil {
		return nil, err
	}
	return result.Stdout, nil
}

func deterministicCounts(content []byte, language string) (lines, nonblank, branches int) {
	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(make([]byte, 64<<10), maximumDerivedFileBytes)
	tokens := complexityTokens(language)
	for scanner.Scan() {
		lines++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		nonblank++
		lower := strings.ToLower(line)
		for _, token := range tokens {
			branches += strings.Count(lower, token)
		}
	}
	return lines, nonblank, branches
}

func complexityTokens(language string) []string {
	switch language {
	case "Python":
		return []string{"if ", "elif ", "for ", "while ", " except ", " and ", " or "}
	case "Go":
		return []string{"if ", "for ", "case ", "&&", "||"}
	default:
		return []string{"if ", "if(", "for ", "for(", "while ", "while(", "case ", "catch ", "&&", "||", " when "}
	}
}

func languageForPath(file string) string {
	switch strings.ToLower(filepath.Ext(file)) {
	case ".go":
		return "Go"
	case ".ts", ".tsx":
		return "TypeScript"
	case ".js", ".jsx", ".mjs", ".cjs":
		return "JavaScript"
	case ".java":
		return "Java"
	case ".kt", ".kts":
		return "Kotlin"
	case ".py":
		return "Python"
	case ".cs":
		return "C#"
	case ".rs":
		return "Rust"
	case ".c", ".h":
		return "C"
	case ".cc", ".cpp", ".cxx", ".hpp":
		return "C++"
	case ".rb":
		return "Ruby"
	case ".php":
		return "PHP"
	case ".swift":
		return "Swift"
	default:
		return ""
	}
}

func derivedMetric(key string, value float64, unit, file, language string, metadata map[string]any) Observation {
	return Observation{
		Kind: KindMetric, Key: key, Value: number(value), Unit: unit, Path: file,
		Language: language, State: StateDerived, Confidence: "syntax_approximation",
		Metadata: cloneMetadata(metadata),
	}
}

func errorsNew(message string) error { return fmt.Errorf("%s", message) }
