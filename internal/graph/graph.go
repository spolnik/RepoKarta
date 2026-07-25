// Package graph derives commit-pinned repository maps without executing source
// code or trusting an AI model to invent structural facts.
package graph

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spolnik/RepoKarta/internal/catalog"
)

const (
	snapshotVersion       = 1
	maximumFiles          = 20_000
	maximumSourceFiles    = 800
	maximumSourceFileSize = 1 << 20
	commandTimeout        = 20 * time.Second
)

// RepositoryStore supplies the catalogue without exposing paths to clients.
type RepositoryStore interface {
	ListRepositories(context.Context) ([]catalog.Repository, error)
	RepositoryByID(context.Context, int64) (catalog.Repository, error)
}

// Evidence ties one structural fact to immutable source.
type Evidence struct {
	RepositoryID int64  `json:"repository_id"`
	Repository   string `json:"repository"`
	Revision     string `json:"revision"`
	Path         string `json:"path"`
	Line         int    `json:"line"`
	Label        string `json:"label"`
	URL          string `json:"url"`
}

// Language records deterministic file counts.
type Language struct {
	Name       string  `json:"name"`
	Files      int     `json:"files"`
	Percentage float64 `json:"percentage"`
}

// Manifest records one package or workspace boundary.
type Manifest struct {
	RepositoryID int64    `json:"repository_id"`
	Repository   string   `json:"repository"`
	Kind         string   `json:"kind"`
	Path         string   `json:"path"`
	Name         string   `json:"name"`
	Dependencies []string `json:"dependencies,omitempty"`
	Evidence     Evidence `json:"evidence"`
}

// Repository describes one mapped commit.
type Repository struct {
	ID        int64      `json:"id"`
	Name      string     `json:"name"`
	Revision  string     `json:"revision"`
	FileCount int        `json:"file_count"`
	Languages []Language `json:"languages"`
}

// Node is one graph fact with at least one supporting source location.
type Node struct {
	ID           string     `json:"id"`
	Kind         string     `json:"kind"`
	Label        string     `json:"label"`
	Subtitle     string     `json:"subtitle,omitempty"`
	Layer        string     `json:"layer"`
	RepositoryID int64      `json:"repository_id,omitempty"`
	Repository   string     `json:"repository,omitempty"`
	Path         string     `json:"path,omitempty"`
	Evidence     []Evidence `json:"evidence"`
}

// Edge is one directed, evidence-backed relationship.
type Edge struct {
	ID       string     `json:"id"`
	Source   string     `json:"source"`
	Target   string     `json:"target"`
	Kind     string     `json:"kind"`
	Label    string     `json:"label"`
	Evidence []Evidence `json:"evidence"`
}

// Snapshot is an immutable map derived from one or more catalogue revisions.
type Snapshot struct {
	Version      int          `json:"version"`
	ID           string       `json:"id"`
	GeneratedAt  time.Time    `json:"generated_at"`
	Repositories []Repository `json:"repositories"`
	Languages    []Language   `json:"languages"`
	Manifests    []Manifest   `json:"manifests"`
	Nodes        []Node       `json:"nodes"`
	Edges        []Edge       `json:"edges"`
	FileCount    int          `json:"file_count"`
	Truncated    bool         `json:"truncated"`
}

// Service caches graph snapshots outside source repositories.
type Service struct {
	store     RepositoryStore
	directory string
	baseURL   string
}

// New creates a read-only structural map service.
func New(store RepositoryStore, directory, baseURL string) (*Service, error) {
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, fmt.Errorf("create graph snapshot directory: %w", err)
	}
	return &Service{
		store:     store,
		directory: directory,
		baseURL:   strings.TrimRight(baseURL, "/"),
	}, nil
}

