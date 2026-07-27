package graph

import (
	"bufio"
	"bytes"
	"net"
	"net/url"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/spolnik/RepoKarta/internal/catalog"
)

const (
	environmentAssignmentOther          = 1
	environmentAssignmentApplication    = 2
	environmentAssignmentInfrastructure = 3
)

var (
	environmentKeyValuePattern = regexp.MustCompile(
		`^\s*(?:-\s*)?(?:export\s+)?["']?([A-Z][A-Z0-9_]*)["']?\s*[:=]\s*(.+?)\s*$`,
	)
	environmentQuotedKeyValuePattern = regexp.MustCompile(
		`["']([A-Z][A-Z0-9_]*)["']\s*[:=]\s*["']([^"']+)["']`,
	)
	kubernetesEnvironmentNamePattern = regexp.MustCompile(
		`^\s*-\s*name\s*:\s*["']?([A-Z][A-Z0-9_]*)["']?\s*$`,
	)
	kubernetesEnvironmentValuePattern = regexp.MustCompile(
		`^\s*value\s*:\s*(.+?)\s*$`,
	)
	environmentRegionPattern = regexp.MustCompile(
		`^(?:[a-z]{2}(?:-gov)?-[a-z]+-\d|[a-z]{2,3}-[a-z]+-\d)$`,
	)
	environmentRegionSearchPattern = regexp.MustCompile(
		`(?:^|[^a-z0-9])([a-z]{2}(?:-gov)?-[a-z]+-\d)(?:$|[^a-z0-9])`,
	)
	environmentTokenPattern = regexp.MustCompile(`[^a-z0-9]+`)
)

func isEnvironmentAssignmentFile(filePath string) bool {
	return !isTopologyAssignmentExcluded(filePath) &&
		environmentAssignmentCandidateRank(filePath) > 0
}

func environmentAssignmentRank(filePath string) int {
	if !isEnvironmentAssignmentFile(filePath) {
		return 0
	}
	return environmentAssignmentCandidateRank(filePath)
}

func isPotentialEnvironmentAssignmentFile(filePath string) bool {
	return environmentAssignmentCandidateRank(filePath) > 0
}

func environmentAssignmentCandidateRank(filePath string) int {
	normalized := "/" + strings.ToLower(strings.ReplaceAll(filePath, "\\", "/"))
	base := strings.ToLower(path.Base(filePath))
	extension := strings.ToLower(path.Ext(base))
	infrastructurePath := false
	for _, marker := range []string{
		"/deploy/", "/deployment/", "/deployments/", "/k8s/", "/kubernetes/",
		"/helm/", "/charts/", "/terraform/", "/infra/", "/infrastructure/",
		"/iac/", "/overlays/", "/environments/", "/cdk/",
	} {
		if strings.Contains(normalized+"/", marker) {
			infrastructurePath = true
			break
		}
	}
	infrastructureBase := base == "docker-compose.yml" ||
		base == "docker-compose.yaml" || base == "compose.yml" ||
		base == "compose.yaml" || base == "cdk.json" ||
		strings.HasPrefix(base, "docker-compose.") ||
		strings.HasPrefix(base, "compose.") ||
		strings.HasPrefix(base, "values") ||
		strings.HasSuffix(base, ".tf") || strings.HasSuffix(base, ".tfvars") ||
		strings.Contains(base, "deployment") || strings.Contains(base, "manifest")
	if infrastructurePath || infrastructureBase {
		switch extension {
		case ".yaml", ".yml", ".json", ".toml", ".properties", ".env",
			".tf", ".tfvars", ".ts", ".js", ".mjs", ".cjs", ".py":
			return environmentAssignmentInfrastructure
		}
	}
	if isServiceConfiguration(filePath) {
		return environmentAssignmentApplication
	}
	configurationPath := strings.Contains(normalized+"/", "/config/") ||
		strings.Contains(normalized+"/", "/configuration/")
	if configurationPath {
		switch extension {
		case ".yaml", ".yml", ".json", ".toml", ".properties", ".env":
			return environmentAssignmentApplication
		}
	}
	if base == ".env" || strings.HasPrefix(base, ".env.") ||
		strings.HasSuffix(base, ".env") {
		return environmentAssignmentOther
	}
	switch base {
	case "config.yaml", "config.yml", "config.json", "config.toml",
		"settings.yaml", "settings.yml", "settings.json", "settings.toml",
		"appsettings.json":
		return environmentAssignmentOther
	default:
		if strings.HasPrefix(base, "appsettings.") && strings.HasSuffix(base, ".json") {
			return environmentAssignmentApplication
		}
		return 0
	}
}

