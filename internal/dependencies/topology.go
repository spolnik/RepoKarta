package dependencies

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/spolnik/RepoKarta/internal/graph"
)

const (
	MaximumTopologyObservations = 5_000
	topologyRetention           = 90 * 24 * time.Hour
	topologyNeighborhoodCap     = 12
	topologyContextCap          = 12
)

// RuntimeTopologyObservation is an aggregate observed interaction from an APM,
// trace, or service-graph provider. Runtime facts are never represented as
// commit-pinned source evidence.
type RuntimeTopologyObservation struct {
	Provider     string    `json:"provider"`
	Environment  string    `json:"environment,omitempty"`
	SourceName   string    `json:"source_name"`
	SourceKind   string    `json:"source_kind,omitempty"`
	TargetName   string    `json:"target_name"`
	TargetKind   string    `json:"target_kind,omitempty"`
	Protocol     string    `json:"protocol"`
	Interaction  string    `json:"interaction"`
	Transport    string    `json:"transport,omitempty"`
	ObservedFrom time.Time `json:"observed_from"`
	ObservedTo   time.Time `json:"observed_to"`
	RequestCount int64     `json:"request_count"`
	ErrorCount   int64     `json:"error_count"`
	LatencyP95MS float64   `json:"latency_p95_ms,omitempty"`
	ImportedAt   time.Time `json:"imported_at"`
}

// TopologyObservationStore keeps mutable runtime evidence outside immutable map
// artifacts.
type TopologyObservationStore interface {
	ListRuntimeTopologyObservations(context.Context, time.Time, time.Time) ([]RuntimeTopologyObservation, error)
	UpsertRuntimeTopologyObservations(context.Context, []RuntimeTopologyObservation, time.Time) error
}

type TopologyImportRequest struct {
	Provider     string                       `json:"provider"`
	Environment  string                       `json:"environment,omitempty"`
	Observations []RuntimeTopologyObservation `json:"observations"`
}

type TopologyImportResult struct {
	Imported      int       `json:"imported"`
	Provider      string    `json:"provider"`
	Environment   string    `json:"environment,omitempty"`
	ImportedAt    time.Time `json:"imported_at"`
	RetentionDays int       `json:"retention_days"`
}

type TopologyOptions struct {
	Query        string
	Protocol     string
	Origin       string
	Environment  string
	Provider     string
	Direction    string
	Depth        int
	ObservedFrom time.Time
	ObservedTo   time.Time
}

type TopologyComponent struct {
	graph.SystemComponent
	Origins          []string `json:"origins"`
	NeighborhoodRole string   `json:"neighborhood_role,omitempty"`
}

type RuntimeMetrics struct {
	Provider     string    `json:"provider"`
	Environment  string    `json:"environment,omitempty"`
	ObservedFrom time.Time `json:"observed_from"`
	ObservedTo   time.Time `json:"observed_to"`
	RequestCount int64     `json:"request_count"`
	ErrorCount   int64     `json:"error_count"`
	ErrorRate    float64   `json:"error_rate"`
	LatencyP95MS float64   `json:"latency_p95_ms,omitempty"`
}

type TopologyConnection struct {
	ID                    string           `json:"id"`
	Source                string           `json:"source"`
	SourceName            string           `json:"source_name"`
	Target                string           `json:"target"`
	TargetName            string           `json:"target_name"`
	Protocol              string           `json:"protocol"`
	Interaction           string           `json:"interaction"`
	Transport             string           `json:"transport,omitempty"`
	Confidence            string           `json:"confidence"`
	State                 string           `json:"state"`
	Origins               []string         `json:"origins"`
	TargetResolved        bool             `json:"target_resolved"`
	EnvironmentVariable   string           `json:"environment_variable,omitempty"`
	ResolutionTier        string           `json:"resolution_tier,omitempty"`
	Environment           string           `json:"environment,omitempty"`
	ResolutionDivergent   bool             `json:"resolution_divergent,omitempty"`
	UnresolvedReason      string           `json:"unresolved_reason,omitempty"`
	Evidence              []graph.Evidence `json:"evidence,omitempty"`
	Runtime               *RuntimeMetrics  `json:"runtime,omitempty"`
	NeighborhoodDirection string           `json:"neighborhood_direction,omitempty"`
}

type TopologyUnresolved struct {
	graph.UnresolvedTopologyConnection
	SourceName            string `json:"source_name"`
	NeighborhoodDirection string `json:"neighborhood_direction,omitempty"`
}

type TopologyNeighborhoodGroup struct {
	ID                     string `json:"id"`
	Direction              string `json:"direction"`
	Label                  string `json:"label"`
	OmittedConnectionCount int    `json:"omitted_connection_count"`
	OmittedComponentCount  int    `json:"omitted_component_count"`
}

