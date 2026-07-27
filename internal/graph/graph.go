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
	"encoding/xml"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
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
	"go.yaml.in/yaml/v4"
)

const (
	snapshotVersion       = 13
	maximumFiles          = 20_000
	maximumSourceFiles    = 5_000
	maximumSourceFileSize = 1 << 20
	// Structural budgets keep syntax-backed inventories useful without turning
	// a repository map into an unbounded dump of every call site.
	maximumStructuralDocuments       = 3_000
	maximumStructuralSymbols         = 12_000
	maximumStructuralTypedRelations  = 96_000
	maximumStructuralImportRelations = 24_000
	maximumStructuralCallRelations   = 24_000
	maximumStructuralBuildFacts      = 4_000
	maximumStructuralReadConcurrency = 8
	// Curated layer budgets keep large Java and Kotlin services legible.
	maximumComponentsPerRepository = 300
	maximumRoutesPerRepository     = 900
	// maximumCollectionRepositories bounds the cross-repository view. A
	// collection of several hundred repositories would otherwise be analyzed in
	// full before anything could render.
	maximumCollectionRepositories = 40
	commandTimeout                = 20 * time.Second
)

// ArtifactVersion is the persisted repository-map snapshot format.
const ArtifactVersion = snapshotVersion

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
	RepositoryID int64                   `json:"repository_id"`
	Repository   string                  `json:"repository"`
	Kind         string                  `json:"kind"`
	Path         string                  `json:"path"`
	Name         string                  `json:"name"`
	Dependencies []string                `json:"dependencies,omitempty"`
	Declarations []DependencyDeclaration `json:"declarations,omitempty"`
	Evidence     Evidence                `json:"evidence"`
}

// DependencyDeclaration preserves the version text and exact committed source
// evidence that the older flattened dependency list intentionally omitted.
// Resolution reports whether the declared value is exact, a constraint, or
// unresolved; it never implies that a registry or lockfile was consulted.
type DependencyDeclaration struct {
	Ecosystem        string   `json:"ecosystem"`
	Package          string   `json:"package"`
	Declared         string   `json:"declared,omitempty"`
	Resolution       string   `json:"resolution"`
	Resolved         string   `json:"resolved,omitempty"`
	ResolutionSource string   `json:"resolution_source,omitempty"`
	Usage            string   `json:"usage,omitempty"`
	Relationship     string   `json:"relationship,omitempty"`
	DeclaredScope    string   `json:"declared_scope,omitempty"`
	Evidence         Evidence `json:"evidence"`
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

// SystemComponent is a deployable service or infrastructure resource in the
// distributed-system topology. Unlike Node, it deliberately stays above the
// source-file/package level and can be reconciled across repositories by its
// stable aliases.
type SystemComponent struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Kind         string     `json:"kind"`
	Technology   string     `json:"technology,omitempty"`
	RepositoryID int64      `json:"repository_id,omitempty"`
	Repository   string     `json:"repository,omitempty"`
	Path         string     `json:"path,omitempty"`
	Aliases      []string   `json:"aliases,omitempty"`
	Capabilities []string   `json:"capabilities,omitempty"`
	External     bool       `json:"external"`
	Evidence     []Evidence `json:"evidence,omitempty"`
}

