package graph

import (
	"bufio"
	"bytes"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/spolnik/RepoKarta/internal/catalog"
)

func isInfrastructureHost(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimPrefix(value, "http://")
	value = strings.TrimPrefix(value, "https://")
	value = strings.TrimPrefix(value, "lb://")
	value = strings.SplitN(value, "/", 2)[0]
	value = strings.TrimSuffix(value, ".")
	value = strings.SplitN(value, ":", 2)[0]
	if infrastructureHosts[value] ||
		infrastructureHosts[NormalizeServiceName(value)] {
		return true
	}
	for _, suffix := range infrastructureHostSuffixes {
		if strings.HasSuffix(value, suffix) {
			return true
		}
	}
	return false
}

func (b *builder) addJavaStructure(
	repository catalog.Repository,
	revision, repositoryNodeID string,
	contents map[string][]byte,
) {
	paths := make([]string, 0)
	for filePath := range contents {
		extension := strings.ToLower(path.Ext(filePath))
		if extension == ".java" || extension == ".kt" {
			paths = append(paths, filePath)
		}
	}
	slices.SortFunc(paths, func(left, right string) int {
		leftConfidence, rightConfidence := sourceConfidence(left), sourceConfidence(right)
		if leftConfidence != rightConfidence {
			if leftConfidence == "high" {
				return -1
			}
			return 1
		}
		return strings.Compare(left, right)
	})
	components, routeCount := 0, 0
	for _, filePath := range paths {
		// Documentation and license headers must not create routes or service
		// relationships, so annotations and URLs are read from code only.
		content := stripJavaComments(contents[filePath])
		routes := springRoutes(content)
		clientTargets := springClientTargets(content)
		if len(routes) == 0 && len(clientTargets) == 0 {
			continue
		}
		// Client targets are recorded before the component budget because
		// service edges land on the repository node, not on a component.
		for _, target := range clientTargets {
			b.clientReferences = append(b.clientReferences, clientReference{
				sourceRepositoryID: repository.ID,
				target:             target.name,
				confidence:         sourceConfidence(filePath),
				evidence: b.evidence(
					repository,
					revision,
					filePath,
					target.line,
					target.name,
				),
			})
		}
		// Bound the curated layers so a large Spring codebase stays a readable
		// map rather than a file-level hairball.
		if components >= maximumComponentsPerRepository {
			b.truncated = true
			continue
		}
		if routeCount+len(routes) > maximumRoutesPerRepository {
			b.truncated = true
			routes = routes[:max(0, maximumRoutesPerRepository-routeCount)]
		}
		if len(routes) == 0 && len(clientTargets) == 0 {
			continue
		}
		routeCount += len(routes)
		components++
		className := strings.TrimSuffix(path.Base(filePath), path.Ext(filePath))
		if match := javaClassPattern.FindSubmatch(content); len(match) == 2 {
			className = string(match[1])
		}
		componentEvidence := b.evidence(
			repository,
			revision,
			filePath,
			lineContaining(content, className),
			className,
		)
		componentID := fmt.Sprintf("component:%d:%s", repository.ID, normalizeID(filePath))
		b.addNode(Node{
			ID:           componentID,
			Kind:         "component",
			Label:        className,
			Subtitle:     filePath,
			Layer:        "Components",
			RepositoryID: repository.ID,
			Repository:   repository.Name,
			Path:         filePath,
			Evidence:     []Evidence{componentEvidence},
		})
		b.addEdge(Edge{
			ID:       edgeID(repositoryNodeID, componentID, "contains"),
			Source:   repositoryNodeID,
			Target:   componentID,
			Kind:     "contains",
			Label:    "contains",
			Evidence: []Evidence{componentEvidence},
		})
		for _, route := range routes {
			evidence := b.evidence(repository, revision, filePath, route.line, route.label)
			routeID := fmt.Sprintf(
				"route:%d:%s:%s",
				repository.ID,
				normalizeID(filePath),
				normalizeID(route.label),
			)
			b.addNode(Node{
				ID:           routeID,
				Kind:         "route",
				Label:        route.label,
				Subtitle:     className,
				Layer:        "Routes",
				RepositoryID: repository.ID,
				Repository:   repository.Name,
				Path:         filePath,
				Evidence:     []Evidence{evidence},
			})
			b.addEdge(Edge{
				ID:       edgeID(componentID, routeID, "serves"),
				Source:   componentID,
				Target:   routeID,
				Kind:     "route",
				Label:    "serves",
				Evidence: []Evidence{evidence},
			})
		}
	}
}

