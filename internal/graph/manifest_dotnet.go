package graph

import (
	"bufio"
	"bytes"
	"encoding/json"
	"encoding/xml"
	"path"
	"slices"
	"strings"
)

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