func isTopologyAssignmentExcluded(filePath string) bool {
	if isTopologyTestArtifact(filePath) {
		return true
	}
	normalized := "/" + strings.ToLower(strings.ReplaceAll(filePath, "\\", "/"))
	base := strings.ToLower(path.Base(filePath))
	for _, marker := range []string{
		"/__snapshots__/", "/snapshots/", "/docs/", "/documentation/",
		"/commit-history/", "/commit_history/", "/history-dumps/", "/generated/",
		"/derived/",
	} {
		if strings.Contains(normalized+"/", marker) {
			return true
		}
	}
	if strings.Contains(base, "snapshot") || strings.HasSuffix(base, ".snap") ||
		strings.HasPrefix(base, "changelog") || strings.HasPrefix(base, "changes") ||
		strings.Contains(base, "commit-history") || strings.Contains(base, "commit_history") {
		return true
	}
	return false
}

func (b *builder) addEnvironmentAssignments(
	repository catalog.Repository,
	revision string,
	contents map[string][]byte,
) {
	_, cdkRepository := contents["cdk.json"]
	paths := make([]string, 0)
	for filePath := range contents {
		if isPotentialEnvironmentAssignmentFile(filePath) ||
			(cdkRepository && isCDKStackFile(filePath)) {
			paths = append(paths, filePath)
		}
	}
	sort.Strings(paths)
	for _, filePath := range paths {
		rank := environmentAssignmentRank(filePath)
		if rank == 0 && cdkRepository && isCDKStackFile(filePath) {
			rank = environmentAssignmentInfrastructure
		}
		if isTopologyAssignmentExcluded(filePath) {
			for _, assignment := range extractEnvironmentAssignments(
				repository, revision, filePath, contents[filePath],
				rank, b.evidence,
			) {
				b.excludedEnvironmentVariables[assignment.Variable] = true
			}
			continue
		}
		if rank == 0 {
			continue
		}
		b.environmentAssignments = append(
			b.environmentAssignments,
			extractEnvironmentAssignments(
				repository, revision, filePath, contents[filePath], rank, b.evidence,
			)...,
		)
	}
}

func isCDKStackFile(filePath string) bool {
	base := strings.ToLower(path.Base(filePath))
	switch strings.ToLower(path.Ext(base)) {
	case ".ts", ".js", ".mjs", ".cjs", ".py":
		return strings.Contains(base, "stack") ||
			strings.Contains("/"+strings.ToLower(strings.ReplaceAll(filePath, "\\", "/")), "/lib/")
	default:
		return false
	}
}

