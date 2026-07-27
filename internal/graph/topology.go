package graph

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/spolnik/RepoKarta/internal/catalog"
	"go.yaml.in/yaml/v4"
)

const topologyEvidenceLimit = 12

var (
	topologyURLPattern = regexp.MustCompile(
		`(?i)\b(?:https?|lb)://[a-z0-9][a-z0-9._-]*(?::[0-9]{2,5})?`,
	)
	topologyDatabaseURLPattern = regexp.MustCompile(
		`(?i)\b(postgres(?:ql)?|mysql|mongodb(?:\+srv)?|redis|rediss|cassandra|neo4j|sqlserver)://[^\s"'` + "`" + `]+`,
	)
	topologyGRPCTargetPattern = regexp.MustCompile(
		`(?i)(?:grpc\.(?:dial|newclient)|foraddress|grpc_target|grpc[-_. ]?endpoint|grpc[-_. ]?host)[^"'` + "`" + `\n]{0,100}["'` + "`" + `]([a-z0-9][a-z0-9._-]*(?::[0-9]{2,5})?)`,
	)
	topologyKafkaTopicPattern = regexp.MustCompile(
		`(?i)(?:topic(?:s)?|destination)\s*(?:=|:|\()\s*["'` + "`" + `]([a-z0-9][a-z0-9._-]{1,126})`,
	)
	topologyKafkaCallPattern = regexp.MustCompile(
		`(?i)(?:subscribe|consumer|listener|consume|receive|producerrecord|publish|produce|send)\s*\(\s*["'` + "`" + `]([a-z0-9][a-z0-9._-]{1,126})`,
	)
	topologyMCPServerIndicator = regexp.MustCompile(
		`(?i)(?:@modelcontextprotocol/sdk|mcpserver|mcp\.newserver|servercapabilities|streamablehttpservertransport|handlefunc\(\s*["'` + "`" + `](?:post )?/mcp)`,
	)
)

// addDistributedTopology extracts service/resource interactions from committed
// source and deployment configuration without executing the repository. It is
// intentionally separate from package dependency extraction: package imports
// do not become distributed-system edges.
func (b *builder) addDistributedTopology(
	repository catalog.Repository,
	revision, evidencePath string,
	contents map[string][]byte,
) {
	defaultID := fmt.Sprintf("system:%d", repository.ID)
	defaultEvidence := b.evidence(repository, revision, evidencePath, 1, repository.Name)
	defaultComponent := SystemComponent{
		ID: defaultID, Name: repository.Name, Kind: "service",
		RepositoryID: repository.ID, Repository: repository.Name,
		Path: ".", Aliases: []string{repository.Name},
		Evidence: []Evidence{defaultEvidence},
	}
	for filePath, content := range contents {
		if !isTopologyTestArtifact(filePath) && isServiceConfiguration(filePath) {
			if name := springApplicationName(filePath, content); name != "" {
				defaultComponent.Aliases = append(defaultComponent.Aliases, name)
			}
		}
		if !isTopologyTestArtifact(filePath) && topologyMCPServerIndicator.Match(content) {
			defaultComponent.Capabilities = append(defaultComponent.Capabilities, "mcp_server")
		}
	}
	b.addSystemComponent(defaultComponent)

	componentRoots := map[string]string{".": defaultID}
	for filePath, content := range contents {
		if isTopologyTestArtifact(filePath) || !isServiceConfiguration(filePath) {
			continue
		}
		name := springApplicationName(filePath, content)
		if name == "" {
			continue
		}
		root := topologyComponentRoot(filePath, contents)
		if root == "." {
			continue
		}
		componentID := fmt.Sprintf(
			"system:%d:%s:%s", repository.ID, normalizeID(root), normalizeID(name),
		)
		evidence := b.evidence(
			repository, revision, filePath, lineContaining(content, name), name,
		)
		b.addSystemComponent(SystemComponent{
			ID: componentID, Name: name, Kind: "service",
			Technology: "Spring", RepositoryID: repository.ID,
			Repository: repository.Name, Path: root,
			Aliases: []string{name, path.Base(root)}, Evidence: []Evidence{evidence},
		})
		componentRoots[root] = componentID
	}

	explicitRefs := b.addExplicitTopology(repository, revision, contents, defaultID)
	for root, componentID := range explicitRefs {
		componentRoots[root] = componentID
	}
	b.addBackstageTopology(repository, revision, contents, defaultID)
	b.addComposeTopology(repository, revision, contents, defaultID)

	paths := make([]string, 0, len(contents))
	for filePath := range contents {
		if isTopologyTestArtifact(filePath) {
			continue
		}
		if isAnalyzedSource(filePath) || isServiceConfiguration(filePath) || isTopologyFile(filePath) {
			paths = append(paths, filePath)
		}
	}
	sort.Strings(paths)
	for _, filePath := range paths {
		content := contents[filePath]
		sourceID := topologySourceComponent(filePath, componentRoots, defaultID)
		b.addMCPConfiguration(repository, revision, filePath, content, sourceID)
		b.addDetectedConnections(repository, revision, filePath, content, sourceID)
	}
}