// Snapshot returns a cached commit-keyed map, regenerating it when requested.
func (s *Service) Snapshot(ctx context.Context, repositoryID int64, refresh bool) (Snapshot, error) {
	repositories, err := s.repositories(ctx, repositoryID)
	if err != nil {
		return Snapshot{}, err
	}
	signature := snapshotSignature(repositories)
	fileName := "all-" + signature + ".json"
	if repositoryID > 0 {
		fileName = fmt.Sprintf("repository-%d-%s.json", repositoryID, signature)
	}
	snapshotPath := filepath.Join(s.directory, fileName)
	if !refresh {
		if content, readErr := os.ReadFile(snapshotPath); readErr == nil {
			var cached Snapshot
			if json.Unmarshal(content, &cached) == nil && cached.Version == snapshotVersion {
				return cached, nil
			}
		}
	}

	builder := newBuilder(s.baseURL)
	for _, repository := range repositories {
		if err := builder.analyzeRepository(ctx, repository); err != nil {
			return Snapshot{}, fmt.Errorf("map repository %s: %w", repository.Name, err)
		}
	}
	snapshot := builder.snapshot(signature)
	content, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return Snapshot{}, fmt.Errorf("encode graph snapshot: %w", err)
	}
	temporary, err := os.CreateTemp(s.directory, fileName+".*.tmp")
	if err != nil {
		return Snapshot{}, fmt.Errorf("create graph snapshot: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return Snapshot{}, fmt.Errorf("write graph snapshot: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return Snapshot{}, fmt.Errorf("close graph snapshot: %w", err)
	}
	if err := os.Rename(temporaryName, snapshotPath); err != nil {
		return Snapshot{}, fmt.Errorf("publish graph snapshot: %w", err)
	}
	return snapshot, nil
}

func (s *Service) repositories(ctx context.Context, repositoryID int64) ([]catalog.Repository, error) {
	if repositoryID > 0 {
		repository, err := s.store.RepositoryByID(ctx, repositoryID)
		if err != nil {
			return nil, err
		}
		return []catalog.Repository{repository}, nil
	}
	repositories, err := s.store.ListRepositories(ctx)
	if err != nil {
		return nil, err
	}
	output := make([]catalog.Repository, 0, len(repositories))
	for _, repository := range repositories {
		if repository.IndexedCommit != "" || repository.HeadCommit != "" {
			output = append(output, repository)
		}
	}
	return output, nil
}

func snapshotSignature(repositories []catalog.Repository) string {
	hash := sha256.New()
	for _, repository := range repositories {
		fmt.Fprintf(hash, "%d\x00%s\x00%s\x00", repository.ID, repository.IndexedCommit, repository.HeadCommit)
	}
	return hex.EncodeToString(hash.Sum(nil))[:16]
}

type builder struct {
	baseURL      string
	repositories []Repository
	languages    map[string]int
	manifests    []Manifest
	nodes        map[string]Node
	edges        map[string]Edge
	fileCount    int
	truncated    bool
}

func newBuilder(baseURL string) *builder {
	return &builder{
		baseURL:   baseURL,
		languages: make(map[string]int),
		nodes:     make(map[string]Node),
		edges:     make(map[string]Edge),
	}
}

func (b *builder) snapshot(signature string) Snapshot {
	nodes := make([]Node, 0, len(b.nodes))
	for _, node := range b.nodes {
		nodes = append(nodes, node)
	}
	slices.SortFunc(nodes, func(left, right Node) int {
		if compared := strings.Compare(left.Layer, right.Layer); compared != 0 {
			return compared
		}
		return strings.Compare(strings.ToLower(left.Label), strings.ToLower(right.Label))
	})
	edges := make([]Edge, 0, len(b.edges))
	for _, edge := range b.edges {
		edges = append(edges, edge)
	}
	slices.SortFunc(edges, func(left, right Edge) int { return strings.Compare(left.ID, right.ID) })
	slices.SortFunc(b.manifests, func(left, right Manifest) int {
		if left.RepositoryID != right.RepositoryID {
			return int(left.RepositoryID - right.RepositoryID)
		}
		return strings.Compare(left.Path, right.Path)
	})
	return Snapshot{
		Version:      snapshotVersion,
		ID:           signature,
		GeneratedAt:  time.Now().UTC(),
		Repositories: b.repositories,
		Languages:    languageSummary(b.languages),
		Manifests:    b.manifests,
		Nodes:        nodes,
		Edges:        edges,
		FileCount:    b.fileCount,
		Truncated:    b.truncated,
	}
}