func springRoutes(content []byte) []springRoute {
	// Idempotent: callers may pass raw or already-stripped source. Annotations
	// named in documentation or commented out are not served routes.
	content = stripJavaComments(content)
	if declarativeClientType.Match(content) {
		return nil
	}
	classOffset := len(content)
	if match := javaClassPattern.FindIndex(content); match != nil {
		classOffset = match[0]
	}
	classPrefixes := []string{""}
	mappings := springMappingPattern.FindAllSubmatchIndex(content, -1)
	for _, match := range mappings {
		if match[0] >= classOffset || string(content[match[2]:match[3]]) != "RequestMapping" {
			continue
		}
		paths := annotationPaths(mappingArguments(content, match))
		if len(paths) > 0 {
			classPrefixes = paths
			break
		}
	}
	byLabel := make(map[string]springRoute)
	for _, match := range mappings {
		if match[0] < classOffset {
			continue
		}
		annotation := string(content[match[2]:match[3]])
		arguments := mappingArguments(content, match)
		paths := annotationPaths(arguments)
		if len(paths) == 0 {
			paths = []string{""}
		}
		method := strings.ToUpper(strings.TrimSuffix(annotation, "Mapping"))
		if annotation == "RequestMapping" {
			method = requestMappingMethod(arguments)
		}
		for _, classPrefix := range classPrefixes {
			for _, routePath := range paths {
				label := strings.TrimSpace(method + " " + joinRoutePath(classPrefix, routePath))
				byLabel[label] = springRoute{label: label, line: lineAtOffset(content, match[0])}
			}
		}
	}
	for _, match := range springFunctionalRoute.FindAllSubmatchIndex(content, -1) {
		method := strings.ToUpper(string(content[match[2]:match[3]]))
		routePath := string(content[match[4]:match[5]])
		label := method + " " + joinRoutePath("", routePath)
		byLabel[label] = springRoute{label: label, line: lineAtOffset(content, match[0])}
	}
	output := make([]springRoute, 0, len(byLabel))
	for _, route := range byLabel {
		output = append(output, route)
	}
	slices.SortFunc(output, func(left, right springRoute) int {
		return strings.Compare(left.label, right.label)
	})
	return output
}

func mappingArguments(content []byte, match []int) string {
	if len(match) < 6 || match[4] < 0 {
		return ""
	}
	return string(content[match[4]:match[5]])
}

func annotationPaths(arguments string) []string {
	matches := quotedJavaString.FindAllStringSubmatchIndex(arguments, -1)
	paths := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) < 4 {
			continue
		}
		prefixStart := max(0, match[0]-80)
		if nonPathMappingAttribute.MatchString(arguments[prefixStart:match[0]]) {
			continue
		}
		value := strings.TrimSpace(arguments[match[2]:match[3]])
		if value == "" || !strings.Contains(value, "=") {
			paths = append(paths, value)
		}
	}
	return uniqueSorted(paths)
}

func requestMappingMethod(arguments string) string {
	for _, method := range []string{"GET", "POST", "PUT", "DELETE", "PATCH"} {
		if strings.Contains(arguments, "RequestMethod."+method) {
			return method
		}
	}
	return "ANY"
}

func joinRoutePath(prefix, suffix string) string {
	joined := "/" + strings.Trim(strings.TrimSpace(prefix), "/")
	if trimmed := strings.Trim(strings.TrimSpace(suffix), "/"); trimmed != "" {
		joined += "/" + trimmed
	}
	joined = strings.ReplaceAll(joined, "//", "/")
	if joined == "" {
		return "/"
	}
	return joined
}