type TopologyNeighborhood struct {
	RepositoryID                   int64                       `json:"repository_id"`
	Direction                      string                      `json:"direction"`
	Depth                          int                         `json:"depth"`
	DisplayCap                     int                         `json:"display_cap"`
	SelectedComponentIDs           []string                    `json:"selected_component_ids"`
	InboundConnectionCount         int                         `json:"inbound_connection_count"`
	OutboundConnectionCount        int                         `json:"outbound_connection_count"`
	ContextConnectionCount         int                         `json:"context_connection_count,omitempty"`
	Groups                         []TopologyNeighborhoodGroup `json:"groups"`
	PossibleInboundUnresolvedCount int                         `json:"possible_inbound_unresolved_count"`
	PossibleInboundUnresolved      []TopologyUnresolved        `json:"possible_inbound_unresolved"`
	FleetSnapshotAt                time.Time                   `json:"fleet_snapshot_at,omitempty"`
	SelectedSnapshotAt             time.Time                   `json:"selected_snapshot_at,omitempty"`
	FleetPartial                   bool                        `json:"fleet_partial"`
	FleetStale                     bool                        `json:"fleet_stale"`
}

type TopologyWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Count   int    `json:"count"`
}

type TopologySummary struct {
	ComponentCount             int `json:"component_count"`
	ConnectionCount            int `json:"connection_count"`
	ServiceCount               int `json:"service_count"`
	ResourceCount              int `json:"resource_count"`
	ConfirmedCount             int `json:"confirmed_count"`
	StaticOnlyCount            int `json:"static_only_count"`
	RuntimeOnlyCount           int `json:"runtime_only_count"`
	ResolvedCount              int `json:"resolved_count"`
	CandidateCount             int `json:"candidate_count"`
	UnresolvedCount            int `json:"unresolved_count"`
	UnresolvedPlaceholderCount int `json:"unresolved_placeholder_count"`
	SuppressedSourceEdges      int `json:"suppressed_source_edges"`
}

type Topology struct {
	GeneratedAt                    time.Time              `json:"generated_at"`
	SnapshotID                     string                 `json:"snapshot_id"`
	Components                     []TopologyComponent    `json:"components"`
	Connections                    []TopologyConnection   `json:"connections"`
	Unresolved                     []TopologyUnresolved   `json:"unresolved"`
	Summary                        TopologySummary        `json:"summary"`
	Protocols                      []string               `json:"protocols"`
	Providers                      []string               `json:"providers"`
	Environments                   []string               `json:"environments"`
	Scope                          graph.Scope            `json:"scope"`
	BuildProgress                  graph.ArtifactProgress `json:"build_progress"`
	RejectedExternalComponentCount int                    `json:"rejected_external_component_count"`
	RejectedComponentCounts        map[string]int         `json:"rejected_component_counts,omitempty"`
	RejectedComponentConnections   int                    `json:"rejected_component_connection_count"`
	Partial                        bool                   `json:"partial"`
	Warnings                       []TopologyWarning      `json:"warnings,omitempty"`
	Neighborhood                   *TopologyNeighborhood  `json:"neighborhood,omitempty"`
	Options                        TopologyOptions        `json:"-"`
}

// Topology merges immutable static facts with independently timestamped runtime
// observations. A static edge and observed edge are considered the same only
// after both endpoints, protocol, interaction, and transport agree.
func (s *Service) Topology(
	ctx context.Context,
	snapshot graph.Snapshot,
	progress graph.ArtifactProgress,
	options TopologyOptions,
) (Topology, error) {
	generatedAt := s.now().UTC()
	observedFrom, observedTo := options.ObservedFrom, options.ObservedTo
	if observedTo.IsZero() {
		observedTo = generatedAt
	}
	if observedFrom.IsZero() {
		observedFrom = observedTo.Add(-24 * time.Hour)
	}
	if observedFrom.After(observedTo) {
		return Topology{}, errors.New("observed_from must not be after observed_to")
	}
	var runtime []RuntimeTopologyObservation
	if store, ok := s.store.(TopologyObservationStore); ok {
		var err error
		runtime, err = store.ListRuntimeTopologyObservations(ctx, observedFrom, observedTo)
		if err != nil {
			return Topology{}, err
		}
	}
	return buildTopology(snapshot, progress, runtime, options, generatedAt), nil
}

func (s *Service) ImportTopologyObservations(
	ctx context.Context,
	request TopologyImportRequest,
) (TopologyImportResult, error) {
	store, ok := s.store.(TopologyObservationStore)
	if !ok {
		return TopologyImportResult{}, errors.New("runtime topology storage is unavailable")
	}
	request.Provider = strings.TrimSpace(request.Provider)
	request.Environment = strings.TrimSpace(request.Environment)
	if request.Provider == "" || len(request.Provider) > 80 {
		return TopologyImportResult{}, errors.New("provider must be between 1 and 80 characters")
	}
	if len(request.Environment) > 80 {
		return TopologyImportResult{}, errors.New("environment exceeds 80 characters")
	}
	if len(request.Observations) == 0 || len(request.Observations) > MaximumTopologyObservations {
		return TopologyImportResult{}, fmt.Errorf(
			"observations must contain between 1 and %d entries",
			MaximumTopologyObservations,
		)
	}
	importedAt := s.now().UTC()
	observations := make([]RuntimeTopologyObservation, len(request.Observations))
	for index, observation := range request.Observations {
		observation.Provider = request.Provider
		observation.Environment = request.Environment
		observation.ImportedAt = importedAt
		if err := validateRuntimeObservation(&observation); err != nil {
			return TopologyImportResult{}, fmt.Errorf("observation %d: %w", index+1, err)
		}
		observations[index] = observation
	}
	if err := store.UpsertRuntimeTopologyObservations(
		ctx, observations, importedAt.Add(-topologyRetention),
	); err != nil {
		return TopologyImportResult{}, err
	}
	return TopologyImportResult{
		Imported: len(observations), Provider: request.Provider,
		Environment: request.Environment, ImportedAt: importedAt,
		RetentionDays: int(topologyRetention / (24 * time.Hour)),
	}, nil
}

