package graph

import (
	"bufio"
	"bytes"
	"path"
	"slices"
	"strings"
)

func pythonLockVersions(
	contents map[string][]byte,
	manifestPath string,
) (map[string][]string, string) {
	lockPath := nearestDependencyFile(contents, path.Dir(manifestPath), "uv.lock", "poetry.lock")
	if lockPath == "" {
		return nil, ""
	}
	return tomlPackageLockVersions(contents[lockPath]), lockPath
}

func parsePythonDeclarations(
	filePath string,
	content []byte,
	lockVersions map[string][]string,
	lockPath string,
) []DependencyDeclaration {
	base := strings.ToLower(path.Base(filePath))
	if strings.HasPrefix(base, "requirements") && strings.HasSuffix(base, ".txt") {
		return parseRequirementsDeclarations(filePath, content, lockVersions, lockPath)
	}
	return parsePyprojectDeclarations(content, lockVersions, lockPath)
}

func parseRequirementsDeclarations(
	filePath string,
	content []byte,
	lockVersions map[string][]string,
	lockPath string,
) []DependencyDeclaration {
	usage := "production"
	lowerPath := strings.ToLower(filePath)
	if strings.Contains(lowerPath, "test") {
		usage = "test"
	} else if strings.Contains(lowerPath, "dev") {
		usage = "development"
	}
	declarations := make([]DependencyDeclaration, 0)
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		name, declared, ok := parsePythonRequirement(scanner.Text())
		if !ok {
			continue
		}
		resolved := selectLockedVersion(lockedVersionsFor(lockVersions, name, true), declared)
		source := ""
		if resolved != "" {
			source = lockPath
		} else if exact, ok := pythonExactVersion(declared); ok {
			resolved = exact
			source = filePath
		}
		declarations = append(declarations, DependencyDeclaration{
			Ecosystem:        "pypi",
			Package:          name,
			Declared:         declared,
			Resolution:       pythonVersionResolution(declared),
			Resolved:         resolved,
			ResolutionSource: source,
			Usage:            usage,
			Relationship:     "required",
			DeclaredScope:    path.Base(filePath),
		})
	}
	return declarations
}

func parsePyprojectDeclarations(
	content []byte,
	lockVersions map[string][]string,
	lockPath string,
) []DependencyDeclaration {
	declarations := make([]DependencyDeclaration, 0)
	section := ""
	arrayUsage := ""
	arrayScope := ""
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(stripTOMLComment(scanner.Text()))
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(strings.Trim(line, "[] "))
			arrayUsage = ""
			continue
		}
		if arrayUsage != "" {
			declarations = appendPythonArrayRequirements(
				declarations, line, arrayUsage, arrayScope, lockVersions, lockPath,
			)
			if strings.Contains(line, "]") {
				arrayUsage = ""
			}
			continue
		}
		if section == "project" && strings.HasPrefix(strings.ToLower(line), "dependencies") {
			_, value, ok := strings.Cut(line, "=")
			if ok {
				arrayUsage, arrayScope = "production", "project.dependencies"
				declarations = appendPythonArrayRequirements(
					declarations, value, arrayUsage, arrayScope, lockVersions, lockPath,
				)
				if strings.Contains(value, "]") {
					arrayUsage = ""
				}
			}
			continue
		}
		if section == "project.optional-dependencies" && strings.Contains(line, "=") {
			group, value, _ := strings.Cut(line, "=")
			group = strings.Trim(strings.TrimSpace(group), `"'`)
			arrayUsage = pythonGroupUsage(group)
			arrayScope = "project.optional-dependencies." + group
			declarations = appendPythonArrayRequirements(
				declarations, value, arrayUsage, arrayScope, lockVersions, lockPath,
			)
			if strings.Contains(value, "]") {
				arrayUsage = ""
			}
			continue
		}
		if strings.HasPrefix(section, "tool.poetry") && strings.HasSuffix(section, ".dependencies") {
			name, value, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			name = strings.Trim(strings.TrimSpace(name), `"'`)
			if strings.EqualFold(name, "python") {
				continue
			}
			declared := strings.Trim(strings.TrimSpace(value), `"'`)
			if strings.HasPrefix(strings.TrimSpace(value), "{") {
				fields := make(map[string]string)
				for _, match := range catalogInlineField.FindAllStringSubmatch(value, -1) {
					fields[strings.ToLower(match[1])] = strings.TrimSpace(match[2])
				}
				declared = fields["version"]
			}
			usage := "production"
			if strings.Contains(section, ".group.") {
				group := strings.Split(section, ".group.")[1]
				group = strings.TrimSuffix(group, ".dependencies")
				usage = pythonGroupUsage(group)
			}
			declarations = appendPythonDeclaration(
				declarations, name, declared, usage, section, lockVersions, lockPath,
			)
		}
	}
	slices.SortFunc(declarations, func(left, right DependencyDeclaration) int {
		return strings.Compare(strings.ToLower(left.Package+"\x00"+left.DeclaredScope),
			strings.ToLower(right.Package+"\x00"+right.DeclaredScope))
	})
	return declarations
}

func parsePythonRequirement(value string) (string, string, bool) {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "#") || strings.HasPrefix(value, "-") {
		return "", "", false
	}
	if before, _, ok := strings.Cut(value, " ;"); ok {
		value = strings.TrimSpace(before)
	}
	if index := strings.Index(value, " #"); index >= 0 {
		value = strings.TrimSpace(value[:index])
	}
	match := pythonRequirementPattern.FindStringSubmatch(value)
	if len(match) != 3 {
		return "", "", false
	}
	return match[1], strings.TrimSpace(match[2]), true
}

func appendPythonArrayRequirements(
	declarations []DependencyDeclaration,
	line, usage, scope string,
	lockVersions map[string][]string,
	lockPath string,
) []DependencyDeclaration {
	for _, match := range pythonQuotedPattern.FindAllStringSubmatch(line, -1) {
		name, declared, ok := parsePythonRequirement(match[1])
		if ok {
			declarations = appendPythonDeclaration(
				declarations, name, declared, usage, scope, lockVersions, lockPath,
			)
		}
	}
	return declarations
}

func appendPythonDeclaration(
	declarations []DependencyDeclaration,
	name, declared, usage, scope string,
	lockVersions map[string][]string,
	lockPath string,
) []DependencyDeclaration {
	resolved := selectLockedVersion(lockedVersionsFor(lockVersions, name, true), declared)
	source := ""
	if resolved != "" {
		source = lockPath
	}
	relationship := "required"
	if strings.HasPrefix(scope, "project.optional-dependencies.") {
		relationship = "optional"
	}
	return append(declarations, DependencyDeclaration{
		Ecosystem:        "pypi",
		Package:          name,
		Declared:         declared,
		Resolution:       pythonVersionResolution(declared),
		Resolved:         resolved,
		ResolutionSource: source,
		Usage:            usage,
		Relationship:     relationship,
		DeclaredScope:    scope,
	})
}

func pythonGroupUsage(group string) string {
	lower := strings.ToLower(group)
	if strings.Contains(lower, "test") {
		return "test"
	}
	return "development"
}

func pythonExactVersion(declared string) (string, bool) {
	declared = strings.TrimSpace(declared)
	for _, prefix := range []string{"===", "=="} {
		if strings.HasPrefix(declared, prefix) {
			version := strings.TrimSpace(strings.TrimPrefix(declared, prefix))
			return version, version != "" && !strings.Contains(version, "*")
		}
	}
	return "", false
}

func pythonVersionResolution(declared string) string {
	if _, ok := pythonExactVersion(declared); ok {
		return "exact"
	}
	return versionResolution(declared)
}