func (b *builder) addDetectedConnections(
	repository catalog.Repository,
	revision, filePath string,
	content []byte,
	sourceID string,
) {
	isMCPConfig := strings.Contains(strings.ToLower(path.Base(filePath)), "mcp") &&
		bytes.Contains(bytes.ToLower(content), []byte("mcpservers"))
	kafkaSource := isKafkaSource(filePath, content)
	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(make([]byte, 0, 64*1024), maximumSourceFileSize)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") ||
			strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "*") {
			continue
		}
		lower := strings.ToLower(line)
		confidence := sourceConfidence(filePath)

		for _, match := range topologyDatabaseURLPattern.FindAllStringSubmatch(line, -1) {
			parsed, err := url.Parse(match[0])
			if err != nil || parsed.Scheme == "" {
				continue
			}
			name := strings.Trim(strings.TrimSpace(parsed.Path), "/")
			if name == "" {
				name = parsed.Hostname()
			}
			if name == "" {
				name = strings.ToLower(match[1])
			}
			target := b.externalSystemComponent(
				"database", name, strings.ToLower(match[1]), []string{name, parsed.Hostname()},
			)
			b.addSystemConnection(SystemConnection{
				Source: sourceID, Target: target, Protocol: "database",
				Interaction: "reads_writes", Transport: strings.ToLower(match[1]),
				Confidence: confidence, EvidenceOrigin: "static",
				Evidence: []Evidence{b.evidence(repository, revision, filePath, lineNumber, match[0])},
			})
		}

		for _, match := range topologyGRPCTargetPattern.FindAllStringSubmatch(line, -1) {
			targetName := topologyPeerName(match[1])
			if targetName == "" {
				continue
			}
			target := b.externalSystemComponent("service", targetName, "gRPC", []string{targetName})
			b.addSystemConnection(SystemConnection{
				Source: sourceID, Target: target, Protocol: "grpc",
				Interaction: "calls", Transport: "grpc",
				Confidence: confidence, EvidenceOrigin: "static",
				Evidence: []Evidence{b.evidence(repository, revision, filePath, lineNumber, match[1])},
			})
		}

		if kafkaSource {
			topic, interaction := kafkaInteraction(line, lower)
			if topic != "" {
				topicID := b.externalSystemComponent("queue", topic, "Kafka", []string{topic})
				connection := SystemConnection{
					Protocol: "kafka", Interaction: interaction, Transport: "kafka",
					Confidence: confidence, EvidenceOrigin: "static",
					Evidence: []Evidence{b.evidence(repository, revision, filePath, lineNumber, topic)},
				}
				if interaction == "consumes" {
					connection.Source, connection.Target = topicID, sourceID
					connection.TargetResolved = true
				} else {
					connection.Source, connection.Target = sourceID, topicID
				}
				b.addSystemConnection(connection)
			}
		}

		if isMCPConfig || !httpClientLine(lower, filePath) {
			continue
		}
		for _, raw := range topologyURLPattern.FindAllString(line, -1) {
			parsed, err := url.Parse(raw)
			if err != nil || parsed.Hostname() == "" {
				continue
			}
			host := parsed.Hostname()
			if isLoopbackHost(host) {
				continue
			}
			targetName := topologyPeerName(host)
			if targetName == "" || infrastructureHosts[targetName] {
				continue
			}
			target := b.externalSystemComponent(
				"service", targetName, "HTTP", []string{targetName, host},
			)
			b.addSystemConnection(SystemConnection{
				Source: sourceID, Target: target, Protocol: "http",
				Interaction: "calls", Transport: strings.ToLower(parsed.Scheme),
				Confidence: confidence, EvidenceOrigin: "static",
				Evidence: []Evidence{b.evidence(repository, revision, filePath, lineNumber, raw)},
			})
		}
	}
}