func validateRuntimeObservation(observation *RuntimeTopologyObservation) error {
	observation.SourceName = strings.TrimSpace(observation.SourceName)
	observation.TargetName = strings.TrimSpace(observation.TargetName)
	observation.SourceKind = normalizedTopologyKind(observation.SourceKind, "service")
	observation.TargetKind = normalizedTopologyKind(observation.TargetKind, "service")
	observation.Protocol = strings.ToLower(strings.TrimSpace(observation.Protocol))
	observation.Interaction = strings.ToLower(strings.TrimSpace(observation.Interaction))
	observation.Transport = strings.ToLower(strings.TrimSpace(observation.Transport))
	if observation.SourceName == "" || observation.TargetName == "" ||
		len(observation.SourceName) > 200 || len(observation.TargetName) > 200 {
		return errors.New("source_name and target_name must be between 1 and 200 characters")
	}
	if !slices.Contains(
		[]string{"http", "grpc", "kafka", "database", "mcp", "amqp", "unknown"},
		observation.Protocol,
	) {
		return errors.New("protocol is unsupported")
	}
	if observation.Interaction == "" || len(observation.Interaction) > 80 {
		return errors.New("interaction must be between 1 and 80 characters")
	}
	if observation.ObservedFrom.IsZero() || observation.ObservedTo.IsZero() ||
		observation.ObservedFrom.After(observation.ObservedTo) {
		return errors.New("observed_from and observed_to must define a valid window")
	}
	if observation.ObservedTo.Sub(observation.ObservedFrom) > 31*24*time.Hour {
		return errors.New("observation window exceeds 31 days")
	}
	if observation.RequestCount < 0 || observation.ErrorCount < 0 ||
		observation.ErrorCount > observation.RequestCount ||
		observation.LatencyP95MS < 0 {
		return errors.New("runtime metrics are invalid")
	}
	return nil
}

func normalizedTopologyKind(value, fallback string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "service", "database", "queue", "broker", "mcp_server", "external_service":
		return value
	default:
		return fallback
	}
}

