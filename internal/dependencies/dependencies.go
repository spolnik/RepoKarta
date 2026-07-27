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
	Query        string
	Ecosystem    string
	Usage        string
	Relationship string
	Resolution   string
	Offset       int
	Limit        int
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
	CheckedCount    int                    `json:"checked_count"`
	CurrentCount    int                    `json:"current_count"`
	UpdateCount     int                    `json:"update_count"`
	ErrorCount      int                    `json:"error_count"`
	ReturnedCount   int                    `json:"returned_count"`
	Declarations    []Declaration          `json:"declarations"`
	Truncated       bool                   `json:"truncated"`
	HasMore         bool                   `json:"has_more"`
	Offset          int                    `json:"offset"`
	Limit           int                    `json:"limit"`
	Query           string                 `json:"query,omitempty"`
	Ecosystem       string                 `json:"ecosystem_filter,omitempty"`
	Usage           string                 `json:"usage_filter,omitempty"`
	Relationship    string                 `json:"relationship_filter,omitempty"`
	Resolution      string                 `json:"resolution_filter,omitempty"`
	Scope           graph.Scope            `json:"scope"`
	BuildProgress   graph.ArtifactProgress `json:"build_progress"`
}

// Declaration is one package declaration in one manifest at one revision.
type Declaration struct {
	RepositoryID     int64          `json:"repository_id"`
	Repository       string         `json:"repository"`
	Revision         string         `json:"revision"`
	ManifestKind     string         `json:"manifest_kind"`
	ManifestPath     string         `json:"manifest_path"`
	Ecosystem        string         `json:"ecosystem"`
	Package          string         `json:"package"`
	Declared         string         `json:"declared,omitempty"`
	Resolution       string         `json:"resolution"`
	Resolved         string         `json:"resolved,omitempty"`
	ResolutionSource string         `json:"resolution_source,omitempty"`
	Usage            string         `json:"usage"`
	Relationship     string         `json:"relationship"`
	DeclaredScope    string         `json:"declared_scope,omitempty"`
	CheckStatus      string         `json:"check_status"`
	LatestStable     string         `json:"latest_stable,omitempty"`
	Registry         string         `json:"registry,omitempty"`
	ObservedAt       string         `json:"observed_at,omitempty"`
	Evidence         graph.Evidence `json:"evidence"`
}

// Build normalizes declarations already captured in a repository map. It does
// not contact registries and therefore gives every row an explicit unchecked
// freshness state.
func Build(snapshot graph.Snapshot) Inventory {
	return BuildPage(snapshot, Options{})
}

// BuildPage normalizes a bounded, filtered page while preserving total counts.
func BuildPage(snapshot graph.Snapshot, options Options) Inventory {
	return buildPage(snapshot, options, nil)
}

func buildPage(snapshot graph.Snapshot, options Options, decorate func(*Declaration)) Inventory {
	options.Query = strings.TrimSpace(options.Query)
	options.Ecosystem = strings.ToLower(strings.TrimSpace(options.Ecosystem))
	options.Usage = strings.ToLower(strings.TrimSpace(options.Usage))
	options.Relationship = strings.ToLower(strings.TrimSpace(options.Relationship))
	options.Resolution = strings.ToLower(strings.TrimSpace(options.Resolution))
	if options.Offset < 0 {
		options.Offset = 0
	}
	if options.Limit <= 0 {
		options.Limit = DefaultPageLimit
	}
	options.Limit = min(options.Limit, MaximumPageLimit)
	declarations := normalizedDeclarations(snapshot)
	if decorate != nil {
		for index := range declarations {
			decorate(&declarations[index])
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
	filtered := filterDeclarations(declarations, options)
	dependencyCount := len(filtered)
	uncheckedCount := 0
	checkedCount := 0
	currentCount := 0
	updateCount := 0
	errorCount := 0
	for _, declaration := range filtered {
		switch declaration.CheckStatus {
		case "current":
			currentCount++
			checkedCount++
		case "update_available":
			updateCount++
			checkedCount++
		case "error":
			errorCount++
		case "unchecked", "checking", "stale":
			uncheckedCount++
		default:
			checkedCount++
		}
	}
	offset := min(options.Offset, dependencyCount)
	end := min(offset+options.Limit, dependencyCount)
	page := filtered[offset:end]

	return Inventory{
		RepositoryCount: len(snapshot.Repositories),
		ManifestCount:   len(snapshot.Manifests),
		TotalCount:      totalCount,
		DependencyCount: dependencyCount,
		UncheckedCount:  uncheckedCount,
		CheckedCount:    checkedCount,
		CurrentCount:    currentCount,
		UpdateCount:     updateCount,
		ErrorCount:      errorCount,
		ReturnedCount:   len(page),
		Declarations:    page,
		Truncated:       snapshot.Truncated || snapshot.StructureTruncated,
		HasMore:         end < dependencyCount,
		Offset:          offset,
		Limit:           options.Limit,
		Query:           options.Query,
		Ecosystem:       options.Ecosystem,
		Usage:           options.Usage,
		Relationship:    options.Relationship,
		Resolution:      options.Resolution,
		Scope:           snapshot.Scope,
	}
}

func normalizedDeclarations(snapshot graph.Snapshot) []Declaration {
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
				RepositoryID:     manifest.RepositoryID,
				Repository:       manifest.Repository,
				Revision:         firstNonEmpty(evidence.Revision, revisions[manifest.RepositoryID]),
				ManifestKind:     manifest.Kind,
				ManifestPath:     firstNonEmpty(evidence.Path, manifest.Path),
				Ecosystem:        firstNonEmpty(dependency.Ecosystem, ecosystemForManifest(manifest.Kind)),
				Package:          dependency.Package,
				Declared:         dependency.Declared,
				Resolution:       firstNonEmpty(dependency.Resolution, "unresolved"),
				Resolved:         dependency.Resolved,
				ResolutionSource: dependency.ResolutionSource,
				Usage:            firstNonEmpty(dependency.Usage, "unknown"),
				Relationship:     firstNonEmpty(dependency.Relationship, "unknown"),
				DeclaredScope:    dependency.DeclaredScope,
				CheckStatus:      "unchecked",
				Evidence:         evidence,
			})
		}
	}
	return declarations
}

func filterDeclarations(declarations []Declaration, options Options) []Declaration {
	filtered := make([]Declaration, 0, len(declarations))
	query := strings.ToLower(strings.TrimSpace(options.Query))
	for _, declaration := range declarations {
		if options.Ecosystem != "" && !strings.EqualFold(declaration.Ecosystem, options.Ecosystem) {
			continue
		}
		if options.Resolution != "" && !strings.EqualFold(declaration.Resolution, options.Resolution) {
			continue
		}
		if options.Usage != "" && !strings.EqualFold(declaration.Usage, options.Usage) {
			continue
		}
		if options.Relationship != "" && !strings.EqualFold(declaration.Relationship, options.Relationship) {
			continue
		}
		if query != "" {
			haystack := strings.ToLower(strings.Join([]string{
				declaration.Package,
				declaration.Repository,
				declaration.ManifestPath,
				declaration.Declared,
				declaration.Resolved,
				declaration.Usage,
				declaration.DeclaredScope,
			}, "\n"))
			if !strings.Contains(haystack, query) {
				continue
			}
		}
		filtered = append(filtered, declaration)
	}
	return filtered
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
	case strings.Contains(lower, ".net"), strings.Contains(lower, "nuget"):
		return "nuget"
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