func isKafkaSource(filePath string, content []byte) bool {
	normalizedPath := strings.ToLower(strings.ReplaceAll(filePath, "\\", "/"))
	if strings.Contains(path.Base(normalizedPath), "kafka") {
		return true
	}
	lower := strings.ToLower(string(content))
	for _, marker := range []string{
		"@kafkalistener", "kafkatemplate", "producerrecord",
		"kafkaconsumer", "kafkaproducer", "kafkajs",
		"confluent_kafka", "confluent-kafka", "segmentio/kafka",
		"twmb/franz-go", "shopify/sarama", "ibm/sarama", "librdkafka",
		"spring.kafka.", "bootstrap.servers", "kafka.bootstrap",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func isTopologyTestArtifact(filePath string) bool {
	normalized := "/" + strings.ToLower(strings.ReplaceAll(filePath, "\\", "/"))
	for _, marker := range []string{
		"/src/test/", "/src/integrationtest/", "/src/integration-test/",
		"/src/inttest/", "/src/e2etest/", "/src/testfixtures/",
		"/test/", "/tests/", "/testdata/", "/fixtures/", "/__tests__/", "/mocks/",
	} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	base := strings.ToLower(path.Base(filePath))
	if strings.HasSuffix(base, "_test.go") || strings.HasPrefix(base, "test_") {
		return true
	}
	for _, marker := range []string{".test.", ".tests.", ".spec."} {
		if strings.Contains(base, marker) {
			return true
		}
	}
	for _, suffix := range []string{
		"test.java", "tests.java", "test.kt", "tests.kt",
		"it.java", "it.kt", "spec.java", "spec.kt",
	} {
		if strings.HasSuffix(base, suffix) {
			return true
		}
	}
	return false
}

func kafkaInteraction(line, lower string) (string, string) {
	match := topologyKafkaCallPattern.FindStringSubmatch(line)
	if len(match) != 2 {
		match = topologyKafkaTopicPattern.FindStringSubmatch(line)
	}
	if len(match) != 2 {
		return "", ""
	}
	switch {
	case strings.Contains(lower, "listener"),
		strings.Contains(lower, "consumer"),
		strings.Contains(lower, "subscribe"),
		strings.Contains(lower, "consume"),
		strings.Contains(lower, "receive"):
		return match[1], "consumes"
	case strings.Contains(lower, "producer"),
		strings.Contains(lower, "publish"),
		strings.Contains(lower, "produce"),
		strings.Contains(lower, "send"):
		return match[1], "publishes"
	default:
		return "", ""
	}
}

func httpClientLine(lower, filePath string) bool {
	if isServiceConfiguration(filePath) || isTopologyFile(filePath) {
		for _, marker := range []string{
			"url", "uri", "endpoint", "base-url", "baseurl", "host", "upstream",
		} {
			if strings.Contains(lower, marker) {
				return true
			}
		}
	}
	for _, marker := range []string{
		"http.get", "http.post", "http.newrequest", "fetch(", "axios",
		"webclient", "restclient", "resttemplate", "feignclient", "new url(",
		"requests.get", "requests.post", "httpclient", "client.do(",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func topologyPeerName(value string) string {
	value = strings.TrimSpace(value)
	host, _, err := net.SplitHostPort(value)
	if err == nil {
		value = host
	}
	return normalizeServiceName(value)
}

func isLoopbackHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func topologyComponentRoot(filePath string, contents map[string][]byte) string {
	directory := path.Dir(filePath)
	if strings.HasPrefix(directory, "src/") || directory == "src" {
		directory = "."
	} else if index := strings.Index(directory, "/src/"); index >= 0 {
		directory = directory[:index]
	} else if strings.HasSuffix(directory, "/src") {
		directory = strings.TrimSuffix(directory, "/src")
	}
	for current := directory; current != "." && current != "/"; current = path.Dir(current) {
		for _, manifest := range []string{
			"build.gradle", "build.gradle.kts", "pom.xml", "go.mod", "package.json",
			"pyproject.toml", "Cargo.toml",
		} {
			if _, ok := contents[path.Join(current, manifest)]; ok {
				return current
			}
		}
	}
	if directory == "" || directory == "." {
		return "."
	}
	return directory
}

func topologySourceComponent(filePath string, roots map[string]string, fallback string) string {
	selectedRoot, selected := "", fallback
	for root, componentID := range roots {
		if root == "." {
			continue
		}
		if (filePath == root || strings.HasPrefix(filePath, root+"/")) && len(root) > len(selectedRoot) {
			selectedRoot, selected = root, componentID
		}
	}
	return selected
}

func isTopologyFile(filePath string) bool {
	base := strings.ToLower(path.Base(filePath))
	if base == ".repokarta.yml" || base == ".repokarta.yaml" ||
		base == ".mcp.json" || base == "mcp.json" ||
		base == "catalog-info.yaml" || base == "catalog-info.yml" ||
		base == "docker-compose.yml" || base == "docker-compose.yaml" ||
		base == "compose.yml" || base == "compose.yaml" {
		return true
	}
	if strings.HasPrefix(base, "docker-compose.") || strings.HasPrefix(base, "compose.") {
		return strings.HasSuffix(base, ".yml") || strings.HasSuffix(base, ".yaml")
	}
	normalized := strings.ToLower(strings.ReplaceAll(filePath, "\\", "/"))
	if strings.Contains(normalized, "/.mcp/") ||
		strings.Contains(normalized, "/k8s/") ||
		strings.Contains(normalized, "/kubernetes/") ||
		strings.Contains(normalized, "/deploy/") {
		switch path.Ext(base) {
		case ".json", ".yaml", ".yml", ".toml", ".properties":
			return true
		}
	}
	return false
}

func (b *builder) addSystemComponent(component SystemComponent) {
	component.Aliases = normalizedAliases(component.Name, component.Aliases...)
	component.Capabilities = uniqueSorted(component.Capabilities)
	if existing, ok := b.components[component.ID]; ok {
		existing.Aliases = normalizedAliases(existing.Name, append(existing.Aliases, component.Aliases...)...)
		existing.Capabilities = uniqueSorted(append(existing.Capabilities, component.Capabilities...))
		existing.Evidence = appendUniqueEvidence(existing.Evidence, component.Evidence...)
		if existing.Technology == "" {
			existing.Technology = component.Technology
		}
		b.components[component.ID] = existing
		return
	}
	b.components[component.ID] = component
}

func normalizedAliases(name string, values ...string) []string {
	values = append(values, name)
	output := make([]string, 0, len(values)*2)
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		output = append(output, value)
		if normalized := normalizeServiceName(value); normalized != value {
			output = append(output, normalized)
		}
	}
	return uniqueSorted(output)
}

func (b *builder) externalSystemComponent(kind, name, technology string, aliases []string) string {
	name = strings.TrimSpace(name)
	id := "external:" + kind + ":" + normalizeID(name)
	b.addSystemComponent(SystemComponent{
		ID: id, Name: name, Kind: kind, Technology: technology,
		Aliases: aliases, External: true,
	})
	return id
}

func (b *builder) addSystemConnection(connection SystemConnection) {
	if connection.Source == "" || connection.Target == "" || connection.Source == connection.Target {
		return
	}
	if connection.Confidence == "" {
		connection.Confidence = "medium"
	}
	if connection.EvidenceOrigin == "" {
		connection.EvidenceOrigin = "static"
	}
	connection.Evidence = connection.Evidence[:min(len(connection.Evidence), topologyEvidenceLimit)]
	connection.ID = systemConnectionID(connection)
	if existing, ok := b.connections[connection.ID]; ok {
		existing.Evidence = appendUniqueEvidence(existing.Evidence, connection.Evidence...)
		existing.Evidence = existing.Evidence[:min(len(existing.Evidence), topologyEvidenceLimit)]
		if confidenceRank(connection.Confidence) > confidenceRank(existing.Confidence) {
			existing.Confidence = connection.Confidence
		}
		existing.TargetResolved = existing.TargetResolved || connection.TargetResolved
		b.connections[connection.ID] = existing
		return
	}
	b.connections[connection.ID] = connection
}

func systemConnectionID(connection SystemConnection) string {
	return "connection:" + normalizeID(strings.Join([]string{
		connection.Source, connection.Target, connection.Protocol,
		connection.Interaction, connection.Transport, connection.EvidenceOrigin,
	}, ":"))
}

func confidenceRank(value string) int {
	switch value {
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	default:
		return 0
	}
}

// resolveSystemConnections reconciles inferred external peers with known
// components by aliases. Ambiguous aliases deliberately stay unresolved.
func (b *builder) resolveSystemConnections() {
	aliases := make(map[string][]string)
	for _, component := range b.components {
		if component.External {
			continue
		}
		for _, alias := range normalizedAliases(component.Name, component.Aliases...) {
			aliases[alias] = append(aliases[alias], component.ID)
		}
	}
	resolved := make(map[string]SystemConnection)
	for _, connection := range b.connections {
		target := b.components[connection.Target]
		if target.External {
			candidates := make(map[string]bool)
			for _, alias := range normalizedAliases(target.Name, target.Aliases...) {
				for _, componentID := range aliases[alias] {
					candidate := b.components[componentID]
					if componentID != connection.Source &&
						topologyKindsCanResolve(target, candidate) {
						candidates[componentID] = true
					}
				}
			}
			if len(candidates) == 1 {
				for componentID := range candidates {
					connection.Target = componentID
					connection.TargetResolved = true
				}
			}
		} else {
			connection.TargetResolved = true
		}
		connection.ID = systemConnectionID(connection)
		if existing, ok := resolved[connection.ID]; ok {
			existing.Evidence = appendUniqueEvidence(existing.Evidence, connection.Evidence...)
			existing.TargetResolved = existing.TargetResolved || connection.TargetResolved
			if confidenceRank(connection.Confidence) > confidenceRank(existing.Confidence) {
				existing.Confidence = connection.Confidence
			}
			resolved[connection.ID] = existing
		} else {
			resolved[connection.ID] = connection
		}
	}
	b.connections = resolved
	used := make(map[string]bool)
	for _, connection := range b.connections {
		used[connection.Source], used[connection.Target] = true, true
	}
	for id, component := range b.components {
		if component.External && !used[id] {
			delete(b.components, id)
		}
	}
}

func topologyKindsCanResolve(external, internal SystemComponent) bool {
	switch external.Kind {
	case "service", "external_service":
		return internal.Kind == "service"
	case "mcp_server":
		return internal.Kind == "mcp_server" || slices.Contains(internal.Capabilities, "mcp_server")
	default:
		return internal.Kind == external.Kind
	}
}

type explicitTopologyFile struct {
	Topology struct {
		Components  []explicitTopologyComponent  `yaml:"components"`
		Connections []explicitTopologyConnection `yaml:"connections"`
	} `yaml:"topology"`
}

type explicitTopologyComponent struct {
	ID           string   `yaml:"id"`
	Name         string   `yaml:"name"`
	Kind         string   `yaml:"kind"`
	Technology   string   `yaml:"technology"`
	Path         string   `yaml:"path"`
	Aliases      []string `yaml:"aliases"`
	Capabilities []string `yaml:"capabilities"`
}

type explicitTopologyConnection struct {
	Source      string `yaml:"source"`
	Target      string `yaml:"target"`
	Protocol    string `yaml:"protocol"`
	Interaction string `yaml:"interaction"`
	Transport   string `yaml:"transport"`
	Confidence  string `yaml:"confidence"`
}

func (b *builder) addExplicitTopology(
	repository catalog.Repository,
	revision string,
	contents map[string][]byte,
	defaultID string,
) map[string]string {
	roots := make(map[string]string)
	for _, filePath := range []string{".repokarta.yml", ".repokarta.yaml"} {
		content, ok := contents[filePath]
		if !ok {
			continue
		}
		var document explicitTopologyFile
		if yaml.Unmarshal(content, &document) != nil {
			continue
		}
		refs := map[string]string{"repository": defaultID}
		for _, declared := range document.Topology.Components {
			name := strings.TrimSpace(declared.Name)
			if name == "" {
				continue
			}
			reference := firstNonEmpty(strings.TrimSpace(declared.ID), name)
			componentID := fmt.Sprintf(
				"system:%d:declared:%s", repository.ID, normalizeID(reference),
			)
			componentPath := strings.Trim(strings.TrimSpace(declared.Path), "/")
			if componentPath == "" {
				componentPath = "."
			}
			kind := strings.ToLower(strings.TrimSpace(declared.Kind))
			if kind == "" {
				kind = "service"
			}
			evidence := b.evidence(
				repository, revision, filePath, lineContaining(content, name), name,
			)
			b.addSystemComponent(SystemComponent{
				ID: componentID, Name: name, Kind: kind, Technology: declared.Technology,
				RepositoryID: repository.ID, Repository: repository.Name,
				Path: componentPath, Aliases: declared.Aliases,
				Capabilities: declared.Capabilities, Evidence: []Evidence{evidence},
			})
			refs[reference], refs[name] = componentID, componentID
			roots[componentPath] = componentID
		}
		for _, declared := range document.Topology.Connections {
			source := refs[declared.Source]
			if source == "" {
				source = defaultID
			}
			target := refs[declared.Target]
			targetResolved := target != ""
			if target == "" {
				target = b.externalSystemComponent(
					explicitTargetKind(declared.Protocol), declared.Target,
					declared.Transport, []string{declared.Target},
				)
			}
			confidence := strings.ToLower(strings.TrimSpace(declared.Confidence))
			if confidence == "" {
				confidence = "high"
			}
			b.addSystemConnection(SystemConnection{
				Source: source, Target: target,
				Protocol:    strings.ToLower(strings.TrimSpace(declared.Protocol)),
				Interaction: strings.ToLower(strings.TrimSpace(declared.Interaction)),
				Transport:   declared.Transport, Confidence: confidence,
				EvidenceOrigin: "declared", TargetResolved: targetResolved,
				Evidence: []Evidence{b.evidence(
					repository, revision, filePath,
					lineContaining(content, declared.Target), declared.Target,
				)},
			})
		}
	}
	return roots
}

func explicitTargetKind(protocol string) string {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "database":
		return "database"
	case "kafka", "amqp", "messaging":
		return "queue"
	case "mcp":
		return "mcp_server"
	default:
		return "service"
	}
}

func (b *builder) addMCPConfiguration(
	repository catalog.Repository,
	revision, filePath string,
	content []byte,
	sourceID string,
) {
	if !strings.Contains(strings.ToLower(string(content)), "mcp") {
		return
	}
	var document any
	if json.Unmarshal(content, &document) != nil {
		return
	}
	servers := findMCPServers(document)
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		config, _ := servers[name].(map[string]any)
		transport := "stdio"
		technology := "MCP server"
		aliases := []string{name}
		if rawURL, _ := config["url"].(string); rawURL != "" {
			transport = "streamable_http"
			technology = "Remote MCP server"
			if parsed, err := url.Parse(rawURL); err == nil && parsed.Hostname() != "" {
				aliases = append(aliases, parsed.Hostname())
			}
		}
		target := b.externalSystemComponent("mcp_server", name, technology, aliases)
		b.addSystemConnection(SystemConnection{
			Source: sourceID, Target: target, Protocol: "mcp",
			Interaction: "invokes", Transport: transport,
			Confidence: "high", EvidenceOrigin: "static",
			Evidence: []Evidence{b.evidence(
				repository, revision, filePath, lineContaining(content, name), name,
			)},
		})
	}
}

func findMCPServers(value any) map[string]any {
	output := make(map[string]any)
	var visit func(any)
	visit = func(current any) {
		switch typed := current.(type) {
		case map[string]any:
			for key, child := range typed {
				normalized := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(key))
				if normalized == "mcpservers" {
					if servers, ok := child.(map[string]any); ok {
						for name, config := range servers {
							output[name] = config
						}
					}
					continue
				}
				visit(child)
			}
		case []any:
			for _, child := range typed {
				visit(child)
			}
		}
	}
	visit(value)
	return output
}

