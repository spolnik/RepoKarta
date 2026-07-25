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
	"sync"
	"time"

	"github.com/spolnik/RepoKarta/internal/analysis"
	"github.com/spolnik/RepoKarta/internal/catalog"
)

const (
	snapshotVersion       = 4
	maximumFiles          = 20_000
	maximumSourceFiles    = 5_000
	maximumSourceFileSize = 1 << 20
	// Structural budgets keep syntax-backed inventories useful without turning
	// a repository map into an unbounded dump of every call site.
	maximumStructuralDocuments  = 3_000
	maximumStructuralSymbols    = 12_000
	maximumStructuralRelations  = 24_000
	maximumStructuralBuildFacts = 4_000
	// Curated layer budgets keep large Java and Kotlin services legible.
	maximumComponentsPerRepository = 300
	maximumRoutesPerRepository     = 900
	// maximumCollectionRepositories bounds the cross-repository view. A
	// collection of several hundred repositories would otherwise be analyzed in
	// full before anything could render.
	maximumCollectionRepositories = 40
	commandTimeout                = 20 * time.Second
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
	ID         string     `json:"id"`
	Source     string     `json:"source"`
	Target     string     `json:"target"`
	Kind       string     `json:"kind"`
	Label      string     `json:"label"`
	Confidence string     `json:"confidence,omitempty"`
	Evidence   []Evidence `json:"evidence"`
}

// Scope makes collection bounding part of the public map contract. Truncated
// still reports any bounded analysis; Scope says specifically whether every
// repository requested by the caller was analyzed.
type Scope struct {
	Kind                  string `json:"kind"`
	Complete              bool   `json:"complete"`
	TotalRepositories     int    `json:"total_repositories"`
	AnalyzedRepositories  int    `json:"analyzed_repositories"`
	OmittedRepositories   int    `json:"omitted_repositories"`
	RepositoryLimit       int    `json:"repository_limit,omitempty"`
	RequestedRepositoryID int64  `json:"requested_repository_id,omitempty"`
}

// StructuralDocument is a bounded syntax-tree index for one immutable source
// file. Framework extractors consume these facts separately from the curated
// graph so generic call sites do not turn the visual map into a hairball.
type StructuralDocument struct {
	RepositoryID  int64                 `json:"repository_id"`
	Repository    string                `json:"repository"`
	Revision      string                `json:"revision"`
	Path          string                `json:"path"`
	Language      string                `json:"language"`
	Parser        string                `json:"parser"`
	ParseComplete bool                  `json:"parse_complete"`
	Truncated     bool                  `json:"truncated"`
	Symbols       []analysis.Symbol     `json:"symbols,omitempty"`
	Relations     []analysis.Relation   `json:"relations,omitempty"`
	BuildFacts    []analysis.BuildFact  `json:"build_facts,omitempty"`
	Diagnostics   []analysis.Diagnostic `json:"diagnostics,omitempty"`
}

// Snapshot is an immutable map derived from one or more catalogue revisions.
type Snapshot struct {
	Version            int                  `json:"version"`
	ID                 string               `json:"id"`
	GeneratedAt        time.Time            `json:"generated_at"`
	Repositories       []Repository         `json:"repositories"`
	Languages          []Language           `json:"languages"`
	Manifests          []Manifest           `json:"manifests"`
	Nodes              []Node               `json:"nodes"`
	Edges              []Edge               `json:"edges"`
	Structure          []StructuralDocument `json:"structure,omitempty"`
	StructureTruncated bool                 `json:"structure_truncated"`
	FileCount          int                  `json:"file_count"`
	Truncated          bool                 `json:"truncated"`
	Scope              Scope                `json:"scope"`
}

func rebaseSnapshotEvidence(snapshot *Snapshot, baseURL string) {
	rebase := func(evidence *Evidence) {
		if evidence.Path == "" {
			evidence.URL = ""
			return
		}
		line := max(1, evidence.Line)
		evidence.URL = fmt.Sprintf(
			"%s/source/%d?rev=%s&path=%s&focus=%d-%d#L%d",
			baseURL,
			evidence.RepositoryID,
			url.QueryEscape(evidence.Revision),
			url.QueryEscape(evidence.Path),
			line,
			line,
			line,
		)
	}
	for index := range snapshot.Manifests {
		rebase(&snapshot.Manifests[index].Evidence)
	}
	for nodeIndex := range snapshot.Nodes {
		for evidenceIndex := range snapshot.Nodes[nodeIndex].Evidence {
			rebase(&snapshot.Nodes[nodeIndex].Evidence[evidenceIndex])
		}
	}
	for edgeIndex := range snapshot.Edges {
		for evidenceIndex := range snapshot.Edges[edgeIndex].Evidence {
			rebase(&snapshot.Edges[edgeIndex].Evidence[evidenceIndex])
		}
	}
}

// Service caches graph snapshots outside source repositories.
type Service struct {
	store     RepositoryStore
	directory string
	mu        sync.RWMutex
	baseURL   string
}

// SetBaseURL changes the absolute URL used for source evidence.
func (s *Service) SetBaseURL(baseURL string) {
	s.mu.Lock()
	s.baseURL = strings.TrimRight(baseURL, "/")
	s.mu.Unlock()
}

func (s *Service) currentBaseURL() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.baseURL
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
				rebaseSnapshotEvidence(&cached, s.currentBaseURL())
				return cached, nil
			}
		}
	}

	builder := newBuilder(s.currentBaseURL())
	// A cross-repository map over a large collection is expensive and rarely
	// legible, so the collection view analyzes a bounded number of
	// repositories and reports the cut explicitly rather than appearing
	// complete. Selecting one repository is never truncated.
	analyzed := repositories
	if repositoryID == 0 && len(analyzed) > maximumCollectionRepositories {
		analyzed = analyzed[:maximumCollectionRepositories]
		builder.truncated = true
	}
	for _, repository := range analyzed {
		if err := builder.analyzeRepository(ctx, repository); err != nil {
			return Snapshot{}, fmt.Errorf("map repository %s: %w", repository.Name, err)
		}
	}
	snapshot := builder.snapshot(signature)
	snapshot.Scope = Scope{
		Kind:                  "repository",
		Complete:              true,
		TotalRepositories:     len(repositories),
		AnalyzedRepositories:  len(analyzed),
		RequestedRepositoryID: repositoryID,
	}
	if repositoryID == 0 {
		snapshot.Scope.Kind = "collection"
		snapshot.Scope.RepositoryLimit = maximumCollectionRepositories
		snapshot.Scope.OmittedRepositories = max(0, len(repositories)-len(analyzed))
		snapshot.Scope.Complete = snapshot.Scope.OmittedRepositories == 0
	}
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
	baseURL              string
	repositories         []Repository
	languages            map[string]int
	manifests            []Manifest
	nodes                map[string]Node
	edges                map[string]Edge
	serviceTargets       map[string]string
	clientReferences     []clientReference
	structure            []StructuralDocument
	structuralSymbols    int
	structuralRelations  int
	structuralBuildFacts int
	structureTruncated   bool
	fileCount            int
	truncated            bool
}