// SystemConnection is a directed interaction between deployable components.
// Protocol and interaction are separate because, for example, Kafka publish
// and consume edges share a protocol but flow in opposite directions.
type SystemConnection struct {
	ID             string     `json:"id"`
	Source         string     `json:"source"`
	Target         string     `json:"target"`
	Protocol       string     `json:"protocol"`
	Interaction    string     `json:"interaction"`
	Transport      string     `json:"transport,omitempty"`
	Confidence     string     `json:"confidence"`
	EvidenceOrigin string     `json:"evidence_origin"`
	TargetResolved bool       `json:"target_resolved"`
	Evidence       []Evidence `json:"evidence,omitempty"`
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

// ArtifactProgress reports fleet readiness for commit-keyed structural
// artifacts without loading their potentially large JSON payloads.
type ArtifactProgress struct {
	State                 string `json:"state"`
	RequestedRepositories int    `json:"requested_repositories"`
	ReadyRepositories     int    `json:"ready_repositories"`
	PendingRepositories   int    `json:"pending_repositories"`
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
	Components         []SystemComponent    `json:"components,omitempty"`
	Connections        []SystemConnection   `json:"connections,omitempty"`
	Structure          []StructuralDocument `json:"structure,omitempty"`
	StructureTruncated bool                 `json:"structure_truncated"`
	FileCount          int                  `json:"file_count"`
	Truncated          bool                 `json:"truncated"`
	Scope              Scope                `json:"scope"`
}

// StructuralIndex is the compact, persisted syntax inventory consumed by
// reference search and structured symbol contexts. Reading it never analyzes
// source or builds a repository map, so an interactive request cannot
// accidentally become an indexing job.
type StructuralIndex struct {
	Version            int                  `json:"version"`
	ID                 string               `json:"id"`
	GeneratedAt        time.Time            `json:"generated_at"`
	Structure          []StructuralDocument `json:"structure"`
	StructureTruncated bool                 `json:"structure_truncated"`
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
	store         RepositoryStore
	directory     string
	mu            sync.RWMutex
	baseURL       string
	snapshotMu    sync.Mutex
	snapshotLocks map[string]*sync.Mutex
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
		store:         store,
		directory:     directory,
		baseURL:       strings.TrimRight(baseURL, "/"),
		snapshotLocks: make(map[string]*sync.Mutex),
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
		if cached, ok := s.readCachedSnapshot(snapshotPath); ok {
			return cached, nil
		}
	}
	snapshotLock := s.snapshotLock(fileName)
	snapshotLock.Lock()
	defer snapshotLock.Unlock()
	if !refresh {
		if cached, ok := s.readCachedSnapshot(snapshotPath); ok {
			return cached, nil
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
	if repositoryID > 0 {
		if err := s.writeStructuralIndex(structuralIndexFromSnapshot(snapshot)); err != nil {
			return Snapshot{}, err
		}
	}
	return snapshot, nil
}

func (s *Service) readCachedSnapshot(snapshotPath string) (Snapshot, bool) {
	content, err := os.ReadFile(snapshotPath)
	if err != nil {
		return Snapshot{}, false
	}
	var cached Snapshot
	if json.Unmarshal(content, &cached) != nil || cached.Version != snapshotVersion {
		return Snapshot{}, false
	}
	rebaseSnapshotEvidence(&cached, s.currentBaseURL())
	return cached, true
}

func (s *Service) repositorySnapshotPath(repository catalog.Repository) string {
	signature := snapshotSignature([]catalog.Repository{repository})
	return filepath.Join(s.directory, fmt.Sprintf("repository-%d-%s.json", repository.ID, signature))
}

// ReadDependencySnapshot composes dependency facts from already-prepared
// per-repository artifacts. It never performs source analysis in an HTTP
// request, so a cold fleet returns immediately with explicit progress.
func (s *Service) ReadDependencySnapshot(
	ctx context.Context,
	repositoryID int64,
) (Snapshot, ArtifactProgress, error) {
	repositories, err := s.repositories(ctx, repositoryID)
	if err != nil {
		return Snapshot{}, ArtifactProgress{}, err
	}
	type result struct {
		snapshot Snapshot
		ok       bool
	}
	results := make([]result, len(repositories))
	workers := make(chan struct{}, maximumStructuralReadConcurrency)
	var wait sync.WaitGroup
	for index, repository := range repositories {
		wait.Add(1)
		go func() {
			defer wait.Done()
			select {
			case workers <- struct{}{}:
				defer func() { <-workers }()
			case <-ctx.Done():
				return
			}
			snapshot, ok := s.readCachedSnapshot(s.repositorySnapshotPath(repository))
			results[index] = result{snapshot: snapshot, ok: ok}
		}()
	}
	wait.Wait()
	if err := ctx.Err(); err != nil {
		return Snapshot{}, ArtifactProgress{}, err
	}

	output := Snapshot{
		Version:      snapshotVersion,
		ID:           snapshotSignature(repositories),
		GeneratedAt:  time.Now().UTC(),
		Repositories: []Repository{},
		Manifests:    []Manifest{},
		Scope: Scope{
			Kind:                  "repository",
			TotalRepositories:     len(repositories),
			RequestedRepositoryID: repositoryID,
		},
	}
	if repositoryID == 0 {
		output.Scope.Kind = "collection"
	}
	for _, result := range results {
		if !result.ok {
			continue
		}
		output.Repositories = append(output.Repositories, result.snapshot.Repositories...)
		output.Manifests = append(output.Manifests, result.snapshot.Manifests...)
		output.Truncated = output.Truncated || result.snapshot.Truncated
		output.StructureTruncated = output.StructureTruncated || result.snapshot.StructureTruncated
		output.Scope.AnalyzedRepositories++
	}
	output.Scope.OmittedRepositories = output.Scope.TotalRepositories - output.Scope.AnalyzedRepositories
	output.Scope.Complete = output.Scope.OmittedRepositories == 0
	output.Truncated = output.Truncated || !output.Scope.Complete
	progress := artifactProgress(output.Scope.TotalRepositories, output.Scope.AnalyzedRepositories)
	return output, progress, nil
}

// ReadTopologySnapshot composes distributed-system components and connections
// from already-prepared per-repository artifacts. The final fleet pass
// reconciles inferred external peers against aliases from every visible
// repository, which is impossible while a repository is indexed in isolation.
func (s *Service) ReadTopologySnapshot(
	ctx context.Context,
	repositoryID int64,
) (Snapshot, ArtifactProgress, error) {
	repositories, err := s.repositories(ctx, repositoryID)
	if err != nil {
		return Snapshot{}, ArtifactProgress{}, err
	}
	type result struct {
		snapshot Snapshot
		ok       bool
	}
	results := make([]result, len(repositories))
	workers := make(chan struct{}, maximumStructuralReadConcurrency)
	var wait sync.WaitGroup
	for index, repository := range repositories {
		wait.Add(1)
		go func() {
			defer wait.Done()
			select {
			case workers <- struct{}{}:
				defer func() { <-workers }()
			case <-ctx.Done():
				return
			}
			snapshot, ok := s.readCachedSnapshot(s.repositorySnapshotPath(repository))
			results[index] = result{snapshot: snapshot, ok: ok}
		}()
	}
	wait.Wait()
	if err := ctx.Err(); err != nil {
		return Snapshot{}, ArtifactProgress{}, err
	}

	output := Snapshot{
		Version:      snapshotVersion,
		ID:           snapshotSignature(repositories),
		GeneratedAt:  time.Now().UTC(),
		Repositories: []Repository{},
		Components:   []SystemComponent{},
		Connections:  []SystemConnection{},
		Scope: Scope{
			Kind:                  "repository",
			TotalRepositories:     len(repositories),
			RequestedRepositoryID: repositoryID,
		},
	}
	if repositoryID == 0 {
		output.Scope.Kind = "collection"
	}
	merged := newBuilder(s.currentBaseURL())
	for _, result := range results {
		if !result.ok {
			continue
		}
		output.Repositories = append(output.Repositories, result.snapshot.Repositories...)
		for _, component := range result.snapshot.Components {
			merged.addSystemComponent(component)
		}
		for _, connection := range result.snapshot.Connections {
			merged.addSystemConnection(connection)
		}
		output.Truncated = output.Truncated || result.snapshot.Truncated
		output.Scope.AnalyzedRepositories++
	}
	merged.resolveSystemConnections()
	for _, component := range merged.components {
		component.Aliases = uniqueSorted(component.Aliases)
		component.Capabilities = uniqueSorted(component.Capabilities)
		output.Components = append(output.Components, component)
	}
	for _, connection := range merged.connections {
		output.Connections = append(output.Connections, connection)
	}
	slices.SortFunc(output.Components, func(left, right SystemComponent) int {
		if left.External != right.External {
			if left.External {
				return 1
			}
			return -1
		}
		return strings.Compare(strings.ToLower(left.Name), strings.ToLower(right.Name))
	})
	slices.SortFunc(output.Connections, func(left, right SystemConnection) int {
		return strings.Compare(left.ID, right.ID)
	})
	output.Scope.OmittedRepositories = output.Scope.TotalRepositories - output.Scope.AnalyzedRepositories
	output.Scope.Complete = output.Scope.OmittedRepositories == 0
	output.Truncated = output.Truncated || !output.Scope.Complete
	progress := artifactProgress(output.Scope.TotalRepositories, output.Scope.AnalyzedRepositories)
	return output, progress, nil
}

// ReadRouteSnapshot composes served-route nodes from already-prepared
// per-repository artifacts. It never analyzes source in the caller's request.
func (s *Service) ReadRouteSnapshot(
	ctx context.Context,
	repositoryID int64,
) (Snapshot, ArtifactProgress, error) {
	repositories, err := s.repositories(ctx, repositoryID)
	if err != nil {
		return Snapshot{}, ArtifactProgress{}, err
	}
	type result struct {
		snapshot Snapshot
		ok       bool
	}
	results := make([]result, len(repositories))
	workers := make(chan struct{}, maximumStructuralReadConcurrency)
	var wait sync.WaitGroup
	for index, repository := range repositories {
		wait.Add(1)
		go func() {
			defer wait.Done()
			select {
			case workers <- struct{}{}:
				defer func() { <-workers }()
			case <-ctx.Done():
				return
			}
			snapshot, ok := s.readCachedSnapshot(s.repositorySnapshotPath(repository))
			results[index] = result{snapshot: snapshot, ok: ok}
		}()
	}
	wait.Wait()
	if err := ctx.Err(); err != nil {
		return Snapshot{}, ArtifactProgress{}, err
	}

	output := Snapshot{
		Version:      snapshotVersion,
		ID:           snapshotSignature(repositories),
		GeneratedAt:  time.Now().UTC(),
		Repositories: []Repository{},
		Nodes:        []Node{},
		Scope: Scope{
			Kind:                  "repository",
			TotalRepositories:     len(repositories),
			RequestedRepositoryID: repositoryID,
		},
	}
	if repositoryID == 0 {
		output.Scope.Kind = "collection"
	}
	for _, result := range results {
		if !result.ok {
			continue
		}
		output.Repositories = append(output.Repositories, result.snapshot.Repositories...)
		for _, node := range result.snapshot.Nodes {
			if node.Kind == "route" {
				output.Nodes = append(output.Nodes, node)
			}
		}
		output.Truncated = output.Truncated || result.snapshot.Truncated
		output.StructureTruncated = output.StructureTruncated || result.snapshot.StructureTruncated
		output.Scope.AnalyzedRepositories++
	}
	output.Scope.OmittedRepositories = output.Scope.TotalRepositories - output.Scope.AnalyzedRepositories
	output.Scope.Complete = output.Scope.OmittedRepositories == 0
	output.Truncated = output.Truncated || !output.Scope.Complete
	progress := artifactProgress(output.Scope.TotalRepositories, output.Scope.AnalyzedRepositories)
	return output, progress, nil
}

// StructureProgress checks exact commit-keyed artifact paths without reading
// the structural documents themselves.
func (s *Service) StructureProgress(ctx context.Context, repositoryID int64) (ArtifactProgress, error) {
	repositories, err := s.repositories(ctx, repositoryID)
	if err != nil {
		return ArtifactProgress{}, err
	}
	ready := 0
	for _, repository := range repositories {
		signature := snapshotSignature([]catalog.Repository{repository})
		if _, err := os.Stat(s.structuralIndexPath(repository.ID, signature)); err == nil {
			ready++
		}
	}
	return artifactProgress(len(repositories), ready), nil
}

func artifactProgress(requested, ready int) ArtifactProgress {
	progress := ArtifactProgress{
		State:                 "ready",
		RequestedRepositories: requested,
		ReadyRepositories:     ready,
		PendingRepositories:   max(0, requested-ready),
	}
	if progress.PendingRepositories > 0 {
		progress.State = "building"
	}
	return progress
}

// PrepareStructure builds or projects the compact relation and symbol artifact
// in the background after normal code indexing completes.
func (s *Service) PrepareStructure(ctx context.Context, repositoryID int64) error {
	if repositoryID <= 0 {
		return errors.New("repository ID is required for structural indexing")
	}
	repository, err := s.store.RepositoryByID(ctx, repositoryID)
	if err != nil {
		return err
	}
	signature := snapshotSignature([]catalog.Repository{repository})
	if _, ok := s.readStructuralIndex(repositoryID, signature); ok {
		return nil
	}
	snapshot, err := s.Snapshot(ctx, repositoryID, false)
	if err != nil {
		return err
	}
	if _, ok := s.readStructuralIndex(repositoryID, signature); ok {
		return nil
	}
	return s.writeStructuralIndex(structuralIndexFromSnapshot(snapshot))
}

// ReadStructure returns only already-persisted per-repository structural
// artifacts. Missing artifacts are reported through Scope and are never built
// in the caller's request.
func (s *Service) ReadStructure(ctx context.Context, repositoryID int64) (StructuralIndex, error) {
	repositories, err := s.repositories(ctx, repositoryID)
	if err != nil {
		return StructuralIndex{}, err
	}
	output := StructuralIndex{
		Version:     snapshotVersion,
		GeneratedAt: time.Now().UTC(),
		Structure:   []StructuralDocument{},
		Scope: Scope{
			Kind:                  "repository",
			Complete:              true,
			TotalRepositories:     len(repositories),
			RequestedRepositoryID: repositoryID,
		},
	}
	if repositoryID == 0 {
		output.Scope.Kind = "collection"
	}
	type readResult struct {
		index StructuralIndex
		ok    bool
	}
	results := make([]readResult, len(repositories))
	workers := make(chan struct{}, maximumStructuralReadConcurrency)
	var wait sync.WaitGroup
	for repositoryIndex, repository := range repositories {
		wait.Add(1)
		go func() {
			defer wait.Done()
			select {
			case workers <- struct{}{}:
				defer func() { <-workers }()
			case <-ctx.Done():
				return
			}
			signature := snapshotSignature([]catalog.Repository{repository})
			index, ok := s.readStructuralIndex(repository.ID, signature)
			results[repositoryIndex] = readResult{index: index, ok: ok}
		}()
	}
	wait.Wait()
	if err := ctx.Err(); err != nil {
		return StructuralIndex{}, err
	}
	for _, result := range results {
		if !result.ok {
			continue
		}
		output.Structure = append(output.Structure, result.index.Structure...)
		output.StructureTruncated = output.StructureTruncated || result.index.StructureTruncated
		output.Scope.AnalyzedRepositories++
	}
	output.Scope.OmittedRepositories = output.Scope.TotalRepositories - output.Scope.AnalyzedRepositories
	output.Scope.Complete = output.Scope.OmittedRepositories == 0
	output.ID = snapshotSignature(repositories)
	return output, nil
}

func structuralIndexFromSnapshot(snapshot Snapshot) StructuralIndex {
	structure := make([]StructuralDocument, 0, len(snapshot.Structure))
	for _, document := range snapshot.Structure {
		structure = append(structure, StructuralDocument{
			RepositoryID:  document.RepositoryID,
			Repository:    document.Repository,
			Revision:      document.Revision,
			Path:          document.Path,
			Language:      document.Language,
			Parser:        document.Parser,
			ParseComplete: document.ParseComplete,
			Truncated:     document.Truncated,
			Symbols:       append([]analysis.Symbol(nil), document.Symbols...),
			Relations:     append([]analysis.Relation(nil), document.Relations...),
		})
	}
	return StructuralIndex{
		Version:            snapshotVersion,
		ID:                 snapshot.ID,
		GeneratedAt:        snapshot.GeneratedAt,
		Structure:          structure,
		StructureTruncated: snapshot.StructureTruncated,
		Scope:              snapshot.Scope,
	}
}

func (s *Service) readStructuralIndex(repositoryID int64, signature string) (StructuralIndex, bool) {
	content, err := os.ReadFile(s.structuralIndexPath(repositoryID, signature))
	if err != nil {
		return StructuralIndex{}, false
	}
	var index StructuralIndex
	if json.Unmarshal(content, &index) != nil ||
		index.Version != snapshotVersion ||
		index.ID != signature {
		return StructuralIndex{}, false
	}
	return index, true
}

func (s *Service) writeStructuralIndex(index StructuralIndex) error {
	if index.Scope.RequestedRepositoryID <= 0 {
		return errors.New("structural artifact must target one repository")
	}
	content, err := json.Marshal(index)
	if err != nil {
		return fmt.Errorf("encode structural index: %w", err)
	}
	fileName := filepath.Base(s.structuralIndexPath(index.Scope.RequestedRepositoryID, index.ID))
	temporary, err := os.CreateTemp(s.directory, fileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("create structural index: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return fmt.Errorf("write structural index: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close structural index: %w", err)
	}
	if err := os.Rename(temporaryName, filepath.Join(s.directory, fileName)); err != nil {
		return fmt.Errorf("publish structural index: %w", err)
	}
	return nil
}

func (s *Service) structuralIndexPath(repositoryID int64, signature string) string {
	return filepath.Join(
		s.directory,
		fmt.Sprintf("structure-repository-%d-%s.json", repositoryID, signature),
	)
}

func (s *Service) snapshotLock(key string) *sync.Mutex {
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()
	lock := s.snapshotLocks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		s.snapshotLocks[key] = lock
	}
	return lock
}

func (s *Service) repositories(ctx context.Context, repositoryID int64) ([]catalog.Repository, error) {
	if repositoryID > 0 {
		repository, err := s.store.RepositoryByID(ctx, repositoryID)
		if err != nil {
			return nil, err
		}
		if repository.IndexedCommit == "" && repository.HeadCommit == "" {
			return []catalog.Repository{}, nil
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
	baseURL                   string
	repositories              []Repository
	languages                 map[string]int
	manifests                 []Manifest
	nodes                     map[string]Node
	edges                     map[string]Edge
	components                map[string]SystemComponent
	connections               map[string]SystemConnection
	serviceTargets            map[string]string
	clientReferences          []clientReference
	structure                 []StructuralDocument
	structuralSymbols         int
	structuralTypedRelations  int
	structuralImportRelations int
	structuralCallRelations   int
	structuralBuildFacts      int
	structureTruncated        bool
	fileCount                 int
	truncated                 bool
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
		components:     make(map[string]SystemComponent),
		connections:    make(map[string]SystemConnection),
		serviceTargets: make(map[string]string),
	}
}

func (b *builder) snapshot(signature string) Snapshot {
	b.resolveClientReferences()
	b.resolveSystemConnections()
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
	components := make([]SystemComponent, 0, len(b.components))
	for _, component := range b.components {
		component.Aliases = uniqueSorted(component.Aliases)
		component.Capabilities = uniqueSorted(component.Capabilities)
		components = append(components, component)
	}
	slices.SortFunc(components, func(left, right SystemComponent) int {
		if left.External != right.External {
			if left.External {
				return 1
			}
			return -1
		}
		return strings.Compare(strings.ToLower(left.Name), strings.ToLower(right.Name))
	})
	connections := make([]SystemConnection, 0, len(b.connections))
	for _, connection := range b.connections {
		connections = append(connections, connection)
	}
	slices.SortFunc(connections, func(left, right SystemConnection) int {
		return strings.Compare(left.ID, right.ID)
	})
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
		Components:         components,
		Connections:        connections,
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

	candidates := make([]string, 0)
	sourceCount := 0
	for _, filePath := range files {
		if !isManifest(filePath) && !isAnalyzedSource(filePath) &&
			!isServiceConfiguration(filePath) && !isTopologyFile(filePath) {
			continue
		}
		if isAnalyzedSource(filePath) {
			if sourceCount >= maximumSourceFiles {
				b.truncated = true
				continue
			}
			sourceCount++
		}
		candidates = append(candidates, filePath)
	}
	contents, batchErr := readFiles(ctx, repository, revision, candidates)
	if batchErr != nil {
		// Preserve the older per-file recovery path for unusual Git versions or
		// malformed paths; one failed batch must not discard the whole map.
		contents = make(map[string][]byte)
		for _, filePath := range candidates {
			content, readErr := readFile(ctx, repository, revision, filePath)
			if readErr == nil {
				contents[filePath] = content
			}
		}
	}

	goModule := ""
	if content, ok := contents["go.mod"]; ok {
		goModule = b.addGoManifest(repository, revision, repositoryNodeID, "go.mod", content, contents)
	}
	packageIDs := b.addPackages(repository, revision, repositoryNodeID, files, contents)
	b.addStructuralAnalysis(repository, revision, files, contents)
	b.addGoImportsAndRoutes(repository, revision, goModule, packageIDs, contents)
	b.addServiceIdentity(repositoryNodeID, contents)
	b.addServiceConfigurationReferences(repository, revision, contents)
	b.addJavaStructure(repository, revision, repositoryNodeID, contents)
	b.addGradleManifests(repository, revision, repositoryNodeID, contents)
	b.addOtherManifests(repository, revision, repositoryNodeID, contents)
	b.addDistributedTopology(repository, revision, evidencePath, contents)
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
		typedRelationBudget := maximumStructuralTypedRelations - b.structuralTypedRelations
		importRelationBudget := maximumStructuralImportRelations - b.structuralImportRelations
		callRelationBudget := maximumStructuralCallRelations - b.structuralCallRelations
		buildFactBudget := maximumStructuralBuildFacts - b.structuralBuildFacts
		if len(document.Symbols) > symbolBudget ||
			len(document.BuildFacts) > buildFactBudget {
			b.structureTruncated = true
		}
		relations := make([]analysis.Relation, 0, len(document.Relations))
		for _, relation := range document.Relations {
			switch relation.Kind {
			case "call":
				if callRelationBudget <= 0 {
					b.structureTruncated = true
					continue
				}
				callRelationBudget--
				b.structuralCallRelations++
			case "import":
				if importRelationBudget <= 0 {
					b.structureTruncated = true
					continue
				}
				importRelationBudget--
				b.structuralImportRelations++
			default:
				if typedRelationBudget <= 0 {
					b.structureTruncated = true
					continue
				}
				typedRelationBudget--
				b.structuralTypedRelations++
			}
			relations = append(relations, relation)
		}
		b.structureTruncated = b.structureTruncated || document.Truncated
		document.Symbols = document.Symbols[:min(len(document.Symbols), symbolBudget)]
		document.Relations = relations
		document.BuildFacts = document.BuildFacts[:min(len(document.BuildFacts), buildFactBudget)]
		b.structuralSymbols += len(document.Symbols)
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
			b.structuralTypedRelations >= maximumStructuralTypedRelations &&
			b.structuralImportRelations >= maximumStructuralImportRelations &&
			b.structuralCallRelations >= maximumStructuralCallRelations &&
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
	contents map[string][]byte,
) string {
	module, dependencies := parseGoMod(content)
	if module == "" {
		module = repository.Name
	}
	evidence := b.evidence(repository, revision, filePath, lineContaining(content, "module "), module)
	declarations := parseGoModDeclarations(content)
	classifyGoDependencyUsage(declarations, contents)
	for index := range declarations {
		declarations[index].Evidence = b.evidence(
			repository,
			revision,
			filePath,
			lineContaining(content, declarations[index].Package),
			declarations[index].Package,
		)
	}
	b.manifests = append(b.manifests, Manifest{
		RepositoryID: repository.ID,
		Repository:   repository.Name,
		Kind:         "Go module",
		Path:         filePath,
		Name:         module,
		Dependencies: dependencies,
		Declarations: declarations,
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

func classifyGoDependencyUsage(
	declarations []DependencyDeclaration,
	contents map[string][]byte,
) {
	production := make(map[string]bool)
	tests := make(map[string]bool)
	for filePath, content := range contents {
		if path.Ext(filePath) != ".go" {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filePath, content, parser.ImportsOnly)
		if err != nil {
			continue
		}
		for _, importSpec := range file.Imports {
			importPath, err := strconv.Unquote(importSpec.Path.Value)
			if err != nil {
				continue
			}
			for _, declaration := range declarations {
				if importPath != declaration.Package &&
					!strings.HasPrefix(importPath, declaration.Package+"/") {
					continue
				}
				if strings.HasSuffix(filePath, "_test.go") {
					tests[declaration.Package] = true
				} else {
					production[declaration.Package] = true
				}
			}
		}
	}
	for index := range declarations {
		switch {
		case production[declarations[index].Package]:
			declarations[index].Usage = "production"
		case tests[declarations[index].Package]:
			declarations[index].Usage = "test"
		default:
			declarations[index].Usage = "unknown"
		}
	}
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
			Name                 string            `json:"name"`
			Dependencies         map[string]string `json:"dependencies"`
			DevDependencies      map[string]string `json:"devDependencies"`
			OptionalDependencies map[string]string `json:"optionalDependencies"`
			PeerDependencies     map[string]string `json:"peerDependencies"`
		}
		content := contents[manifestPath]
		if json.Unmarshal(content, &manifest) != nil {
			continue
		}
		lockVersions, lockPath := npmLockVersions(contents, manifestPath)
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
		dependencies := sortedKeys(
			manifest.Dependencies,
			manifest.DevDependencies,
			manifest.OptionalDependencies,
			manifest.PeerDependencies,
		)
		declarations := make([]DependencyDeclaration, 0, len(dependencies))
		for _, dependency := range dependencies {
			declared, usage, relationship, declaredScope := npmDependencyMetadata(
				manifest.OptionalDependencies, dependency, "optionalDependencies",
			)
			if declaredScope == "" {
				declared, usage, relationship, declaredScope = npmDependencyMetadata(
					manifest.Dependencies, dependency, "dependencies",
				)
			}
			if declaredScope == "" {
				declared, usage, relationship, declaredScope = npmDependencyMetadata(
					manifest.PeerDependencies, dependency, "peerDependencies",
				)
			}
			if declaredScope == "" {
				declared, usage, relationship, declaredScope = npmDependencyMetadata(
					manifest.DevDependencies, dependency, "devDependencies",
				)
			}
			resolved := lockVersions[dependency]
			resolutionSource := ""
			if resolved != "" {
				resolutionSource = lockPath
			}
			declarations = append(declarations, DependencyDeclaration{
				Ecosystem:        "npm",
				Package:          dependency,
				Declared:         declared,
				Resolution:       versionResolution(declared),
				Resolved:         resolved,
				ResolutionSource: resolutionSource,
				Usage:            usage,
				Relationship:     relationship,
				DeclaredScope:    declaredScope,
				Evidence: b.evidence(
					repository,
					revision,
					manifestPath,
					lineContaining(content, `"`+dependency+`"`),
					dependency,
				),
			})
		}
		b.manifests = append(b.manifests, Manifest{
			RepositoryID: repository.ID,
			Repository:   repository.Name,
			Kind:         "npm package",
			Path:         manifestPath,
			Name:         label,
			Dependencies: dependencies,
			Declarations: declarations,
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

func npmDependencyMetadata(
	dependencies map[string]string,
	name string,
	scope string,
) (declared, usage, relationship, declaredScope string) {
	declared, ok := dependencies[name]
	if !ok {
		return "", "", "", ""
	}
	usage = "production"
	relationship = "required"
	switch scope {
	case "devDependencies":
		usage = "development"
	case "optionalDependencies":
		relationship = "optional"
	case "peerDependencies":
		relationship = "peer"
	}
	return declared, usage, relationship, scope
}

func npmLockVersions(contents map[string][]byte, manifestPath string) (map[string]string, string) {
	lockPath := nearestDependencyFile(contents, path.Dir(manifestPath), "npm-shrinkwrap.json", "package-lock.json")
	if lockPath == "" {
		lockPath = nearestDependencyFile(contents, path.Dir(manifestPath), "pnpm-lock.yaml")
		if lockPath == "" {
			return nil, ""
		}
		return pnpmLockVersions(contents[lockPath], manifestPath, lockPath), lockPath
	}
	var lock struct {
		Packages map[string]struct {
			Version string `json:"version"`
		} `json:"packages"`
		Dependencies map[string]struct {
			Version string `json:"version"`
		} `json:"dependencies"`
	}
	if json.Unmarshal(contents[lockPath], &lock) != nil {
		return nil, ""
	}
	versions := make(map[string]string)
	for packagePath, dependency := range lock.Packages {
		if !strings.HasPrefix(packagePath, "node_modules/") || dependency.Version == "" {
			continue
		}
		name := strings.TrimPrefix(packagePath, "node_modules/")
		if strings.Contains(name, "/node_modules/") {
			continue
		}
		versions[name] = dependency.Version
	}
	for name, dependency := range lock.Dependencies {
		if versions[name] == "" {
			versions[name] = dependency.Version
		}
	}
	return versions, lockPath
}

func pnpmLockVersions(content []byte, manifestPath, lockPath string) map[string]string {
	var document struct {
		Importers map[string]map[string]map[string]any `yaml:"importers"`
	}
	if yaml.Unmarshal(content, &document) != nil {
		return nil
	}
	lockDirectory := path.Dir(lockPath)
	if lockDirectory == "." {
		lockDirectory = ""
	}
	manifestDirectory := path.Dir(manifestPath)
	if manifestDirectory == "." {
		manifestDirectory = ""
	}
	importer := strings.TrimPrefix(strings.TrimPrefix(manifestDirectory, lockDirectory), "/")
	if importer == "" {
		importer = "."
	}
	versions := make(map[string]string)
	for _, section := range []string{"dependencies", "devDependencies", "optionalDependencies"} {
		for name, raw := range document.Importers[importer][section] {
			var version string
			switch value := raw.(type) {
			case string:
				version = value
			case map[string]any:
				version, _ = value["version"].(string)
			}
			version = strings.TrimSpace(strings.SplitN(version, "(", 2)[0])
			if version != "" && !strings.HasPrefix(version, "link:") &&
				!strings.HasPrefix(version, "workspace:") {
				versions[name] = version
			}
		}
	}
	return versions
}

func nearestDependencyFile(contents map[string][]byte, directory string, names ...string) string {
	if directory == "." {
		directory = ""
	}
	for {
		for _, name := range names {
			candidate := path.Join(directory, name)
			if directory == "" {
				candidate = name
			}
			if _, ok := contents[candidate]; ok {
				return candidate
			}
		}
		if directory == "" {
			return ""
		}
		parent := path.Dir(directory)
		if parent == "." || parent == directory {
			directory = ""
		} else {
			directory = parent
		}
	}
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
	coordinate    string
	line          int
	configuration string
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
		lockVersions, lockPath := gradleLockVersions(contents, filePath)
		labels := make([]string, 0, len(dependencies))
		declarations := make([]DependencyDeclaration, 0, len(dependencies))
		for _, dependency := range dependencies {
			labels = append(labels, dependency.coordinate)
			label, version := gradleCoordinateParts(dependency.coordinate)
			resolved := lockVersions[label]
			resolutionSource := ""
			if resolved != "" {
				resolutionSource = lockPath
			}
			declarations = append(declarations, DependencyDeclaration{
				Ecosystem:        "maven",
				Package:          label,
				Declared:         version,
				Resolution:       versionResolution(version),
				Resolved:         resolved,
				ResolutionSource: resolutionSource,
				Usage:            gradleDependencyUsage(dependency.configuration),
				Relationship:     "required",
				DeclaredScope:    dependency.configuration,
				Evidence: b.evidence(
					repository,
					revision,
					filePath,
					dependency.line,
					dependency.coordinate,
				),
			})
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
			Declarations: declarations,
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
		configuration := gradleConfigurationAt(content[match[0]:match[1]])
		byCoordinate[coordinate+"\x00"+configuration] = gradleDependency{
			coordinate:    coordinate,
			line:          lineAtOffset(content, match[0]),
			configuration: configuration,
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
		configuration := gradleConfigurationAt(content[match[0]:match[1]])
		byCoordinate[coordinate+"\x00"+configuration] = gradleDependency{
			coordinate:    coordinate,
			line:          lineAtOffset(content, match[0]),
			configuration: configuration,
		}
	}
	for _, match := range gradleProjectDependency.FindAllSubmatchIndex(content, -1) {
		projectName := strings.TrimPrefix(strings.TrimSpace(string(content[match[2]:match[3]])), ":")
		coordinate := "project:" + strings.ReplaceAll(projectName, ":", "/")
		configuration := gradleConfigurationAt(content[match[0]:match[1]])
		byCoordinate[coordinate+"\x00"+configuration] = gradleDependency{
			coordinate:    coordinate,
			line:          lineAtOffset(content, match[0]),
			configuration: configuration,
		}
	}
	for _, match := range gradleCatalogDependency.FindAllSubmatchIndex(content, -1) {
		alias := normalizeCatalogAlias(string(content[match[2]:match[3]]))
		coordinate := catalog[alias]
		if coordinate == "" {
			continue
		}
		configuration := gradleConfigurationAt(content[match[0]:match[1]])
		byCoordinate[coordinate+"\x00"+configuration] = gradleDependency{
			coordinate:    coordinate,
			line:          lineAtOffset(content, match[0]),
			configuration: configuration,
		}
	}
	output := make([]gradleDependency, 0, len(byCoordinate))
	for _, dependency := range byCoordinate {
		output = append(output, dependency)
	}
	slices.SortFunc(output, func(left, right gradleDependency) int {
		if comparison := strings.Compare(left.coordinate, right.coordinate); comparison != 0 {
			return comparison
		}
		return strings.Compare(left.configuration, right.configuration)
	})
	return output
}

func gradleConfigurationAt(content []byte) string {
	fields := strings.Fields(strings.TrimSpace(string(content)))
	if len(fields) == 0 {
		return ""
	}
	configuration := fields[0]
	if index := strings.IndexAny(configuration, "( \t"); index >= 0 {
		configuration = configuration[:index]
	}
	return strings.TrimSpace(configuration)
}

func gradleDependencyUsage(configuration string) string {
	lower := strings.ToLower(strings.TrimSpace(configuration))
	switch {
	case lower == "", lower == "versioncatalog":
		return "unknown"
	case strings.Contains(lower, "test"), strings.Contains(lower, "e2e"):
		return "test"
	case strings.Contains(lower, "annotationprocessor"), strings.HasPrefix(lower, "kapt"),
		strings.HasPrefix(lower, "ksp"), lower == "classpath":
		return "build"
	case lower == "developmentonly":
		return "development"
	default:
		return "production"
	}
}

func gradleLockVersions(contents map[string][]byte, manifestPath string) (map[string]string, string) {
	lockPath := nearestDependencyFile(contents, path.Dir(manifestPath), "gradle.lockfile")
	if lockPath == "" {
		return nil, ""
	}
	versions := make(map[string]string)
	scanner := bufio.NewScanner(bytes.NewReader(contents[lockPath]))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		coordinate, _, _ := strings.Cut(line, "=")
		parts := strings.Split(strings.TrimSpace(coordinate), ":")
		if len(parts) >= 3 {
			versions[parts[0]+":"+parts[1]] = parts[2]
		}
	}
	return versions, lockPath
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
			coordinate:    entry.value,
			line:          entry.line,
			configuration: "versionCatalog",
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
		declarations := make([]DependencyDeclaration, 0)
		dependencyLabels := make([]string, 0)
		switch kind {
		case "Maven project":
			declarations = parseMavenDeclarations(contents[filePath])
		case "Cargo package":
			lock, lockPath := cargoLockVersions(contents, filePath)
			declarations = parseCargoDeclarations(contents[filePath], lock, lockPath)
		case "Python requirements", "Python project":
			lock, lockPath := pythonLockVersions(contents, filePath)
			declarations = parsePythonDeclarations(filePath, contents[filePath], lock, lockPath)
		case ".NET project":
			lock, lockPath := nugetLockVersions(contents, filePath)
			declarations = parseNuGetDeclarations(filePath, contents[filePath], lock, lockPath)
		}
		for index := range declarations {
			declarations[index].Evidence = b.evidence(
				repository,
				revision,
				filePath,
				dependencyDeclarationLine(contents[filePath], declarations[index]),
				declarations[index].Package,
			)
			dependencyLabels = append(dependencyLabels, declarations[index].Package)
		}
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
			Dependencies: dependencyLabels,
			Declarations: declarations,
			Evidence:     evidence,
		})
		for _, declaration := range declarations {
			dependencyNodeID := "dependency:" + declaration.Ecosystem + ":" + normalizeID(declaration.Package)
			subtitle := declaration.Ecosystem
			if declaration.Declared != "" {
				subtitle += " · " + declaration.Declared
			}
			b.addNode(Node{
				ID:       dependencyNodeID,
				Kind:     "dependency",
				Label:    declaration.Package,
				Subtitle: subtitle,
				Layer:    "Dependencies",
				Evidence: []Evidence{declaration.Evidence},
			})
			b.addEdge(Edge{
				ID:       edgeID(manifestID, dependencyNodeID, "depends"),
				Source:   manifestID,
				Target:   dependencyNodeID,
				Kind:     "dependency",
				Label:    "declares",
				Evidence: []Evidence{declaration.Evidence},
			})
		}
	}
}

type mavenDependencyXML struct {
	GroupID    string `xml:"groupId"`
	ArtifactID string `xml:"artifactId"`
	Version    string `xml:"version"`
	Scope      string `xml:"scope"`
	Optional   bool   `xml:"optional"`
}

func parseMavenDeclarations(content []byte) []DependencyDeclaration {
	var project struct {
		Properties struct {
			InnerXML []byte `xml:",innerxml"`
		} `xml:"properties"`
		Dependencies []mavenDependencyXML `xml:"dependencies>dependency"`
	}
	if xml.Unmarshal(content, &project) != nil {
		return nil
	}
	properties := parseMavenProperties(project.Properties.InnerXML)
	declarations := make([]DependencyDeclaration, 0, len(project.Dependencies))
	for _, dependency := range project.Dependencies {
		groupID := resolveMavenProperty(strings.TrimSpace(dependency.GroupID), properties)
		artifactID := resolveMavenProperty(strings.TrimSpace(dependency.ArtifactID), properties)
		if groupID == "" || artifactID == "" {
			continue
		}
		version := resolveMavenProperty(strings.TrimSpace(dependency.Version), properties)
		scope := strings.ToLower(strings.TrimSpace(dependency.Scope))
		if scope == "" {
			scope = "compile"
		}
		usage := "production"
		if scope == "test" {
			usage = "test"
		} else if scope == "import" {
			usage = "build"
		}
		relationship := "required"
		if dependency.Optional {
			relationship = "optional"
		}
		declarations = append(declarations, DependencyDeclaration{
			Ecosystem:     "maven",
			Package:       groupID + ":" + artifactID,
			Declared:      version,
			Resolution:    versionResolution(version),
			Usage:         usage,
			Relationship:  relationship,
			DeclaredScope: scope,
		})
	}
	slices.SortFunc(declarations, func(left, right DependencyDeclaration) int {
		return strings.Compare(left.Package+"\x00"+left.DeclaredScope, right.Package+"\x00"+right.DeclaredScope)
	})
	return declarations
}

func parseMavenProperties(content []byte) map[string]string {
	properties := make(map[string]string)
	decoder := xml.NewDecoder(bytes.NewReader(content))
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		var value string
		if decoder.DecodeElement(&value, &start) == nil {
			properties[start.Name.Local] = strings.TrimSpace(value)
		}
	}
	return properties
}

func resolveMavenProperty(value string, properties map[string]string) string {
	if strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}") {
		if resolved := properties[strings.TrimSuffix(strings.TrimPrefix(value, "${"), "}")]; resolved != "" {
			return resolved
		}
	}
	return value
}

func dependencyDeclarationLine(content []byte, declaration DependencyDeclaration) int {
	needle := declaration.Package
	if declaration.Ecosystem == "maven" {
		if _, artifact, ok := strings.Cut(declaration.Package, ":"); ok {
			needle = artifact
		}
	}
	return lineContaining(content, needle)
}

func cargoLockVersions(
	contents map[string][]byte,
	manifestPath string,
) (map[string][]string, string) {
	lockPath := nearestDependencyFile(contents, path.Dir(manifestPath), "Cargo.lock")
	if lockPath == "" {
		return nil, ""
	}
	return tomlPackageLockVersions(contents[lockPath]), lockPath
}

func parseCargoDeclarations(
	content []byte,
	lockVersions map[string][]string,
	lockPath string,
) []DependencyDeclaration {
	declarations := make([]DependencyDeclaration, 0)
	section := ""
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(stripTOMLComment(scanner.Text()))
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(strings.Trim(line, "[] "))
			continue
		}
		usage := cargoSectionUsage(section)
		if usage == "" || line == "" {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		name = strings.Trim(strings.TrimSpace(name), `"'`)
		value = strings.TrimSpace(value)
		if name == "" || value == "" {
			continue
		}
		packageName := name
		declared := ""
		relationship := "required"
		switch {
		case strings.HasPrefix(value, "{"):
			fields := make(map[string]string)
			for _, match := range catalogInlineField.FindAllStringSubmatch(value, -1) {
				fields[strings.ToLower(match[1])] = strings.TrimSpace(match[2])
			}
			if fields["package"] != "" {
				packageName = fields["package"]
			}
			declared = fields["version"]
			if fields["git"] != "" {
				declared = "git+" + fields["git"]
			} else if fields["path"] != "" {
				declared = "file:" + fields["path"]
			}
			if regexp.MustCompile(`(?i)\boptional\s*=\s*true\b`).MatchString(value) {
				relationship = "optional"
			}
		case strings.HasPrefix(value, `"`), strings.HasPrefix(value, `'`):
			declared = strings.Trim(value, `"'`)
		default:
			continue
		}
		resolved := selectLockedVersion(lockVersions[packageName], declared)
		source := ""
		if resolved != "" {
			source = lockPath
		}
		declarations = append(declarations, DependencyDeclaration{
			Ecosystem:        "cargo",
			Package:          packageName,
			Declared:         declared,
			Resolution:       versionResolution(declared),
			Resolved:         resolved,
			ResolutionSource: source,
			Usage:            usage,
			Relationship:     relationship,
			DeclaredScope:    section,
		})
	}
	slices.SortFunc(declarations, func(left, right DependencyDeclaration) int {
		return strings.Compare(left.Package+"\x00"+left.DeclaredScope, right.Package+"\x00"+right.DeclaredScope)
	})
	return declarations
}

func cargoSectionUsage(section string) string {
	switch {
	case section == "dev-dependencies", strings.HasSuffix(section, ".dev-dependencies"):
		return "test"
	case section == "build-dependencies", strings.HasSuffix(section, ".build-dependencies"):
		return "build"
	case section == "dependencies", section == "workspace.dependencies",
		strings.HasSuffix(section, ".dependencies"):
		return "production"
	default:
		return ""
	}
}

func pythonLockVersions(
	contents map[string][]byte,
	manifestPath string,
) (map[string][]string, string) {
	lockPath := nearestDependencyFile(contents, path.Dir(manifestPath), "uv.lock", "poetry.lock")
	if lockPath == "" {
		return nil, ""
	}
	return tomlPackageLockVersions(contents[lockPath]), lockPath
}

func parsePythonDeclarations(
	filePath string,
	content []byte,
	lockVersions map[string][]string,
	lockPath string,
) []DependencyDeclaration {
	base := strings.ToLower(path.Base(filePath))
	if strings.HasPrefix(base, "requirements") && strings.HasSuffix(base, ".txt") {
		return parseRequirementsDeclarations(filePath, content, lockVersions, lockPath)
	}
	return parsePyprojectDeclarations(content, lockVersions, lockPath)
}

func parseRequirementsDeclarations(
	filePath string,
	content []byte,
	lockVersions map[string][]string,
	lockPath string,
) []DependencyDeclaration {
	usage := "production"
	lowerPath := strings.ToLower(filePath)
	if strings.Contains(lowerPath, "test") {
		usage = "test"
	} else if strings.Contains(lowerPath, "dev") {
		usage = "development"
	}
	declarations := make([]DependencyDeclaration, 0)
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		name, declared, ok := parsePythonRequirement(scanner.Text())
		if !ok {
			continue
		}
		resolved := selectLockedVersion(lockedVersionsFor(lockVersions, name, true), declared)
		source := ""
		if resolved != "" {
			source = lockPath
		} else if exact, ok := pythonExactVersion(declared); ok {
			resolved = exact
			source = filePath
		}
		declarations = append(declarations, DependencyDeclaration{
			Ecosystem:        "pypi",
			Package:          name,
			Declared:         declared,
			Resolution:       pythonVersionResolution(declared),
			Resolved:         resolved,
			ResolutionSource: source,
			Usage:            usage,
			Relationship:     "required",
			DeclaredScope:    path.Base(filePath),
		})
	}
	return declarations
}

