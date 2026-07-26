// Package dependencies builds the read-only dependency-management inventory
// from commit-pinned repository map artifacts.
package dependencies

import (
	"slices"
	"strings"

	"github.com/spolnik/RepoKarta/internal/graph"
)

const (
	DefaultPageLimit = 100
	MaximumPageLimit = 500
)

// Options bounds and filters one dependency inventory page.
type Options struct {
	Query      string
	Ecosystem  string
	Resolution string
	Offset     int
	Limit      int
}

// Inventory is a deterministic fleet or repository-scoped declaration view.
// Registry freshness is deliberately represented as unchecked until a later
// observation is recorded; declaration facts never masquerade as live data.
type Inventory struct {
	RepositoryCount int                    `json:"repository_count"`
	ManifestCount   int                    `json:"manifest_count"`
	TotalCount      int                    `json:"total_count"`
	DependencyCount int                    `json:"dependency_count"`
	UncheckedCount  int                    `json:"unchecked_count"`
	ReturnedCount   int                    `json:"returned_count"`
	Declarations    []Declaration          `json:"declarations"`
	Truncated       bool                   `json:"truncated"`
	HasMore         bool                   `json:"has_more"`
	Offset          int                    `json:"offset"`
	Limit           int                    `json:"limit"`
	Query           string                 `json:"query,omitempty"`
	Ecosystem       string                 `json:"ecosystem_filter,omitempty"`
	Resolution      string                 `json:"resolution_filter,omitempty"`
	Scope           graph.Scope            `json:"scope"`
	BuildProgress   graph.ArtifactProgress `json:"build_progress"`
}

// Declaration is one package declaration in one manifest at one revision.
type Declaration struct {
	RepositoryID int64          `json:"repository_id"`
	Repository   string         `json:"repository"`
	Revision     string         `json:"revision"`
	ManifestKind string         `json:"manifest_kind"`
	ManifestPath string         `json:"manifest_path"`
	Ecosystem    string         `json:"ecosystem"`
	Package      string         `json:"package"`
	Declared     string         `json:"declared,omitempty"`
	Resolution   string         `json:"resolution"`
	CheckStatus  string         `json:"check_status"`
	LatestStable string         `json:"latest_stable,omitempty"`
	Registry     string         `json:"registry,omitempty"`
	ObservedAt   string         `json:"observed_at,omitempty"`
	Evidence     graph.Evidence `json:"evidence"`
}

// Build normalizes declarations already captured in a repository map. It does
// not contact registries and therefore gives every row an explicit unchecked
// freshness state.
func Build(snapshot graph.Snapshot) Inventory {
	return BuildPage(snapshot, Options{})
}

// BuildPage normalizes a bounded, filtered page while preserving total counts.
func BuildPage(snapshot graph.Snapshot, options Options) Inventory {
	options.Query = strings.TrimSpace(options.Query)
	options.Ecosystem = strings.ToLower(strings.TrimSpace(options.Ecosystem))
	options.Resolution = strings.ToLower(strings.TrimSpace(options.Resolution))
	if options.Offset < 0 {
		options.Offset = 0
	}
	if options.Limit <= 0 {
		options.Limit = DefaultPageLimit
	}
	options.Limit = min(options.Limit, MaximumPageLimit)
	revisions := make(map[int64]string, len(snapshot.Repositories))
	for _, repository := range snapshot.Repositories {
		revisions[repository.ID] = repository.Revision
	}

	declarations := make([]Declaration, 0)
	for _, manifest := range snapshot.Manifests {
		normalized := manifest.Declarations
		if len(normalized) == 0 {
			normalized = legacyDeclarations(manifest)
		}
		for _, dependency := range normalized {
			evidence := dependency.Evidence
			if evidence.Path == "" {
				evidence = manifest.Evidence
			}
			declarations = append(declarations, Declaration{
				RepositoryID: manifest.RepositoryID,
				Repository:   manifest.Repository,
				Revision:     firstNonEmpty(evidence.Revision, revisions[manifest.RepositoryID]),
				ManifestKind: manifest.Kind,
				ManifestPath: manifest.Path,
				Ecosystem:    firstNonEmpty(dependency.Ecosystem, ecosystemForManifest(manifest.Kind)),
				Package:      dependency.Package,
				Declared:     dependency.Declared,
				Resolution:   firstNonEmpty(dependency.Resolution, "unresolved"),
				CheckStatus:  "unchecked",
				Evidence:     evidence,
			})
		}
	}

	slices.SortFunc(declarations, func(left, right Declaration) int {
		for _, comparison := range []int{
			strings.Compare(strings.ToLower(left.Repository), strings.ToLower(right.Repository)),
			strings.Compare(left.ManifestPath, right.ManifestPath),
			strings.Compare(left.Ecosystem, right.Ecosystem),
			strings.Compare(strings.ToLower(left.Package), strings.ToLower(right.Package)),
		} {
			if comparison != 0 {
				return comparison
			}
		}
		return strings.Compare(left.Declared, right.Declared)
	})

	totalCount := len(declarations)
	filtered := declarations[:0]
	query := strings.ToLower(options.Query)
	for _, declaration := range declarations {
		if options.Ecosystem != "" && !strings.EqualFold(declaration.Ecosystem, options.Ecosystem) {
			continue
		}
		if options.Resolution != "" && !strings.EqualFold(declaration.Resolution, options.Resolution) {
			continue
		}
		if query != "" {
			haystack := strings.ToLower(strings.Join([]string{
				declaration.Package,
				declaration.Repository,
				declaration.ManifestPath,
				declaration.Declared,
			}, "\n"))
			if !strings.Contains(haystack, query) {
				continue
			}
		}
		filtered = append(filtered, declaration)
	}
	dependencyCount := len(filtered)
	offset := min(options.Offset, dependencyCount)
	end := min(offset+options.Limit, dependencyCount)
	page := filtered[offset:end]

	return Inventory{
		RepositoryCount: len(snapshot.Repositories),
		ManifestCount:   len(snapshot.Manifests),
		TotalCount:      totalCount,
		DependencyCount: dependencyCount,
		UncheckedCount:  dependencyCount,
		ReturnedCount:   len(page),
		Declarations:    page,
		Truncated:       snapshot.Truncated || snapshot.StructureTruncated,
		HasMore:         end < dependencyCount,
		Offset:          offset,
		Limit:           options.Limit,
		Query:           options.Query,
		Ecosystem:       options.Ecosystem,
		Resolution:      options.Resolution,
		Scope:           snapshot.Scope,
	}
}

func legacyDeclarations(manifest graph.Manifest) []graph.DependencyDeclaration {
	output := make([]graph.DependencyDeclaration, 0, len(manifest.Dependencies))
	for _, coordinate := range manifest.Dependencies {
		output = append(output, graph.DependencyDeclaration{
			Ecosystem:  ecosystemForManifest(manifest.Kind),
			Package:    coordinate,
			Resolution: "unresolved",
			Evidence:   manifest.Evidence,
		})
	}
	return output
}

func ecosystemForManifest(kind string) string {
	lower := strings.ToLower(kind)
	switch {
	case strings.Contains(lower, "npm"), strings.Contains(lower, "pnpm"):
		return "npm"
	case strings.Contains(lower, "go module"):
		return "go"
	case strings.Contains(lower, "gradle"), strings.Contains(lower, "maven"):
		return "maven"
	case strings.Contains(lower, "cargo"):
		return "cargo"
	case strings.Contains(lower, "python"):
		return "pypi"
	default:
		return "unknown"
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