type clientReference struct {
	sourceRepositoryID int64
	target             string
	confidence         string
	evidence           Evidence
}

func newBuilder(baseURL string) *builder {
	return &builder{
		baseURL:        baseURL,
		languages:      make(map[string]int),
		nodes:          make(map[string]Node),
		edges:          make(map[string]Edge),
		serviceTargets: make(map[string]string),
	}
}

func (b *builder) snapshot(signature string) Snapshot {
	b.resolveClientReferences()
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
	slices.SortFunc(b.structure, func(left, right StructuralDocument) int {
		if left.RepositoryID != right.RepositoryID {
			return int(left.RepositoryID - right.RepositoryID)
		}
		return strings.Compare(left.Path, right.Path)
	})
	return Snapshot{
		Version:            snapshotVersion,
		ID:                 signature,
		GeneratedAt:        time.Now().UTC(),
		Repositories:       b.repositories,
		Languages:          languageSummary(b.languages),
		Manifests:          b.manifests,
		Nodes:              nodes,
		Edges:              edges,
		Structure:          b.structure,
		StructureTruncated: b.structureTruncated,
		FileCount:          b.fileCount,
		Truncated:          b.truncated,
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
	b.registerServiceTarget(repository.Name, repositoryNodeID)
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
		if !isManifest(filePath) && !isAnalyzedSource(filePath) &&
			!isServiceConfiguration(filePath) {
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
	b.addStructuralAnalysis(repository, revision, files, contents)
	b.addGoImportsAndRoutes(repository, revision, goModule, packageIDs, contents)
	b.addServiceIdentity(repositoryNodeID, contents)
	b.addServiceConfigurationReferences(repository, revision, contents)
	b.addJavaStructure(repository, revision, repositoryNodeID, contents)
	b.addGradleManifests(repository, revision, repositoryNodeID, contents)
	b.addOtherManifests(repository, revision, repositoryNodeID, contents)
	return nil
}

func (b *builder) addStructuralAnalysis(
	repository catalog.Repository,
	revision string,
	files []string,
	contents map[string][]byte,
) {
	for _, filePath := range files {
		content, exists := contents[filePath]
		if !exists {
			continue
		}
		document, supported := analysis.Analyze(filePath, content)
		if !supported {
			continue
		}
		if len(b.structure) >= maximumStructuralDocuments {
			b.structureTruncated = true
			return
		}
		symbolBudget := maximumStructuralSymbols - b.structuralSymbols
		relationBudget := maximumStructuralRelations - b.structuralRelations
		buildFactBudget := maximumStructuralBuildFacts - b.structuralBuildFacts
		if len(document.Symbols) > symbolBudget ||
			len(document.Relations) > relationBudget ||
			len(document.BuildFacts) > buildFactBudget {
			b.structureTruncated = true
		}
		b.structureTruncated = b.structureTruncated || document.Truncated
		document.Symbols = document.Symbols[:min(len(document.Symbols), symbolBudget)]
		document.Relations = document.Relations[:min(len(document.Relations), relationBudget)]
		document.BuildFacts = document.BuildFacts[:min(len(document.BuildFacts), buildFactBudget)]
		b.structuralSymbols += len(document.Symbols)
		b.structuralRelations += len(document.Relations)
		b.structuralBuildFacts += len(document.BuildFacts)
		b.structure = append(b.structure, StructuralDocument{
			RepositoryID:  repository.ID,
			Repository:    repository.Name,
			Revision:      revision,
			Path:          document.Path,
			Language:      document.Language,
			Parser:        document.Parser,
			ParseComplete: document.ParseComplete,
			Truncated:     document.Truncated,
			Symbols:       document.Symbols,
			Relations:     document.Relations,
			BuildFacts:    document.BuildFacts,
			Diagnostics:   document.Diagnostics,
		})
		if b.structuralSymbols >= maximumStructuralSymbols &&
			b.structuralRelations >= maximumStructuralRelations &&
			b.structuralBuildFacts >= maximumStructuralBuildFacts {
			b.structureTruncated = true
			return
		}
	}
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

type gradleDependency struct {
	coordinate string
	line       int
}

// gradleConfigurations lists the dependency configurations RepoKarta reads from
// Groovy and Kotlin Gradle build scripts. gradleContainers are the wrapper
// functions that may surround a coordinate, notably BOM imports.
const (
	gradleConfigurations = `(?:api|implementation|compileOnly|compileOnlyApi|runtimeOnly|developmentOnly|` +
		`annotationProcessor|kapt|kaptTest|ksp|kspTest|classpath|` +
		`testApi|testImplementation|testCompileOnly|testRuntimeOnly|testAnnotationProcessor|` +
		`testFixturesApi|testFixturesImplementation|` +
		`integrationTestImplementation|intTestImplementation|e2eTestImplementation)`
	gradleContainers = `(?:(?:enforcedPlatform|platform|testFixtures)\s*\(\s*)?`
	gradleConfigOpen = `\s*(?:\(\s*)?`
)

var (
	gradleStringDependency = regexp.MustCompile(
		`(?m)^\s*` + gradleConfigurations + gradleConfigOpen + gradleContainers + `["']([^"']+)["']`,
	)
	// gradleNamedDependency covers both Groovy map syntax
	// (`implementation group: "g", name: "a", version: "v"`) and Kotlin named
	// arguments (`implementation(group = "g", name = "a", version = "v")`) in
	// any argument order.
	gradleNamedDependency = regexp.MustCompile(
		`(?m)^\s*` + gradleConfigurations + gradleConfigOpen + gradleContainers +
			`((?:group|name|version|module)\s*[:=]\s*["'][^"']*["']` +
			`(?:\s*,\s*[A-Za-z]+\s*[:=]\s*["'][^"']*["'])*)`,
	)
	gradleNamedField = regexp.MustCompile(
		`(?:^|,)\s*([A-Za-z]+)\s*[:=]\s*["']([^"']*)["']`,
	)
	catalogInlineField = regexp.MustCompile(
		`(?:^|[,{]\s*)([A-Za-z][A-Za-z.]*)\s*=\s*["']([^"']*)["']`,
	)
	gradleProjectDependency = regexp.MustCompile(
		`(?m)^\s*` + gradleConfigurations + gradleConfigOpen + gradleContainers +
			`project\s*\(\s*(?:path\s*[:=]\s*)?["'](:?[^"']+)["']`,
	)
	gradleCatalogDependency = regexp.MustCompile(
		`(?m)^\s*` + gradleConfigurations + gradleConfigOpen + gradleContainers +
			`libs\.([A-Za-z0-9_.-]+)`,
	)
	gradleProjectName = regexp.MustCompile(
		`(?m)^\s*rootProject\.name\s*[:=]\s*["']([^"']+)["']`,
	)
	gradleVersionAssignment = regexp.MustCompile(
		`(?m)^\s*(?:def\s+|val\s+|var\s+)?(?:ext\.)?([A-Za-z_][A-Za-z0-9_.-]*)\s*=\s*["']([^"'$]+)["']`,
	)
	gradleExtraVersionAssignment = regexp.MustCompile(
		`(?m)^\s*extra\s*\[\s*["']([A-Za-z_][A-Za-z0-9_.-]*)["']\s*]\s*=\s*["']([^"'$]+)["']`,
	)
	gradleInterpolation           = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_.-]*)\}|\$([A-Za-z_][A-Za-z0-9_.-]*)`)
	springApplicationNameProperty = regexp.MustCompile(
		`(?m)^\s*spring\.application\.name\s*[=:]\s*["']?([A-Za-z0-9][A-Za-z0-9_.-]*)["']?\s*$`,
	)
	yamlKeyValue     = regexp.MustCompile(`^(\s*)([A-Za-z0-9_.-]+)\s*:\s*(.*)$`)
	javaClassPattern = regexp.MustCompile(
		`(?m)^[^/*\n]*\b(?:class|interface|object)\s+([A-Za-z_$][A-Za-z0-9_$]*)`,
	)
	springMappingPattern = regexp.MustCompile(
		`(?s)@(?:[A-Za-z0-9_$.]+\.)?(GetMapping|PostMapping|PutMapping|DeleteMapping|PatchMapping|RequestMapping)\s*(?:\((.*?)\))?`,
	)
	// A type annotated with @FeignClient or @HttpExchange declares endpoints it
	// calls, not endpoints it serves, so its mappings never become routes.
	declarativeClientType = regexp.MustCompile(`@(?:FeignClient|HttpExchange)\b`)
	quotedJavaString      = regexp.MustCompile(`["']([^"']*)["']`)
	springFunctionalRoute = regexp.MustCompile(
		`(?m)\b(?:RequestPredicates\.)?(GET|POST|PUT|DELETE|PATCH)\s*\(\s*["']([^"']+)["']`,
	)
	feignClientPattern = regexp.MustCompile(`(?s)@FeignClient\s*\((.*?)\)`)
	feignNamedTarget   = regexp.MustCompile(
		`(?i)(?:name|value|serviceId)\s*=\s*["']([^"']+)["']`,
	)
	// springClientIndicator gates URL harvesting so RepoKarta only reads hosts
	// out of files that actually build an outbound HTTP client.
	springClientIndicator = regexp.MustCompile(
		`@FeignClient|@HttpExchange|\bWebClient\b|\bRestClient\b|\bRestTemplate\b|` +
			`HttpServiceProxyFactory|\bbaseUrl\s*\(|\brootUri\s*\(|URI\.create|\bWebTarget\b|\bHttpRequest\b`,
	)
	serviceConfigurationKey = regexp.MustCompile(
		`(?i)(?:base[-_.]?url|url|uri|host|endpoint|address|service)`,
	)
	configuredServiceValue = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*(?::\d+)?$`)
	serviceURLPattern      = regexp.MustCompile(
		`(?i)(?:https?://|lb://)([a-z0-9][a-z0-9_.-]*)(?::\d+)?`,
	)
)