func extractEnvironmentAssignments(
	repository catalog.Repository,
	revision, filePath string,
	content []byte,
	rank int,
	evidence func(catalog.Repository, string, string, int, string) Evidence,
) []EnvironmentAssignment {
	output := make([]EnvironmentAssignment, 0)
	seen := make(map[string]bool)
	add := func(variable, rawValue string, line int) {
		value := cleanEnvironmentAssignmentValue(rawValue)
		if variable == "" || value == "" {
			return
		}
		indirect := isIndirectEnvironmentValue(value)
		if indirect {
			if !isConnectionEnvironmentVariable(variable) {
				return
			}
			value = ""
		} else {
			var ok bool
			value, ok = sanitizedEnvironmentAssignmentValue(variable, value)
			if !ok {
				return
			}
		}
		key := variable + "\x00" + value + "\x00" + strconv.Itoa(line)
		if seen[key] {
			return
		}
		seen[key] = true
		output = append(output, EnvironmentAssignment{
			Variable:    variable,
			Value:       value,
			Rank:        rank,
			Environment: environmentFromAssignmentPath(filePath),
			Indirect:    indirect,
			Evidence:    evidence(repository, revision, filePath, line, variable+" assignment"),
		})
	}

	type pendingKubernetesVariable struct {
		name string
		line int
	}
	pending := pendingKubernetesVariable{}
	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(make([]byte, 0, 64*1024), maximumSourceFileSize)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") ||
			strings.HasPrefix(trimmed, "//") {
			continue
		}
		if match := kubernetesEnvironmentNamePattern.FindStringSubmatch(line); len(match) == 2 {
			pending = pendingKubernetesVariable{name: match[1], line: lineNumber}
			continue
		}
		if pending.name != "" {
			if match := kubernetesEnvironmentValuePattern.FindStringSubmatch(line); len(match) == 2 {
				add(pending.name, match[1], lineNumber)
				pending = pendingKubernetesVariable{}
				continue
			}
			if strings.Contains(strings.ToLower(trimmed), "valuefrom:") ||
				strings.HasPrefix(trimmed, "- name:") {
				if strings.Contains(strings.ToLower(trimmed), "valuefrom:") {
					add(pending.name, "valueFrom: secret reference", lineNumber)
				}
				pending = pendingKubernetesVariable{}
			}
		}
		if match := environmentKeyValuePattern.FindStringSubmatch(line); len(match) == 3 {
			add(match[1], match[2], lineNumber)
		}
		for _, match := range environmentQuotedKeyValuePattern.FindAllStringSubmatch(line, -1) {
			add(match[1], match[2], lineNumber)
		}
	}
	return output
}

func cleanEnvironmentAssignmentValue(raw string) string {
	value := strings.TrimSpace(raw)
	value = strings.TrimSuffix(value, ",")
	value = strings.TrimSpace(value)
	if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') {
		quote := value[0]
		if end := strings.IndexByte(value[1:], quote); end >= 0 {
			return strings.TrimSpace(value[1 : end+1])
		}
	}
	if comment := strings.Index(value, " #"); comment >= 0 {
		value = value[:comment]
	}
	value = strings.TrimSpace(strings.TrimRight(value, ",}"))
	return strings.Trim(value, `"'`)
}

func isIndirectEnvironmentValue(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	for _, marker := range []string{
		"${", "{{", "vault:", "vault/", "secret://", "secretkeyref",
		"secretsmanager", "keyvault", "valuefrom:", "ssm:", "ref(",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func isConnectionEnvironmentVariable(variable string) bool {
	variable = strings.ToUpper(strings.TrimSpace(variable))
	for _, marker := range []string{
		"URL", "URI", "HOST", "ENDPOINT", "ADDRESS", "UPSTREAM", "ROUTE", "SERVICE",
	} {
		if strings.Contains(variable, marker) {
			return true
		}
	}
	return false
}

func sanitizedEnvironmentAssignmentValue(variable, value string) (string, bool) {
	host, transport, ok := environmentTargetHost(value)
	if !ok {
		return "", false
	}
	hasExplicitServiceScheme := strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), "http://") ||
		strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), "https://") ||
		strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), "lb://")
	if !hasExplicitServiceScheme && !isConnectionEnvironmentVariable(variable) {
		return "", false
	}
	if hasExplicitServiceScheme {
		return transport + "://" + host, true
	}
	return host, true
}