func buildTopology(
	snapshot graph.Snapshot,
	progress graph.ArtifactProgress,
	runtime []RuntimeTopologyObservation,
	options TopologyOptions,
	generatedAt time.Time,
) Topology {
	nodes := make(map[string]TopologyComponent)
	aliasTargets := make(map[string][]string)
	for _, component := range snapshot.Components {
		node := TopologyComponent{SystemComponent: component, Origins: []string{"static"}}
		nodes[component.ID] = node
		for _, alias := range append(component.Aliases, component.Name) {
			normalized := topologyName(alias)
			if normalized != "" && !slices.Contains(aliasTargets[normalized], component.ID) {
				aliasTargets[normalized] = append(aliasTargets[normalized], component.ID)
			}
		}
	}
	resolve := func(name, kind, origin string) string {
		normalized := topologyName(name)
		targets := make([]string, 0)
		for _, candidateID := range aliasTargets[normalized] {
			if runtimeKindCanResolve(kind, nodes[candidateID].SystemComponent) {
				targets = append(targets, candidateID)
			}
		}
		if len(targets) == 1 {
			node := nodes[targets[0]]
			if !slices.Contains(node.Origins, origin) {
				node.Origins = append(node.Origins, origin)
				sort.Strings(node.Origins)
				nodes[targets[0]] = node
			}
			return targets[0]
		}
		id := "runtime:" + normalizedTopologyKind(kind, "service") + ":" + normalized
		node := nodes[id]
		if node.ID == "" {
			node = TopologyComponent{
				SystemComponent: graph.SystemComponent{
					ID: id, Name: name, Kind: normalizedTopologyKind(kind, "service"),
					Aliases: []string{name}, External: true,
				},
			}
		}
		if !slices.Contains(node.Origins, origin) {
			node.Origins = append(node.Origins, origin)
		}
		nodes[id] = node
		return id
	}

	connections := make(map[string]TopologyConnection)
	missingComponentReferences := 0
	for _, connection := range snapshot.Connections {
		if _, sourceExists := nodes[connection.Source]; !sourceExists {
			missingComponentReferences++
			continue
		}
		if _, targetExists := nodes[connection.Target]; !targetExists {
			missingComponentReferences++
			continue
		}
		view := TopologyConnection{
			Source: connection.Source, Target: connection.Target,
			Protocol: connection.Protocol, Interaction: connection.Interaction,
			Transport: connection.Transport, Confidence: connection.Confidence,
			State: "static_only", Origins: []string{connection.EvidenceOrigin},
			TargetResolved: connection.TargetResolved, Evidence: connection.Evidence,
			EnvironmentVariable: connection.EnvironmentVariable,
			ResolutionTier:      connection.ResolutionTier,
			Environment:         connection.Environment,
			ResolutionDivergent: connection.ResolutionDivergent,
			UnresolvedReason:    connection.UnresolvedReason,
		}
		view.ID = topologyConnectionKey(view)
		connections[view.ID] = view
	}
	providers, environments := make(map[string]bool), make(map[string]bool)
	for _, observation := range runtime {
		if options.Provider != "" && !strings.EqualFold(options.Provider, observation.Provider) {
			continue
		}
		if options.Environment != "" && !strings.EqualFold(options.Environment, observation.Environment) {
			continue
		}
		providers[observation.Provider] = true
		if observation.Environment != "" {
			environments[observation.Environment] = true
		}
		source := resolve(observation.SourceName, observation.SourceKind, "runtime")
		target := resolve(observation.TargetName, observation.TargetKind, "runtime")
		view := TopologyConnection{
			Source: source, Target: target, Protocol: observation.Protocol,
			Interaction: observation.Interaction, Transport: observation.Transport,
			Confidence: "observed", State: "runtime_only",
			Origins: []string{"runtime"}, TargetResolved: !strings.HasPrefix(target, "runtime:"),
			Environment: observation.Environment,
		}
		view.ID = topologyConnectionKey(view)
		existing, ok := connections[view.ID]
		if !ok && observation.Environment != "" {
			unqualified := view
			unqualified.Environment = ""
			existing, ok = connections[topologyConnectionKey(unqualified)]
		}
		if ok {
			view = existing
			view.State = "confirmed"
			if !slices.Contains(view.Origins, "runtime") {
				view.Origins = append(view.Origins, "runtime")
			}
		}
		if view.Runtime == nil {
			view.Runtime = &RuntimeMetrics{
				Provider: observation.Provider, Environment: observation.Environment,
				ObservedFrom: observation.ObservedFrom, ObservedTo: observation.ObservedTo,
			}
		}
		view.Runtime.ObservedFrom = earliestTime(view.Runtime.ObservedFrom, observation.ObservedFrom)
		view.Runtime.ObservedTo = latestTime(view.Runtime.ObservedTo, observation.ObservedTo)
		view.Runtime.RequestCount += observation.RequestCount
		view.Runtime.ErrorCount += observation.ErrorCount
		view.Runtime.LatencyP95MS = max(view.Runtime.LatencyP95MS, observation.LatencyP95MS)
		if view.Runtime.RequestCount > 0 {
			view.Runtime.ErrorRate = float64(view.Runtime.ErrorCount) / float64(view.Runtime.RequestCount)
		}
		if view.Runtime.Provider != observation.Provider {
			view.Runtime.Provider = "multiple"
		}
		if view.Runtime.Environment != observation.Environment {
			view.Runtime.Environment = "multiple"
		}
		connections[view.ID] = view
	}

	output := Topology{
		GeneratedAt: generatedAt, SnapshotID: snapshot.ID,
		Components:  make([]TopologyComponent, 0),
		Connections: make([]TopologyConnection, 0),
		Unresolved:  make([]TopologyUnresolved, 0),
		Scope:       snapshot.Scope, BuildProgress: progress,
		RejectedExternalComponentCount: snapshot.RejectedExternalCount,
		RejectedComponentCounts:        make(map[string]int, len(snapshot.RejectedComponentCounts)),
		RejectedComponentConnections:   snapshot.RejectedComponentConnections,
		Partial:                        snapshot.Truncated || !snapshot.Scope.Complete,
		Options:                        options,
	}
	for reason, count := range snapshot.RejectedComponentCounts {
		output.RejectedComponentCounts[reason] = count
	}
	output.Summary.SuppressedSourceEdges = snapshot.SuppressedSourceEdges
	if missingComponentReferences > 0 {
		addMissingComponentWarning(&output, missingComponentReferences)
	}
	protocols := make(map[string]bool)
	visibleNodeIDs := make(map[string]bool)
	for _, connection := range connections {
		connection.SourceName = nodes[connection.Source].Name
		connection.TargetName = nodes[connection.Target].Name
		if !topologyConnectionMatches(connection, nodes, options) {
			continue
		}
		protocols[connection.Protocol] = true
		if connection.Environment != "" {
			environments[connection.Environment] = true
		}
		visibleNodeIDs[connection.Source], visibleNodeIDs[connection.Target] = true, true
		output.Connections = append(output.Connections, connection)
		switch connection.State {
		case "confirmed":
			output.Summary.ConfirmedCount++
		case "runtime_only":
			output.Summary.RuntimeOnlyCount++
		default:
			output.Summary.StaticOnlyCount++
		}
		target := nodes[connection.Target]
		if target.Candidate {
			output.Summary.CandidateCount++
		} else if strings.HasPrefix(connection.Target, "runtime:") &&
			!connection.TargetResolved {
			output.Summary.UnresolvedCount++
		} else {
			output.Summary.ResolvedCount++
		}
	}
	for _, unresolved := range snapshot.UnresolvedTopology {
		source := nodes[unresolved.Source]
		if source.ID == "" {
			continue
		}
		if options.Protocol != "" &&
			!strings.EqualFold(options.Protocol, unresolved.Protocol) {
			continue
		}
		if options.Origin != "" && options.Origin != "static_only" {
			continue
		}
		if options.Environment != "" || options.Provider != "" ||
			!options.ObservedFrom.IsZero() || !options.ObservedTo.IsZero() {
			continue
		}
		if options.Query != "" {
			query := strings.ToLower(options.Query)
			if !strings.Contains(strings.ToLower(source.Name), query) &&
				!strings.Contains(strings.ToLower(unresolved.Variable), query) &&
				!strings.Contains(strings.ToLower(unresolved.Candidate), query) {
				continue
			}
		}
		output.Unresolved = append(output.Unresolved, TopologyUnresolved{
			UnresolvedTopologyConnection: unresolved,
			SourceName:                   source.Name,
		})
		visibleNodeIDs[unresolved.Source] = true
	}
	output.Summary.UnresolvedCount += len(output.Unresolved)
	output.Summary.UnresolvedPlaceholderCount = len(output.Unresolved)
	for id, component := range nodes {
		selectedComponent := snapshot.Scope.RequestedRepositoryID > 0 &&
			component.RepositoryID == snapshot.Scope.RequestedRepositoryID
		if len(output.Connections) > 0 && !visibleNodeIDs[id] && !selectedComponent {
			continue
		}
		if options.Query != "" && len(output.Connections) == 0 &&
			!strings.Contains(strings.ToLower(component.Name), strings.ToLower(options.Query)) {
			continue
		}
		output.Components = append(output.Components, component)
		if component.Kind == "service" {
			output.Summary.ServiceCount++
		} else {
			output.Summary.ResourceCount++
		}
	}
	output.Summary.ComponentCount = len(output.Components)
	output.Summary.ConnectionCount = len(output.Connections)
	slices.SortFunc(output.Components, func(left, right TopologyComponent) int {
		return strings.Compare(strings.ToLower(left.Name), strings.ToLower(right.Name))
	})
	slices.SortFunc(output.Connections, func(left, right TopologyConnection) int {
		return strings.Compare(left.ID, right.ID)
	})
	slices.SortFunc(output.Unresolved, func(left, right TopologyUnresolved) int {
		return strings.Compare(left.ID, right.ID)
	})
	output.Protocols = sortedSet(protocols)
	output.Providers = sortedSet(providers)
	output.Environments = sortedSet(environments)
	if snapshot.Scope.RequestedRepositoryID > 0 {
		output = scopeTopologyNeighborhood(output, snapshot, options)
	}
	return output
}