// springClientTargets reads outbound service names from Spring HTTP clients.
// Bare URLs are only harvested from files that actually construct a client
// (Feign, WebClient, RestClient, RestTemplate, or an HTTP interface proxy), so
// license headers and XML namespaces never become service relationships.
func springClientTargets(content []byte) []springClientTarget {
	// Idempotent, and it keeps license URLs in headers out of service calls
	// while preserving URLs written in string literals.
	content = stripJavaComments(content)
	byName := make(map[string]springClientTarget)
	for _, match := range feignClientPattern.FindAllSubmatchIndex(content, -1) {
		arguments := string(content[match[2]:match[3]])
		target := ""
		if named := feignNamedTarget.FindStringSubmatch(arguments); len(named) == 2 {
			target = named[1]
		} else if quoted := quotedJavaString.FindStringSubmatch(arguments); len(quoted) == 2 {
			target = quoted[1]
		}
		if isInfrastructureHost(target) {
			continue
		}
		if target = NormalizeServiceName(target); target != "" {
			byName[target] = springClientTarget{name: target, line: lineAtOffset(content, match[0])}
		}
	}
	if springClientIndicator.Match(content) {
		for _, match := range serviceURLPattern.FindAllSubmatchIndex(content, -1) {
			host := string(content[match[2]:match[3]])
			target := NormalizeServiceName(host)
			if target == "" || isInfrastructureHost(host) {
				continue
			}
			byName[target] = springClientTarget{
				name: target,
				line: lineAtOffset(content, match[0]),
			}
		}
	}
	output := make([]springClientTarget, 0, len(byName))
	for _, target := range byName {
		output = append(output, target)
	}
	slices.SortFunc(output, func(left, right springClientTarget) int {
		return strings.Compare(left.name, right.name)
	})
	return output
}

// NormalizeServiceName reduces a Feign service id, Spring application name, or
// URL host to its first DNS label so `inventory-service`,
// `inventory-service:8080`, and `inventory-service.default.svc.cluster.local`
// resolve to the same service.
func NormalizeServiceName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimPrefix(value, "http://")
	value = strings.TrimPrefix(value, "https://")
	value = strings.TrimPrefix(value, "lb://")
	value = strings.SplitN(value, "/", 2)[0]
	value = strings.SplitN(value, ":", 2)[0]
	value = strings.SplitN(value, ".", 2)[0]
	value = strings.NewReplacer("_", "-", " ", "-").Replace(value)
	return strings.Trim(value, "-")
}

// registerServiceTarget records one name that identifies this repository as a
// callable service.
func (b *builder) registerServiceTarget(name, repositoryNodeID string) {
	normalized := NormalizeServiceName(name)
	if normalized == "" || isInfrastructureHost(name) {
		return
	}
	b.serviceTargets[normalized] = repositoryNodeID
}

// addServiceIdentity registers the Spring application name so a Feign client
// naming a logical service resolves even when it differs from the directory
// name.
func (b *builder) addServiceIdentity(repositoryNodeID string, contents map[string][]byte) {
	paths := make([]string, 0)
	for filePath := range contents {
		if isServiceConfiguration(filePath) {
			paths = append(paths, filePath)
		}
	}
	sort.Strings(paths)
	for _, filePath := range paths {
		if name := springApplicationName(filePath, contents[filePath]); name != "" {
			b.registerServiceTarget(name, repositoryNodeID)
		}
	}
}

