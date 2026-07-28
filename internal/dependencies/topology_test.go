package dependencies

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/spolnik/RepoKarta/internal/graph"
)

type topologyTestStore struct {
	runtime  []RuntimeTopologyObservation
	imported []RuntimeTopologyObservation
	prunedAt time.Time
}

func (s *topologyTestStore) ListDependencyObservations(context.Context) ([]Observation, error) {
	return nil, nil
}

func (s *topologyTestStore) UpsertDependencyObservation(context.Context, Observation) error {
	return nil
}

func (s *topologyTestStore) ListRuntimeTopologyObservations(
	context.Context,
	time.Time,
	time.Time,
) ([]RuntimeTopologyObservation, error) {
	return append([]RuntimeTopologyObservation(nil), s.runtime...), nil
}

func (s *topologyTestStore) UpsertRuntimeTopologyObservations(
	_ context.Context,
	observations []RuntimeTopologyObservation,
	prunedAt time.Time,
) error {
	s.imported = append([]RuntimeTopologyObservation(nil), observations...)
	s.prunedAt = prunedAt
	return nil
}

func TestTopologyMergesStaticAndRuntimeAndKeepsDriftVisible(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	store := &topologyTestStore{runtime: []RuntimeTopologyObservation{
		{
			Provider: "tempo", Environment: "prod",
			SourceName: "checkout", SourceKind: "service",
			TargetName: "orders", TargetKind: "service",
			Protocol: "http", Interaction: "calls", Transport: "https",
			ObservedFrom: now.Add(-time.Hour), ObservedTo: now,
			RequestCount: 100, ErrorCount: 2, LatencyP95MS: 83,
		},
		{
			Provider: "tempo", Environment: "prod",
			SourceName: "orders", SourceKind: "service",
			TargetName: "payments", TargetKind: "service",
			Protocol: "grpc", Interaction: "calls", Transport: "grpc",
			ObservedFrom: now.Add(-time.Hour), ObservedTo: now,
			RequestCount: 40, ErrorCount: 1, LatencyP95MS: 47,
		},
	}}
	service := NewService(context.Background(), store, nil)
	service.now = func() time.Time { return now }
	snapshot := graph.Snapshot{
		ID:    "static-1",
		Scope: graph.Scope{Kind: "collection", Complete: true, TotalRepositories: 2, AnalyzedRepositories: 2},
		Components: []graph.SystemComponent{
			{ID: "checkout", Name: "checkout", Kind: "service", Aliases: []string{"checkout"}},
			{ID: "orders", Name: "orders", Kind: "service", Aliases: []string{"orders"}},
		},
		Connections: []graph.SystemConnection{{
			Source: "checkout", Target: "orders", Protocol: "http",
			Interaction: "calls", Transport: "https", Confidence: "high",
			EvidenceOrigin: "static", TargetResolved: true,
		}},
	}
	topology, err := service.Topology(
		context.Background(),
		snapshot,
		graph.ArtifactProgress{State: "ready", RequestedRepositories: 2, ReadyRepositories: 2},
		TopologyOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if topology.Summary.ConfirmedCount != 1 || topology.Summary.RuntimeOnlyCount != 1 ||
		topology.Summary.StaticOnlyCount != 0 {
		t.Fatalf("unexpected topology summary: %+v", topology.Summary)
	}
	var confirmed TopologyConnection
	for _, connection := range topology.Connections {
		if connection.State == "confirmed" {
			confirmed = connection
		}
	}
	if confirmed.Runtime == nil || confirmed.Runtime.RequestCount != 100 ||
		confirmed.Runtime.ErrorRate != 0.02 {
		t.Fatalf("runtime metrics not merged: %+v", confirmed)
	}
}

func TestImportTopologyObservationsValidatesAndAppliesRetention(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	store := &topologyTestStore{}
	service := NewService(context.Background(), store, nil)
	service.now = func() time.Time { return now }
	result, err := service.ImportTopologyObservations(context.Background(), TopologyImportRequest{
		Provider: "Grafana Tempo", Environment: "prod",
		Observations: []RuntimeTopologyObservation{{
			SourceName: "checkout", TargetName: "orders",
			Protocol: "http", Interaction: "calls", Transport: "https",
			ObservedFrom: now.Add(-5 * time.Minute), ObservedTo: now,
			RequestCount: 50, ErrorCount: 1, LatencyP95MS: 21.5,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Imported != 1 || result.RetentionDays != 90 ||
		len(store.imported) != 1 || store.imported[0].Provider != "Grafana Tempo" ||
		store.prunedAt != now.Add(-topologyRetention) {
		t.Fatalf("unexpected import: result=%+v observation=%+v prune=%s", result, store.imported, store.prunedAt)
	}
	_, err = service.ImportTopologyObservations(context.Background(), TopologyImportRequest{
		Provider: "tempo",
		Observations: []RuntimeTopologyObservation{{
			SourceName: "checkout", TargetName: "orders", Protocol: "ftp",
			Interaction: "calls", ObservedFrom: now.Add(-time.Minute), ObservedTo: now,
		}},
	})
	if err == nil {
		t.Fatal("unsupported runtime protocol was accepted")
	}
}

func TestRuntimeIdentityResolutionIsKindAware(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	store := &topologyTestStore{runtime: []RuntimeTopologyObservation{{
		Provider: "tempo", SourceName: "producer", SourceKind: "service",
		TargetName: "orders", TargetKind: "queue",
		Protocol: "kafka", Interaction: "publishes", Transport: "kafka",
		ObservedFrom: now.Add(-time.Minute), ObservedTo: now, RequestCount: 1,
	}}}
	service := NewService(context.Background(), store, nil)
	service.now = func() time.Time { return now }
	topology, err := service.Topology(
		context.Background(),
		graph.Snapshot{
			ID:    "kinds",
			Scope: graph.Scope{Complete: true},
			Components: []graph.SystemComponent{{
				ID: "orders-service", Name: "orders", Kind: "service", Aliases: []string{"orders"},
			}},
		},
		graph.ArtifactProgress{State: "ready"},
		TopologyOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(topology.Connections) != 1 ||
		topology.Connections[0].Target == "orders-service" ||
		topology.Connections[0].TargetName != "orders" {
		t.Fatalf("queue observation resolved to same-named service: %+v", topology.Connections)
	}
}

func TestEmptyTopologyUsesJSONArrays(t *testing.T) {
	service := NewService(context.Background(), &topologyTestStore{}, nil)
	topology, err := service.Topology(
		context.Background(),
		graph.Snapshot{ID: "empty", Scope: graph.Scope{Kind: "collection", Complete: true}},
		graph.ArtifactProgress{State: "ready"},
		TopologyOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if topology.Components == nil || topology.Connections == nil || topology.Unresolved == nil ||
		topology.Protocols == nil || topology.Providers == nil || topology.Environments == nil {
		t.Fatalf("empty topology must serialize collections as arrays: %+v", topology)
	}
}

func TestTopologyStripsConnectionsWithMissingComponentsAndWarns(t *testing.T) {
	topology := buildTopology(
		graph.Snapshot{
			ID: "referential-integrity",
			Components: []graph.SystemComponent{
				{ID: "checkout", Name: "checkout", Kind: "service"},
				{ID: "orders", Name: "orders", Kind: "service"},
			},
			Connections: []graph.SystemConnection{
				{
					Source: "checkout", Target: "orders", Protocol: "http",
					Interaction: "calls", EvidenceOrigin: "static",
				},
				{
					Source: "suppressed-deployment", Target: "orders", Protocol: "http",
					Interaction: "calls", EvidenceOrigin: "static",
				},
				{
					Source: "checkout", Target: "missing-target", Protocol: "grpc",
					Interaction: "calls", EvidenceOrigin: "static",
				},
			},
			RejectedExternalCount:        1,
			RejectedComponentCounts:      map[string]int{"invalid_name": 1},
			RejectedComponentConnections: 1,
			SuppressedSourceEdges:        4,
			Scope:                        graph.Scope{Complete: true},
		},
		graph.ArtifactProgress{State: "ready"},
		nil,
		TopologyOptions{},
		time.Now(),
	)

	if len(topology.Connections) != 1 ||
		topology.Connections[0].Source != "checkout" ||
		topology.Connections[0].Target != "orders" {
		t.Fatalf("connections with missing endpoints were not stripped: %+v", topology.Connections)
	}
	if len(topology.Warnings) != 1 ||
		topology.Warnings[0].Code != "missing_component_reference" ||
		topology.Warnings[0].Count != 2 ||
		topology.Warnings[0].Message != "2 connections referenced missing components and were hidden" {
		t.Fatalf("referential-integrity warnings = %+v", topology.Warnings)
	}
	if topology.Summary.SuppressedSourceEdges != 4 {
		t.Fatalf("suppressed source edge summary = %+v", topology.Summary)
	}
	if topology.RejectedExternalComponentCount != 1 ||
		topology.RejectedComponentCounts["invalid_name"] != 1 ||
		topology.RejectedComponentConnections != 1 {
		t.Fatalf(
			"rejected component diagnostics were not preserved: external=%d reasons=%v connections=%d",
			topology.RejectedExternalComponentCount,
			topology.RejectedComponentCounts,
			topology.RejectedComponentConnections,
		)
	}
	encoded, err := json.Marshal(topology)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte(
		`"rejected_component_counts":{"invalid_name":1}`,
	)) || !bytes.Contains(encoded, []byte(
		`"rejected_component_connection_count":1`,
	)) {
		t.Fatalf("rejection diagnostics missing from topology payload: %s", encoded)
	}
}

func TestSanitizeTopologyChecksFinalResponsePayload(t *testing.T) {
	topology := SanitizeTopology(Topology{
		Components: []TopologyComponent{
			{SystemComponent: graph.SystemComponent{ID: "checkout", Name: "checkout", Kind: "service"}},
			{SystemComponent: graph.SystemComponent{ID: "orders", Name: "orders", Kind: "service"}},
		},
		Connections: []TopologyConnection{
			{
				ID: "valid", Source: "checkout", Target: "orders",
				Protocol: "http", State: "static_only", TargetResolved: true,
			},
			{
				ID: "dangling", Source: "suppressed-deployment", Target: "orders",
				Protocol: "grpc", State: "static_only", TargetResolved: true,
			},
		},
		Summary: TopologySummary{
			ConnectionCount: 2, StaticOnlyCount: 2, ResolvedCount: 2,
		},
		Protocols: []string{"grpc", "http"},
	})

	if len(topology.Connections) != 1 || topology.Connections[0].ID != "valid" {
		t.Fatalf("final response retained dangling connections: %+v", topology.Connections)
	}
	if topology.Summary.ConnectionCount != 1 ||
		topology.Summary.StaticOnlyCount != 1 ||
		topology.Summary.ResolvedCount != 1 ||
		!slices.Equal(topology.Protocols, []string{"http"}) {
		t.Fatalf("sanitized response summary = %+v, protocols = %v", topology.Summary, topology.Protocols)
	}
	if len(topology.Warnings) != 1 ||
		topology.Warnings[0].Message != "1 connection referenced missing components and was hidden" {
		t.Fatalf("sanitized response warnings = %+v", topology.Warnings)
	}
}

func TestTopologyReportsAndFiltersStaticPlaceholderMetadata(t *testing.T) {
	snapshot := graph.Snapshot{
		ID: "placeholder-summary",
		Components: []graph.SystemComponent{
			{ID: "checkout", Name: "checkout", Kind: "service"},
			{ID: "stripe", Name: "stripe.com", Kind: "service", External: true},
			{ID: "tax", Name: "tax-service", Kind: "service", External: true, Candidate: true},
		},
		Connections: []graph.SystemConnection{
			{
				ID: "staging-edge", Source: "checkout", Target: "stripe",
				Protocol: "http", Interaction: "calls", Confidence: "high",
				EvidenceOrigin: "static", EnvironmentVariable: "PAYMENTS_URL",
				ResolutionTier: "cross_repository_assignment", Environment: "staging",
				ResolutionDivergent: true,
			},
			{
				ID: "candidate-edge", Source: "checkout", Target: "tax",
				Protocol: "http", Interaction: "calls", Confidence: "medium",
				EvidenceOrigin: "static", EnvironmentVariable: "TAX_API_URL",
				ResolutionTier: "in_file_default",
			},
		},
		UnresolvedTopology: []graph.UnresolvedTopologyConnection{{
			ID: "unresolved-edge", Source: "checkout", Variable: "BILLING_SERVICE_URL",
			Candidate: "billing-service", Protocol: "http", Interaction: "calls",
			Reason: "no-indexed-assignment", Evidence: []graph.Evidence{},
		}},
		Scope: graph.Scope{Complete: true, TotalRepositories: 1, AnalyzedRepositories: 1},
	}
	topology := buildTopology(
		snapshot, graph.ArtifactProgress{State: "ready"}, nil,
		TopologyOptions{}, time.Now(),
	)
	if topology.Summary.UnresolvedPlaceholderCount != 1 ||
		topology.Summary.ResolvedCount != 1 ||
		topology.Summary.CandidateCount != 1 ||
		topology.Summary.UnresolvedCount != 1 ||
		!slices.Contains(topology.Environments, "staging") {
		t.Fatalf("placeholder honesty summary = %+v, environments = %v", topology.Summary, topology.Environments)
	}
	filtered := buildTopology(
		snapshot, graph.ArtifactProgress{State: "ready"}, nil,
		TopologyOptions{Environment: "staging"}, time.Now(),
	)
	if len(filtered.Connections) != 1 ||
		filtered.Connections[0].EnvironmentVariable != "PAYMENTS_URL" {
		t.Fatalf("static environment filter = %+v", filtered.Connections)
	}
}

func TestScopedTopologyBuildsBidirectionalFleetNeighborhood(t *testing.T) {
	now := time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC)
	snapshot := graph.Snapshot{
		ID: "three-repository-fleet",
		Components: []graph.SystemComponent{
			{ID: "a", Name: "A", Kind: "service", RepositoryID: 1, Repository: "repo-a"},
			{ID: "b", Name: "B", Kind: "service", RepositoryID: 2, Repository: "repo-b"},
			{ID: "c", Name: "C", Kind: "service", RepositoryID: 3, Repository: "repo-c"},
		},
		Connections: []graph.SystemConnection{
			{
				ID: "a-b", Source: "a", Target: "b", Protocol: "http",
				Interaction: "calls", EvidenceOrigin: "static", TargetResolved: true,
			},
			{
				ID: "c-b", Source: "c", Target: "b", Protocol: "grpc",
				Interaction: "calls", EvidenceOrigin: "static", TargetResolved: true,
			},
		},
		Scope: graph.Scope{
			Kind: "repository", Complete: true, TotalRepositories: 3,
			AnalyzedRepositories: 3, RequestedRepositoryID: 2,
		},
		TopologyFleetGeneratedAt:    now.Add(-time.Hour),
		TopologySelectedGeneratedAt: now,
	}
	topology := buildTopology(
		snapshot, graph.ArtifactProgress{State: "ready"}, nil,
		TopologyOptions{Direction: "both", Depth: 1}, now,
	)
	if topology.Neighborhood == nil ||
		topology.Neighborhood.InboundConnectionCount != 2 ||
		topology.Neighborhood.OutboundConnectionCount != 0 ||
		!topology.Neighborhood.FleetStale ||
		len(topology.Connections) != 2 {
		t.Fatalf("B neighborhood = %+v, connections = %+v", topology.Neighborhood, topology.Connections)
	}
	for _, connection := range topology.Connections {
		if connection.Target != "b" || connection.NeighborhoodDirection != "inbound" {
			t.Fatalf("B direction split = %+v", topology.Connections)
		}
	}
	if roleForComponent(topology.Components, "b") != "selected" ||
		roleForComponent(topology.Components, "a") != "inbound" ||
		roleForComponent(topology.Components, "c") != "inbound" {
		t.Fatalf("B component roles = %+v", topology.Components)
	}

	snapshot.Scope.RequestedRepositoryID = 1
	snapshot.TopologySelectedGeneratedAt = now.Add(-time.Hour)
	topology = buildTopology(
		snapshot, graph.ArtifactProgress{State: "ready"}, nil,
		TopologyOptions{Direction: "both", Depth: 1}, now,
	)
	if topology.Neighborhood == nil ||
		topology.Neighborhood.InboundConnectionCount != 0 ||
		topology.Neighborhood.OutboundConnectionCount != 1 ||
		len(topology.Connections) != 1 ||
		topology.Connections[0].Source != "a" ||
		topology.Connections[0].Target != "b" ||
		topology.Connections[0].NeighborhoodDirection != "outbound" {
		t.Fatalf("A direction split = %+v, connections = %+v", topology.Neighborhood, topology.Connections)
	}
}

func TestScopedTopologyGroupsHubConnectionsBeyondDisplayCap(t *testing.T) {
	components := []graph.SystemComponent{{
		ID: "hub", Name: "Hub", Kind: "service", RepositoryID: 100, Repository: "hub",
	}}
	connections := make([]graph.SystemConnection, 0, topologyNeighborhoodCap+3)
	for index := 0; index < topologyNeighborhoodCap+3; index++ {
		id := fmt.Sprintf("caller-%02d", index)
		components = append(components, graph.SystemComponent{
			ID: id, Name: id, Kind: "service", RepositoryID: int64(index + 1),
		})
		connections = append(connections, graph.SystemConnection{
			ID: id + "-hub", Source: id, Target: "hub", Protocol: "http",
			Interaction: "calls", EvidenceOrigin: "static", TargetResolved: true,
		})
	}
	topology := buildTopology(
		graph.Snapshot{
			ID: "hub-fleet", Components: components, Connections: connections,
			Scope: graph.Scope{
				Kind: "repository", Complete: true,
				TotalRepositories: len(components), AnalyzedRepositories: len(components),
				RequestedRepositoryID: 100,
			},
		},
		graph.ArtifactProgress{State: "ready"}, nil,
		TopologyOptions{Direction: "both", Depth: 1}, time.Now(),
	)
	if topology.Neighborhood == nil ||
		topology.Neighborhood.InboundConnectionCount != topologyNeighborhoodCap+3 ||
		len(topology.Connections) != topologyNeighborhoodCap ||
		len(topology.Neighborhood.Groups) != 1 ||
		topology.Neighborhood.Groups[0].Direction != "inbound" ||
		topology.Neighborhood.Groups[0].OmittedConnectionCount != 3 ||
		topology.Neighborhood.Groups[0].OmittedComponentCount != 3 ||
		len(topology.Components) != topologyNeighborhoodCap+1 {
		t.Fatalf(
			"hub neighborhood = %+v, connections = %d, components = %d",
			topology.Neighborhood, len(topology.Connections), len(topology.Components),
		)
	}
}

func TestScopedTopologyDepthTwoAddsBoundedContext(t *testing.T) {
	topology := buildTopology(
		graph.Snapshot{
			ID: "depth-two",
			Components: []graph.SystemComponent{
				{ID: "a", Name: "A", Kind: "service", RepositoryID: 1},
				{ID: "b", Name: "B", Kind: "service", RepositoryID: 2},
				{ID: "c", Name: "C", Kind: "service", RepositoryID: 3},
			},
			Connections: []graph.SystemConnection{
				{
					ID: "a-b", Source: "a", Target: "b", Protocol: "http",
					Interaction: "calls", EvidenceOrigin: "static", TargetResolved: true,
				},
				{
					ID: "b-c", Source: "b", Target: "c", Protocol: "grpc",
					Interaction: "calls", EvidenceOrigin: "static", TargetResolved: true,
				},
			},
			Scope: graph.Scope{
				Kind: "repository", Complete: true, TotalRepositories: 3,
				AnalyzedRepositories: 3, RequestedRepositoryID: 1,
			},
		},
		graph.ArtifactProgress{State: "ready"}, nil,
		TopologyOptions{Direction: "both", Depth: 2}, time.Now(),
	)
	if topology.Neighborhood == nil ||
		topology.Neighborhood.Depth != 2 ||
		topology.Neighborhood.ContextConnectionCount != 1 ||
		len(topology.Connections) != 2 ||
		topology.Connections[0].NeighborhoodDirection != "outbound" ||
		topology.Connections[1].NeighborhoodDirection != "context" ||
		roleForComponent(topology.Components, "c") != "context" {
		t.Fatalf("depth-two neighborhood = %+v, connections = %+v", topology.Neighborhood, topology.Connections)
	}
}

func TestScopedTopologySeparatesPossibleUnresolvedInboundCallers(t *testing.T) {
	topology := buildTopology(
		graph.Snapshot{
			ID: "possible-inbound",
			Components: []graph.SystemComponent{
				{
					ID: "checkout", Name: "checkout-service", Kind: "service",
					RepositoryID: 1, Aliases: []string{"checkout"},
				},
				{ID: "caller", Name: "caller", Kind: "service", RepositoryID: 2},
			},
			UnresolvedTopology: []graph.UnresolvedTopologyConnection{{
				ID: "caller-checkout", Source: "caller",
				Variable: "CHECKOUT_URL", Candidate: "checkout",
				Protocol: "http", Interaction: "calls", Reason: "no-indexed-assignment",
			}},
			Scope: graph.Scope{
				Kind: "repository", Complete: false, TotalRepositories: 3,
				AnalyzedRepositories: 2, OmittedRepositories: 1,
				RequestedRepositoryID: 1,
			},
		},
		graph.ArtifactProgress{State: "building", PendingRepositories: 1}, nil,
		TopologyOptions{Direction: "both", Depth: 1}, time.Now(),
	)
	if topology.Neighborhood == nil || !topology.Neighborhood.FleetPartial ||
		topology.Neighborhood.PossibleInboundUnresolvedCount != 1 ||
		len(topology.Neighborhood.PossibleInboundUnresolved) != 1 ||
		topology.Neighborhood.PossibleInboundUnresolved[0].SourceName != "caller" ||
		topology.Neighborhood.PossibleInboundUnresolved[0].NeighborhoodDirection != "possible_inbound" ||
		len(topology.Unresolved) != 0 ||
		topology.Summary.UnresolvedPlaceholderCount != 1 {
		t.Fatalf("possible inbound unresolved = %+v, summary = %+v", topology.Neighborhood, topology.Summary)
	}
}

func roleForComponent(components []TopologyComponent, id string) string {
	for _, component := range components {
		if component.ID == id {
			return component.NeighborhoodRole
		}
	}
	return ""
}

func TestNeighborhoodCapPrefersUsefulConnectionsOverIDOrder(t *testing.T) {
	connections := []TopologyConnection{
		{ID: "a-low", Source: "selected", Target: "candidate", State: "static_only", Confidence: "low"},
		{ID: "z-confirmed", Source: "selected", Target: "resolved", State: "confirmed", Confidence: "high", TargetResolved: true},
	}
	kept, group := capNeighborhoodConnections(
		connections, "outbound", 1, map[string]bool{"selected": true},
	)
	if len(kept) != 1 || kept[0].ID != "z-confirmed" || group == nil ||
		group.OmittedConnectionCount != 1 {
		t.Fatalf("usefulness cap kept=%+v group=%+v", kept, group)
	}
}