func (b *builder) analyzeRepository(ctx context.Context, repository catalog.Repository) error {
	revision := repository.IndexedCommit
	if revision == "" {
		revision = repository.HeadCommit
	}
	if revision == "" {
		return errors.New("repository has no recorded commit")
	}
	files, truncated, err := listFiles(ctx, repository, revision)
	if err != nil {
		return err
	}
	b.truncated = b.truncated || truncated
	b.fileCount += len(files)

	repositoryLanguages := make(map[string]int)
	for _, filePath := range files {
		if language := languageForPath(filePath); language != "" {
			repositoryLanguages[language]++
			b.languages[language]++
		}
	}
	b.repositories = append(b.repositories, Repository{
		ID:        repository.ID,
		Name:      repository.Name,
		Revision:  revision,
		FileCount: len(files),
		Languages: languageSummary(repositoryLanguages),
	})

	evidencePath := evidenceFile(files)
	repositoryEvidence := b.evidence(repository, revision, evidencePath, 1, repository.Name)
	repositoryNodeID := fmt.Sprintf("repository:%d", repository.ID)
	b.addNode(Node{
		ID:           repositoryNodeID,
		Kind:         "repository",
		Label:        repository.Name,
		Subtitle:     shortCommit(revision),
		Layer:        "Repositories",
		RepositoryID: repository.ID,
		Repository:   repository.Name,
		Path:         evidencePath,
		Evidence:     []Evidence{repositoryEvidence},
	})

	contents := make(map[string][]byte)
	sourceCount := 0
	for _, filePath := range files {
		if !isManifest(filePath) && !isAnalyzedSource(filePath) {
			continue
		}
		if isAnalyzedSource(filePath) {
			if sourceCount >= maximumSourceFiles {
				b.truncated = true
				continue
			}
			sourceCount++
		}
		content, readErr := readFile(ctx, repository, revision, filePath)
		if readErr != nil {
			continue
		}
		contents[filePath] = content
	}

	goModule := ""
	if content, ok := contents["go.mod"]; ok {
		goModule = b.addGoManifest(repository, revision, repositoryNodeID, "go.mod", content)
	}
	packageIDs := b.addPackages(repository, revision, repositoryNodeID, files, contents)
	b.addGoImportsAndRoutes(repository, revision, goModule, packageIDs, contents)
	b.addOtherManifests(repository, revision, repositoryNodeID, contents)
	return nil
}

func (b *builder) addGoManifest(
	repository catalog.Repository,
	revision, repositoryNodeID, filePath string,
	content []byte,
) string {
	module, dependencies := parseGoMod(content)
	if module == "" {
		module = repository.Name
	}
	evidence := b.evidence(repository, revision, filePath, lineContaining(content, "module "), module)
	b.manifests = append(b.manifests, Manifest{
		RepositoryID: repository.ID,
		Repository:   repository.Name,
		Kind:         "Go module",
		Path:         filePath,
		Name:         module,
		Dependencies: dependencies,
		Evidence:     evidence,
	})
	for _, dependency := range dependencies {
		dependencyEvidence := b.evidence(
			repository,
			revision,
			filePath,
			lineContaining(content, dependency),
			dependency,
		)
		dependencyNodeID := "dependency:" + normalizeID(dependency)
		b.addNode(Node{
			ID:       dependencyNodeID,
			Kind:     "dependency",
			Label:    dependency,
			Subtitle: "Go module",
			Layer:    "Dependencies",
			Evidence: []Evidence{dependencyEvidence},
		})
		b.addEdge(Edge{
			ID:       edgeID(repositoryNodeID, dependencyNodeID, "depends"),
			Source:   repositoryNodeID,
			Target:   dependencyNodeID,
			Kind:     "dependency",
			Label:    "requires",
			Evidence: []Evidence{dependencyEvidence},
		})
	}
	return module
}

