package graph

import (
	"bufio"
	"bytes"
	"fmt"
	"go/parser"
	"go/token"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/spolnik/RepoKarta/internal/catalog"
)

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

func (b *builder) addGoImportsAndRoutes(
	repository catalog.Repository,
	revision, module string,
	packageIDs map[string]string,
	contents map[string][]byte,
) {
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
		for _, match := range goRoutePattern.FindAllSubmatchIndex(content, -1) {
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
