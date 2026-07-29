package graph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/spolnik/RepoKarta/internal/analysis"
	"github.com/spolnik/RepoKarta/internal/atomicfile"
	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/telemetry"
)

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
	snapshotLock := s.snapshotLock(fileName)
	snapshotLock.Lock()
	defer snapshotLock.Unlock()
	if !refresh {
		if cached, ok := s.readCachedSnapshot(snapshotPath, signature); ok {
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
	if err := atomicfile.Write(snapshotPath, content, atomicfile.Options{
		Pattern: fileName + ".*.tmp",
	}); err != nil {
		return Snapshot{}, fmt.Errorf("publish graph snapshot: %w", err)
	}
	if repositoryID > 0 {
		if err := s.writeStructuralIndex(structuralIndexFromSnapshot(snapshot)); err != nil {
			return Snapshot{}, err
		}
	}
	s.removeSupersededSnapshots(repositoryID, fileName)
	return snapshot, nil
}

func (s *Service) readCachedSnapshot(snapshotPath, expectedSignature string) (Snapshot, bool) {
	content, err := os.ReadFile(snapshotPath)
	if err != nil {
		return Snapshot{}, false
	}
	var cached Snapshot
	if json.Unmarshal(content, &cached) != nil || cached.Version != snapshotVersion ||
		cached.ID != expectedSignature {
		return Snapshot{}, false
	}
	rebaseSnapshotEvidence(&cached, s.currentBaseURL())
	return cached, true
}

func (s *Service) cachedRepositorySnapshot(repository catalog.Repository) (Snapshot, bool) {
	signature := snapshotSignature([]catalog.Repository{repository})
	fileName := fmt.Sprintf("repository-%d-%s.json", repository.ID, signature)
	lock := s.snapshotLock(fileName)
	lock.Lock()
	defer lock.Unlock()
	return s.readCachedSnapshot(filepath.Join(s.directory, fileName), signature)
}

func (s *Service) removeSupersededSnapshots(repositoryID int64, keepFile string) {
	prefix := "all-"
	structuralPrefix := ""
	keepStructural := ""
	if repositoryID > 0 {
		prefix = fmt.Sprintf("repository-%d-", repositoryID)
		structuralPrefix = fmt.Sprintf("structure-repository-%d-", repositoryID)
		signature := strings.TrimSuffix(strings.TrimPrefix(keepFile, prefix), ".json")
		keepStructural = structuralPrefix + signature + ".json"
	}
	entries, err := os.ReadDir(s.directory)
	if err != nil {
		return
	}
	type removedLock struct {
		name string
		lock *sync.Mutex
	}
	removedLocks := make([]removedLock, 0)
	for _, entry := range entries {
		name := entry.Name()
		if name == keepFile || name == keepStructural ||
			entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		if strings.HasPrefix(name, prefix) ||
			(structuralPrefix != "" && strings.HasPrefix(name, structuralPrefix)) {
			lock := s.snapshotLock(name)
			lock.Lock()
			removed := os.Remove(filepath.Join(s.directory, name)) == nil
			lock.Unlock()
			if removed {
				removedLocks = append(removedLocks, removedLock{name: name, lock: lock})
			}
		}
	}
	s.snapshotMu.Lock()
	for _, removed := range removedLocks {
		if s.snapshotLocks[removed.name] == removed.lock {
			delete(s.snapshotLocks, removed.name)
		}
	}
	s.snapshotMu.Unlock()
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
	results, err := readRepositoryArtifacts(ctx, repositories, s.cachedRepositorySnapshot)
	if err != nil {
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
		if !result.Ready {
			continue
		}
		output.Repositories = append(output.Repositories, result.Value.Repositories...)
		output.Manifests = append(output.Manifests, result.Value.Manifests...)
		output.Truncated = output.Truncated || result.Value.Truncated
		output.StructureTruncated = output.StructureTruncated || result.Value.StructureTruncated
		output.Scope.AnalyzedRepositories++
	}
	output.Scope.OmittedRepositories = output.Scope.TotalRepositories - output.Scope.AnalyzedRepositories
	output.Scope.Complete = output.Scope.OmittedRepositories == 0
	output.Truncated = output.Truncated || !output.Scope.Complete
	progress := artifactProgress(output.Scope.TotalRepositories, output.Scope.AnalyzedRepositories)
	return output, progress, nil
}

// ReadTopologySnapshot composes distributed-system components and connections
// from already-prepared per-repository artifacts. Repository selection records
// the component to focus but deliberately retains the complete fleet snapshot:
// inbound edges originate in other repositories and cannot be recovered from
// the selected repository's extraction. The final fleet pass also reconciles
// inferred external peers against aliases from every visible repository.
func (s *Service) ReadTopologySnapshot(
	ctx context.Context,
	repositoryID int64,
) (Snapshot, ArtifactProgress, error) {
	requestedRepositories, err := s.repositories(ctx, repositoryID)
	if err != nil {
		return Snapshot{}, ArtifactProgress{}, err
	}
	resolutionRepositories := requestedRepositories
	if repositoryID > 0 {
		resolutionRepositories, err = s.repositories(ctx, 0)
		if err != nil {
			return Snapshot{}, ArtifactProgress{}, err
		}
	}
	results, err := readRepositoryArtifacts(
		ctx,
		resolutionRepositories,
		s.cachedRepositorySnapshot,
	)
	if err != nil {
		return Snapshot{}, ArtifactProgress{}, err
	}

	output := Snapshot{
		Version:      snapshotVersion,
		ID:           snapshotSignature(resolutionRepositories),
		GeneratedAt:  time.Now().UTC(),
		Repositories: []Repository{},
		Components:   []SystemComponent{},
		Connections:  []SystemConnection{},
		Scope: Scope{
			Kind:                  "repository",
			TotalRepositories:     len(resolutionRepositories),
			RequestedRepositoryID: repositoryID,
		},
	}
	if repositoryID == 0 {
		output.Scope.Kind = "collection"
	}
	merged := newBuilder(s.currentBaseURL())
	suppressedSourceEdges := 0
	for _, result := range results {
		if !result.Ready {
			continue
		}
		snapshot := result.Value
		output.Repositories = append(output.Repositories, snapshot.Repositories...)
		output.Scope.AnalyzedRepositories++
		if output.TopologyFleetGeneratedAt.IsZero() ||
			(!snapshot.GeneratedAt.IsZero() &&
				snapshot.GeneratedAt.Before(output.TopologyFleetGeneratedAt)) {
			output.TopologyFleetGeneratedAt = snapshot.GeneratedAt
		}
		if result.Repository.ID == repositoryID {
			output.TopologySelectedGeneratedAt = snapshot.GeneratedAt
		}
		for _, component := range snapshot.Components {
			merged.addSystemComponent(component)
		}
		for _, connection := range snapshot.Connections {
			if connection.EnvironmentVariable != "" {
				continue
			}
			merged.addSystemConnection(connection)
		}
		merged.topologyPlaceholders = append(
			merged.topologyPlaceholders, snapshot.TopologyPlaceholders...,
		)
		merged.environmentAssignments = append(
			merged.environmentAssignments, snapshot.EnvironmentAssignments...,
		)
		merged.rejectedExternalCount += snapshot.RejectedExternalCount
		for reason, count := range snapshot.RejectedComponentCounts {
			merged.rejectedComponentCounts[reason] += count
		}
		merged.rejectedComponentConnections +=
			snapshot.RejectedComponentConnections
		for _, variable := range snapshot.ExcludedEnvironmentVariables {
			merged.excludedEnvironmentVariables[variable] = true
		}
		suppressedSourceEdges += snapshot.SuppressedSourceEdges
		output.Truncated = output.Truncated || snapshot.Truncated
	}
	merged.resolveTopologyPlaceholders()
	merged.resolveSystemConnections()
	var newlySuppressed int
	output.Components, output.Connections, newlySuppressed = assembleSystemTopology(
		merged.components, merged.connections,
	)
	output.SuppressedSourceEdges = suppressedSourceEdges + newlySuppressed
	output.RejectedExternalCount = merged.rejectedExternalCount
	output.RejectedComponentCounts = cloneStringIntMap(
		merged.rejectedComponentCounts,
	)
	output.RejectedComponentConnections =
		merged.rejectedComponentConnections
	output.UnresolvedTopology = append(
		[]UnresolvedTopologyConnection(nil), merged.unresolvedTopology...,
	)
	slices.SortFunc(output.UnresolvedTopology, func(left, right UnresolvedTopologyConnection) int {
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
	results, err := readRepositoryArtifacts(ctx, repositories, s.cachedRepositorySnapshot)
	if err != nil {
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
		if !result.Ready {
			continue
		}
		output.Repositories = append(output.Repositories, result.Value.Repositories...)
		for _, node := range result.Value.Nodes {
			if node.Kind == "route" {
				output.Nodes = append(output.Nodes, node)
			}
		}
		output.Truncated = output.Truncated || result.Value.Truncated
		output.StructureTruncated = output.StructureTruncated || result.Value.StructureTruncated
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
func (s *Service) PrepareStructure(ctx context.Context, repositoryID int64) (resultErr error) {
	ctx, finish := telemetry.StartOperation(ctx, telemetry.OperationTopologyBuild, telemetry.Labels{
		Trigger: "index",
	})
	defer func() { finish(resultErr) }()
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
	results, err := readRepositoryArtifacts(
		ctx,
		repositories,
		func(repository catalog.Repository) (StructuralIndex, bool) {
			signature := snapshotSignature([]catalog.Repository{repository})
			return s.readStructuralIndex(repository.ID, signature)
		},
	)
	if err != nil {
		return StructuralIndex{}, err
	}
	for _, result := range results {
		if !result.Ready {
			continue
		}
		output.Structure = append(output.Structure, result.Value.Structure...)
		output.Ownership = append(output.Ownership, result.Value.Ownership...)
		output.StructureTruncated = output.StructureTruncated || result.Value.StructureTruncated
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
			NodeKinds:     append([]string(nil), document.NodeKinds...),
			Symbols:       append([]analysis.Symbol(nil), document.Symbols...),
			Relations:     append([]analysis.Relation(nil), document.Relations...),
			BuildFacts:    append([]analysis.BuildFact(nil), document.BuildFacts...),
			Diagnostics:   append([]analysis.Diagnostic(nil), document.Diagnostics...),
		})
	}
	return StructuralIndex{
		Version:            snapshotVersion,
		ID:                 snapshot.ID,
		GeneratedAt:        snapshot.GeneratedAt,
		Structure:          structure,
		Ownership:          append([]OwnershipIndex(nil), snapshot.Ownership...),
		StructureTruncated: snapshot.StructureTruncated,
		Scope:              snapshot.Scope,
	}
}

func (s *Service) readStructuralIndex(repositoryID int64, signature string) (StructuralIndex, bool) {
	fileName := filepath.Base(s.structuralIndexPath(repositoryID, signature))
	lock := s.snapshotLock(fileName)
	lock.Lock()
	defer lock.Unlock()
	content, err := os.ReadFile(filepath.Join(s.directory, fileName))
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
	lock := s.snapshotLock(fileName)
	lock.Lock()
	defer lock.Unlock()
	if err := atomicfile.Write(
		filepath.Join(s.directory, fileName),
		content,
		atomicfile.Options{Pattern: fileName + ".*.tmp"},
	); err != nil {
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
