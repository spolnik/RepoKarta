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