func (b *builder) addPackages(
	repository catalog.Repository,
	revision, repositoryNodeID string,
	files []string,
	contents map[string][]byte,
) map[string]string {
	firstGoFile := make(map[string]string)
	for _, filePath := range files {
		if path.Ext(filePath) == ".go" && !strings.HasSuffix(filePath, "_test.go") {
			directory := path.Dir(filePath)
			if directory == "." {
				directory = ""
			}
			if firstGoFile[directory] == "" {
				firstGoFile[directory] = filePath
			}
		}
	}
	packageIDs := make(map[string]string)
	directories := make([]string, 0, len(firstGoFile))
	for directory := range firstGoFile {
		directories = append(directories, directory)
	}
	sort.Strings(directories)
	for _, directory := range directories {
		filePath := firstGoFile[directory]
		label := path.Base(directory)
		if directory == "" {
			label = repository.Name
		}
		nodeID := fmt.Sprintf("package:%d:%s", repository.ID, normalizeID(directory))
		packageIDs[directory] = nodeID
		evidence := b.evidence(repository, revision, filePath, 1, label)
		b.addNode(Node{
			ID:           nodeID,
			Kind:         "package",
			Label:        label,
			Subtitle:     firstNonEmpty(directory, "repository root"),
			Layer:        "Packages",
			RepositoryID: repository.ID,
			Repository:   repository.Name,
			Path:         directory,
			Evidence:     []Evidence{evidence},
		})
		b.addEdge(Edge{
			ID:       edgeID(repositoryNodeID, nodeID, "contains"),
			Source:   repositoryNodeID,
			Target:   nodeID,
			Kind:     "contains",
			Label:    "contains",
			Evidence: []Evidence{evidence},
		})
		if strings.HasPrefix(directory, "cmd/") || directory == "cmd" {
			entryID := fmt.Sprintf("entrypoint:%d:%s", repository.ID, normalizeID(directory))
			b.addNode(Node{
				ID:           entryID,
				Kind:         "entrypoint",
				Label:        label,
				Subtitle:     filePath,
				Layer:        "Entrypoints",
				RepositoryID: repository.ID,
				Repository:   repository.Name,
				Path:         filePath,
				Evidence:     []Evidence{evidence},
			})
			b.addEdge(Edge{
				ID:       edgeID(nodeID, entryID, "exposes"),
				Source:   nodeID,
				Target:   entryID,
				Kind:     "entrypoint",
				Label:    "builds",
				Evidence: []Evidence{evidence},
			})
		}
	}

	packageManifests := make([]string, 0)
	for filePath := range contents {
		if path.Base(filePath) == "package.json" {
			packageManifests = append(packageManifests, filePath)
		}
	}
	sort.Strings(packageManifests)
	for _, manifestPath := range packageManifests {
		var manifest struct {
			Name            string            `json:"name"`
			Dependencies    map[string]string `json:"dependencies"`
			DevDependencies map[string]string `json:"devDependencies"`
		}
		content := contents[manifestPath]
		if json.Unmarshal(content, &manifest) != nil {
			continue
		}
		directory := path.Dir(manifestPath)
		if directory == "." {
			directory = ""
		}
		label := firstNonEmpty(manifest.Name, repository.Name)
		nodeID := fmt.Sprintf("package:%d:npm:%s", repository.ID, normalizeID(firstNonEmpty(directory, "root")))
		packageIDs["npm:"+directory] = nodeID
		evidence := b.evidence(repository, revision, manifestPath, lineContaining(content, `"name"`), label)
		b.addNode(Node{
			ID:           nodeID,
			Kind:         "package",
			Label:        label,
			Subtitle:     firstNonEmpty(directory, "repository root"),
			Layer:        "Packages",
			RepositoryID: repository.ID,
			Repository:   repository.Name,
			Path:         directory,
			Evidence:     []Evidence{evidence},
		})
		b.addEdge(Edge{
			ID:       edgeID(repositoryNodeID, nodeID, "contains"),
			Source:   repositoryNodeID,
			Target:   nodeID,
			Kind:     "contains",
			Label:    "contains",
			Evidence: []Evidence{evidence},
		})
		dependencies := sortedKeys(manifest.Dependencies, manifest.DevDependencies)
		b.manifests = append(b.manifests, Manifest{
			RepositoryID: repository.ID,
			Repository:   repository.Name,
			Kind:         "npm package",
			Path:         manifestPath,
			Name:         label,
			Dependencies: dependencies,
			Evidence:     evidence,
		})
		for _, dependency := range dependencies {
			dependencyEvidence := b.evidence(repository, revision, manifestPath, lineContaining(content, `"`+dependency+`"`), dependency)
			dependencyNodeID := "dependency:" + normalizeID(dependency)
			b.addNode(Node{
				ID:       dependencyNodeID,
				Kind:     "dependency",
				Label:    dependency,
				Subtitle: "npm package",
				Layer:    "Dependencies",
				Evidence: []Evidence{dependencyEvidence},
			})
			b.addEdge(Edge{
				ID:       edgeID(nodeID, dependencyNodeID, "depends"),
				Source:   nodeID,
				Target:   dependencyNodeID,
				Kind:     "dependency",
				Label:    "depends on",
				Evidence: []Evidence{dependencyEvidence},
			})
		}
	}
	return packageIDs
}

