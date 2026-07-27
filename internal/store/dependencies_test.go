package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/spolnik/RepoKarta/internal/dependencies"
)

func TestDependencyObservationRoundTrip(t *testing.T) {
	storage, err := Open(filepath.Join(t.TempDir(), "dependencies.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	now := time.Date(2026, time.July, 26, 12, 30, 0, 123, time.UTC)
	observation := dependencies.Observation{
		RegistryKey: dependencies.RegistryKey{
			Ecosystem: "npm",
			Registry:  dependencies.PublicNPMRegistry,
			Package:   "marked",
		},
		LatestStable: "17.0.0",
		Status:       "ok",
		ETag:         `"registry-etag"`,
		ObservedAt:   now,
		ExpiresAt:    now.Add(24 * time.Hour),
	}
	if err := storage.UpsertDependencyObservation(context.Background(), observation); err != nil {
		t.Fatal(err)
	}
	observations, err := storage.ListDependencyObservations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 1 || observations[0].Package != "marked" ||
		observations[0].LatestStable != "17.0.0" ||
		!observations[0].ObservedAt.Equal(now) {
		t.Fatalf("observations = %#v", observations)
	}
}

func TestRuntimeTopologyObservationRoundTripAndRetention(t *testing.T) {
	storage, err := Open(filepath.Join(t.TempDir(), "topology.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	now := time.Date(2026, time.July, 27, 12, 30, 0, 0, time.UTC)
	observations := []dependencies.RuntimeTopologyObservation{
		{
			Provider: "tempo", Environment: "prod",
			SourceName: "checkout", SourceKind: "service",
			TargetName: "orders", TargetKind: "service",
			Protocol: "http", Interaction: "calls", Transport: "https",
			ObservedFrom: now.Add(-time.Hour), ObservedTo: now,
			RequestCount: 120, ErrorCount: 3, LatencyP95MS: 42.5, ImportedAt: now,
		},
		{
			Provider: "tempo", Environment: "prod",
			SourceName: "legacy", SourceKind: "service",
			TargetName: "database", TargetKind: "database",
			Protocol: "database", Interaction: "reads_writes", Transport: "postgresql",
			ObservedFrom: now.Add(-100 * 24 * time.Hour),
			ObservedTo:   now.Add(-99 * 24 * time.Hour),
			RequestCount: 1, ImportedAt: now,
		},
	}
	if err := storage.UpsertRuntimeTopologyObservations(
		context.Background(), observations, now.Add(-90*24*time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	got, err := storage.ListRuntimeTopologyObservations(
		context.Background(), now.Add(-24*time.Hour), now.Add(time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SourceName != "checkout" ||
		got[0].RequestCount != 120 || got[0].LatencyP95MS != 42.5 {
		t.Fatalf("runtime observations = %#v", got)
	}
}