func scopeTopologyNeighborhood(
	topology Topology,
	snapshot graph.Snapshot,
	options TopologyOptions,
) Topology {
	direction := strings.ToLower(strings.TrimSpace(options.Direction))
	if direction == "" {
		direction = "both"
	}
	depth := options.Depth
	if depth < 1 {
		depth = 1
	}
	if depth > 2 {
		depth = 2
	}
	componentByID := make(map[string]TopologyComponent, len(topology.Components))
	selected := make(map[string]bool)
	selectedAliases := make(map[string]bool)
	for _, component := range topology.Components {
		componentByID[component.ID] = component
		if component.RepositoryID != snapshot.Scope.RequestedRepositoryID {
			continue
		}
		selected[component.ID] = true
		for _, alias := range append(component.Aliases, component.Name) {
			if normalized := topologyName(alias); normalized != "" {
				selectedAliases[normalized] = true
			}
		}
	}

	allConnections := append([]TopologyConnection(nil), topology.Connections...)
	slices.SortFunc(allConnections, func(left, right TopologyConnection) int {
		return strings.Compare(left.ID, right.ID)
	})
	inbound := make([]TopologyConnection, 0)
	outbound := make([]TopologyConnection, 0)
	directNeighborIDs := make(map[string]bool)
	for _, connection := range allConnections {
		sourceSelected, targetSelected := selected[connection.Source], selected[connection.Target]
		switch {
		case targetSelected && !sourceSelected:
			connection.NeighborhoodDirection = "inbound"
			inbound = append(inbound, connection)
			directNeighborIDs[connection.Source] = true
		case sourceSelected:
			connection.NeighborhoodDirection = "outbound"
			outbound = append(outbound, connection)
			directNeighborIDs[connection.Target] = true
		}
	}

	neighborhood := &TopologyNeighborhood{
		RepositoryID:              snapshot.Scope.RequestedRepositoryID,
		Direction:                 direction,
		Depth:                     depth,
		DisplayCap:                topologyNeighborhoodCap,
		SelectedComponentIDs:      sortedBoolSet(selected),
		InboundConnectionCount:    len(inbound),
		OutboundConnectionCount:   len(outbound),
		Groups:                    []TopologyNeighborhoodGroup{},
		PossibleInboundUnresolved: []TopologyUnresolved{},
		FleetSnapshotAt:           snapshot.TopologyFleetGeneratedAt,
		SelectedSnapshotAt:        snapshot.TopologySelectedGeneratedAt,
		FleetPartial:              !snapshot.Scope.Complete,
	}
	neighborhood.FleetStale =
		!neighborhood.FleetSnapshotAt.IsZero() &&
			!neighborhood.SelectedSnapshotAt.IsZero() &&
			neighborhood.FleetSnapshotAt.Before(neighborhood.SelectedSnapshotAt)

	visibleConnections := make([]TopologyConnection, 0, topologyNeighborhoodCap*2)
	if direction == "both" || direction == "inbound" {
		kept, group := capNeighborhoodConnections(
			inbound, "inbound", topologyNeighborhoodCap, selected,
		)
		visibleConnections = append(visibleConnections, kept...)
		if group != nil {
			neighborhood.Groups = append(neighborhood.Groups, *group)
		}
	}
	if direction == "both" || direction == "outbound" {
		kept, group := capNeighborhoodConnections(
			outbound, "outbound", topologyNeighborhoodCap, selected,
		)
		visibleConnections = append(visibleConnections, kept...)
		if group != nil {
			neighborhood.Groups = append(neighborhood.Groups, *group)
		}
	}

	if depth == 2 {
		contextConnections := make([]TopologyConnection, 0)
		directIDs := make(map[string]bool, len(inbound)+len(outbound))
		for _, connection := range append(append(
			[]TopologyConnection(nil), inbound...), outbound...) {
			directIDs[connection.ID] = true
		}
		for _, connection := range allConnections {
			if directIDs[connection.ID] || selected[connection.Source] ||
				selected[connection.Target] {
				continue
			}
			if !directNeighborIDs[connection.Source] && !directNeighborIDs[connection.Target] {
				continue
			}
			connection.NeighborhoodDirection = "context"
			contextConnections = append(contextConnections, connection)
		}
		neighborhood.ContextConnectionCount = len(contextConnections)
		kept, group := capNeighborhoodConnections(
			contextConnections, "context", topologyContextCap, selected,
		)
		visibleConnections = append(visibleConnections, kept...)
		if group != nil {
			neighborhood.Groups = append(neighborhood.Groups, *group)
		}
	}

	visibleComponentIDs := make(map[string]bool, len(selected)+len(visibleConnections)*2)
	for id := range selected {
		visibleComponentIDs[id] = true
	}
	for _, connection := range visibleConnections {
		visibleComponentIDs[connection.Source] = true
		visibleComponentIDs[connection.Target] = true
	}

	outgoingUnresolved := make([]TopologyUnresolved, 0)
	for _, unresolved := range topology.Unresolved {
		if selected[unresolved.Source] {
			if direction == "both" || direction == "outbound" {
				unresolved.NeighborhoodDirection = "outbound"
				outgoingUnresolved = append(outgoingUnresolved, unresolved)
				visibleComponentIDs[unresolved.Source] = true
			}
			continue
		}
		if direction != "both" && direction != "inbound" {
			continue
		}
		if candidate := topologyName(unresolved.Candidate); candidate != "" &&
			selectedAliases[candidate] {
			unresolved.NeighborhoodDirection = "possible_inbound"
			neighborhood.PossibleInboundUnresolved = append(
				neighborhood.PossibleInboundUnresolved, unresolved,
			)
		}
	}

	components := make([]TopologyComponent, 0, len(visibleComponentIDs))
	for id := range visibleComponentIDs {
		component, ok := componentByID[id]
		if !ok {
			continue
		}
		inboundNeighbor := componentIsInboundNeighbor(id, visibleConnections, selected)
		outboundNeighbor := componentIsOutboundNeighbor(id, visibleConnections, selected)
		switch {
		case selected[id]:
			component.NeighborhoodRole = "selected"
		case inboundNeighbor && outboundNeighbor:
			component.NeighborhoodRole = "bidirectional"
		case inboundNeighbor:
			component.NeighborhoodRole = "inbound"
		case outboundNeighbor:
			component.NeighborhoodRole = "outbound"
		default:
			component.NeighborhoodRole = "context"
		}
		components = append(components, component)
	}
	slices.SortFunc(components, func(left, right TopologyComponent) int {
		if left.NeighborhoodRole != right.NeighborhoodRole {
			return strings.Compare(left.NeighborhoodRole, right.NeighborhoodRole)
		}
		return strings.Compare(strings.ToLower(left.Name), strings.ToLower(right.Name))
	})
	slices.SortFunc(visibleConnections, func(left, right TopologyConnection) int {
		return strings.Compare(left.ID, right.ID)
	})
	slices.SortFunc(outgoingUnresolved, func(left, right TopologyUnresolved) int {
		return strings.Compare(left.ID, right.ID)
	})
	slices.SortFunc(
		neighborhood.PossibleInboundUnresolved,
		func(left, right TopologyUnresolved) int {
			return strings.Compare(left.ID, right.ID)
		},
	)
	neighborhood.PossibleInboundUnresolvedCount =
		len(neighborhood.PossibleInboundUnresolved)
	if len(neighborhood.PossibleInboundUnresolved) > topologyNeighborhoodCap {
		neighborhood.PossibleInboundUnresolved =
			neighborhood.PossibleInboundUnresolved[:topologyNeighborhoodCap]
	}
	topology.Components = components
	topology.Connections = visibleConnections
	topology.Unresolved = outgoingUnresolved
	topology.Neighborhood = neighborhood
	rebuildTopologySummary(&topology)
	return topology
}

