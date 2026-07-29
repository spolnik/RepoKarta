// Package graph derives commit-pinned repository maps without executing source
// code or trusting an AI model to invent structural facts.
package graph

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spolnik/RepoKarta/internal/analysis"
	"github.com/spolnik/RepoKarta/internal/catalog"
)

const (
	snapshotVersion       = 22
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
	Candidate    bool       `json:"candidate,omitempty"`
	Evidence     []Evidence `json:"evidence,omitempty"`
}

// SystemConnection is a directed interaction between deployable components.
// Protocol and interaction are separate because, for example, Kafka publish
// and consume edges share a protocol but flow in opposite directions.
type SystemConnection struct {
	ID                  string     `json:"id"`
	Source              string     `json:"source"`
	Target              string     `json:"target"`
	Protocol            string     `json:"protocol"`
	Interaction         string     `json:"interaction"`
	Transport           string     `json:"transport,omitempty"`
	Confidence          string     `json:"confidence"`
	EvidenceOrigin      string     `json:"evidence_origin"`
	TargetResolved      bool       `json:"target_resolved"`
	EnvironmentVariable string     `json:"environment_variable,omitempty"`
	ResolutionTier      string     `json:"resolution_tier,omitempty"`
	Environment         string     `json:"environment,omitempty"`
	ResolutionDivergent bool       `json:"resolution_divergent,omitempty"`
	UnresolvedReason    string     `json:"unresolved_reason,omitempty"`
	Evidence            []Evidence `json:"evidence,omitempty"`
}

// TopologyPlaceholder is an indexed configuration consumption site. It stays
// in the per-repository artifact so a fleet read can resolve it from an
// assignment indexed in a different repository.
type TopologyPlaceholder struct {
	Source              string   `json:"source"`
	Variable            string   `json:"variable"`
	Default             string   `json:"default,omitempty"`
	MapKeyCandidate     string   `json:"map_key_candidate,omitempty"`
	Protocol            string   `json:"protocol"`
	Interaction         string   `json:"interaction"`
	ConsumptionEvidence Evidence `json:"consumption_evidence"`
}

// UnresolvedTopologyConnection preserves a configuration consumption site
// whose target cannot yet be made into a truthful component. Placeholder text
// and secret references stay here instead of becoming graph node names.
type UnresolvedTopologyConnection struct {
	ID          string     `json:"id"`
	Source      string     `json:"source"`
	Variable    string     `json:"variable"`
	Candidate   string     `json:"candidate,omitempty"`
	Protocol    string     `json:"protocol"`
	Interaction string     `json:"interaction"`
	Reason      string     `json:"reason"`
	Evidence    []Evidence `json:"evidence"`
}

// EnvironmentAssignment is an exact configuration-key assignment found in a
// recognized committed configuration format. Rank preserves the preference
// for deployment configuration over application defaults and other config.
type EnvironmentAssignment struct {
	Variable    string   `json:"variable"`
	Value       string   `json:"value,omitempty"`
	Rank        int      `json:"rank"`
	Environment string   `json:"environment,omitempty"`
	Indirect    bool     `json:"indirect,omitempty"`
	Evidence    Evidence `json:"evidence"`
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
	NodeKinds     []string              `json:"node_kinds,omitempty"`
	Symbols       []analysis.Symbol     `json:"symbols,omitempty"`
	Relations     []analysis.Relation   `json:"relations,omitempty"`
	BuildFacts    []analysis.BuildFact  `json:"build_facts,omitempty"`
	Diagnostics   []analysis.Diagnostic `json:"diagnostics,omitempty"`
}

