package dependencies

import (
	"context"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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
		inventory.Declarations[0].CheckStatus != "behind" ||
		inventory.Declarations[0].VersionDistance != "major" ||
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

func TestDefaultDependencyClientHonorsSSLCertFileForRegistriesAndOSV(t *testing.T) {
	requests := make(map[string]int)
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests[request.URL.Path]++
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/packages/marked":
			_, _ = io.WriteString(response, `{"dist-tags":{"latest":"16.4.1"},"versions":{"16.4.1":{}}}`)
		case "/v1/querybatch":
			_, _ = io.WriteString(response, `{"results":[]}`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	certificate := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: server.Certificate().Raw,
	})
	certificateFile := t.TempDir() + "/warp-root.pem"
	if err := os.WriteFile(certificateFile, certificate, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSL_CERT_FILE", certificateFile)
	t.Setenv("SSL_CERT_DIR", "")

	service := NewService(context.Background(), &observationMemoryStore{}, nil)
	service.UseRegistries([]RegistryConfig{{
		Ecosystem:           "npm",
		BaseURL:             server.URL,
		MetadataURLTemplate: server.URL + "/packages/{package}",
		PackagePrefixes:     []string{"marked"},
	}})
	observation := service.lookup(context.Background(), RegistryKey{
		Ecosystem: "npm",
		Registry:  server.URL,
		Package:   "marked",
	}, Observation{})
	if observation.Status != "ok" || observation.LatestStable != "16.4.1" {
		t.Fatalf("registry observation = %#v", observation)
	}

	service.advisoryBaseURL = server.URL
	if _, err := service.queryAdvisoryBatch(t.Context(), []osvBatchQuery{{
		Package: OSVPackage{Ecosystem: "npm", Name: "marked"},
	}}); err != nil {
		t.Fatalf("OSV query through SSL_CERT_FILE trust failed: %v", err)
	}
	if requests["/packages/marked"] != 1 || requests["/v1/querybatch"] != 1 {
		t.Fatalf("TLS requests = %#v", requests)
	}
}

func TestDefaultDependencyClientReportsInvalidSSLCertFile(t *testing.T) {
	t.Setenv("SSL_CERT_FILE", t.TempDir()+"/missing.pem")
	t.Setenv("SSL_CERT_DIR", "")
	request, err := http.NewRequest(http.MethodGet, "https://registry.example", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewService(context.Background(), &observationMemoryStore{}, nil).client.Do(request)
	if err == nil || !strings.Contains(err.Error(), "configure dependency TLS trust") ||
		!strings.Contains(err.Error(), "SSL_CERT_FILE") {
		t.Fatalf("invalid SSL_CERT_FILE error = %v", err)
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

func TestAdditionalRegistryMetadataParsing(t *testing.T) {
	latest, err := pypiLatestStable([]byte(
		`{"info":{"version":"3.0.0rc1"},"releases":{"2.10.0":[],"3.0.0rc1":[]}}`,
	))
	if err != nil || latest != "2.10.0" {
		t.Fatalf("PyPI latest = %q, %v", latest, err)
	}
	latest, err = pypiLatestStable([]byte(
		`{"info":{"version":"2.9.0"},"releases":{"2.9.0":[],"10.0.0":[]}}`,
	))
	if err != nil || latest != "2.9.0" {
		t.Fatalf("PyPI selected latest = %q, %v", latest, err)
	}
	latest, err = cargoLatestStable([]byte(
		`{"crate":{"max_stable_version":"1.82.0","max_version":"2.0.0-beta.1"}}`,
	))
	if err != nil || latest != "1.82.0" {
		t.Fatalf("Cargo latest = %q, %v", latest, err)
	}
	latest, err = goLatestStable([]byte("v1.9.0\nv1.10.2\nv1.11.0-rc.1\n"))
	if err != nil || latest != "v1.10.2" {
		t.Fatalf("Go latest = %q, %v", latest, err)
	}
	latest, err = nugetLatestStable([]byte(
		`{"versions":["8.0.1","9.0.0-preview.1","8.1.0"]}`,
	))
	if err != nil || latest != "8.1.0" {
		t.Fatalf("NuGet latest = %q, %v", latest, err)
	}
	if escaped := escapeGoModulePath("github.com/Azure/azure-sdk-for-go"); escaped !=
		"github.com/!azure/azure-sdk-for-go" {
		t.Fatalf("escaped Go module = %q", escaped)
	}
}

func TestDeclarationStatusUsesResolvedVersionAndDistinguishesAhead(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		declaration Declaration
		latest      string
		want        string
	}{
		{
			name:        "lockfile current",
			declaration: Declaration{Declared: "^2", Resolved: "2.3.0", Resolution: "constraint"},
			latest:      "2.3.0",
			want:        "current",
		},
		{
			name:        "unresolved constraint",
			declaration: Declaration{Declared: "^2", Resolution: "constraint"},
			latest:      "2.3.0",
			want:        "unresolved",
		},
		{
			name:        "ahead",
			declaration: Declaration{Declared: "3.0.0", Resolution: "exact"},
			latest:      "2.3.0",
			want:        "ahead",
		},
		{
			name:        "prerelease",
			declaration: Declaration{Declared: "3.0.0-rc.1", Resolution: "exact"},
			latest:      "2.3.0",
			want:        "prerelease",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := declarationStatus(testCase.declaration, testCase.latest); got != testCase.want {
				t.Fatalf("status = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestPrivateLookingPackageIsNotSentToPublicRegistryWithoutExplicitRoute(t *testing.T) {
	requests := 0
	client := &http.Client{Transport: dependencyRoundTripper(func(request *http.Request) (*http.Response, error) {
		requests++
		return nil, errors.New("unexpected public registry request")
	})}
	service := NewService(context.Background(), &observationMemoryStore{}, client)
	snapshot := dependencySnapshot(graph.DependencyDeclaration{
		Ecosystem: "npm", Package: "@acme/widget", Declared: "1.0.0",
		Resolution: "exact", Usage: "production",
	})
	progress, err := service.StartRefresh(snapshot, Options{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if progress.Total != 0 || progress.Skipped != 1 || requests != 0 {
		t.Fatalf("refresh = %#v, requests = %d", progress, requests)
	}
	inventory, err := service.Inventory(context.Background(), snapshot, Options{})
	if err != nil {
		t.Fatal(err)
	}
	declaration := inventory.Declarations[0]
	if declaration.CheckStatus != "private_internal" ||
		!strings.Contains(declaration.CheckDetail, "not checked publicly") ||
		inventory.PrivateCount != 1 {
		t.Fatalf("private declaration = %#v; inventory = %#v", declaration, inventory)
	}
}

func TestExplicitPublicPrefixRouteAllowsScopedPackage(t *testing.T) {
	configs, err := ParseRegistryConfigs(`[{
  "ecosystem": "npm",
  "base_url": "https://registry.npmjs.org",
  "metadata_url_template": "https://registry.npmjs.org/{package}",
  "package_prefixes": ["@types/"]
}]`)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(context.Background(), &observationMemoryStore{}, nil)
	service.UseRegistries(configs)
	decision := service.registryDecisionFor(Declaration{
		Ecosystem: "npm", Package: "@types/node", Declared: "24.0.0", Resolution: "exact",
	})
	if decision.Status != "" || decision.Key.Registry != PublicNPMRegistry {
		t.Fatalf("public allowlist decision = %#v", decision)
	}
}

func TestPrivateRegistryConfigurationRoutesPrefixAndReadsTokenAtRequestTime(t *testing.T) {
	configs, err := ParseRegistryConfigs(`[{
  "ecosystem": "npm",
  "base_url": "https://npm.example.com",
  "metadata_url_template": "https://npm.example.com/{package}",
  "package_prefixes": ["@acme/"],
  "token_env": "REPOKARTA_TEST_NPM_TOKEN"
}]`)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("REPOKARTA_TEST_NPM_TOKEN", "secret-token")
	storage := &observationMemoryStore{}
	requests := 0
	client := &http.Client{Transport: dependencyRoundTripper(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.URL.Host != "npm.example.com" ||
			request.Header.Get("Authorization") != "Bearer secret-token" {
			t.Fatalf("private registry request = %s, auth = %q",
				request.URL, request.Header.Get("Authorization"))
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				`{"dist-tags":{"latest":"2.0.0"},"versions":{"2.0.0":{}}}`,
			)),
			Request: request,
		}, nil
	})}
	service := NewService(context.Background(), storage, client)
	service.UseRegistries(configs)
	progress, err := service.StartRefresh(dependencySnapshot(graph.DependencyDeclaration{
		Ecosystem: "npm", Package: "@acme/widget", Declared: "1.0.0",
		Resolution: "exact", Usage: "production",
	}), Options{}, false)
	if err != nil || progress.Total != 1 {
		t.Fatalf("private refresh = %#v, %v", progress, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for service.Progress().State == "running" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if requests != 1 || service.Progress().Failed != 0 {
		t.Fatalf("private requests = %d, progress = %#v", requests, service.Progress())
	}
	observations, err := storage.ListDependencyObservations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 1 || observations[0].Registry != "https://npm.example.com" {
		t.Fatalf("private observations = %#v", observations)
	}
}

func TestPrivateRegistryConfigurationRejectsUnsafeOrSecretValues(t *testing.T) {
	for _, input := range []string{
		`[{"ecosystem":"npm","base_url":"http://registry.example.com","metadata_url_template":"http://registry.example.com/{package}","package_prefixes":["@acme/"]}]`,
		`[{"ecosystem":"npm","base_url":"https://registry.example.com","metadata_url_template":"https://registry.example.com/{package}","package_prefixes":["@acme/"],"token_env":"literal secret"}]`,
		`[{"ecosystem":"npm","base_url":"https://registry.example.com","metadata_url_template":"https://registry.example.com/static","package_prefixes":["@acme/"]}]`,
		`[{"ecosystem":"npm","base_url":"https://registry.example.com","metadata_url_template":"https://tokens.example.com/{package}","package_prefixes":["@acme/"],"token_env":"ACME_TOKEN"}]`,
	} {
		if _, err := ParseRegistryConfigs(input); err == nil {
			t.Fatalf("unsafe registry config accepted: %s", input)
		}
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
