package graph

import (
	"fmt"
	"path"
	"sort"

	"github.com/spolnik/RepoKarta/internal/catalog"
)

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
