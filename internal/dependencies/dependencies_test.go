package dependencies

import (
	"testing"

	"github.com/spolnik/RepoKarta/internal/graph"
)

func TestBuildPreservesNormalizedDeclarationEvidence(t *testing.T) {
	snapshot := graph.Snapshot{
		Repositories: []graph.Repository{{
			ID:       7,
			Name:     "service",
			Revision: "abcdef",
		}},
		Manifests: []graph.Manifest{{
			RepositoryID: 7,
			Repository:   "service",
			Kind:         "npm package",
			Path:         "web/package.json",
			Declarations: []graph.DependencyDeclaration{{
				Ecosystem:     "npm",
				Package:       "marked",
				Declared:      "^16.4.1",
				Resolution:    "constraint",
				Usage:         "production",
				Relationship:  "required",
				DeclaredScope: "dependencies",
				Evidence: graph.Evidence{
					RepositoryID: 7,
					Repository:   "service",
					Revision:     "abcdef",
					Path:         "web/package.json",
					Line:         14,
					URL:          "http://ui/source/7#L14",
				},
			}},
		}},
	}

	inventory := Build(snapshot)
	if inventory.RepositoryCount != 1 || inventory.ManifestCount != 1 ||
		inventory.DependencyCount != 1 || inventory.UncheckedCount != 1 {
		t.Fatalf("inventory counts = %#v", inventory)
	}
	declaration := inventory.Declarations[0]
	if declaration.Package != "marked" || declaration.Declared != "^16.4.1" ||
		declaration.Resolution != "constraint" || declaration.CheckStatus != "unchecked" ||
		declaration.Usage != "production" || declaration.Relationship != "required" ||
		declaration.Revision != "abcdef" || declaration.Evidence.Line != 14 {
		t.Fatalf("declaration = %#v", declaration)
	}
}

func TestBuildKeepsLegacyManifestDependenciesHonest(t *testing.T) {
	inventory := Build(graph.Snapshot{
		Repositories: []graph.Repository{{ID: 3, Name: "legacy", Revision: "old"}},
		Manifests: []graph.Manifest{{
			RepositoryID: 3,
			Repository:   "legacy",
			Kind:         "Go module",
			Path:         "go.mod",
			Dependencies: []string{"example.com/module"},
			Evidence:     graph.Evidence{Revision: "old", Path: "go.mod", Line: 1},
		}},
	})

	declaration := inventory.Declarations[0]
	if declaration.Ecosystem != "go" || declaration.Package != "example.com/module" ||
		declaration.Declared != "" || declaration.Resolution != "unresolved" {
		t.Fatalf("legacy declaration = %#v", declaration)
	}
}

func TestBuildUsesExactDeclarationEvidencePathInsteadOfOwningManifest(t *testing.T) {
	inventory := Build(graph.Snapshot{
		Repositories: []graph.Repository{{ID: 8, Name: "service", Revision: "abc123"}},
		Manifests: []graph.Manifest{{
			RepositoryID: 8,
			Repository:   "service",
			Kind:         "Gradle project",
			Path:         "build.gradle",
			Declarations: []graph.DependencyDeclaration{{
				Ecosystem:  "maven",
				Package:    "org.apache.kafka:kafka-clients",
				Declared:   "3.9.0",
				Resolution: "exact",
				Evidence: graph.Evidence{
					RepositoryID: 8,
					Repository:   "service",
					Revision:     "abc123",
					Path:         "gradle/libs.versions.toml",
					Line:         17,
					URL:          "http://ui/source/8?path=gradle%2Flibs.versions.toml#L17",
				},
			}},
		}},
	})

	declaration := inventory.Declarations[0]
	if declaration.ManifestPath != "gradle/libs.versions.toml" ||
		declaration.Evidence.Path != declaration.ManifestPath ||
		declaration.Evidence.Line != 17 {
		t.Fatalf("declaration evidence path was replaced by owning manifest: %#v", declaration)
	}
}

func TestBuildPageFiltersAndBoundsDeclarations(t *testing.T) {
	snapshot := graph.Snapshot{
		Repositories: []graph.Repository{{ID: 1, Name: "service", Revision: "abc"}},
		Manifests: []graph.Manifest{{
			RepositoryID: 1,
			Repository:   "service",
			Kind:         "npm package",
			Path:         "package.json",
			Declarations: []graph.DependencyDeclaration{
				{Ecosystem: "npm", Package: "alpha", Declared: "^1", Resolution: "constraint", Usage: "production"},
				{Ecosystem: "npm", Package: "beta", Declared: "2.0.0", Resolution: "exact", Usage: "development"},
				{Ecosystem: "npm", Package: "gamma", Declared: "^3", Resolution: "constraint", Usage: "production"},
			},
		}},
	}
	inventory := BuildPage(snapshot, Options{Query: "a", Resolution: "constraint", Limit: 1})
	if inventory.TotalCount != 3 || inventory.DependencyCount != 2 ||
		inventory.ReturnedCount != 1 || !inventory.HasMore || inventory.Limit != 1 ||
		inventory.Declarations[0].Package != "alpha" {
		t.Fatalf("first dependency page = %#v", inventory)
	}
	inventory = BuildPage(snapshot, Options{Resolution: "constraint", Offset: 1, Limit: 1})
	if inventory.ReturnedCount != 1 || inventory.HasMore ||
		inventory.Declarations[0].Package != "gamma" {
		t.Fatalf("second dependency page = %#v", inventory)
	}
	inventory = BuildPage(snapshot, Options{Usage: "development"})
	if inventory.DependencyCount != 1 || inventory.Declarations[0].Package != "beta" {
		t.Fatalf("development dependency page = %#v", inventory)
	}
}