func capNeighborhoodConnections(
	connections []TopologyConnection,
	direction string,
	limit int,
	selected map[string]bool,
) ([]TopologyConnection, *TopologyNeighborhoodGroup) {
	if len(connections) <= limit {
		return connections, nil
	}
	ranked := append([]TopologyConnection(nil), connections...)
	slices.SortStableFunc(ranked, func(left, right TopologyConnection) int {
		if difference := topologyConnectionUsefulness(right) -
			topologyConnectionUsefulness(left); difference != 0 {
			return difference
		}
		return strings.Compare(left.ID, right.ID)
	})
	kept := append([]TopologyConnection(nil), ranked[:limit]...)
	omittedComponents := make(map[string]bool)
	for _, connection := range ranked[limit:] {
		if !selected[connection.Source] {
			omittedComponents[connection.Source] = true
		}
		if !selected[connection.Target] {
			omittedComponents[connection.Target] = true
		}
	}
	connectionCount := len(connections) - limit
	componentCount := len(omittedComponents)
	label := fmt.Sprintf("+%d more %s connections", connectionCount, direction)
	if direction == "inbound" {
		label = fmt.Sprintf("+%d more callers", componentCount)
	} else if direction == "outbound" {
		label = fmt.Sprintf("+%d more dependencies", componentCount)
	}
	return kept, &TopologyNeighborhoodGroup{
		ID: "neighborhood-group:" + direction, Direction: direction, Label: label,
		OmittedConnectionCount: connectionCount,
		OmittedComponentCount:  componentCount,
	}
}