func environmentFromAssignmentPath(filePath string) string {
	normalized := strings.ToLower(strings.ReplaceAll(filePath, "\\", "/"))
	segments := strings.Split(normalized, "/")
	for _, segment := range segments {
		stem := strings.TrimSuffix(segment, path.Ext(segment))
		if environmentRegionPattern.MatchString(stem) {
			return stem
		}
		if match := environmentRegionSearchPattern.FindStringSubmatch(stem); len(match) == 2 {
			return match[1]
		}
		for _, token := range environmentTokenPattern.Split(stem, -1) {
			switch token {
			case "production", "prod", "staging", "stage", "stg", "development",
				"dev", "qa", "uat", "sandbox", "preview":
				return token
			}
		}
		if environmentRegionPattern.MatchString(segment) {
			return segment
		}
	}
	return ""
}

type resolvedEnvironmentTarget struct {
	target         string
	transport      string
	targetResolved bool
	assignment     EnvironmentAssignment
}

func (b *builder) resolveTopologyPlaceholders() {
	if len(b.topologyPlaceholders) == 0 {
		return
	}
	slices.SortFunc(b.topologyPlaceholders, func(left, right TopologyPlaceholder) int {
		if left.ConsumptionEvidence.RepositoryID != right.ConsumptionEvidence.RepositoryID {
			return int(left.ConsumptionEvidence.RepositoryID - right.ConsumptionEvidence.RepositoryID)
		}
		if compared := strings.Compare(
			left.ConsumptionEvidence.Path, right.ConsumptionEvidence.Path,
		); compared != 0 {
			return compared
		}
		return left.ConsumptionEvidence.Line - right.ConsumptionEvidence.Line
	})
	for _, placeholder := range b.topologyPlaceholders {
		b.resolveTopologyPlaceholder(placeholder)
	}
}