func parsePyprojectDeclarations(
	content []byte,
	lockVersions map[string][]string,
	lockPath string,
) []DependencyDeclaration {
	declarations := make([]DependencyDeclaration, 0)
	section := ""
	arrayUsage := ""
	arrayScope := ""
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(stripTOMLComment(scanner.Text()))
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(strings.Trim(line, "[] "))
			arrayUsage = ""
			continue
		}
		if arrayUsage != "" {
			declarations = appendPythonArrayRequirements(
				declarations, line, arrayUsage, arrayScope, lockVersions, lockPath,
			)
			if strings.Contains(line, "]") {
				arrayUsage = ""
			}
			continue
		}
		if section == "project" && strings.HasPrefix(strings.ToLower(line), "dependencies") {
			_, value, ok := strings.Cut(line, "=")
			if ok {
				arrayUsage, arrayScope = "production", "project.dependencies"
				declarations = appendPythonArrayRequirements(
					declarations, value, arrayUsage, arrayScope, lockVersions, lockPath,
				)
				if strings.Contains(value, "]") {
					arrayUsage = ""
				}
			}
			continue
		}
		if section == "project.optional-dependencies" && strings.Contains(line, "=") {
			group, value, _ := strings.Cut(line, "=")
			group = strings.Trim(strings.TrimSpace(group), `"'`)
			arrayUsage = pythonGroupUsage(group)
			arrayScope = "project.optional-dependencies." + group
			declarations = appendPythonArrayRequirements(
				declarations, value, arrayUsage, arrayScope, lockVersions, lockPath,
			)
			if strings.Contains(value, "]") {
				arrayUsage = ""
			}
			continue
		}
		if strings.HasPrefix(section, "tool.poetry") && strings.HasSuffix(section, ".dependencies") {
			name, value, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			name = strings.Trim(strings.TrimSpace(name), `"'`)
			if strings.EqualFold(name, "python") {
				continue
			}
			declared := strings.Trim(strings.TrimSpace(value), `"'`)
			if strings.HasPrefix(strings.TrimSpace(value), "{") {
				fields := make(map[string]string)
				for _, match := range catalogInlineField.FindAllStringSubmatch(value, -1) {
					fields[strings.ToLower(match[1])] = strings.TrimSpace(match[2])
				}
				declared = fields["version"]
			}
			usage := "production"
			if strings.Contains(section, ".group.") {
				group := strings.Split(section, ".group.")[1]
				group = strings.TrimSuffix(group, ".dependencies")
				usage = pythonGroupUsage(group)
			}
			declarations = appendPythonDeclaration(
				declarations, name, declared, usage, section, lockVersions, lockPath,
			)
		}
	}
	slices.SortFunc(declarations, func(left, right DependencyDeclaration) int {
		return strings.Compare(strings.ToLower(left.Package+"\x00"+left.DeclaredScope),
			strings.ToLower(right.Package+"\x00"+right.DeclaredScope))
	})
	return declarations
}