// infrastructureHosts are hostname labels that never identify a sibling service
// in the indexed collection. They keep license headers, XML schema namespaces,
// and package registries from inventing inter-service relationships.
var infrastructureHosts = map[string]bool{
	"0-0-0-0":         true,
	"127-0-0-1":       true,
	"apache":          true,
	"amazonaws":       true,
	"azure":           true,
	"bitbucket":       true,
	"cloudflare":      true,
	"docker":          true,
	"eclipse":         true,
	"example":         true,
	"github":          true,
	"gitlab":          true,
	"google":          true,
	"googleapis":      true,
	"host":            true,
	"java":            true,
	"jcenter":         true,
	"jetbrains":       true,
	"json-schema":     true,
	"localhost":       true,
	"maven":           true,
	"microsoft":       true,
	"mozilla":         true,
	"mvnrepository":   true,
	"npmjs":           true,
	"opensource":      true,
	"oracle":          true,
	"plugins":         true,
	"registry":        true,
	"repo1":           true,
	"schemas":         true,
	"sonatype":        true,
	"springframework": true,
	"sun":             true,
	"w3":              true,
	"www":             true,
	"xmlns":           true,
}

func (b *builder) addGradleManifests(
	repository catalog.Repository,
	revision, repositoryNodeID string,
	contents map[string][]byte,
) {
	paths := make([]string, 0)
	for filePath := range contents {
		switch path.Base(filePath) {
		case "build.gradle", "build.gradle.kts", "settings.gradle", "settings.gradle.kts",
			"libs.versions.toml":
			paths = append(paths, filePath)
		}
	}
	versionCatalog := parseGradleVersionCatalogs(contents)
	versionVariables := parseGradleVersionVariables(contents)
	sort.Strings(paths)
	for _, filePath := range paths {
		content := contents[filePath]
		if match := gradleProjectName.FindSubmatch(content); len(match) == 2 {
			b.registerServiceTarget(string(match[1]), repositoryNodeID)
		}
		dependencies := parseGradleDependencies(content, versionCatalog, versionVariables)
		if path.Base(filePath) == "libs.versions.toml" {
			dependencies = catalogDependencies(content)
		}
		labels := make([]string, 0, len(dependencies))
		for _, dependency := range dependencies {
			labels = append(labels, dependency.coordinate)
		}
		kind := manifestKind(filePath)
		projectName := repository.Name
		if directory := path.Dir(filePath); directory != "." {
			projectName = path.Base(directory)
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
			Name:         projectName,
			Dependencies: labels,
			Evidence:     evidence,
		})
		for _, dependency := range dependencies {
			dependencyEvidence := b.evidence(
				repository,
				revision,
				filePath,
				dependency.line,
				dependency.coordinate,
			)
			label, version := gradleCoordinateParts(dependency.coordinate)
			subtitle := "Gradle"
			if version != "" {
				subtitle += " · " + version
			}
			dependencyNodeID := "dependency:gradle:" + normalizeID(dependency.coordinate)
			b.addNode(Node{
				ID:       dependencyNodeID,
				Kind:     "dependency",
				Label:    label,
				Subtitle: subtitle,
				Layer:    "Dependencies",
				Evidence: []Evidence{dependencyEvidence},
			})
			b.addEdge(Edge{
				ID:       edgeID(manifestID, dependencyNodeID, "depends"),
				Source:   manifestID,
				Target:   dependencyNodeID,
				Kind:     "dependency",
				Label:    "declares",
				Evidence: []Evidence{dependencyEvidence},
			})
		}
	}
}

