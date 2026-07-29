package graph

import (
	"bufio"
	"bytes"
	"path"
	"slices"
	"strings"
)

func cargoLockVersions(
	contents map[string][]byte,
	manifestPath string,
) (map[string][]string, string) {
	lockPath := nearestDependencyFile(contents, path.Dir(manifestPath), "Cargo.lock")
	if lockPath == "" {
		return nil, ""
	}
	return tomlPackageLockVersions(contents[lockPath]), lockPath
}

func parseCargoDeclarations(
	content []byte,
	lockVersions map[string][]string,
	lockPath string,
) []DependencyDeclaration {
	declarations := make([]DependencyDeclaration, 0)
	section := ""
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(stripTOMLComment(scanner.Text()))
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(strings.Trim(line, "[] "))
			continue
		}
		usage := cargoSectionUsage(section)
		if usage == "" || line == "" {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		name = strings.Trim(strings.TrimSpace(name), `"'`)
		value = strings.TrimSpace(value)
		if name == "" || value == "" {
			continue
		}
		packageName := name
		declared := ""
		relationship := "required"
		switch {
		case strings.HasPrefix(value, "{"):
			fields := make(map[string]string)
			for _, match := range catalogInlineField.FindAllStringSubmatch(value, -1) {
				fields[strings.ToLower(match[1])] = strings.TrimSpace(match[2])
			}
			if fields["package"] != "" {
				packageName = fields["package"]
			}
			declared = fields["version"]
			if fields["git"] != "" {
				declared = "git+" + fields["git"]
			} else if fields["path"] != "" {
				declared = "file:" + fields["path"]
			}
			if cargoOptionalPattern.MatchString(value) {
				relationship = "optional"
			}
		case strings.HasPrefix(value, `"`), strings.HasPrefix(value, `'`):
			declared = strings.Trim(value, `"'`)
		default:
			continue
		}
		resolved := selectLockedVersion(lockVersions[packageName], declared)
		source := ""
		if resolved != "" {
			source = lockPath
		}
		declarations = append(declarations, DependencyDeclaration{
			Ecosystem:        "cargo",
			Package:          packageName,
			Declared:         declared,
			Resolution:       versionResolution(declared),
			Resolved:         resolved,
			ResolutionSource: source,
			Usage:            usage,
			Relationship:     relationship,
			DeclaredScope:    section,
		})
	}
	slices.SortFunc(declarations, func(left, right DependencyDeclaration) int {
		return strings.Compare(left.Package+"\x00"+left.DeclaredScope, right.Package+"\x00"+right.DeclaredScope)
	})
	return declarations
}

func cargoSectionUsage(section string) string {
	switch {
	case section == "dev-dependencies", strings.HasSuffix(section, ".dev-dependencies"):
		return "test"
	case section == "build-dependencies", strings.HasSuffix(section, ".build-dependencies"):
		return "build"
	case section == "dependencies", section == "workspace.dependencies",
		strings.HasSuffix(section, ".dependencies"):
		return "production"
	default:
		return ""
	}
}