var pythonRequirementPattern = regexp.MustCompile(
	`^([A-Za-z0-9][A-Za-z0-9._-]*)(?:\[[^]]+])?\s*(.*)$`,
)

func parsePythonRequirement(value string) (string, string, bool) {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "#") || strings.HasPrefix(value, "-") {
		return "", "", false
	}
	if before, _, ok := strings.Cut(value, " ;"); ok {
		value = strings.TrimSpace(before)
	}
	if index := strings.Index(value, " #"); index >= 0 {
		value = strings.TrimSpace(value[:index])
	}
	match := pythonRequirementPattern.FindStringSubmatch(value)
	if len(match) != 3 {
		return "", "", false
	}
	return match[1], strings.TrimSpace(match[2]), true
}

func appendPythonArrayRequirements(
	declarations []DependencyDeclaration,
	line, usage, scope string,
	lockVersions map[string][]string,
	lockPath string,
) []DependencyDeclaration {
	quoted := regexp.MustCompile(`["']([^"']+)["']`)
	for _, match := range quoted.FindAllStringSubmatch(line, -1) {
		name, declared, ok := parsePythonRequirement(match[1])
		if ok {
			declarations = appendPythonDeclaration(
				declarations, name, declared, usage, scope, lockVersions, lockPath,
			)
		}
	}
	return declarations
}

