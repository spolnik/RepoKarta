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
	ObservedFrom time.Time
	ObservedTo   time.Time
}

type TopologyComponent struct {
	graph.SystemComponent
	Origins []string `json:"origins"`
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
	ID             string           `json:"id"`
	Source         string           `json:"source"`
	SourceName     string           `json:"source_name"`
	Target         string           `json:"target"`
	TargetName     string           `json:"target_name"`
	Protocol       string           `json:"protocol"`
	Interaction    string           `json:"interaction"`
	Transport      string           `json:"transport,omitempty"`
	Confidence     string           `json:"confidence"`
	State          string           `json:"state"`
	Origins        []string         `json:"origins"`
	TargetResolved bool             `json:"target_resolved"`
	Evidence       []graph.Evidence `json:"evidence,omitempty"`
	Runtime        *RuntimeMetrics  `json:"runtime,omitempty"`
}

type TopologySummary struct {
	ComponentCount   int `json:"component_count"`
	ConnectionCount  int `json:"connection_count"`
	ServiceCount     int `json:"service_count"`
	ResourceCount    int `json:"resource_count"`
	ConfirmedCount   int `json:"confirmed_count"`
	StaticOnlyCount  int `json:"static_only_count"`
	RuntimeOnlyCount int `json:"runtime_only_count"`
	UnresolvedCount  int `json:"unresolved_count"`
}

type Topology struct {
	GeneratedAt   time.Time              `json:"generated_at"`
	SnapshotID    string                 `json:"snapshot_id"`
	Components    []TopologyComponent    `json:"components"`
	Connections   []TopologyConnection   `json:"connections"`
	Summary       TopologySummary        `json:"summary"`
	Protocols     []string               `json:"protocols"`
	Providers     []string               `json:"providers"`
	Environments  []string               `json:"environments"`
	Scope         graph.Scope            `json:"scope"`
	BuildProgress graph.ArtifactProgress `json:"build_progress"`
	Partial       bool                   `json:"partial"`
	Options       TopologyOptions        `json:"-"`
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
	for _, connection := range snapshot.Connections {
		view := TopologyConnection{
			Source: connection.Source, Target: connection.Target,
			Protocol: connection.Protocol, Interaction: connection.Interaction,
			Transport: connection.Transport, Confidence: connection.Confidence,
			State: "static_only", Origins: []string{connection.EvidenceOrigin},
			TargetResolved: connection.TargetResolved, Evidence: connection.Evidence,
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
		}
		view.ID = topologyConnectionKey(view)
		if existing, ok := connections[view.ID]; ok {
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
		Scope:       snapshot.Scope, BuildProgress: progress,
		Partial: snapshot.Truncated || !snapshot.Scope.Complete,
		Options: options,
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
		targetKind := nodes[connection.Target].Kind
		if !connection.TargetResolved && slices.Contains(
			[]string{"service", "external_service", "mcp_server"}, targetKind,
		) {
			output.Summary.UnresolvedCount++
		}
	}
	for id, component := range nodes {
		if len(output.Connections) > 0 && !visibleNodeIDs[id] {
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
	output.Protocols = sortedSet(protocols)
	output.Providers = sortedSet(providers)
	output.Environments = sortedSet(environments)
	return output
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
	query := strings.ToLower(strings.TrimSpace(options.Query))
	if query == "" {
		return true
	}
	source, target := nodes[connection.Source], nodes[connection.Target]
	haystack := strings.Join([]string{
		source.Name, target.Name, connection.Protocol, connection.Interaction,
		connection.Transport, connection.State,
	}, " ")
	return strings.Contains(strings.ToLower(haystack), query)
}

func topologyConnectionKey(connection TopologyConnection) string {
	return "connection:" + strings.Join([]string{
		connection.Source, connection.Target, strings.ToLower(connection.Protocol),
		strings.ToLower(connection.Interaction), strings.ToLower(connection.Transport),
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
