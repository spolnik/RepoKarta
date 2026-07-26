package dependencies

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spolnik/RepoKarta/internal/graph"
)

type observationMemoryStore struct {
	mu           sync.Mutex
	observations map[string]Observation
}

func (s *observationMemoryStore) ListDependencyObservations(context.Context) ([]Observation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	output := make([]Observation, 0, len(s.observations))
	for _, observation := range s.observations {
		output = append(output, observation)
	}
	return output, nil
}

func (s *observationMemoryStore) UpsertDependencyObservation(_ context.Context, observation Observation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.observations == nil {
		s.observations = make(map[string]Observation)
	}
	s.observations[registryKeyString(observation.RegistryKey)] = observation
	return nil
}

type dependencyRoundTripper func(*http.Request) (*http.Response, error)

func (transport dependencyRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestInventoryJoinsFreshObservationsWithoutNetwork(t *testing.T) {
	now := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	key := RegistryKey{Ecosystem: "npm", Registry: PublicNPMRegistry, Package: "marked"}
	storage := &observationMemoryStore{observations: map[string]Observation{
		registryKeyString(key): {
			RegistryKey:  key,
			LatestStable: "17.0.0",
			Status:       "ok",
			ObservedAt:   now.Add(-time.Hour),
			ExpiresAt:    now.Add(time.Hour),
		},
	}}
	service := NewService(context.Background(), storage, nil)
	service.now = func() time.Time { return now }
	inventory, err := service.Inventory(context.Background(), dependencySnapshot(
		graph.DependencyDeclaration{
			Ecosystem: "npm", Package: "marked", Declared: "16.4.1",
			Resolution: "exact", Usage: "production", Relationship: "required",
		},
	), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if inventory.UpdateCount != 1 || inventory.UncheckedCount != 0 ||
		inventory.Declarations[0].CheckStatus != "update_available" ||
		inventory.Declarations[0].LatestStable != "17.0.0" {
		t.Fatalf("inventory = %#v", inventory)
	}
}

func TestRefreshDeduplicatesPackagesAndCachesNPMMetadata(t *testing.T) {
	storage := &observationMemoryStore{}
	requests := 0
	client := &http.Client{Transport: dependencyRoundTripper(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.Header.Get("Accept") != "application/vnd.npm.install-v1+json" {
			t.Fatalf("Accept = %q", request.Header.Get("Accept"))
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Etag": {`"npm-etag"`}},
			Body: io.NopCloser(strings.NewReader(
				`{"dist-tags":{"latest":"2.0.0"},"versions":{"1.0.0":{},"2.0.0":{}}}`,
			)),
			Request: request,
		}, nil
	})}
	service := NewService(context.Background(), storage, client)
	snapshot := dependencySnapshot(
		graph.DependencyDeclaration{
			Ecosystem: "npm", Package: "alpha", Declared: "1.0.0",
			Resolution: "exact", Usage: "production",
		},
		graph.DependencyDeclaration{
			Ecosystem: "npm", Package: "alpha", Declared: "^1.0.0",
			Resolution: "constraint", Usage: "development",
		},
	)
	progress, err := service.StartRefresh(snapshot, Options{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if progress.Total != 1 {
		t.Fatalf("initial progress = %#v", progress)
	}
	deadline := time.Now().Add(2 * time.Second)
	for service.Progress().State == "running" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	progress = service.Progress()
	if progress.State != "complete" || progress.Completed != 1 || progress.Failed != 0 {
		t.Fatalf("completed progress = %#v", progress)
	}
	if requests != 1 {
		t.Fatalf("registry requests = %d, want 1", requests)
	}
	observations, err := storage.ListDependencyObservations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 1 || observations[0].LatestStable != "2.0.0" ||
		observations[0].ETag != `"npm-etag"` {
		t.Fatalf("observations = %#v", observations)
	}
}

func TestRegistryMetadataParsing(t *testing.T) {
	latest, err := npmLatestStable([]byte(
		`{"dist-tags":{"latest":"3.0.0-beta.1"},"versions":{"1.9.0":{},"2.10.0":{},"3.0.0-beta.1":{}}}`,
	))
	if err != nil || latest != "2.10.0" {
		t.Fatalf("npm latest = %q, %v", latest, err)
	}
	latest, err = mavenLatestStable([]byte(
		`{"response":{"docs":[{"latestVersion":"6.2.1"}]}}`,
	))
	if err != nil || latest != "6.2.1" {
		t.Fatalf("Maven latest = %q, %v", latest, err)
	}
}

func dependencySnapshot(declarations ...graph.DependencyDeclaration) graph.Snapshot {
	return graph.Snapshot{
		Repositories: []graph.Repository{{ID: 1, Name: "service", Revision: "abc"}},
		Manifests: []graph.Manifest{{
			RepositoryID: 1,
			Repository:   "service",
			Kind:         "npm package",
			Path:         "package.json",
			Declarations: declarations,
		}},
	}
}