func appendPythonDeclaration(
	declarations []DependencyDeclaration,
	name, declared, usage, scope string,
	lockVersions map[string][]string,
	lockPath string,
) []DependencyDeclaration {
	resolved := selectLockedVersion(lockedVersionsFor(lockVersions, name, true), declared)
	source := ""
	if resolved != "" {
		source = lockPath
	}
	relationship := "required"
	if strings.HasPrefix(scope, "project.optional-dependencies.") {
		relationship = "optional"
	}
	return append(declarations, DependencyDeclaration{
		Ecosystem:        "pypi",
		Package:          name,
		Declared:         declared,
		Resolution:       pythonVersionResolution(declared),
		Resolved:         resolved,
		ResolutionSource: source,
		Usage:            usage,
		Relationship:     relationship,
		DeclaredScope:    scope,
	})
}

func pythonGroupUsage(group string) string {
	lower := strings.ToLower(group)
	if strings.Contains(lower, "test") {
		return "test"
	}
	return "development"
}

func pythonExactVersion(declared string) (string, bool) {
	declared = strings.TrimSpace(declared)
	for _, prefix := range []string{"===", "=="} {
		if strings.HasPrefix(declared, prefix) {
			version := strings.TrimSpace(strings.TrimPrefix(declared, prefix))
			return version, version != "" && !strings.Contains(version, "*")
		}
	}
	return "", false
}

