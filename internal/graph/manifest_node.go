package graph

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/spolnik/RepoKarta/internal/catalog"
	"go.yaml.in/yaml/v4"
)

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
