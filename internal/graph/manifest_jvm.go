package graph

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/spolnik/RepoKarta/internal/catalog"
)

func (b *builder) addGradleManifests(
	repository catalog.Repository,
	revision, repositoryNodeID string,
	contents map[string][]byte,
) {
	paths := make([]string, 0)
	for filePath := range contents {
		switch path.Base(filePath) {
		case "build.gradle", "build.gradle.kts", "settings.gradle", "settings.gradle.kts",
			"libs.versions.toml":
			paths = append(paths, filePath)
		}
	}
	versionVariables := parseGradleVersionVariables(contents)
	sort.Strings(paths)
	for _, filePath := range paths {
		content := contents[filePath]
		if match := gradleProjectName.FindSubmatch(content); len(match) == 2 {
			b.registerServiceTarget(string(match[1]), repositoryNodeID)
		}
		versionCatalog := gradleVersionCatalogReferences(contents, filePath)
		dependencies := parseGradleDependencies(content, versionCatalog, versionVariables)
		if path.Base(filePath) == "libs.versions.toml" {
			dependencies = catalogDependencies(content)
		}
		lockVersions, lockPath := gradleLockVersions(contents, filePath)
		labels := make([]string, 0, len(dependencies))
		declarations := make([]DependencyDeclaration, 0, len(dependencies))
		for _, dependency := range dependencies {
			labels = append(labels, dependency.coordinate)
			label, version := gradleCoordinateParts(dependency.coordinate)
			declarationPath := firstNonEmpty(dependency.evidencePath, filePath)
			resolved := lockVersions[label]
			resolutionSource := ""
			if resolved != "" {
				resolutionSource = lockPath
			}
			declarations = append(declarations, DependencyDeclaration{
				Ecosystem:        "maven",
				Package:          label,
				Declared:         version,
				Resolution:       versionResolution(version),
				Resolved:         resolved,
				ResolutionSource: resolutionSource,
				Usage:            gradleDependencyUsage(dependency.configuration),
				Relationship:     "required",
				DeclaredScope:    dependency.configuration,
				Evidence: b.evidence(
					repository,
					revision,
					declarationPath,
					dependency.line,
					dependency.coordinate,
				),
			})
		}
		kind := manifestKind(filePath)
		projectName := repository.Name
		if directory := path.Dir(filePath); directory != "." {
			projectName = path.Base(directory)
		}
		evidence := b.evidence(repository, revision, filePath, 1, kind)
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
			Name:         projectName,
			Dependencies: labels,
			Declarations: declarations,
			Evidence:     evidence,
		})
		for _, dependency := range dependencies {
			declarationPath := firstNonEmpty(dependency.evidencePath, filePath)
			dependencyEvidence := b.evidence(
				repository,
				revision,
				declarationPath,
				dependency.line,
				dependency.coordinate,
			)
			label, version := gradleCoordinateParts(dependency.coordinate)
			subtitle := "Gradle"
			if version != "" {
				subtitle += " · " + version
			}
			dependencyNodeID := "dependency:gradle:" + normalizeID(dependency.coordinate)
			b.addNode(Node{
				ID:       dependencyNodeID,
				Kind:     "dependency",
				Label:    label,
				Subtitle: subtitle,
				Layer:    "Dependencies",
				Evidence: []Evidence{dependencyEvidence},
			})
			b.addEdge(Edge{
				ID:       edgeID(manifestID, dependencyNodeID, "depends"),
				Source:   manifestID,
				Target:   dependencyNodeID,
				Kind:     "dependency",
				Label:    "declares",
				Evidence: []Evidence{dependencyEvidence},
			})
		}
	}
}