func parseGradleDependencies(
	content []byte,
	catalog map[string]string,
	versionVariables map[string]string,
) []gradleDependency {
	byCoordinate := make(map[string]gradleDependency)
	for _, match := range gradleStringDependency.FindAllSubmatchIndex(content, -1) {
		coordinate := resolveGradleCoordinate(
			strings.TrimSpace(string(content[match[2]:match[3]])),
			versionVariables,
		)
		if !validGradleCoordinate(coordinate) {
			continue
		}
		byCoordinate[coordinate] = gradleDependency{
			coordinate: coordinate,
			line:       lineAtOffset(content, match[0]),
		}
	}
	for _, match := range gradleNamedDependency.FindAllSubmatchIndex(content, -1) {
		coordinate := parseGradleNamedCoordinate(
			string(content[match[2]:match[3]]),
			versionVariables,
		)
		if !validGradleCoordinate(coordinate) {
			continue
		}
		byCoordinate[coordinate] = gradleDependency{
			coordinate: coordinate,
			line:       lineAtOffset(content, match[0]),
		}
	}
	for _, match := range gradleProjectDependency.FindAllSubmatchIndex(content, -1) {
		projectName := strings.TrimPrefix(strings.TrimSpace(string(content[match[2]:match[3]])), ":")
		coordinate := "project:" + strings.ReplaceAll(projectName, ":", "/")
		byCoordinate[coordinate] = gradleDependency{
			coordinate: coordinate,
			line:       lineAtOffset(content, match[0]),
		}
	}
	for _, match := range gradleCatalogDependency.FindAllSubmatchIndex(content, -1) {
		alias := normalizeCatalogAlias(string(content[match[2]:match[3]]))
		coordinate := catalog[alias]
		if coordinate == "" {
			continue
		}
		byCoordinate[coordinate] = gradleDependency{
			coordinate: coordinate,
			line:       lineAtOffset(content, match[0]),
		}
	}
	output := make([]gradleDependency, 0, len(byCoordinate))
	for _, dependency := range byCoordinate {
		output = append(output, dependency)
	}
	slices.SortFunc(output, func(left, right gradleDependency) int {
		return strings.Compare(left.coordinate, right.coordinate)
	})
	return output
}

// parseGradleVersionVariables resolves the common repository-local sources
// used by Groovy and Kotlin builds: gradle.properties plus literal assignments
// in settings and build scripts. Only literal values are accepted.
func parseGradleVersionVariables(contents map[string][]byte) map[string]string {
	output := make(map[string]string)
	paths := make([]string, 0, len(contents))
	for filePath := range contents {
		base := path.Base(filePath)
		if base == "gradle.properties" ||
			base == "build.gradle" || base == "build.gradle.kts" ||
			base == "settings.gradle" || base == "settings.gradle.kts" {
			paths = append(paths, filePath)
		}
	}
	sort.Strings(paths)
	for _, filePath := range paths {
		content := contents[filePath]
		if path.Base(filePath) == "gradle.properties" {
			scanner := bufio.NewScanner(bytes.NewReader(content))
			scanner.Buffer(make([]byte, 0, 64*1024), maximumSourceFileSize)
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
					continue
				}
				key, value, ok := strings.Cut(line, "=")
				if !ok {
					key, value, ok = strings.Cut(line, ":")
				}
				key, value = strings.TrimSpace(key), strings.TrimSpace(value)
				if ok && key != "" && value != "" && !strings.Contains(value, "$") {
					output[key] = value
				}
			}
		}
		for _, pattern := range []*regexp.Regexp{
			gradleVersionAssignment,
			gradleExtraVersionAssignment,
		} {
			for _, match := range pattern.FindAllSubmatch(content, -1) {
				output[string(match[1])] = string(match[2])
			}
		}
	}
	return output
}

func parseGradleVersionCatalogs(contents map[string][]byte) map[string]string {
	output := make(map[string]string)
	for filePath, content := range contents {
		if path.Base(filePath) != "libs.versions.toml" {
			continue
		}
		for alias, entry := range catalogCoordinates(content) {
			output[alias] = entry.value
		}
	}
	return output
}

type catalogEntry struct {
	value string
	line  int
}

// catalogCoordinates resolves every `[libraries]` alias of one version catalog
// to `group:artifact[:version]` and records the exact declaring line. Versions
// are read in a separate pass so `version.ref` resolves regardless of table
// order.
func catalogCoordinates(content []byte) map[string]catalogEntry {
	versions := make(map[string]string)
	for alias, entry := range catalogSection(content, "versions") {
		versions[alias] = strings.Trim(strings.TrimSpace(stripTOMLComment(entry.value)), `"'`)
	}
	output := make(map[string]catalogEntry)
	for alias, entry := range catalogSection(content, "libraries") {
		if coordinate := parseCatalogCoordinate(entry.value, versions); coordinate != "" {
			output[alias] = catalogEntry{value: coordinate, line: entry.line}
		}
	}
	return output
}

// catalogSection returns the normalized alias, raw value, and one-based line of
// every entry in one TOML table of a Gradle version catalog.
func catalogSection(content []byte, wanted string) map[string]catalogEntry {
	values := make(map[string]catalogEntry)
	section := ""
	number := 0
	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(make([]byte, 0, 64*1024), maximumSourceFileSize)
	for scanner.Scan() {
		number++
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.Trim(line, "[] ")
			continue
		}
		if section != wanted {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		alias := normalizeCatalogAlias(strings.TrimSpace(key))
		if alias == "" {
			continue
		}
		values[alias] = catalogEntry{value: strings.TrimSpace(value), line: number}
	}
	return values
}

// stripTOMLComment removes a trailing comment while preserving `#` characters
// inside quoted values.
func stripTOMLComment(value string) string {
	quote := byte(0)
	for index := 0; index < len(value); index++ {
		switch character := value[index]; {
		case quote != 0 && character == quote:
			quote = 0
		case quote == 0 && (character == '"' || character == '\''):
			quote = character
		case quote == 0 && character == '#':
			return value[:index]
		}
	}
	return value
}