// addServiceConfigurationReferences reads outbound base URLs from Spring
// application configuration. RestTemplate and WebClient URLs are commonly
// injected from properties instead of appearing as literals in client code.
func (b *builder) addServiceConfigurationReferences(
	repository catalog.Repository,
	revision string,
	contents map[string][]byte,
) {
	paths := make([]string, 0)
	for filePath := range contents {
		if isServiceConfiguration(filePath) {
			paths = append(paths, filePath)
		}
	}
	slices.SortFunc(paths, func(left, right string) int {
		leftConfidence, rightConfidence := sourceConfidence(left), sourceConfidence(right)
		if leftConfidence != rightConfidence {
			if leftConfidence == "high" {
				return -1
			}
			return 1
		}
		return strings.Compare(left, right)
	})
	for _, filePath := range paths {
		for _, target := range serviceConfigurationTargets(contents[filePath]) {
			b.clientReferences = append(b.clientReferences, clientReference{
				sourceRepositoryID: repository.ID,
				target:             target.name,
				confidence:         sourceConfidence(filePath),
				evidence: b.evidence(
					repository,
					revision,
					filePath,
					target.line,
					target.name,
				),
			})
		}
	}
}

func serviceConfigurationTargets(content []byte) []springClientTarget {
	byName := make(map[string]springClientTarget)
	type configurationLevel struct {
		key    string
		indent int
	}
	stack := make([]configurationLevel, 0, 8)
	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(make([]byte, 0, 64*1024), maximumSourceFileSize)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		keyText := ""
		configuredValue := ""
		if match := yamlKeyValue.FindStringSubmatch(line); match != nil {
			indent := len(match[1])
			for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
				stack = stack[:len(stack)-1]
			}
			stack = append(stack, configurationLevel{
				key:    strings.ToLower(match[2]),
				indent: indent,
			})
			keys := make([]string, 0, len(stack))
			for _, level := range stack {
				keys = append(keys, level.key)
			}
			keyText = strings.Join(keys, ".")
			configuredValue = match[3]
		} else if key, value, ok := strings.Cut(line, "="); ok {
			keyText = strings.ToLower(strings.TrimSpace(key))
			configuredValue = value
		}
		lowerLine := strings.ToLower(line)
		urlOffset := strings.Index(lowerLine, "http")
		if lbOffset := strings.Index(lowerLine, "lb://"); urlOffset < 0 ||
			(lbOffset >= 0 && lbOffset < urlOffset) {
			urlOffset = lbOffset
		}
		if keyText == "" && urlOffset >= 0 {
			keyText = strings.ToLower(line[:urlOffset])
		}
		if !serviceConfigurationKey.MatchString(keyText) {
			continue
		}
		if strings.Contains(keyText, "docs") ||
			strings.Contains(keyText, "swagger") ||
			strings.Contains(keyText, "openapi") {
			continue
		}
		foundURL := false
		for _, match := range serviceURLPattern.FindAllStringSubmatch(line, -1) {
			host := match[1]
			target := NormalizeServiceName(host)
			if target == "" || isInfrastructureHost(host) {
				continue
			}
			foundURL = true
			byName[target] = springClientTarget{name: target, line: lineNumber}
		}
		if !foundURL {
			target := configuredServiceName(configuredValue)
			if target != "" && !isInfrastructureHost(configuredValue) {
				byName[target] = springClientTarget{name: target, line: lineNumber}
			}
		}
	}
	output := make([]springClientTarget, 0, len(byName))
	for _, target := range byName {
		output = append(output, target)
	}
	slices.SortFunc(output, func(left, right springClientTarget) int {
		return strings.Compare(left.name, right.name)
	})
	return output
}

func configuredServiceName(value string) string {
	value = strings.TrimSpace(strings.SplitN(value, "#", 2)[0])
	value = strings.Trim(value, `"'`)
	if strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}") {
		placeholder := strings.TrimSuffix(strings.TrimPrefix(value, "${"), "}")
		_, fallback, ok := strings.Cut(placeholder, ":")
		if !ok {
			return ""
		}
		value = fallback
	}
	if value == "" || strings.Contains(value, "$") ||
		strings.Contains(value, " ") || strings.Contains(value, "://") {
		return ""
	}
	candidate := strings.SplitN(value, "/", 2)[0]
	if !configuredServiceValue.MatchString(candidate) {
		return ""
	}
	return NormalizeServiceName(candidate)
}