func parseGradleDependencies(
	content []byte,
	catalog map[string]gradleCatalogReference,
	versionVariables map[string]string,
) []gradleDependency {
	byCoordinate := make(map[string]gradleDependency)
	for _, match := range gradleStringDependency.FindAllSubmatchIndex(content, -1) {
		coordinate := resolveGradleCoordinate(
			strings.TrimSpace(string(content[match[2]:match[3]])),
			versionVariables,
		)
		if !validGradleCoordinate(coordinate) {
			continue
		}
		configuration := gradleConfigurationAt(content[match[0]:match[1]])
		byCoordinate[coordinate+"\x00"+configuration] = gradleDependency{
			coordinate:    coordinate,
			line:          lineAtOffset(content, match[0]),
			configuration: configuration,
		}
	}
	for _, match := range gradleNamedDependency.FindAllSubmatchIndex(content, -1) {
		coordinate := parseGradleNamedCoordinate(
			string(content[match[2]:match[3]]),
			versionVariables,
		)
		if !validGradleCoordinate(coordinate) {
			continue
		}
		configuration := gradleConfigurationAt(content[match[0]:match[1]])
		byCoordinate[coordinate+"\x00"+configuration] = gradleDependency{
			coordinate:    coordinate,
			line:          lineAtOffset(content, match[0]),
			configuration: configuration,
		}
	}
	for _, match := range gradleProjectDependency.FindAllSubmatchIndex(content, -1) {
		projectName := strings.TrimPrefix(strings.TrimSpace(string(content[match[2]:match[3]])), ":")
		coordinate := "project:" + strings.ReplaceAll(projectName, ":", "/")
		configuration := gradleConfigurationAt(content[match[0]:match[1]])
		byCoordinate[coordinate+"\x00"+configuration] = gradleDependency{
			coordinate:    coordinate,
			line:          lineAtOffset(content, match[0]),
			configuration: configuration,
		}
	}
	for _, match := range gradleCatalogDependency.FindAllSubmatchIndex(content, -1) {
		alias := normalizeCatalogAlias(string(content[match[2]:match[3]]))
		reference, ok := catalog[alias]
		if !ok || reference.coordinate == "" {
			continue
		}
		configuration := gradleConfigurationAt(content[match[0]:match[1]])
		byCoordinate[reference.coordinate+"\x00"+configuration] = gradleDependency{
			coordinate:    reference.coordinate,
			line:          reference.line,
			configuration: configuration,
			evidencePath:  reference.path,
		}
	}
	output := make([]gradleDependency, 0, len(byCoordinate))
	for _, dependency := range byCoordinate {
		output = append(output, dependency)
	}
	slices.SortFunc(output, func(left, right gradleDependency) int {
		if comparison := strings.Compare(left.coordinate, right.coordinate); comparison != 0 {
			return comparison
		}
		return strings.Compare(left.configuration, right.configuration)
	})
	return output
}

func gradleConfigurationAt(content []byte) string {
	fields := strings.Fields(strings.TrimSpace(string(content)))
	if len(fields) == 0 {
		return ""
	}
	configuration := fields[0]
	if index := strings.IndexAny(configuration, "( \t"); index >= 0 {
		configuration = configuration[:index]
	}
	return strings.TrimSpace(configuration)
}

func gradleDependencyUsage(configuration string) string {
	lower := strings.ToLower(strings.TrimSpace(configuration))
	switch {
	case lower == "", lower == "versioncatalog":
		return "unknown"
	case strings.Contains(lower, "test"), strings.Contains(lower, "e2e"):
		return "test"
	case strings.Contains(lower, "annotationprocessor"), strings.HasPrefix(lower, "kapt"),
		strings.HasPrefix(lower, "ksp"), lower == "classpath":
		return "build"
	case lower == "developmentonly":
		return "development"
	default:
		return "production"
	}
}

func gradleLockVersions(contents map[string][]byte, manifestPath string) (map[string]string, string) {
	lockPath := nearestDependencyFile(contents, path.Dir(manifestPath), "gradle.lockfile")
	if lockPath == "" {
		return nil, ""
	}
	versions := make(map[string]string)
	scanner := bufio.NewScanner(bytes.NewReader(contents[lockPath]))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		coordinate, _, _ := strings.Cut(line, "=")
		parts := strings.Split(strings.TrimSpace(coordinate), ":")
		if len(parts) >= 3 {
			versions[parts[0]+":"+parts[1]] = parts[2]
		}
	}
	return versions, lockPath
}