func (b *builder) addGoImportsAndRoutes(
	repository catalog.Repository,
	revision, module string,
	packageIDs map[string]string,
	contents map[string][]byte,
) {
	routePattern := regexp.MustCompile(`(?:Handle|HandleFunc)\(\s*["` + "`" + `]([^"` + "`" + `]+)`)
	for filePath, content := range contents {
		if path.Ext(filePath) != ".go" {
			continue
		}
		directory := path.Dir(filePath)
		if directory == "." {
			directory = ""
		}
		sourceID := packageIDs[directory]
		if sourceID == "" {
			continue
		}
		fileSet := token.NewFileSet()
		parsed, err := parser.ParseFile(fileSet, filePath, content, parser.ImportsOnly)
		if err == nil {
			for _, importSpec := range parsed.Imports {
				importPath, unquoteErr := strconv.Unquote(importSpec.Path.Value)
				if unquoteErr != nil || module == "" || !strings.HasPrefix(importPath, module) {
					continue
				}
				targetDirectory := strings.TrimPrefix(strings.TrimPrefix(importPath, module), "/")
				targetID := packageIDs[targetDirectory]
				if targetID == "" || targetID == sourceID {
					continue
				}
				line := fileSet.Position(importSpec.Pos()).Line
				evidence := b.evidence(repository, revision, filePath, line, importPath)
				b.addEdge(Edge{
					ID:       edgeID(sourceID, targetID, "imports"),
					Source:   sourceID,
					Target:   targetID,
					Kind:     "import",
					Label:    "imports",
					Evidence: []Evidence{evidence},
				})
			}
		}
		for _, match := range routePattern.FindAllSubmatchIndex(content, -1) {
			route := string(content[match[2]:match[3]])
			line := lineAtOffset(content, match[0])
			evidence := b.evidence(repository, revision, filePath, line, route)
			routeID := fmt.Sprintf("route:%d:%s", repository.ID, normalizeID(route))
			b.addNode(Node{
				ID:           routeID,
				Kind:         "route",
				Label:        route,
				Subtitle:     path.Base(filePath),
				Layer:        "Routes",
				RepositoryID: repository.ID,
				Repository:   repository.Name,
				Path:         filePath,
				Evidence:     []Evidence{evidence},
			})
			b.addEdge(Edge{
				ID:       edgeID(sourceID, routeID, "serves"),
				Source:   sourceID,
				Target:   routeID,
				Kind:     "route",
				Label:    "serves",
				Evidence: []Evidence{evidence},
			})
		}
	}
}

func (b *builder) addOtherManifests(
	repository catalog.Repository,
	revision, repositoryNodeID string,
	contents map[string][]byte,
) {
	paths := make([]string, 0, len(contents))
	for filePath := range contents {
		if isManifest(filePath) && path.Base(filePath) != "go.mod" && path.Base(filePath) != "package.json" {
			paths = append(paths, filePath)
		}
	}
	sort.Strings(paths)
	for _, filePath := range paths {
		kind := manifestKind(filePath)
		if kind == "" {
			continue
		}
		evidence := b.evidence(repository, revision, filePath, 1, kind)
		manifestID := fmt.Sprintf("manifest:%d:%s", repository.ID, normalizeID(filePath))
		b.addNode(Node{
			ID:           manifestID,
			Kind:         "manifest",
			Label:        path.Base(filePath),
			Subtitle:     kind,
			Layer:        "Packages",
			RepositoryID: repository.ID,
			Repository:   repository.Name,
			Path:         filePath,
			Evidence:     []Evidence{evidence},
		})
		b.addEdge(Edge{
			ID:       edgeID(repositoryNodeID, manifestID, "declares"),
			Source:   repositoryNodeID,
			Target:   manifestID,
			Kind:     "manifest",
			Label:    "declares",
			Evidence: []Evidence{evidence},
		})
		b.manifests = append(b.manifests, Manifest{
			RepositoryID: repository.ID,
			Repository:   repository.Name,
			Kind:         kind,
			Path:         filePath,
			Name:         path.Base(filePath),
			Evidence:     evidence,
		})
	}
}