func pythonVersionResolution(declared string) string {
	if _, ok := pythonExactVersion(declared); ok {
		return "exact"
	}
	return versionResolution(declared)
}

func nugetLockVersions(
	contents map[string][]byte,
	manifestPath string,
) (map[string][]string, string) {
	lockPath := nearestDependencyFile(contents, path.Dir(manifestPath), "packages.lock.json")
	if lockPath == "" {
		return nil, ""
	}
	var document struct {
		Dependencies map[string]map[string]struct {
			Resolved string `json:"resolved"`
		} `json:"dependencies"`
	}
	if json.Unmarshal(contents[lockPath], &document) != nil {
		return nil, ""
	}
	versions := make(map[string][]string)
	for _, framework := range document.Dependencies {
		for name, dependency := range framework {
			if dependency.Resolved != "" && !slices.Contains(versions[name], dependency.Resolved) {
				versions[name] = append(versions[name], dependency.Resolved)
			}
		}
	}
	return versions, lockPath
}

func parseNuGetDeclarations(
	filePath string,
	content []byte,
	lockVersions map[string][]string,
	lockPath string,
) []DependencyDeclaration {
	var project struct {
		References []struct {
			Include       string `xml:"Include,attr"`
			Update        string `xml:"Update,attr"`
			VersionAttr   string `xml:"Version,attr"`
			Version       string `xml:"Version"`
			PrivateAssets string `xml:"PrivateAssets"`
		} `xml:"ItemGroup>PackageReference"`
	}
	if xml.Unmarshal(content, &project) != nil {
		return nil
	}
	usage := "production"
	lowerPath := strings.ToLower(filePath)
	if strings.Contains(lowerPath, "test") {
		usage = "test"
	}
	declarations := make([]DependencyDeclaration, 0, len(project.References))
	for _, reference := range project.References {
		name := firstNonEmpty(reference.Include, reference.Update)
		if name == "" {
			continue
		}
		declared := firstNonEmpty(reference.VersionAttr, reference.Version)
		resolved := selectLockedVersion(lockedVersionsFor(lockVersions, name, false), declared)
		source := ""
		if resolved != "" {
			source = lockPath
		}
		declarations = append(declarations, DependencyDeclaration{
			Ecosystem:        "nuget",
			Package:          name,
			Declared:         declared,
			Resolution:       versionResolution(declared),
			Resolved:         resolved,
			ResolutionSource: source,
			Usage:            usage,
			Relationship:     "required",
			DeclaredScope:    "PackageReference",
		})
	}
	slices.SortFunc(declarations, func(left, right DependencyDeclaration) int {
		return strings.Compare(strings.ToLower(left.Package), strings.ToLower(right.Package))
	})
	return declarations
}