// parseGradleVersionVariables resolves the common repository-local sources
// used by Groovy and Kotlin builds: gradle.properties plus literal assignments
// in settings and build scripts. Only literal values are accepted.
func parseGradleVersionVariables(contents map[string][]byte) map[string]string {
	output := make(map[string]string)
	paths := make([]string, 0, len(contents))
	for filePath := range contents {
		base := path.Base(filePath)
		if base == "gradle.properties" ||
			base == "build.gradle" || base == "build.gradle.kts" ||
			base == "settings.gradle" || base == "settings.gradle.kts" {
			paths = append(paths, filePath)
		}
	}
	sort.Strings(paths)
	for _, filePath := range paths {
		content := contents[filePath]
		if path.Base(filePath) == "gradle.properties" {
			scanner := bufio.NewScanner(bytes.NewReader(content))
			scanner.Buffer(make([]byte, 0, 64*1024), maximumSourceFileSize)
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
					continue
				}
				key, value, ok := strings.Cut(line, "=")
				if !ok {
					key, value, ok = strings.Cut(line, ":")
				}
				key, value = strings.TrimSpace(key), strings.TrimSpace(value)
				if ok && key != "" && value != "" && !strings.Contains(value, "$") {
					output[key] = value
				}
			}
		}
		for _, pattern := range []*regexp.Regexp{
			gradleVersionAssignment,
			gradleExtraVersionAssignment,
		} {
			for _, match := range pattern.FindAllSubmatch(content, -1) {
				output[string(match[1])] = string(match[2])
			}
		}
	}
	return output
}

func parseGradleVersionCatalogs(contents map[string][]byte) map[string]string {
	output := make(map[string]string)
	for filePath, content := range contents {
		if path.Base(filePath) != "libs.versions.toml" {
			continue
		}
		for alias, entry := range catalogCoordinates(content) {
			output[alias] = entry.value
		}
	}
	return output
}

// gradleVersionCatalogReferences resolves the catalog visible to one Gradle
// manifest and retains the exact library-entry location. The build-script
// accessor proves usage, but the catalog entry is the package/version
// declaration that dependency freshness and advisory findings must cite.
func gradleVersionCatalogReferences(
	contents map[string][]byte,
	manifestPath string,
) map[string]gradleCatalogReference {
	catalogPath := nearestDependencyFile(
		contents,
		path.Dir(manifestPath),
		"libs.versions.toml",
		"gradle/libs.versions.toml",
	)
	if catalogPath == "" {
		return nil
	}
	output := make(map[string]gradleCatalogReference)
	for alias, entry := range catalogCoordinates(contents[catalogPath]) {
		output[alias] = gradleCatalogReference{
			coordinate: entry.value,
			path:       catalogPath,
			line:       entry.line,
		}
	}
	return output
}

// catalogCoordinates resolves every `[libraries]` alias of one version catalog
// to `group:artifact[:version]` and records the exact declaring line. Versions
// are read in a separate pass so `version.ref` resolves regardless of table
// order.
func catalogCoordinates(content []byte) map[string]catalogEntry {
	versions := make(map[string]string)
	for alias, entry := range catalogSection(content, "versions") {
		versions[alias] = strings.Trim(strings.TrimSpace(stripTOMLComment(entry.value)), `"'`)
	}
	output := make(map[string]catalogEntry)
	for alias, entry := range catalogSection(content, "libraries") {
		if coordinate := parseCatalogCoordinate(entry.value, versions); coordinate != "" {
			output[alias] = catalogEntry{value: coordinate, line: entry.line}
		}
	}
	return output
}

// catalogSection returns the normalized alias, raw value, and one-based line of
// every entry in one TOML table of a Gradle version catalog.
func catalogSection(content []byte, wanted string) map[string]catalogEntry {
	values := make(map[string]catalogEntry)
	section := ""
	number := 0
	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(make([]byte, 0, 64*1024), maximumSourceFileSize)
	for scanner.Scan() {
		number++
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.Trim(line, "[] ")
			continue
		}
		if section != wanted {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		alias := normalizeCatalogAlias(strings.TrimSpace(key))
		if alias == "" {
			continue
		}
		values[alias] = catalogEntry{value: strings.TrimSpace(value), line: number}
	}
	return values
}