func (b *builder) evidence(
	repository catalog.Repository,
	revision, filePath string,
	line int,
	label string,
) Evidence {
	if line <= 0 {
		line = 1
	}
	sourceURL := ""
	if filePath != "" {
		sourceURL = fmt.Sprintf(
			"%s/source/%d?rev=%s&path=%s&focus=%d-%d#L%d",
			b.baseURL,
			repository.ID,
			url.QueryEscape(revision),
			url.QueryEscape(filePath),
			line,
			line,
			line,
		)
	}
	return Evidence{
		RepositoryID: repository.ID,
		Repository:   repository.Name,
		Revision:     revision,
		Path:         filePath,
		Line:         line,
		Label:        label,
		URL:          sourceURL,
	}
}

func (b *builder) addNode(node Node) {
	if existing, ok := b.nodes[node.ID]; ok {
		existing.Evidence = appendUniqueEvidence(existing.Evidence, node.Evidence...)
		b.nodes[node.ID] = existing
		return
	}
	b.nodes[node.ID] = node
}

func (b *builder) addEdge(edge Edge) {
	if existing, ok := b.edges[edge.ID]; ok {
		existing.Evidence = appendUniqueEvidence(existing.Evidence, edge.Evidence...)
		b.edges[edge.ID] = existing
		return
	}
	b.edges[edge.ID] = edge
}

func appendUniqueEvidence(existing []Evidence, candidates ...Evidence) []Evidence {
	for _, candidate := range candidates {
		found := false
		for _, evidence := range existing {
			if evidence.RepositoryID == candidate.RepositoryID &&
				evidence.Revision == candidate.Revision &&
				evidence.Path == candidate.Path &&
				evidence.Line == candidate.Line {
				found = true
				break
			}
		}
		if !found {
			existing = append(existing, candidate)
		}
	}
	return existing
}

func listFiles(ctx context.Context, repository catalog.Repository, revision string) ([]string, bool, error) {
	output, err := gitOutput(ctx, repository, "ls-tree", "-r", "--name-only", revision)
	if err != nil {
		return nil, false, err
	}
	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 64*1024), 2<<20)
	files := make([]string, 0)
	truncated := false
	for scanner.Scan() {
		filePath := strings.TrimSpace(strings.ReplaceAll(scanner.Text(), "\\", "/"))
		if filePath == "" {
			continue
		}
		if len(files) >= maximumFiles {
			truncated = true
			break
		}
		files = append(files, filePath)
	}
	return files, truncated, scanner.Err()
}

func readFile(ctx context.Context, repository catalog.Repository, revision, filePath string) ([]byte, error) {
	sizeOutput, err := gitOutput(ctx, repository, "cat-file", "-s", revision+":"+filePath)
	if err != nil {
		return nil, err
	}
	size, err := strconv.ParseInt(strings.TrimSpace(string(sizeOutput)), 10, 64)
	if err != nil || size < 0 || size > maximumSourceFileSize {
		return nil, errors.New("source file is outside graph analysis bounds")
	}
	return gitOutput(ctx, repository, "cat-file", "blob", revision+":"+filePath)
}

func gitOutput(ctx context.Context, repository catalog.Repository, arguments ...string) ([]byte, error) {
	bounded, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	commandArguments := make([]string, 0, len(arguments)+2)
	if repository.Bare {
		commandArguments = append(commandArguments, "--git-dir", repository.Path)
	} else {
		commandArguments = append(commandArguments, "-C", repository.Path)
	}
	commandArguments = append(commandArguments, arguments...)
	command := exec.CommandContext(bounded, "git", commandArguments...)
	command.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_EXTERNAL_DIFF=", "LC_ALL=C")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %s", arguments[0], firstNonEmpty(strings.TrimSpace(stderr.String()), err.Error()))
	}
	return output, nil
}

func parseGoMod(content []byte) (string, []string) {
	module := ""
	dependencies := make([]string, 0)
	inRequire := false
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(strings.SplitN(scanner.Text(), "//", 2)[0])
		switch {
		case strings.HasPrefix(line, "module "):
			module = strings.TrimSpace(strings.TrimPrefix(line, "module "))
		case line == "require (":
			inRequire = true
		case inRequire && line == ")":
			inRequire = false
		case strings.HasPrefix(line, "require "):
			fields := strings.Fields(strings.TrimPrefix(line, "require "))
			if len(fields) > 0 {
				dependencies = append(dependencies, fields[0])
			}
		case inRequire:
			fields := strings.Fields(line)
			if len(fields) > 0 {
				dependencies = append(dependencies, fields[0])
			}
		}
	}
	dependencies = uniqueSorted(dependencies)
	return module, dependencies
}