func tomlPackageLockVersions(content []byte) map[string][]string {
	versions := make(map[string][]string)
	name := ""
	version := ""
	flush := func() {
		if name != "" && version != "" && !slices.Contains(versions[name], version) {
			versions[name] = append(versions[name], version)
		}
		name, version = "", ""
	}
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(stripTOMLComment(scanner.Text()))
		if line == "[[package]]" {
			flush()
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "name":
			name = strings.Trim(strings.TrimSpace(value), `"'`)
		case "version":
			version = strings.Trim(strings.TrimSpace(value), `"'`)
		}
	}
	flush()
	return versions
}

func selectLockedVersion(versions []string, declared string) string {
	if len(versions) == 1 {
		return versions[0]
	}
	exact := strings.TrimSpace(strings.TrimPrefix(declared, "v"))
	for _, prefix := range []string{"===", "==", "="} {
		exact = strings.TrimSpace(strings.TrimPrefix(exact, prefix))
	}
	for _, version := range versions {
		if strings.EqualFold(strings.TrimPrefix(version, "v"), exact) {
			return version
		}
	}
	return ""
}

func lockedVersionsFor(
	versions map[string][]string,
	name string,
	pythonNormalization bool,
) []string {
	if matched := versions[name]; len(matched) > 0 {
		return matched
	}
	normalizedName := strings.ToLower(name)
	if pythonNormalization {
		normalizedName = strings.NewReplacer("_", "-", ".", "-").Replace(normalizedName)
	}
	for candidate, matched := range versions {
		normalizedCandidate := strings.ToLower(candidate)
		if pythonNormalization {
			normalizedCandidate = strings.NewReplacer("_", "-", ".", "-").Replace(normalizedCandidate)
		}
		if normalizedCandidate == normalizedName {
			return matched
		}
	}
	return nil
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

func readFiles(
	ctx context.Context,
	repository catalog.Repository,
	revision string,
	filePaths []string,
) (map[string][]byte, error) {
	output := make(map[string][]byte, len(filePaths))
	if len(filePaths) == 0 {
		return output, nil
	}
	bounded, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	commandArguments := make([]string, 0, 4)
	if repository.Bare {
		commandArguments = append(commandArguments, "--git-dir", repository.Path)
	} else {
		commandArguments = append(commandArguments, "-C", repository.Path)
	}
	commandArguments = append(commandArguments, "cat-file", "--batch")
	command := exec.CommandContext(bounded, "git", commandArguments...)
	command.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_EXTERNAL_DIFF=", "LC_ALL=C")
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open git cat-file input: %w", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open git cat-file output: %w", err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start git cat-file batch: %w", err)
	}
	writer := bufio.NewWriter(stdin)
	reader := bufio.NewReader(stdout)
	var batchErr error
	for _, filePath := range filePaths {
		if _, err := fmt.Fprintf(writer, "%s:%s\n", revision, filePath); err != nil {
			batchErr = err
			break
		}
		if err := writer.Flush(); err != nil {
			batchErr = err
			break
		}
		header, err := reader.ReadString('\n')
		if err != nil {
			batchErr = err
			break
		}
		fields := strings.Fields(header)
		if len(fields) == 2 && fields[1] == "missing" {
			continue
		}
		if len(fields) != 3 || fields[1] != "blob" {
			batchErr = fmt.Errorf("unexpected git cat-file header %q", strings.TrimSpace(header))
			break
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || size < 0 {
			batchErr = fmt.Errorf("invalid git cat-file size %q", fields[2])
			break
		}
		if size > maximumSourceFileSize {
			_, err = io.CopyN(io.Discard, reader, size)
		} else {
			content := make([]byte, size)
			_, err = io.ReadFull(reader, content)
			if err == nil {
				output[filePath] = content
			}
		}
		if err != nil {
			batchErr = err
			break
		}
		delimiter, err := reader.ReadByte()
		if err != nil || delimiter != '\n' {
			if err == nil {
				err = errors.New("git cat-file response is missing its delimiter")
			}
			batchErr = err
			break
		}
	}
	_ = stdin.Close()
	if batchErr != nil && command.Process != nil {
		_ = command.Process.Kill()
	}
	waitErr := command.Wait()
	if batchErr != nil {
		return nil, fmt.Errorf("read git cat-file batch: %w", batchErr)
	}
	if waitErr != nil {
		return nil, fmt.Errorf("git cat-file batch: %s", firstNonEmpty(strings.TrimSpace(stderr.String()), waitErr.Error()))
	}
	return output, nil
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

func parseGoModDeclarations(content []byte) []DependencyDeclaration {
	declarations := make([]DependencyDeclaration, 0)
	inRequire := false
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(strings.SplitN(scanner.Text(), "//", 2)[0])
		switch {
		case line == "require (":
			inRequire = true
			continue
		case inRequire && line == ")":
			inRequire = false
			continue
		}
		if strings.HasPrefix(line, "require ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "require "))
		} else if !inRequire {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		declared := ""
		if len(fields) > 1 {
			declared = fields[1]
		}
		declarations = append(declarations, DependencyDeclaration{
			Ecosystem:        "go",
			Package:          fields[0],
			Declared:         declared,
			Resolution:       versionResolution(declared),
			Resolved:         declared,
			ResolutionSource: "go.mod",
			Usage:            "unknown",
			Relationship:     "required",
			DeclaredScope:    "require",
		})
	}
	slices.SortFunc(declarations, func(left, right DependencyDeclaration) int {
		return strings.Compare(left.Package, right.Package)
	})
	return declarations
}

func versionResolution(declared string) string {
	declared = strings.TrimSpace(declared)
	if declared == "" || strings.ContainsAny(declared, "$*+") {
		return "unresolved"
	}
	if strings.HasPrefix(declared, "v") {
		declared = strings.TrimPrefix(declared, "v")
	}
	for _, prefix := range []string{"^", "~", ">", "<", "=", "workspace:", "file:", "link:", "git+", "http:", "https:"} {
		if strings.HasPrefix(declared, prefix) {
			return "constraint"
		}
	}
	if strings.ContainsAny(declared, " |,") {
		return "constraint"
	}
	parts := strings.SplitN(declared, "-", 2)
	numbers := strings.Split(parts[0], ".")
	if len(numbers) >= 2 {
		for _, number := range numbers {
			if number == "" {
				return "constraint"
			}
			for _, character := range number {
				if character < '0' || character > '9' {
					return "constraint"
				}
			}
		}
		return "exact"
	}
	return "constraint"
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
		"Gemfile", "Package.swift", "package-lock.json", "npm-shrinkwrap.json",
		"pnpm-lock.yaml", "yarn.lock", "Cargo.lock", "poetry.lock", "uv.lock",
		"packages.lock.json", "gradle.lockfile":
		return true
	default:
		base := strings.ToLower(path.Base(filePath))
		return (strings.HasPrefix(base, "requirements") && strings.HasSuffix(base, ".txt")) ||
			strings.HasSuffix(filePath, ".csproj") || strings.HasSuffix(filePath, ".sln")
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
		case strings.HasPrefix(strings.ToLower(path.Base(filePath)), "requirements") &&
			strings.HasSuffix(strings.ToLower(path.Base(filePath)), ".txt"):
			return "Python requirements"
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