func (b *builder) resolveTopologyPlaceholder(placeholder TopologyPlaceholder) {
	assignments := b.topRankedEnvironmentAssignments(placeholder.Variable)
	if placeholder.MapKeyCandidate != "" {
		if targetID, ok := b.knownServiceTarget(
			placeholder.MapKeyCandidate, placeholder.Source,
		); ok {
			evidence := []Evidence{placeholder.ConsumptionEvidence}
			transport := "http"
			groups := make(map[string][]resolvedEnvironmentTarget)
			for _, assignment := range assignments {
				if assignment.Indirect {
					continue
				}
				if target, resolved := b.resolvedEnvironmentTarget(placeholder, assignment); resolved {
					groups[target.target] = append(groups[target.target], target)
				}
			}
			divergent := len(groups) > 1
			registryEnvironment := ""
			if corroborating := groups[targetID]; len(corroborating) > 0 {
				transport = corroborating[0].transport
				evidence = appendUniqueEvidence(evidence, corroborating[0].assignment.Evidence)
				if divergent {
					registryEnvironment = corroborating[0].assignment.Environment
				}
			} else if len(groups) > 0 {
				divergent = true
			}
			b.addSystemConnection(SystemConnection{
				Source: placeholder.Source, Target: targetID,
				Protocol: placeholder.Protocol, Interaction: placeholder.Interaction,
				Transport: transport, Confidence: "high", EvidenceOrigin: "static",
				TargetResolved: true, EnvironmentVariable: placeholder.Variable,
				ResolutionTier: "map_key_registry", Environment: registryEnvironment,
				ResolutionDivergent: divergent, Evidence: evidence,
			})
			otherTargets := make([]string, 0, len(groups))
			for assignmentTarget := range groups {
				if assignmentTarget != targetID {
					otherTargets = append(otherTargets, assignmentTarget)
				}
			}
			sort.Strings(otherTargets)
			for _, assignmentTarget := range otherTargets {
				candidates := groups[assignmentTarget]
				environment := ""
				if divergent {
					environment = candidates[0].assignment.Environment
				}
				b.addResolvedPlaceholderConnection(
					placeholder, candidates[0], "cross_repository_assignment",
					divergent, environment,
				)
			}
			return
		}
	}

	if placeholder.Default != "" && !isIndirectEnvironmentValue(placeholder.Default) {
		assignment := EnvironmentAssignment{
			Variable: placeholder.Variable,
			Value:    placeholder.Default,
			Rank:     environmentAssignmentApplication,
			Evidence: placeholder.ConsumptionEvidence,
		}
		assignment.Evidence.Label = placeholder.Variable + " in-file default"
		if target, ok := b.resolvedEnvironmentTarget(placeholder, assignment); ok {
			b.addResolvedPlaceholderConnection(
				placeholder, target, "in_file_default", false, "",
			)
			return
		}
	}

	if len(assignments) > 0 {
		groups := make(map[string][]resolvedEnvironmentTarget)
		indirect := false
		unresolvedEvidence := make([]Evidence, 0, len(assignments))
		for _, assignment := range assignments {
			indirect = indirect || assignment.Indirect
			unresolvedEvidence = appendUniqueEvidence(unresolvedEvidence, assignment.Evidence)
			if assignment.Indirect {
				continue
			}
			if target, ok := b.resolvedEnvironmentTarget(placeholder, assignment); ok {
				groups[target.target] = append(groups[target.target], target)
			}
		}
		if len(groups) > 0 {
			divergent := len(groups) > 1
			targetIDs := make([]string, 0, len(groups))
			for targetID := range groups {
				targetIDs = append(targetIDs, targetID)
			}
			sort.Strings(targetIDs)
			for _, targetID := range targetIDs {
				candidates := groups[targetID]
				slices.SortFunc(candidates, func(left, right resolvedEnvironmentTarget) int {
					if left.assignment.Rank != right.assignment.Rank {
						return right.assignment.Rank - left.assignment.Rank
					}
					if left.assignment.Evidence.RepositoryID != right.assignment.Evidence.RepositoryID {
						return int(left.assignment.Evidence.RepositoryID - right.assignment.Evidence.RepositoryID)
					}
					if compared := strings.Compare(
						left.assignment.Evidence.Path, right.assignment.Evidence.Path,
					); compared != 0 {
						return compared
					}
					return left.assignment.Evidence.Line - right.assignment.Evidence.Line
				})
				environment := ""
				if divergent {
					environment = candidates[0].assignment.Environment
				}
				b.addResolvedPlaceholderConnection(
					placeholder, candidates[0], "cross_repository_assignment",
					divergent, environment,
				)
			}
			return
		}
		reason := "assignment-not-a-literal-target"
		if indirect {
			reason = "secret-indirection"
		}
		b.addUnresolvedPlaceholderConnection(placeholder, reason, unresolvedEvidence...)
		return
	}

	if targetID, ok := b.knownServiceTarget(
		nameShapeServiceCandidate(placeholder.Variable), placeholder.Source,
	); ok {
		if b.excludedEnvironmentVariables[placeholder.Variable] {
			b.addUnresolvedPlaceholderConnection(placeholder, "only-excluded-assignments")
			return
		}
		b.addSystemConnection(SystemConnection{
			Source: placeholder.Source, Target: targetID,
			Protocol: placeholder.Protocol, Interaction: placeholder.Interaction,
			Transport: "http", Confidence: "low", EvidenceOrigin: "static",
			TargetResolved: true, EnvironmentVariable: placeholder.Variable,
			ResolutionTier: "name_shape_heuristic",
			Evidence:       []Evidence{placeholder.ConsumptionEvidence},
		})
		return
	}
	if b.excludedEnvironmentVariables[placeholder.Variable] {
		b.addUnresolvedPlaceholderConnection(placeholder, "only-excluded-assignments")
		return
	}
	b.addUnresolvedPlaceholderConnection(placeholder, "no-indexed-assignment")
}