func parseCatalogCoordinate(value string, versions map[string]string) string {
	if !strings.HasPrefix(strings.TrimSpace(value), "{") {
		coordinate := strings.Trim(strings.TrimSpace(stripTOMLComment(value)), `"'`)
		if validGradleCoordinate(coordinate) {
			return coordinate
		}
		return ""
	}
	fields := make(map[string]string)
	for _, match := range catalogInlineField.FindAllStringSubmatch(value, -1) {
		fields[strings.ToLower(match[1])] = strings.TrimSpace(match[2])
	}
	module := fields["module"]
	if module == "" && fields["group"] != "" && fields["name"] != "" {
		module = fields["group"] + ":" + fields["name"]
	}
	if module == "" {
		return ""
	}
	version := fields["version"]
	// Both `version.ref = "x"` and the nested `version = { ref = "x" }` form
	// point at the `[versions]` table.
	if reference := firstNonEmpty(fields["version.ref"], fields["ref"]); reference != "" {
		version = versions[normalizeCatalogAlias(reference)]
	}
	if version != "" && !strings.Contains(version, "$") {
		return module + ":" + version
	}
	return module
}

// catalogDependencies lists every library declared by one version catalog with
// the exact line that declares it.
func catalogDependencies(content []byte) []gradleDependency {
	entries := catalogCoordinates(content)
	output := make([]gradleDependency, 0, len(entries))
	for _, entry := range entries {
		output = append(output, gradleDependency{
			coordinate: entry.value,
			line:       entry.line,
		})
	}
	slices.SortFunc(output, func(left, right gradleDependency) int {
		return strings.Compare(left.coordinate, right.coordinate)
	})
	return output
}

func normalizeCatalogAlias(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	value = strings.TrimSuffix(value, ".get")
	value = strings.NewReplacer("-", ".", "_", ".").Replace(value)
	for strings.Contains(value, "..") {
		value = strings.ReplaceAll(value, "..", ".")
	}
	return strings.Trim(value, ".")
}

// parseGradleNamedCoordinate reads Groovy map arguments and Kotlin named
// arguments in any order and returns `group:artifact[:version]`. Interpolated
// versions are dropped rather than recorded as literal build-script text.
func parseGradleNamedCoordinate(arguments string, versionVariables map[string]string) string {
	fields := make(map[string]string)
	for _, match := range gradleNamedField.FindAllStringSubmatch(arguments, -1) {
		fields[strings.ToLower(match[1])] = strings.TrimSpace(match[2])
	}
	module := fields["module"]
	if module == "" && fields["group"] != "" && fields["name"] != "" {
		module = fields["group"] + ":" + fields["name"]
	}
	if module == "" {
		return ""
	}
	if version := fields["version"]; version != "" {
		if resolved, ok := resolveGradleInterpolation(version, versionVariables); ok {
			return module + ":" + resolved
		}
	}
	return module
}

// resolveGradleCoordinate keeps group:artifact when an interpolated version
// cannot be proven, and preserves the version when its repository-local
// variable resolves to a literal value.
func resolveGradleCoordinate(coordinate string, versionVariables map[string]string) string {
	parts := strings.SplitN(coordinate, ":", 3)
	if len(parts) == 3 {
		if resolved, ok := resolveGradleInterpolation(parts[2], versionVariables); ok {
			return parts[0] + ":" + parts[1] + ":" + resolved
		}
		return parts[0] + ":" + parts[1]
	}
	return coordinate
}

func resolveGradleInterpolation(value string, versionVariables map[string]string) (string, bool) {
	if !strings.Contains(value, "$") {
		return value, value != ""
	}
	missing := false
	resolved := gradleInterpolation.ReplaceAllStringFunc(value, func(match string) string {
		submatch := gradleInterpolation.FindStringSubmatch(match)
		key := firstNonEmpty(submatch[1], submatch[2])
		replacement := versionVariables[key]
		if replacement == "" {
			missing = true
			return match
		}
		return replacement
	})
	return resolved, !missing && resolved != "" && !strings.Contains(resolved, "$")
}

func validGradleCoordinate(value string) bool {
	if strings.HasPrefix(value, "project:") {
		return true
	}
	if strings.Count(value, ":") < 1 ||
		strings.HasPrefix(value, "libs.") ||
		strings.Contains(value, " ") ||
		strings.Contains(value, "/") {
		return false
	}
	// An unresolved Groovy or Kotlin interpolation is not evidence of a
	// specific artifact, so only fully literal group and artifact segments
	// become dependency nodes.
	parts := strings.SplitN(value, ":", 3)
	return !strings.Contains(parts[0], "$") && !strings.Contains(parts[1], "$") &&
		parts[0] != "" && parts[1] != ""
}

func gradleCoordinateParts(coordinate string) (string, string) {
	if strings.HasPrefix(coordinate, "project:") {
		return coordinate, ""
	}
	parts := strings.Split(coordinate, ":")
	if len(parts) < 2 {
		return coordinate, ""
	}
	label := parts[0] + ":" + parts[1]
	if len(parts) > 2 {
		return label, strings.Join(parts[2:], ":")
	}
	return label, ""
}

func (b *builder) addJavaStructure(
	repository catalog.Repository,
	revision, repositoryNodeID string,
	contents map[string][]byte,
) {
	paths := make([]string, 0)
	for filePath := range contents {
		extension := strings.ToLower(path.Ext(filePath))
		if extension == ".java" || extension == ".kt" {
			paths = append(paths, filePath)
		}
	}
	slices.SortFunc(paths, func(left, right string) int {
		leftConfidence, rightConfidence := sourceConfidence(left), sourceConfidence(right)
		if leftConfidence != rightConfidence {
			if leftConfidence == "high" {
				return -1
			}
			return 1
		}
		return strings.Compare(left, right)
	})
	components, routeCount := 0, 0
	for _, filePath := range paths {
		// Documentation and license headers must not create routes or service
		// relationships, so annotations and URLs are read from code only.
		content := stripJavaComments(contents[filePath])
		routes := springRoutes(content)
		clientTargets := springClientTargets(content)
		if len(routes) == 0 && len(clientTargets) == 0 {
			continue
		}
		// Client targets are recorded before the component budget because
		// service edges land on the repository node, not on a component.
		for _, target := range clientTargets {
			b.clientReferences = append(b.clientReferences, clientReference{
				sourceRepositoryID: repository.ID,
				target:             target.name,
				confidence:         sourceConfidence(filePath),
				evidence: b.evidence(
					repository,
					revision,
					filePath,
					target.line,
					target.name,
				),
			})
		}
		// Bound the curated layers so a large Spring codebase stays a readable
		// map rather than a file-level hairball.
		if components >= maximumComponentsPerRepository {
			b.truncated = true
			continue
		}
		if routeCount+len(routes) > maximumRoutesPerRepository {
			b.truncated = true
			routes = routes[:max(0, maximumRoutesPerRepository-routeCount)]
		}
		if len(routes) == 0 && len(clientTargets) == 0 {
			continue
		}
		routeCount += len(routes)
		components++
		className := strings.TrimSuffix(path.Base(filePath), path.Ext(filePath))
		if match := javaClassPattern.FindSubmatch(content); len(match) == 2 {
			className = string(match[1])
		}
		componentEvidence := b.evidence(
			repository,
			revision,
			filePath,
			lineContaining(content, className),
			className,
		)
		componentID := fmt.Sprintf("component:%d:%s", repository.ID, normalizeID(filePath))
		b.addNode(Node{
			ID:           componentID,
			Kind:         "component",
			Label:        className,
			Subtitle:     filePath,
			Layer:        "Components",
			RepositoryID: repository.ID,
			Repository:   repository.Name,
			Path:         filePath,
			Evidence:     []Evidence{componentEvidence},
		})
		b.addEdge(Edge{
			ID:       edgeID(repositoryNodeID, componentID, "contains"),
			Source:   repositoryNodeID,
			Target:   componentID,
			Kind:     "contains",
			Label:    "contains",
			Evidence: []Evidence{componentEvidence},
		})
		for _, route := range routes {
			evidence := b.evidence(repository, revision, filePath, route.line, route.label)
			routeID := fmt.Sprintf(
				"route:%d:%s:%s",
				repository.ID,
				normalizeID(filePath),
				normalizeID(route.label),
			)
			b.addNode(Node{
				ID:           routeID,
				Kind:         "route",
				Label:        route.label,
				Subtitle:     className,
				Layer:        "Routes",
				RepositoryID: repository.ID,
				Repository:   repository.Name,
				Path:         filePath,
				Evidence:     []Evidence{evidence},
			})
			b.addEdge(Edge{
				ID:       edgeID(componentID, routeID, "serves"),
				Source:   componentID,
				Target:   routeID,
				Kind:     "route",
				Label:    "serves",
				Evidence: []Evidence{evidence},
			})
		}
	}
}