// Snapshot is an immutable map derived from one or more catalogue revisions.
type Snapshot struct {
	Version                      int                            `json:"version"`
	ID                           string                         `json:"id"`
	GeneratedAt                  time.Time                      `json:"generated_at"`
	Repositories                 []Repository                   `json:"repositories"`
	Languages                    []Language                     `json:"languages"`
	Manifests                    []Manifest                     `json:"manifests"`
	Nodes                        []Node                         `json:"nodes"`
	Edges                        []Edge                         `json:"edges"`
	Ownership                    []OwnershipIndex               `json:"ownership,omitempty"`
	Components                   []SystemComponent              `json:"components,omitempty"`
	Connections                  []SystemConnection             `json:"connections,omitempty"`
	TopologyPlaceholders         []TopologyPlaceholder          `json:"topology_placeholders,omitempty"`
	UnresolvedTopology           []UnresolvedTopologyConnection `json:"unresolved_topology,omitempty"`
	EnvironmentAssignments       []EnvironmentAssignment        `json:"environment_assignments,omitempty"`
	ExcludedEnvironmentVariables []string                       `json:"excluded_environment_variables,omitempty"`
	RejectedExternalCount        int                            `json:"rejected_external_component_count,omitempty"`
	RejectedComponentCounts      map[string]int                 `json:"rejected_component_counts,omitempty"`
	RejectedComponentConnections int                            `json:"rejected_component_connection_count,omitempty"`
	SuppressedSourceEdges        int                            `json:"suppressed_source_edges,omitempty"`
	TopologyFleetGeneratedAt     time.Time                      `json:"topology_fleet_generated_at,omitempty"`
	TopologySelectedGeneratedAt  time.Time                      `json:"topology_selected_generated_at,omitempty"`
	Structure                    []StructuralDocument           `json:"structure,omitempty"`
	StructureTruncated           bool                           `json:"structure_truncated"`
	FileCount                    int                            `json:"file_count"`
	Truncated                    bool                           `json:"truncated"`
	Scope                        Scope                          `json:"scope"`
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
	Ownership          []OwnershipIndex     `json:"ownership,omitempty"`
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
	for componentIndex := range snapshot.Components {
		for evidenceIndex := range snapshot.Components[componentIndex].Evidence {
			rebase(&snapshot.Components[componentIndex].Evidence[evidenceIndex])
		}
	}
	for connectionIndex := range snapshot.Connections {
		for evidenceIndex := range snapshot.Connections[connectionIndex].Evidence {
			rebase(&snapshot.Connections[connectionIndex].Evidence[evidenceIndex])
		}
	}
	for placeholderIndex := range snapshot.TopologyPlaceholders {
		rebase(&snapshot.TopologyPlaceholders[placeholderIndex].ConsumptionEvidence)
	}
	for ownershipIndex := range snapshot.Ownership {
		rebase(&snapshot.Ownership[ownershipIndex].Evidence)
	}
	for assignmentIndex := range snapshot.EnvironmentAssignments {
		rebase(&snapshot.EnvironmentAssignments[assignmentIndex].Evidence)
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

type builder struct {
	baseURL                      string
	repositories                 []Repository
	languages                    map[string]int
	manifests                    []Manifest
	nodes                        map[string]Node
	edges                        map[string]Edge
	components                   map[string]SystemComponent
	connections                  map[string]SystemConnection
	serviceTargets               map[string]string
	clientReferences             []clientReference
	structure                    []StructuralDocument
	ownership                    []OwnershipIndex
	structuralSymbols            int
	structuralTypedRelations     int
	structuralImportRelations    int
	structuralCallRelations      int
	structuralBuildFacts         int
	topologyPlaceholders         []TopologyPlaceholder
	unresolvedTopology           []UnresolvedTopologyConnection
	environmentAssignments       []EnvironmentAssignment
	excludedEnvironmentVariables map[string]bool
	rejectedExternalCount        int
	rejectedComponentCounts      map[string]int
	rejectedComponentIDs         map[string]bool
	rejectedComponentConnections int
	structureTruncated           bool
	fileCount                    int
	truncated                    bool
}

type clientReference struct {
	sourceRepositoryID int64
	target             string
	confidence         string
	evidence           Evidence
}

func newBuilder(baseURL string) *builder {
	return &builder{
		baseURL:                      baseURL,
		languages:                    make(map[string]int),
		nodes:                        make(map[string]Node),
		edges:                        make(map[string]Edge),
		components:                   make(map[string]SystemComponent),
		connections:                  make(map[string]SystemConnection),
		serviceTargets:               make(map[string]string),
		excludedEnvironmentVariables: make(map[string]bool),
		rejectedComponentCounts:      make(map[string]int),
		rejectedComponentIDs:         make(map[string]bool),
	}
}

func cloneStringIntMap(values map[string]int) map[string]int {
	if len(values) == 0 {
		return nil
	}
	cloned := make(map[string]int, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func (b *builder) snapshot(signature string) Snapshot {
	b.resolveClientReferences()
	b.resolveTopologyPlaceholders()
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
	components, connections, suppressedSourceEdges := assembleSystemTopology(
		b.components, b.connections,
	)
	slices.SortFunc(b.unresolvedTopology, func(left, right UnresolvedTopologyConnection) int {
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
		Version:                      snapshotVersion,
		ID:                           signature,
		GeneratedAt:                  time.Now().UTC(),
		Repositories:                 b.repositories,
		Languages:                    languageSummary(b.languages),
		Manifests:                    b.manifests,
		Nodes:                        nodes,
		Edges:                        edges,
		Ownership:                    append([]OwnershipIndex(nil), b.ownership...),
		Components:                   components,
		Connections:                  connections,
		TopologyPlaceholders:         append([]TopologyPlaceholder(nil), b.topologyPlaceholders...),
		UnresolvedTopology:           append([]UnresolvedTopologyConnection(nil), b.unresolvedTopology...),
		EnvironmentAssignments:       append([]EnvironmentAssignment(nil), b.environmentAssignments...),
		ExcludedEnvironmentVariables: sortedEnvironmentVariables(b.excludedEnvironmentVariables),
		RejectedExternalCount:        b.rejectedExternalCount,
		RejectedComponentCounts:      cloneStringIntMap(b.rejectedComponentCounts),
		RejectedComponentConnections: b.rejectedComponentConnections,
		SuppressedSourceEdges:        suppressedSourceEdges,
		Structure:                    b.structure,
		StructureTruncated:           b.structureTruncated,
		FileCount:                    b.fileCount,
		Truncated:                    b.truncated,
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
	ownerPath := codeownersPath(files)
	for _, filePath := range files {
		if !isManifest(filePath) && !isAnalyzedSource(filePath) &&
			!isServiceConfiguration(filePath) && !isTopologyFile(filePath) &&
			!isPotentialEnvironmentAssignmentFile(filePath) &&
			filePath != ownerPath {
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
	ownership := OwnershipIndex{
		RepositoryID: repository.ID,
		Repository:   repository.Name,
		Revision:     revision,
		Available:    false,
	}
	if ownerPath != "" {
		if content, ok := contents[ownerPath]; ok {
			ownership = parseCODEOWNERS(
				Repository{ID: repository.ID, Name: repository.Name, Revision: revision},
				ownerPath,
				content,
				func(filePath string, line int, label string) Evidence {
					return b.evidence(repository, revision, filePath, line, label)
				},
			)
		}
	}
	b.ownership = append(b.ownership, ownership)

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
			NodeKinds:     document.NodeKinds,
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

type gradleDependency struct {
	coordinate    string
	line          int
	configuration string
	evidencePath  string
}

type gradleCatalogReference struct {
	coordinate string
	path       string
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
	nonPathMappingAttribute = regexp.MustCompile(
		`(?i)(?:produces|consumes|headers|params|name)\s*=\s*(?:\{[^{}]*)?$`,
	)
	// A type annotated with @FeignClient or @HttpExchange declares endpoints it
	// calls, not endpoints it serves, so its mappings never become routes.
	declarativeClientType = regexp.MustCompile(`@(?:FeignClient|HttpExchange)\b`)
	quotedJavaString      = regexp.MustCompile(`["']([^"']*)["']`)
	springFunctionalRoute = regexp.MustCompile(
		`(?m)\b(?:RequestPredicates\.)?(GET|POST|PUT|DELETE|PATCH)\s*\(\s*["']([^"']+)["']`,
	)
	goRoutePattern = regexp.MustCompile(
		`(?:Handle|HandleFunc|GET|POST|PUT|DELETE|PATCH|Any|Route|Mount)\(\s*["` + "`" + `]([^"` + "`" + `]+)`,
	)
	cargoOptionalPattern = regexp.MustCompile(`(?i)\boptional\s*=\s*true\b`)
	pythonQuotedPattern  = regexp.MustCompile(`["']([^"']+)["']`)
	normalizeIDPattern   = regexp.MustCompile(`[^a-z0-9]+`)
	feignClientPattern   = regexp.MustCompile(`(?s)@FeignClient\s*\((.*?)\)`)
	feignNamedTarget     = regexp.MustCompile(
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

// infrastructureHosts are hostnames and normalized hostname labels that never
// identify a sibling service in the indexed collection. They keep cluster DNS
// roots, license headers, XML schema namespaces, and package registries from
// inventing inter-service relationships.
var infrastructureHosts = map[string]bool{
	"0-0-0-0":           true,
	"127-0-0-1":         true,
	"apache":            true,
	"amazonaws":         true,
	"azure":             true,
	"bitbucket":         true,
	"cluster.local":     true,
	"cloudflare":        true,
	"docker":            true,
	"eclipse":           true,
	"example":           true,
	"github":            true,
	"gitlab":            true,
	"google":            true,
	"googleapis":        true,
	"host":              true,
	"java":              true,
	"jcenter":           true,
	"jetbrains":         true,
	"json-schema":       true,
	"localhost":         true,
	"maven":             true,
	"microsoft":         true,
	"mozilla":           true,
	"mvnrepository":     true,
	"npmjs":             true,
	"opensource":        true,
	"oracle":            true,
	"plugins":           true,
	"registry":          true,
	"repo1":             true,
	"schemas":           true,
	"sonatype":          true,
	"springframework":   true,
	"svc.cluster.local": true,
	"sun":               true,
	"w3":                true,
	"www":               true,
	"xmlns":             true,
}

var infrastructureHostSuffixes = []string{
	".cluster.local",
	".svc.cluster.local",
}

type catalogEntry struct {
	value string
	line  int
}

type springRoute struct {
	label string
	line  int
}

type springClientTarget struct {
	name string
	line int
}

type mavenDependencyXML struct {
	GroupID    string `xml:"groupId"`
	ArtifactID string `xml:"artifactId"`
	Version    string `xml:"version"`
	Scope      string `xml:"scope"`
	Optional   bool   `xml:"optional"`
}

var pythonRequirementPattern = regexp.MustCompile(
	`^([A-Za-z0-9][A-Za-z0-9._-]*)(?:\[[^]]+])?\s*(.*)$`,
)

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
	value = normalizeIDPattern.ReplaceAllString(value, "-")
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