// stripTOMLComment removes a trailing comment while preserving `#` characters
// inside quoted values.
func stripTOMLComment(value string) string {
	quote := byte(0)
	for index := 0; index < len(value); index++ {
		switch character := value[index]; {
		case quote != 0 && character == quote:
			quote = 0
		case quote == 0 && (character == '"' || character == '\''):
			quote = character
		case quote == 0 && character == '#':
			return value[:index]
		}
	}
	return value
}

func parseCatalogCoordinate(value string, versions map[string]string) string {
	if !strings.HasPrefix(strings.TrimSpace(value), "{") {
		coordinate := strings.Trim(strings.TrimSpace(stripTOMLComment(value)), `"'`)
		if validGradleCoordinate(coordinate) {
			return coordinate
		}
		return ""
	}
	fields := make(map[string]string)
	for _, match := range catalogInlineField.FindAllStringSubmatch(value, -1) {
		fields[strings.ToLower(match[1])] = strings.TrimSpace(match[2])
	}
	module := fields["module"]
	if module == "" && fields["group"] != "" && fields["name"] != "" {
		module = fields["group"] + ":" + fields["name"]
	}
	if module == "" {
		return ""
	}
	version := fields["version"]
	// Both `version.ref = "x"` and the nested `version = { ref = "x" }` form
	// point at the `[versions]` table.
	if reference := firstNonEmpty(fields["version.ref"], fields["ref"]); reference != "" {
		version = versions[normalizeCatalogAlias(reference)]
	}
	if version != "" && !strings.Contains(version, "$") {
		return module + ":" + version
	}
	return module
}

// catalogDependencies lists every library declared by one version catalog with
// the exact line that declares it.
func catalogDependencies(content []byte) []gradleDependency {
	entries := catalogCoordinates(content)
	output := make([]gradleDependency, 0, len(entries))
	for _, entry := range entries {
		output = append(output, gradleDependency{
			coordinate:    entry.value,
			line:          entry.line,
			configuration: "versionCatalog",
		})
	}
	slices.SortFunc(output, func(left, right gradleDependency) int {
		return strings.Compare(left.coordinate, right.coordinate)
	})
	return output
}

func normalizeCatalogAlias(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	value = strings.TrimSuffix(value, ".get")
	value = strings.NewReplacer("-", ".", "_", ".").Replace(value)
	for strings.Contains(value, "..") {
		value = strings.ReplaceAll(value, "..", ".")
	}
	return strings.Trim(value, ".")
}

// parseGradleNamedCoordinate reads Groovy map arguments and Kotlin named
// arguments in any order and returns `group:artifact[:version]`. Interpolated
// versions are dropped rather than recorded as literal build-script text.
func parseGradleNamedCoordinate(arguments string, versionVariables map[string]string) string {
	fields := make(map[string]string)
	for _, match := range gradleNamedField.FindAllStringSubmatch(arguments, -1) {
		fields[strings.ToLower(match[1])] = strings.TrimSpace(match[2])
	}
	module := fields["module"]
	if module == "" && fields["group"] != "" && fields["name"] != "" {
		module = fields["group"] + ":" + fields["name"]
	}
	if module == "" {
		return ""
	}
	if version := fields["version"]; version != "" {
		if resolved, ok := resolveGradleInterpolation(version, versionVariables); ok {
			return module + ":" + resolved
		}
	}
	return module
}

// resolveGradleCoordinate keeps group:artifact when an interpolated version
// cannot be proven, and preserves the version when its repository-local
// variable resolves to a literal value.
func resolveGradleCoordinate(coordinate string, versionVariables map[string]string) string {
	parts := strings.SplitN(coordinate, ":", 3)
	if len(parts) == 3 {
		if resolved, ok := resolveGradleInterpolation(parts[2], versionVariables); ok {
			return parts[0] + ":" + parts[1] + ":" + resolved
		}
		return parts[0] + ":" + parts[1]
	}
	return coordinate
}