type springRoute struct {
	label string
	line  int
}

func springRoutes(content []byte) []springRoute {
	// Idempotent: callers may pass raw or already-stripped source. Annotations
	// named in documentation or commented out are not served routes.
	content = stripJavaComments(content)
	if declarativeClientType.Match(content) {
		return nil
	}
	classOffset := len(content)
	if match := javaClassPattern.FindIndex(content); match != nil {
		classOffset = match[0]
	}
	classPrefix := ""
	mappings := springMappingPattern.FindAllSubmatchIndex(content, -1)
	for _, match := range mappings {
		if match[0] >= classOffset || string(content[match[2]:match[3]]) != "RequestMapping" {
			continue
		}
		paths := annotationPaths(mappingArguments(content, match))
		if len(paths) > 0 {
			classPrefix = paths[0]
			break
		}
	}
	byLabel := make(map[string]springRoute)
	for _, match := range mappings {
		if match[0] < classOffset {
			continue
		}
		annotation := string(content[match[2]:match[3]])
		arguments := mappingArguments(content, match)
		paths := annotationPaths(arguments)
		if len(paths) == 0 {
			paths = []string{""}
		}
		method := strings.ToUpper(strings.TrimSuffix(annotation, "Mapping"))
		if annotation == "RequestMapping" {
			method = requestMappingMethod(arguments)
		}
		for _, routePath := range paths {
			label := strings.TrimSpace(method + " " + joinRoutePath(classPrefix, routePath))
			byLabel[label] = springRoute{label: label, line: lineAtOffset(content, match[0])}
		}
	}
	for _, match := range springFunctionalRoute.FindAllSubmatchIndex(content, -1) {
		method := strings.ToUpper(string(content[match[2]:match[3]]))
		routePath := string(content[match[4]:match[5]])
		label := method + " " + joinRoutePath("", routePath)
		byLabel[label] = springRoute{label: label, line: lineAtOffset(content, match[0])}
	}
	output := make([]springRoute, 0, len(byLabel))
	for _, route := range byLabel {
		output = append(output, route)
	}
	slices.SortFunc(output, func(left, right springRoute) int {
		return strings.Compare(left.label, right.label)
	})
	return output
}

func mappingArguments(content []byte, match []int) string {
	if len(match) < 6 || match[4] < 0 {
		return ""
	}
	return string(content[match[4]:match[5]])
}

func annotationPaths(arguments string) []string {
	matches := quotedJavaString.FindAllStringSubmatch(arguments, -1)
	paths := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) != 2 {
			continue
		}
		value := strings.TrimSpace(match[1])
		if strings.HasPrefix(value, "/") || value == "" {
			paths = append(paths, value)
		}
	}
	return uniqueSorted(paths)
}

func requestMappingMethod(arguments string) string {
	for _, method := range []string{"GET", "POST", "PUT", "DELETE", "PATCH"} {
		if strings.Contains(arguments, "RequestMethod."+method) {
			return method
		}
	}
	return "ANY"
}

func joinRoutePath(prefix, suffix string) string {
	joined := "/" + strings.Trim(strings.TrimSpace(prefix), "/")
	if trimmed := strings.Trim(strings.TrimSpace(suffix), "/"); trimmed != "" {
		joined += "/" + trimmed
	}
	joined = strings.ReplaceAll(joined, "//", "/")
	if joined == "" {
		return "/"
	}
	return joined
}

type springClientTarget struct {
	name string
	line int
}

// springClientTargets reads outbound service names from Spring HTTP clients.
// Bare URLs are only harvested from files that actually construct a client
// (Feign, WebClient, RestClient, RestTemplate, or an HTTP interface proxy), so
// license headers and XML namespaces never become service relationships.
func springClientTargets(content []byte) []springClientTarget {
	// Idempotent, and it keeps license URLs in headers out of service calls
	// while preserving URLs written in string literals.
	content = stripJavaComments(content)
	byName := make(map[string]springClientTarget)
	for _, match := range feignClientPattern.FindAllSubmatchIndex(content, -1) {
		arguments := string(content[match[2]:match[3]])
		target := ""
		if named := feignNamedTarget.FindStringSubmatch(arguments); len(named) == 2 {
			target = named[1]
		} else if quoted := quotedJavaString.FindStringSubmatch(arguments); len(quoted) == 2 {
			target = quoted[1]
		}
		if target = normalizeServiceName(target); target != "" {
			byName[target] = springClientTarget{name: target, line: lineAtOffset(content, match[0])}
		}
	}
	if springClientIndicator.Match(content) {
		for _, match := range serviceURLPattern.FindAllSubmatchIndex(content, -1) {
			target := normalizeServiceName(string(content[match[2]:match[3]]))
			if target == "" || infrastructureHosts[target] {
				continue
			}
			byName[target] = springClientTarget{
				name: target,
				line: lineAtOffset(content, match[0]),
			}
		}
	}
	output := make([]springClientTarget, 0, len(byName))
	for _, target := range byName {
		output = append(output, target)
	}
	slices.SortFunc(output, func(left, right springClientTarget) int {
		return strings.Compare(left.name, right.name)
	})
	return output
}