func topologyConnectionUsefulness(connection TopologyConnection) int {
	score := 0
	switch connection.State {
	case "confirmed":
		score += 100
	case "static_only":
		score += 60
	case "runtime_only":
		score += 40
	}
	switch connection.Confidence {
	case "high":
		score += 30
	case "medium":
		score += 20
	case "low":
		score += 10
	}
	if connection.TargetResolved {
		score += 15
	}
	if connection.Runtime != nil {
		score += min(10, int(connection.Runtime.RequestCount))
	}
	score += min(5, len(connection.Evidence))
	return score
}

func componentIsInboundNeighbor(
	id string,
	connections []TopologyConnection,
	selected map[string]bool,
) bool {
	for _, connection := range connections {
		if connection.NeighborhoodDirection == "inbound" &&
			connection.Source == id && selected[connection.Target] {
			return true
		}
	}
	return false
}

func componentIsOutboundNeighbor(
	id string,
	connections []TopologyConnection,
	selected map[string]bool,
) bool {
	for _, connection := range connections {
		if connection.NeighborhoodDirection == "outbound" &&
			connection.Target == id && selected[connection.Source] {
			return true
		}
	}
	return false
}

func sortedBoolSet(values map[string]bool) []string {
	output := make([]string, 0, len(values))
	for value := range values {
		output = append(output, value)
	}
	sort.Strings(output)
	return output
}

func rebuildTopologySummary(topology *Topology) {
	suppressedSourceEdges := topology.Summary.SuppressedSourceEdges
	topology.Summary = TopologySummary{SuppressedSourceEdges: suppressedSourceEdges}
	topology.Summary.ComponentCount = len(topology.Components)
	componentByID := make(map[string]TopologyComponent, len(topology.Components))
	for _, component := range topology.Components {
		componentByID[component.ID] = component
		if component.Kind == "service" {
			topology.Summary.ServiceCount++
		} else {
			topology.Summary.ResourceCount++
		}
	}
	protocols := make(map[string]bool)
	for _, connection := range topology.Connections {
		protocols[connection.Protocol] = true
		switch connection.State {
		case "confirmed":
			topology.Summary.ConfirmedCount++
		case "runtime_only":
			topology.Summary.RuntimeOnlyCount++
		default:
			topology.Summary.StaticOnlyCount++
		}
		target := componentByID[connection.Target]
		if target.Candidate {
			topology.Summary.CandidateCount++
		} else if strings.HasPrefix(connection.Target, "runtime:") &&
			!connection.TargetResolved {
			topology.Summary.UnresolvedCount++
		} else {
			topology.Summary.ResolvedCount++
		}
	}
	topology.Summary.ConnectionCount = len(topology.Connections)
	topology.Summary.UnresolvedPlaceholderCount = len(topology.Unresolved)
	if topology.Neighborhood != nil {
		topology.Summary.UnresolvedPlaceholderCount +=
			topology.Neighborhood.PossibleInboundUnresolvedCount
	}
	topology.Summary.UnresolvedCount += topology.Summary.UnresolvedPlaceholderCount
	topology.Protocols = sortedSet(protocols)
}