func languageForPath(filePath string) string {
	switch strings.ToLower(path.Ext(filePath)) {
	case ".go":
		return "Go"
	case ".ts", ".tsx":
		return "TypeScript"
	case ".js", ".jsx", ".mjs", ".cjs":
		return "JavaScript"
	case ".py":
		return "Python"
	case ".rs":
		return "Rust"
	case ".java":
		return "Java"
	case ".cs":
		return "C#"
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
	case ".kt", ".kts":
		return "Kotlin"
	case ".html", ".htm":
		return "HTML"
	case ".css", ".scss", ".sass":
		return "CSS"
	case ".sql":
		return "SQL"
	case ".md", ".markdown":
		return "Markdown"
	case ".yaml", ".yml":
		return "YAML"
	case ".json":
		return "JSON"
	case ".sh", ".bash", ".zsh":
		return "Shell"
	default:
		return ""
	}
}

func languageSummary(counts map[string]int) []Language {
	total := 0
	for _, count := range counts {
		total += count
	}
	languages := make([]Language, 0, len(counts))
	for name, count := range counts {
		percentage := 0.0
		if total > 0 {
			percentage = float64(count) / float64(total) * 100
		}
		languages = append(languages, Language{Name: name, Files: count, Percentage: percentage})
	}
	slices.SortFunc(languages, func(left, right Language) int {
		if left.Files != right.Files {
			return right.Files - left.Files
		}
		return strings.Compare(left.Name, right.Name)
	})
	return languages
}

func isManifest(filePath string) bool {
	switch path.Base(filePath) {
	case "go.mod", "package.json", "pnpm-workspace.yaml", "Cargo.toml", "pyproject.toml",
		"requirements.txt", "pom.xml", "build.gradle", "build.gradle.kts", "composer.json",
		"Gemfile", "Package.swift":
		return true
	default:
		return strings.HasSuffix(filePath, ".csproj") || strings.HasSuffix(filePath, ".sln")
	}
}

func manifestKind(filePath string) string {
	switch path.Base(filePath) {
	case "pnpm-workspace.yaml":
		return "pnpm workspace"
	case "Cargo.toml":
		return "Cargo package"
	case "pyproject.toml":
		return "Python project"
	case "requirements.txt":
		return "Python requirements"
	case "pom.xml":
		return "Maven project"
	case "build.gradle", "build.gradle.kts":
		return "Gradle project"
	case "composer.json":
		return "Composer package"
	case "Gemfile":
		return "Ruby bundle"
	case "Package.swift":
		return "Swift package"
	default:
		switch {
		case strings.HasSuffix(filePath, ".csproj"):
			return ".NET project"
		case strings.HasSuffix(filePath, ".sln"):
			return ".NET solution"
		default:
			return ""
		}
	}
}

func isAnalyzedSource(filePath string) bool {
	switch strings.ToLower(path.Ext(filePath)) {
	case ".go":
		return true
	default:
		return false
	}
}

func evidenceFile(files []string) string {
	for _, candidate := range []string{"README.md", "readme.md", "go.mod", "package.json", "pyproject.toml", "Cargo.toml"} {
		if slices.Contains(files, candidate) {
			return candidate
		}
	}
	if len(files) > 0 {
		return files[0]
	}
	return ""
}

func lineContaining(content []byte, value string) int {
	lines := bytes.Split(content, []byte("\n"))
	for index, line := range lines {
		if bytes.Contains(line, []byte(value)) {
			return index + 1
		}
	}
	return 1
}

func lineAtOffset(content []byte, offset int) int {
	if offset <= 0 {
		return 1
	}
	return bytes.Count(content[:min(offset, len(content))], []byte("\n")) + 1
}

func sortedKeys(maps ...map[string]string) []string {
	values := make([]string, 0)
	for _, items := range maps {
		for key := range items {
			values = append(values, key)
		}
	}
	return uniqueSorted(values)
}

func uniqueSorted(values []string) []string {
	sort.Strings(values)
	output := values[:0]
	for _, value := range values {
		if value == "" || (len(output) > 0 && output[len(output)-1] == value) {
			continue
		}
		output = append(output, value)
	}
	return output
}

func normalizeID(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(value, "-")
	return strings.Trim(value, "-")
}

func edgeID(source, target, kind string) string {
	return kind + ":" + source + ":" + target
}

func shortCommit(revision string) string {
	if len(revision) > 8 {
		return revision[:8]
	}
	return revision
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
