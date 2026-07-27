package dependencies

import (
	"context"
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
	if topology.Components == nil || topology.Connections == nil ||
		topology.Protocols == nil || topology.Providers == nil || topology.Environments == nil {
		t.Fatalf("empty topology must serialize collections as arrays: %+v", topology)
	}
}
