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
				Ecosystem:  "npm",
				Package:    "marked",
				Declared:   "^16.4.1",
				Resolution: "constraint",
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