func (b *builder) topRankedEnvironmentAssignments(variable string) []EnvironmentAssignment {
	assignments := make([]EnvironmentAssignment, 0)
	maximumRank := 0
	for _, assignment := range b.environmentAssignments {
		if assignment.Variable != variable {
			continue
		}
		if assignment.Rank > maximumRank {
			maximumRank = assignment.Rank
			assignments = assignments[:0]
		}
		if assignment.Rank == maximumRank {
			assignments = append(assignments, assignment)
		}
	}
	slices.SortFunc(assignments, func(left, right EnvironmentAssignment) int {
		if left.Evidence.RepositoryID != right.Evidence.RepositoryID {
			return int(left.Evidence.RepositoryID - right.Evidence.RepositoryID)
		}
		if compared := strings.Compare(left.Evidence.Path, right.Evidence.Path); compared != 0 {
			return compared
		}
		return left.Evidence.Line - right.Evidence.Line
	})
	return assignments
}

func sortedEnvironmentVariables(values map[string]bool) []string {
	output := make([]string, 0, len(values))
	for value := range values {
		if value != "" {
			output = append(output, value)
		}
	}
	sort.Strings(output)
	return output
}

func (b *builder) resolvedEnvironmentTarget(
	placeholder TopologyPlaceholder,
	assignment EnvironmentAssignment,
) (resolvedEnvironmentTarget, bool) {
	host, transport, ok := environmentTargetHost(assignment.Value)
	if !ok {
		return resolvedEnvironmentTarget{}, false
	}
	if targetID, resolved := b.knownServiceTarget(host, placeholder.Source); resolved {
		return resolvedEnvironmentTarget{
			target: targetID, transport: transport, targetResolved: true,
			assignment: assignment,
		}, true
	}
	name := topologyExternalPeerName(host)
	if name == "" {
		return resolvedEnvironmentTarget{}, false
	}
	candidate := !strings.Contains(host, ".") &&
		!genericExternalHostLabels[normalizeServiceName(host)]
	targetID := b.externalSystemComponentWithCandidate(
		"service", name, strings.ToUpper(transport),
		[]string{name, host, normalizeServiceName(host)}, candidate,
	)
	return resolvedEnvironmentTarget{
		target: targetID, transport: transport, assignment: assignment,
	}, true
}

func environmentTargetHost(value string) (string, string, bool) {
	value = strings.TrimSpace(value)
	if value == "" || isIndirectEnvironmentValue(value) {
		return "", "", false
	}
	if parsed, err := url.Parse(value); err == nil && parsed.Scheme != "" {
		if parsed.Scheme != "http" && parsed.Scheme != "https" && parsed.Scheme != "lb" {
			return "", "", false
		}
		if parsed.Hostname() == "" {
			return "", "", false
		}
		return strings.ToLower(parsed.Hostname()), strings.ToLower(parsed.Scheme), true
	}
	host := strings.Trim(strings.SplitN(value, "/", 2)[0], "[]")
	if splitHost, _, err := net.SplitHostPort(host); err == nil {
		host = splitHost
	}
	if host == "" || strings.ContainsAny(host, " \t{}$") {
		return "", "", false
	}
	return strings.ToLower(host), "http", true
}

func (b *builder) knownServiceTarget(value, source string) (string, bool) {
	normalized := normalizeServiceName(value)
	if normalized == "" {
		return "", false
	}
	if repositoryNodeID := b.serviceTargets[normalized]; repositoryNodeID != "" {
		if componentID, ok := b.registeredServiceComponent(
			repositoryNodeID, normalized, source,
		); ok {
			return componentID, true
		}
	}
	candidates := make(map[string]bool)
	for componentID, component := range b.components {
		if component.External || componentID == source || component.Kind != "service" {
			continue
		}
		for _, alias := range normalizedAliases(component.Name, component.Aliases...) {
			if alias == normalized {
				candidates[componentID] = true
				break
			}
		}
	}
	if len(candidates) != 1 {
		return "", false
	}
	for componentID := range candidates {
		return componentID, true
	}
	return "", false
}