func resolveGradleInterpolation(value string, versionVariables map[string]string) (string, bool) {
	if !strings.Contains(value, "$") {
		return value, value != ""
	}
	missing := false
	resolved := gradleInterpolation.ReplaceAllStringFunc(value, func(match string) string {
		submatch := gradleInterpolation.FindStringSubmatch(match)
		key := firstNonEmpty(submatch[1], submatch[2])
		replacement := versionVariables[key]
		if replacement == "" {
			missing = true
			return match
		}
		return replacement
	})
	return resolved, !missing && resolved != "" && !strings.Contains(resolved, "$")
}

func validGradleCoordinate(value string) bool {
	if strings.HasPrefix(value, "project:") {
		return true
	}
	if strings.Count(value, ":") < 1 ||
		strings.HasPrefix(value, "libs.") ||
		strings.Contains(value, " ") ||
		strings.Contains(value, "/") {
		return false
	}
	// An unresolved Groovy or Kotlin interpolation is not evidence of a
	// specific artifact, so only fully literal group and artifact segments
	// become dependency nodes.
	parts := strings.SplitN(value, ":", 3)
	return !strings.Contains(parts[0], "$") && !strings.Contains(parts[1], "$") &&
		parts[0] != "" && parts[1] != ""
}

func gradleCoordinateParts(coordinate string) (string, string) {
	if strings.HasPrefix(coordinate, "project:") {
		return coordinate, ""
	}
	parts := strings.Split(coordinate, ":")
	if len(parts) < 2 {
		return coordinate, ""
	}
	label := parts[0] + ":" + parts[1]
	if len(parts) > 2 {
		return label, strings.Join(parts[2:], ":")
	}
	return label, ""
}

func parseMavenDeclarations(content []byte) []DependencyDeclaration {
	var project struct {
		Properties struct {
			InnerXML []byte `xml:",innerxml"`
		} `xml:"properties"`
		Dependencies []mavenDependencyXML `xml:"dependencies>dependency"`
	}
	if xml.Unmarshal(content, &project) != nil {
		return nil
	}
	properties := parseMavenProperties(project.Properties.InnerXML)
	declarations := make([]DependencyDeclaration, 0, len(project.Dependencies))
	for _, dependency := range project.Dependencies {
		groupID := resolveMavenProperty(strings.TrimSpace(dependency.GroupID), properties)
		artifactID := resolveMavenProperty(strings.TrimSpace(dependency.ArtifactID), properties)
		if groupID == "" || artifactID == "" {
			continue
		}
		version := resolveMavenProperty(strings.TrimSpace(dependency.Version), properties)
		scope := strings.ToLower(strings.TrimSpace(dependency.Scope))
		if scope == "" {
			scope = "compile"
		}
		usage := "production"
		if scope == "test" {
			usage = "test"
		} else if scope == "import" {
			usage = "build"
		}
		relationship := "required"
		if dependency.Optional {
			relationship = "optional"
		}
		declarations = append(declarations, DependencyDeclaration{
			Ecosystem:     "maven",
			Package:       groupID + ":" + artifactID,
			Declared:      version,
			Resolution:    versionResolution(version),
			Usage:         usage,
			Relationship:  relationship,
			DeclaredScope: scope,
		})
	}
	slices.SortFunc(declarations, func(left, right DependencyDeclaration) int {
		return strings.Compare(left.Package+"\x00"+left.DeclaredScope, right.Package+"\x00"+right.DeclaredScope)
	})
	return declarations
}

func parseMavenProperties(content []byte) map[string]string {
	properties := make(map[string]string)
	decoder := xml.NewDecoder(bytes.NewReader(content))
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		var value string
		if decoder.DecodeElement(&value, &start) == nil {
			properties[start.Name.Local] = strings.TrimSpace(value)
		}
	}
	return properties
}

func resolveMavenProperty(value string, properties map[string]string) string {
	if strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}") {
		if resolved := properties[strings.TrimSuffix(strings.TrimPrefix(value, "${"), "}")]; resolved != "" {
			return resolved
		}
	}
	return value
}

func dependencyDeclarationLine(content []byte, declaration DependencyDeclaration) int {
	needle := declaration.Package
	if declaration.Ecosystem == "maven" {
		if _, artifact, ok := strings.Cut(declaration.Package, ":"); ok {
			needle = artifact
		}
	}
	return lineContaining(content, needle)
}