// normalizeServiceName reduces a Feign service id, Spring application name, or
// URL host to its first DNS label so `inventory-service`,
// `inventory-service:8080`, and `inventory-service.default.svc.cluster.local`
// resolve to the same service.
func normalizeServiceName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimPrefix(value, "http://")
	value = strings.TrimPrefix(value, "https://")
	value = strings.TrimPrefix(value, "lb://")
	value = strings.SplitN(value, "/", 2)[0]
	value = strings.SplitN(value, ":", 2)[0]
	value = strings.SplitN(value, ".", 2)[0]
	value = strings.NewReplacer("_", "-", " ", "-").Replace(value)
	return strings.Trim(value, "-")
}

// registerServiceTarget records one name that identifies this repository as a
// callable service.
func (b *builder) registerServiceTarget(name, repositoryNodeID string) {
	normalized := normalizeServiceName(name)
	if normalized == "" || infrastructureHosts[normalized] {
		return
	}
	b.serviceTargets[normalized] = repositoryNodeID
}

// addServiceIdentity registers the Spring application name so a Feign client
// naming a logical service resolves even when it differs from the directory
// name.
func (b *builder) addServiceIdentity(repositoryNodeID string, contents map[string][]byte) {
	paths := make([]string, 0)
	for filePath := range contents {
		if isServiceConfiguration(filePath) {
			paths = append(paths, filePath)
		}
	}
	sort.Strings(paths)
	for _, filePath := range paths {
		if name := springApplicationName(filePath, contents[filePath]); name != "" {
			b.registerServiceTarget(name, repositoryNodeID)
		}
	}
}

// addServiceConfigurationReferences reads outbound base URLs from Spring
// application configuration. RestTemplate and WebClient URLs are commonly
// injected from properties instead of appearing as literals in client code.
func (b *builder) addServiceConfigurationReferences(
	repository catalog.Repository,
	revision string,
	contents map[string][]byte,
) {
	paths := make([]string, 0)
	for filePath := range contents {
		if isServiceConfiguration(filePath) {
			paths = append(paths, filePath)
		}
	}
	slices.SortFunc(paths, func(left, right string) int {
		leftConfidence, rightConfidence := sourceConfidence(left), sourceConfidence(right)
		if leftConfidence != rightConfidence {
			if leftConfidence == "high" {
				return -1
			}
			return 1
		}
		return strings.Compare(left, right)
	})
	for _, filePath := range paths {
		for _, target := range serviceConfigurationTargets(contents[filePath]) {
			b.clientReferences = append(b.clientReferences, clientReference{
				sourceRepositoryID: repository.ID,
				target:             target.name,
				confidence:         sourceConfidence(filePath),
				evidence: b.evidence(
					repository,
					revision,
					filePath,
					target.line,
					target.name,
				),
			})
		}
	}
}

func serviceConfigurationTargets(content []byte) []springClientTarget {
	byName := make(map[string]springClientTarget)
	type configurationLevel struct {
		key    string
		indent int
	}
	stack := make([]configurationLevel, 0, 8)
	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(make([]byte, 0, 64*1024), maximumSourceFileSize)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		keyText := ""
		configuredValue := ""
		if match := yamlKeyValue.FindStringSubmatch(line); match != nil {
			indent := len(match[1])
			for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
				stack = stack[:len(stack)-1]
			}
			stack = append(stack, configurationLevel{
				key:    strings.ToLower(match[2]),
				indent: indent,
			})
			keys := make([]string, 0, len(stack))
			for _, level := range stack {
				keys = append(keys, level.key)
			}
			keyText = strings.Join(keys, ".")
			configuredValue = match[3]
		} else if key, value, ok := strings.Cut(line, "="); ok {
			keyText = strings.ToLower(strings.TrimSpace(key))
			configuredValue = value
		}
		lowerLine := strings.ToLower(line)
		urlOffset := strings.Index(lowerLine, "http")
		if lbOffset := strings.Index(lowerLine, "lb://"); urlOffset < 0 ||
			(lbOffset >= 0 && lbOffset < urlOffset) {
			urlOffset = lbOffset
		}
		if keyText == "" && urlOffset >= 0 {
			keyText = strings.ToLower(line[:urlOffset])
		}
		if !serviceConfigurationKey.MatchString(keyText) {
			continue
		}
		if strings.Contains(keyText, "docs") ||
			strings.Contains(keyText, "swagger") ||
			strings.Contains(keyText, "openapi") {
			continue
		}
		foundURL := false
		for _, match := range serviceURLPattern.FindAllStringSubmatch(line, -1) {
			target := normalizeServiceName(match[1])
			if target == "" || infrastructureHosts[target] {
				continue
			}
			foundURL = true
			byName[target] = springClientTarget{name: target, line: lineNumber}
		}
		if !foundURL {
			target := configuredServiceName(configuredValue)
			if target != "" && !infrastructureHosts[target] {
				byName[target] = springClientTarget{name: target, line: lineNumber}
			}
		}
	}
	output := make([]springClientTarget, 0, len(byName))
	for _, target := range byName {
		output = append(output, target)
	}
	slices.SortFunc(output, func(left, right springClientTarget) int {
		return strings.Compare(left.name, right.name)
	})
	return output
}

func configuredServiceName(value string) string {
	value = strings.TrimSpace(strings.SplitN(value, "#", 2)[0])
	value = strings.Trim(value, `"'`)
	if strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}") {
		placeholder := strings.TrimSuffix(strings.TrimPrefix(value, "${"), "}")
		_, fallback, ok := strings.Cut(placeholder, ":")
		if !ok {
			return ""
		}
		value = fallback
	}
	if value == "" || strings.Contains(value, "$") ||
		strings.Contains(value, " ") || strings.Contains(value, "://") {
		return ""
	}
	candidate := strings.SplitN(value, "/", 2)[0]
	if !configuredServiceValue.MatchString(candidate) {
		return ""
	}
	return normalizeServiceName(candidate)
}

// isServiceConfiguration reports whether a file can declare a Spring
// application name or outbound service URL.
func isServiceConfiguration(filePath string) bool {
	base := strings.ToLower(path.Base(filePath))
	switch base {
	case "application.yml", "application.yaml", "application.properties",
		"bootstrap.yml", "bootstrap.yaml", "bootstrap.properties":
		return true
	default:
		for _, prefix := range []string{"application-", "bootstrap-"} {
			if strings.HasPrefix(base, prefix) &&
				(strings.HasSuffix(base, ".yml") ||
					strings.HasSuffix(base, ".yaml") ||
					strings.HasSuffix(base, ".properties")) {
				return true
			}
		}
		return false
	}
}