// isServiceConfiguration reports whether a file can declare a Spring
// application name or outbound service URL.
func isServiceConfiguration(filePath string) bool {
	base := strings.ToLower(path.Base(filePath))
	switch base {
	case "application.yml", "application.yaml", "application.properties",
		"bootstrap.yml", "bootstrap.yaml", "bootstrap.properties":
		return true
	default:
		for _, prefix := range []string{"application-", "bootstrap-"} {
			if strings.HasPrefix(base, prefix) &&
				(strings.HasSuffix(base, ".yml") ||
					strings.HasSuffix(base, ".yaml") ||
					strings.HasSuffix(base, ".properties")) {
				return true
			}
		}
		return false
	}
}

func sourceConfidence(filePath string) string {
	normalized := "/" + strings.ToLower(strings.ReplaceAll(filePath, "\\", "/")) + "/"
	for _, marker := range []string{
		"/src/test/", "/src/integrationtest/", "/src/integration-test/",
		"/src/inttest/", "/src/e2etest/", "/src/testfixtures/",
	} {
		if strings.Contains(normalized, marker) {
			return "low"
		}
	}
	base := strings.ToLower(path.Base(filePath))
	for _, suffix := range []string{
		"test.java", "tests.java", "test.kt", "tests.kt",
		"it.java", "it.kt", "spec.java", "spec.kt",
	} {
		if strings.HasSuffix(base, suffix) {
			return "low"
		}
	}
	return "high"
}

// springApplicationName reads `spring.application.name` from either the flat
// property form or the nested YAML form.
func springApplicationName(filePath string, content []byte) string {
	if match := springApplicationNameProperty.FindSubmatch(content); len(match) == 2 {
		return string(match[1])
	}
	if strings.HasSuffix(filePath, ".properties") {
		return ""
	}
	type level struct {
		key    string
		indent int
	}
	stack := make([]level, 0, 8)
	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(make([]byte, 0, 64*1024), maximumSourceFileSize)
	for scanner.Scan() {
		line := strings.ReplaceAll(scanner.Text(), "\t", "  ")
		if trimmed := strings.TrimSpace(line); trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		match := yamlKeyValue.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		indent := len(match[1])
		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		stack = append(stack, level{key: strings.ToLower(match[2]), indent: indent})
		value := strings.Trim(strings.TrimSpace(match[3]), `"'`)
		if value == "" || len(stack) != 3 {
			continue
		}
		if stack[0].key == "spring" && stack[1].key == "application" && stack[2].key == "name" {
			return value
		}
	}
	return ""
}

// resolveClientReferences turns observed client targets into repository edges,
// but only when the target resolves to a discovered repository or Spring
// application name. Unresolved hosts and interpolated placeholders are dropped
// rather than invented as relationships.
func (b *builder) resolveClientReferences() {
	type resolvedReference struct {
		confidence string
		evidence   []Evidence
	}
	resolved := make(map[string]resolvedReference)
	for _, reference := range b.clientReferences {
		targetID := b.serviceTargets[NormalizeServiceName(reference.target)]
		sourceID := fmt.Sprintf("repository:%d", reference.sourceRepositoryID)
		if targetID == "" || targetID == sourceID {
			continue
		}
		key := sourceID + "\x00" + targetID
		current := resolved[key]
		if current.confidence == "high" && reference.confidence != "high" {
			continue
		}
		if reference.confidence == "high" && current.confidence != "high" {
			current = resolvedReference{confidence: "high"}
		}
		if current.confidence == "" {
			current.confidence = reference.confidence
		}
		current.evidence = appendUniqueEvidence(current.evidence, reference.evidence)
		resolved[key] = current
	}
	for key, reference := range resolved {
		sourceID, targetID, _ := strings.Cut(key, "\x00")
		label := "calls over HTTP"
		if reference.confidence == "low" {
			label += " (test-only)"
		}
		b.addEdge(Edge{
			ID:         edgeID(sourceID, targetID, "service-call"),
			Source:     sourceID,
			Target:     targetID,
			Kind:       "service_call",
			Label:      label,
			Confidence: reference.confidence,
			Evidence:   reference.evidence,
		})
	}
}