func (b *builder) addBackstageTopology(
	repository catalog.Repository,
	revision string,
	contents map[string][]byte,
	defaultID string,
) {
	for filePath, content := range contents {
		base := strings.ToLower(path.Base(filePath))
		if base != "catalog-info.yaml" && base != "catalog-info.yml" {
			continue
		}
		var document struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
			Spec struct {
				DependsOn    []string `yaml:"dependsOn"`
				ConsumesAPIs []string `yaml:"consumesApis"`
			} `yaml:"spec"`
		}
		if yaml.Unmarshal(content, &document) != nil ||
			!strings.EqualFold(document.Kind, "Component") {
			continue
		}
		source := defaultID
		if document.Metadata.Name != "" {
			component := b.components[defaultID]
			component.Aliases = append(component.Aliases, document.Metadata.Name)
			component.Evidence = appendUniqueEvidence(component.Evidence, b.evidence(
				repository, revision, filePath,
				lineContaining(content, document.Metadata.Name), document.Metadata.Name,
			))
			b.components[defaultID] = component
		}
		for _, dependency := range append(document.Spec.DependsOn, document.Spec.ConsumesAPIs...) {
			name := backstageReferenceName(dependency)
			if name == "" {
				continue
			}
			target := b.externalSystemComponent("service", name, "Catalog entity", []string{name})
			b.addSystemConnection(SystemConnection{
				Source: source, Target: target, Protocol: "unknown",
				Interaction: "depends_on", Confidence: "high",
				EvidenceOrigin: "declared",
				Evidence: []Evidence{b.evidence(
					repository, revision, filePath, lineContaining(content, dependency), dependency,
				)},
			})
		}
	}
}