func (b *builder) registeredServiceComponent(
	repositoryNodeID, alias, source string,
) (string, bool) {
	node, ok := b.nodes[repositoryNodeID]
	if !ok || node.Kind != "repository" || node.RepositoryID == 0 {
		return "", false
	}
	componentID := "system:" + strconv.FormatInt(node.RepositoryID, 10)
	if componentID == source {
		return "", false
	}
	if component, exists := b.components[componentID]; exists {
		return componentID, !component.External && component.Kind == "service"
	}
	name := strings.TrimSpace(node.Label)
	if name == "" {
		name = alias
	}
	repository := strings.TrimSpace(node.Repository)
	if repository == "" {
		repository = name
	}
	b.addSystemComponent(SystemComponent{
		ID: componentID, Name: name, Kind: "service",
		RepositoryID: node.RepositoryID, Repository: repository,
		Path: ".", Aliases: []string{name, alias}, Evidence: node.Evidence,
	})
	return componentID, true
}

func nameShapeServiceCandidate(variable string) string {
	for _, suffix := range []string{"_BASE_URL", "_URL", "_HOST"} {
		if strings.HasSuffix(variable, suffix) {
			name := strings.TrimSuffix(variable, suffix)
			return strings.ToLower(strings.ReplaceAll(name, "_", "-"))
		}
	}
	return ""
}

func (b *builder) addResolvedPlaceholderConnection(
	placeholder TopologyPlaceholder,
	target resolvedEnvironmentTarget,
	tier string,
	divergent bool,
	environment string,
) {
	confidence := "medium"
	if (tier == "in_file_default" && target.targetResolved) ||
		(tier == "cross_repository_assignment" &&
			target.assignment.Rank == environmentAssignmentInfrastructure &&
			target.targetResolved) {
		confidence = "high"
	}
	evidence := []Evidence{placeholder.ConsumptionEvidence}
	if target.assignment.Evidence.Path != placeholder.ConsumptionEvidence.Path ||
		target.assignment.Evidence.Line != placeholder.ConsumptionEvidence.Line ||
		target.assignment.Evidence.RepositoryID != placeholder.ConsumptionEvidence.RepositoryID {
		evidence = appendUniqueEvidence(evidence, target.assignment.Evidence)
	}
	b.addSystemConnection(SystemConnection{
		Source: placeholder.Source, Target: target.target,
		Protocol: placeholder.Protocol, Interaction: placeholder.Interaction,
		Transport: target.transport, Confidence: confidence, EvidenceOrigin: "static",
		TargetResolved:      target.targetResolved,
		EnvironmentVariable: placeholder.Variable, ResolutionTier: tier,
		Environment: environment, ResolutionDivergent: divergent,
		Evidence: evidence,
	})
}

func (b *builder) addUnresolvedPlaceholderConnection(
	placeholder TopologyPlaceholder,
	reason string,
	additionalEvidence ...Evidence,
) {
	evidence := appendUniqueEvidence(
		[]Evidence{placeholder.ConsumptionEvidence},
		additionalEvidence...,
	)
	unresolved := UnresolvedTopologyConnection{
		ID: normalizeID(strings.Join([]string{
			"unresolved", placeholder.Source, placeholder.Variable,
			placeholder.MapKeyCandidate,
		}, ":")),
		Source: placeholder.Source, Variable: placeholder.Variable,
		Candidate: placeholder.MapKeyCandidate,
		Protocol:  placeholder.Protocol, Interaction: placeholder.Interaction,
		Reason: reason, Evidence: evidence,
	}
	for index, existing := range b.unresolvedTopology {
		if existing.ID != unresolved.ID {
			continue
		}
		existing.Evidence = appendUniqueEvidence(existing.Evidence, evidence...)
		b.unresolvedTopology[index] = existing
		return
	}
	b.unresolvedTopology = append(b.unresolvedTopology, unresolved)
}