// SanitizeTopology is the final response-level guard for topology consumers.
// The builder owns the invariant, but API adapters call this again so a stale
// artifact or alternate service implementation cannot expose dangling edges.
func SanitizeTopology(topology Topology) Topology {
	componentByID := make(map[string]TopologyComponent, len(topology.Components))
	for _, component := range topology.Components {
		componentByID[component.ID] = component
	}
	connections := make([]TopologyConnection, 0, len(topology.Connections))
	hidden := 0
	for _, connection := range topology.Connections {
		_, sourceExists := componentByID[connection.Source]
		_, targetExists := componentByID[connection.Target]
		if !sourceExists || !targetExists {
			hidden++
			continue
		}
		connections = append(connections, connection)
	}
	if hidden == 0 {
		return topology
	}
	topology.Connections = connections
	addMissingComponentWarning(&topology, hidden)
	rebuildTopologySummary(&topology)
	return topology
}

func addMissingComponentWarning(topology *Topology, count int) {
	for index := range topology.Warnings {
		if topology.Warnings[index].Code == "missing_component_reference" {
			topology.Warnings[index].Count += count
			topology.Warnings[index].Message =
				missingComponentWarningMessage(topology.Warnings[index].Count)
			return
		}
	}
	topology.Warnings = append(topology.Warnings, TopologyWarning{
		Code:    "missing_component_reference",
		Message: missingComponentWarningMessage(count),
		Count:   count,
	})
}

func missingComponentWarningMessage(count int) string {
	noun, verb := "connections", "were"
	if count == 1 {
		noun, verb = "connection", "was"
	}
	return fmt.Sprintf(
		"%d %s referenced missing components and %s hidden",
		count, noun, verb,
	)
}

func runtimeKindCanResolve(observedKind string, candidate graph.SystemComponent) bool {
	observedKind = normalizedTopologyKind(observedKind, "service")
	switch observedKind {
	case "service", "external_service":
		return candidate.Kind == "service"
	case "mcp_server":
		return candidate.Kind == "mcp_server" ||
			slices.Contains(candidate.Capabilities, "mcp_server")
	default:
		return candidate.Kind == observedKind
	}
}

func topologyConnectionMatches(
	connection TopologyConnection,
	nodes map[string]TopologyComponent,
	options TopologyOptions,
) bool {
	if options.Protocol != "" && !strings.EqualFold(options.Protocol, connection.Protocol) {
		return false
	}
	if options.Origin != "" {
		switch options.Origin {
		case "static":
			if connection.State == "runtime_only" {
				return false
			}
		case "runtime":
			if connection.State == "static_only" {
				return false
			}
		case "confirmed":
			if connection.State != "confirmed" {
				return false
			}
		}
	}
	if options.Environment != "" &&
		!strings.EqualFold(options.Environment, connection.Environment) {
		return false
	}
	query := strings.ToLower(strings.TrimSpace(options.Query))
	if query == "" {
		return true
	}
	source, target := nodes[connection.Source], nodes[connection.Target]
	haystack := strings.Join([]string{
		source.Name, target.Name, connection.Protocol, connection.Interaction,
		connection.Transport, connection.State,
		connection.EnvironmentVariable, connection.ResolutionTier,
		connection.Environment, connection.UnresolvedReason,
	}, " ")
	return strings.Contains(strings.ToLower(haystack), query)
}

func topologyConnectionKey(connection TopologyConnection) string {
	return "connection:" + strings.Join([]string{
		connection.Source, connection.Target, strings.ToLower(connection.Protocol),
		strings.ToLower(connection.Interaction), strings.ToLower(connection.Transport),
		strings.ToLower(connection.Environment),
	}, "|")
}

func topologyName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimPrefix(value, "http://")
	value = strings.TrimPrefix(value, "https://")
	value = strings.SplitN(value, "/", 2)[0]
	value = strings.SplitN(value, ":", 2)[0]
	value = strings.SplitN(value, ".", 2)[0]
	value = strings.NewReplacer("_", "-", " ", "-").Replace(value)
	return strings.Trim(value, "-")
}

func earliestTime(left, right time.Time) time.Time {
	if left.IsZero() || (!right.IsZero() && right.Before(left)) {
		return right
	}
	return left
}

func latestTime(left, right time.Time) time.Time {
	if right.After(left) {
		return right
	}
	return left
}

func sortedSet(values map[string]bool) []string {
	output := make([]string, 0, len(values))
	for value := range values {
		if value != "" {
			output = append(output, value)
		}
	}
	sort.Strings(output)
	return output
}