func backstageReferenceName(reference string) string {
	reference = strings.TrimSpace(reference)
	if _, remainder, ok := strings.Cut(reference, ":"); ok {
		reference = remainder
	}
	if _, name, ok := strings.Cut(reference, "/"); ok {
		reference = name
	}
	return normalizeServiceName(reference)
}

func (b *builder) addComposeTopology(
	repository catalog.Repository,
	revision string,
	contents map[string][]byte,
	defaultID string,
) {
	for filePath, content := range contents {
		base := strings.ToLower(path.Base(filePath))
		if !strings.Contains(base, "compose") ||
			(!strings.HasSuffix(base, ".yml") && !strings.HasSuffix(base, ".yaml")) {
			continue
		}
		var document struct {
			Services map[string]struct {
				DependsOn any `yaml:"depends_on"`
			} `yaml:"services"`
		}
		if yaml.Unmarshal(content, &document) != nil {
			continue
		}
		refs := make(map[string]string)
		for name := range document.Services {
			id := fmt.Sprintf("system:%d:compose:%s", repository.ID, normalizeID(name))
			b.addSystemComponent(SystemComponent{
				ID: id, Name: name, Kind: "service", Technology: "Docker Compose",
				RepositoryID: repository.ID, Repository: repository.Name,
				Path: path.Dir(filePath), Aliases: []string{name},
				Evidence: []Evidence{b.evidence(
					repository, revision, filePath, lineContaining(content, name+":"), name,
				)},
			})
			refs[name] = id
		}
		for name, service := range document.Services {
			source := refs[name]
			for _, dependency := range composeDependencies(service.DependsOn) {
				target := refs[dependency]
				resolved := target != ""
				if target == "" {
					target = b.externalSystemComponent(
						"service", dependency, "Docker Compose service", []string{dependency},
					)
				}
				b.addSystemConnection(SystemConnection{
					Source: source, Target: target, Protocol: "unknown",
					Interaction: "depends_on", Confidence: "high",
					EvidenceOrigin: "declared", TargetResolved: resolved,
					Evidence: []Evidence{b.evidence(
						repository, revision, filePath,
						lineContaining(content, dependency), dependency,
					)},
				})
			}
		}
		if len(document.Services) == 1 {
			for _, id := range refs {
				component := b.components[defaultID]
				component.Aliases = append(component.Aliases, b.components[id].Aliases...)
				b.components[defaultID] = component
			}
		}
	}
}

func composeDependencies(value any) []string {
	output := make([]string, 0)
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			if name, ok := item.(string); ok {
				output = append(output, name)
			}
		}
	case map[string]any:
		for name := range typed {
			output = append(output, name)
		}
	}
	slices.Sort(output)
	return output
}