func sourceConfidence(filePath string) string {
	normalized := "/" + strings.ToLower(strings.ReplaceAll(filePath, "\\", "/")) + "/"
	for _, marker := range []string{
		"/src/test/", "/src/integrationtest/", "/src/integration-test/",
		"/src/inttest/", "/src/e2etest/", "/src/testfixtures/",
	} {
		if strings.Contains(normalized, marker) {
			return "low"
		}
	}
	base := strings.ToLower(path.Base(filePath))
	for _, suffix := range []string{
		"test.java", "tests.java", "test.kt", "tests.kt",
		"it.java", "it.kt", "spec.java", "spec.kt",
	} {
		if strings.HasSuffix(base, suffix) {
			return "low"
		}
	}
	return "high"
}

// springApplicationName reads `spring.application.name` from either the flat
// property form or the nested YAML form.
func springApplicationName(filePath string, content []byte) string {
	if match := springApplicationNameProperty.FindSubmatch(content); len(match) == 2 {
		return string(match[1])
	}
	if strings.HasSuffix(filePath, ".properties") {
		return ""
	}
	type level struct {
		key    string
		indent int
	}
	stack := make([]level, 0, 8)
	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(make([]byte, 0, 64*1024), maximumSourceFileSize)
	for scanner.Scan() {
		line := strings.ReplaceAll(scanner.Text(), "\t", "  ")
		if trimmed := strings.TrimSpace(line); trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		match := yamlKeyValue.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		indent := len(match[1])
		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		stack = append(stack, level{key: strings.ToLower(match[2]), indent: indent})
		value := strings.Trim(strings.TrimSpace(match[3]), `"'`)
		if value == "" || len(stack) != 3 {
			continue
		}
		if stack[0].key == "spring" && stack[1].key == "application" && stack[2].key == "name" {
			return value
		}
	}
	return ""
}

// resolveClientReferences turns observed client targets into repository edges,
// but only when the target resolves to a discovered repository or Spring
// application name. Unresolved hosts and interpolated placeholders are dropped
// rather than invented as relationships.
func (b *builder) resolveClientReferences() {
	type resolvedReference struct {
		confidence string
		evidence   []Evidence
	}
	resolved := make(map[string]resolvedReference)
	for _, reference := range b.clientReferences {
		targetID := b.serviceTargets[normalizeServiceName(reference.target)]
		sourceID := fmt.Sprintf("repository:%d", reference.sourceRepositoryID)
		if targetID == "" || targetID == sourceID {
			continue
		}
		key := sourceID + "\x00" + targetID
		current := resolved[key]
		if current.confidence == "high" && reference.confidence != "high" {
			continue
		}
		if reference.confidence == "high" && current.confidence != "high" {
			current = resolvedReference{confidence: "high"}
		}
		if current.confidence == "" {
			current.confidence = reference.confidence
		}
		current.evidence = appendUniqueEvidence(current.evidence, reference.evidence)
		resolved[key] = current
	}
	for key, reference := range resolved {
		sourceID, targetID, _ := strings.Cut(key, "\x00")
		label := "calls over HTTP"
		if reference.confidence == "low" {
			label += " (test-only)"
		}
		b.addEdge(Edge{
			ID:         edgeID(sourceID, targetID, "service-call"),
			Source:     sourceID,
			Target:     targetID,
			Kind:       "service_call",
			Label:      label,
			Confidence: reference.confidence,
			Evidence:   reference.evidence,
		})
	}
}

func (b *builder) addOtherManifests(
	repository catalog.Repository,
	revision, repositoryNodeID string,
	contents map[string][]byte,
) {
	paths := make([]string, 0, len(contents))
	for filePath := range contents {
		if isManifest(filePath) &&
			path.Base(filePath) != "go.mod" &&
			path.Base(filePath) != "package.json" &&
			path.Base(filePath) != "build.gradle" &&
			path.Base(filePath) != "build.gradle.kts" &&
			path.Base(filePath) != "settings.gradle" &&
			path.Base(filePath) != "settings.gradle.kts" &&
			path.Base(filePath) != "libs.versions.toml" {
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
		if existing.Confidence != "high" && edge.Confidence == "high" {
			existing.Confidence = "high"
			existing.Label = edge.Label
			existing.Evidence = nil
		}
		if existing.Confidence == "high" && edge.Confidence == "low" {
			b.edges[edge.ID] = existing
			return
		}
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
		"requirements.txt", "pom.xml", "build.gradle", "build.gradle.kts",
		"settings.gradle", "settings.gradle.kts", "gradle.properties", "libs.versions.toml", "composer.json",
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
	case "settings.gradle", "settings.gradle.kts":
		return "Gradle settings"
	case "gradle.properties":
		return "Gradle properties"
	case "libs.versions.toml":
		return "Gradle version catalog"
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
	case ".go", ".java", ".kt", ".kts", ".gradle", ".groovy", ".gvy",
		".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs",
		".py", ".sql", ".sh", ".bash":
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

// stripJavaComments blanks line and block comments in Java and Kotlin source
// while preserving every byte offset and line break, so annotations mentioned
// in documentation never become routes or service calls and extracted evidence
// keeps exact line numbers. String and character literals are preserved, so a
// URL such as "http://inventory-service" is still visible to client detection.
func stripJavaComments(content []byte) []byte {
	const (
		code = iota
		lineComment
		blockComment
		stringLiteral
		charLiteral
		rawStringLiteral
	)
	output := make([]byte, len(content))
	copy(output, content)
	state := code
	for index := 0; index < len(content); index++ {
		character := content[index]
		switch state {
		case code:
			switch {
			case character == '/' && index+1 < len(content) && content[index+1] == '/':
				state = lineComment
				output[index], output[index+1] = ' ', ' '
				index++
			case character == '/' && index+1 < len(content) && content[index+1] == '*':
				state = blockComment
				output[index], output[index+1] = ' ', ' '
				index++
			case character == '"' && bytes.HasPrefix(content[index:], []byte(`"""`)):
				state = rawStringLiteral
				index += 2
			case character == '"':
				state = stringLiteral
			case character == '\'':
				state = charLiteral
			}
		case lineComment:
			if character == '\n' {
				state = code
				continue
			}
			output[index] = ' '
		case blockComment:
			if character == '*' && index+1 < len(content) && content[index+1] == '/' {
				output[index], output[index+1] = ' ', ' '
				index++
				state = code
				continue
			}
			if character != '\n' && character != '\r' {
				output[index] = ' '
			}
		case stringLiteral, charLiteral:
			if character == '\\' && index+1 < len(content) {
				index++
				continue
			}
			if (state == stringLiteral && character == '"') ||
				(state == charLiteral && character == '\'') ||
				character == '\n' {
				state = code
			}
		case rawStringLiteral:
			if character == '"' && bytes.HasPrefix(content[index:], []byte(`"""`)) {
				index += 2
				state = code
			}
		}
	}
	return output
}
