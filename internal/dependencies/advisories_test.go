package dependencies

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/spolnik/RepoKarta/internal/graph"
)

func TestAffectedVersionUsesEcosystemOrderingAtRangeBoundaries(t *testing.T) {
	tests := []struct {
		name       string
		ecosystem  string
		introduced string
		fixed      string
		before     string
		first      string
		last       string
		after      string
	}{
		{name: "npm", ecosystem: "npm", introduced: "1.2.0", fixed: "1.3.0", before: "1.1.9", first: "1.2.0", last: "1.2.99", after: "1.3.0"},
		{name: "PyPI", ecosystem: "pypi", introduced: "1.2rc1", fixed: "1.2", before: "1.1.9", first: "1.2rc1", last: "1.2rc2", after: "1.2"},
		{name: "Go", ecosystem: "go", introduced: "v1.2.0", fixed: "v1.3.0", before: "v1.1.9", first: "v1.2.0", last: "v1.2.9", after: "v1.3.0"},
		{name: "crates.io", ecosystem: "cargo", introduced: "1.2.0", fixed: "1.3.0", before: "1.1.9", first: "1.2.0", last: "1.2.9", after: "1.3.0"},
		{name: "NuGet", ecosystem: "nuget", introduced: "1.2.0", fixed: "1.3.0", before: "1.1.9", first: "1.2.0", last: "1.2.9", after: "1.3.0"},
		{
			name: "Maven service-pack qualifier trap", ecosystem: "maven",
			introduced: "1.0.0", fixed: "1.0.1", before: "1.0.0-rc1",
			first: "1.0.0", last: "1.0.0-sp", after: "1.0.1",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			affected := OSVAffected{Ranges: []OSVRange{{
				Type:   "ECOSYSTEM",
				Events: []OSVEvent{{Introduced: test.introduced}, {Fixed: test.fixed}},
			}}}
			for version, want := range map[string]bool{
				test.before: false, test.first: true, test.last: true, test.after: false,
			} {
				got, _, fixed := affectedVersion(test.ecosystem, version, affected)
				if got != want {
					t.Fatalf("%s affected = %t, want %t", version, got, want)
				}
				if got && fixed != test.fixed {
					t.Fatalf("%s fixed = %q, want %q", version, fixed, test.fixed)
				}
			}
		})
	}
}

func TestGoVersionMatchesOSVSemverRangeWithoutLeadingV(t *testing.T) {
	affected := OSVAffected{Ranges: []OSVRange{{
		Type:   "SEMVER",
		Events: []OSVEvent{{Introduced: "1.2.0"}, {Fixed: "1.3.0"}},
	}}}
	if matched, _, fixed := affectedVersion("go", "v1.2.5", affected); !matched || fixed != "1.3.0" {
		t.Fatalf("Go SEMVER match=%t fixed=%q", matched, fixed)
	}
	if matched, _, _ := affectedVersion("go", "v1.3.0", affected); matched {
		t.Fatal("fixed Go version matched SEMVER range")
	}
}

func TestFindingsPreferResolvedVersionsAndPreserveTriageScope(t *testing.T) {
	inventory := findingInventory(
		graph.DependencyDeclaration{
			Ecosystem: "npm", Package: "left-pad", Declared: "^9.0.0",
			Resolved: "1.5.0", Resolution: "exact", ResolutionSource: "package-lock.json",
			Usage: "production", Relationship: "required", DeclaredScope: "implementation",
			Evidence: findingEvidence("package.json", 12),
		},
		graph.DependencyDeclaration{
			Ecosystem: "npm", Package: "left-pad", Declared: "1.5.0",
			Resolution: "exact", Usage: "test", Relationship: "required",
			DeclaredScope: "devDependencies", Evidence: findingEvidence("test/package.json", 4),
		},
	)
	advisories := fixtureSnapshot(inventory, OSVAdvisory{
		ID: "GHSA-test-0001", Aliases: []string{"CVE-2026-0001"},
		Summary: "Fixture advisory", DatabaseSpecific: map[string]any{"severity": "CRITICAL"},
		Affected: []OSVAffected{{
			Package: OSVPackage{Ecosystem: "npm", Name: "left-pad"},
			Ranges: []OSVRange{{Type: "ECOSYSTEM", Events: []OSVEvent{
				{Introduced: "1.0.0"}, {Fixed: "2.0.0"},
			}}},
		}},
	})
	response := buildFindings(inventory, &advisories, []Observation{{
		RegistryKey: RegistryKey{Ecosystem: "npm", Package: "left-pad"},
		Status:      "ok", LatestStable: "3.0.0",
	}}, AdvisoryOptions{}, time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC))
	if response.CheckState != "ready" || response.CheckedDeclarationCount != 2 ||
		len(response.Findings) != 1 ||
		response.Findings[0].ManifestOccurrenceCount != 2 {
		t.Fatalf("response = %#v", response)
	}
	resolved := response.Findings[0]
	if resolved.Version != "1.5.0" || resolved.MatchBasis != "resolved" ||
		resolved.MatchConfidence != "high" || resolved.ResolutionSource != "package-lock.json" ||
		resolved.Usage != "production" || resolved.DeclaredScope != "implementation" ||
		resolved.FixedVersion != "2.0.0" || resolved.LatestStable != "3.0.0" {
		t.Fatalf("resolved finding = %#v", resolved)
	}
	declared := resolved.Occurrences[1]
	if declared.MatchBasis != "declared" || declared.MatchConfidence != "lower" ||
		declared.Usage != "test" || declared.DeclaredScope != "devDependencies" {
		t.Fatalf("declared occurrence = %#v", declared)
	}
}

func TestGradleCatalogVulnerabilityCitesExactLibraryEntry(t *testing.T) {
	inventory := graph.Snapshot{
		Repositories: []graph.Repository{{ID: 1, Name: "service", Revision: "abc123"}},
		Manifests: []graph.Manifest{{
			RepositoryID: 1,
			Repository:   "service",
			Kind:         "Gradle project",
			Path:         "build.gradle",
			Declarations: []graph.DependencyDeclaration{{
				Ecosystem:  "maven",
				Package:    "org.apache.kafka:kafka-clients",
				Declared:   "3.9.0",
				Resolution: "exact",
				Usage:      "production",
				Evidence: findingEvidence(
					"gradle/libs.versions.toml",
					17,
				),
			}},
		}},
	}
	advisories := fixtureSnapshot(
		inventory,
		OSVAdvisory{
			ID:               "GHSA-gradle-catalog",
			DatabaseSpecific: map[string]any{"severity": "HIGH"},
			Affected: []OSVAffected{{
				Package: OSVPackage{
					Ecosystem: "Maven",
					Name:      "org.apache.kafka:kafka-clients",
				},
				Ranges: []OSVRange{{
					Type: "ECOSYSTEM",
					Events: []OSVEvent{
						{Introduced: "3.0.0"},
						{Fixed: "4.0.0"},
					},
				}},
			}},
		},
	)
	response := buildFindings(
		inventory, &advisories, nil, AdvisoryOptions{}, advisories.RetrievedAt,
	)
	if len(response.Findings) != 1 ||
		response.Findings[0].ManifestPath != "gradle/libs.versions.toml" ||
		response.Findings[0].ManifestEvidence.Path != "gradle/libs.versions.toml" ||
		response.Findings[0].ManifestEvidence.Line != 17 {
		t.Fatalf("Gradle catalog finding cites the accessor instead of its declaration: %#v", response.Findings)
	}
}

func TestFindingsAreByteDeterministicForSnapshotAndInventory(t *testing.T) {
	first := graph.DependencyDeclaration{
		Ecosystem: "npm", Package: "alpha", Resolved: "1.0.0",
		Resolution: "exact", Usage: "production", Evidence: findingEvidence("package-lock.json", 7),
	}
	second := graph.DependencyDeclaration{
		Ecosystem: "npm", Package: "beta", Resolved: "2.0.0",
		Resolution: "exact", Usage: "build", Evidence: findingEvidence("package-lock.json", 9),
	}
	inventoryA := findingInventory(second, first)
	inventoryB := findingInventory(first, second)
	snapshot := fixtureSnapshot(inventoryA,
		fixtureAdvisory("GHSA-beta", "beta", "1.0.0", "3.0.0"),
		fixtureAdvisory("GHSA-alpha", "alpha", "0.5.0", "1.1.0"),
	)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	left, _ := json.Marshal(buildFindings(inventoryA, &snapshot, nil, AdvisoryOptions{}, now).Findings)
	right, _ := json.Marshal(buildFindings(inventoryB, &snapshot, nil, AdvisoryOptions{}, now).Findings)
	if !bytes.Equal(left, right) {
		t.Fatalf("findings differ:\n%s\n%s", left, right)
	}
}

func TestFindingsDeduplicateManifestOccurrencesByAdvisoryRepositoryPackageAndVersion(t *testing.T) {
	inventory := findingInventory(
		graph.DependencyDeclaration{
			Ecosystem: "npm", Package: "left-pad", Resolved: "1.5.0",
			Resolution: "exact", Usage: "production",
			Evidence: findingEvidence("apps/api/package-lock.json", 17),
		},
		graph.DependencyDeclaration{
			Ecosystem: "npm", Package: "left-pad", Resolved: "1.5.0",
			Resolution: "exact", Usage: "development",
			Evidence: findingEvidence("apps/admin/package-lock.json", 29),
		},
	)
	snapshot := fixtureSnapshot(
		inventory, fixtureAdvisory("GHSA-left-pad", "left-pad", "1.0.0", "2.0.0"),
	)
	response := buildFindings(
		inventory, &snapshot, nil, AdvisoryOptions{}, snapshot.RetrievedAt,
	)
	if response.TotalFindingCount != 1 ||
		response.TotalManifestOccurrenceCount != 2 ||
		len(response.Findings) != 1 ||
		response.Findings[0].ManifestOccurrenceCount != 2 ||
		len(response.Findings[0].Occurrences) != 2 {
		t.Fatalf("deduplicated response = %#v", response)
	}
	finding := response.Findings[0]
	if finding.Usage != "production" ||
		finding.ManifestPath != "apps/api/package-lock.json" ||
		finding.Occurrences[0].ManifestPath != "apps/api/package-lock.json" ||
		finding.Occurrences[1].ManifestPath != "apps/admin/package-lock.json" {
		t.Fatalf("group representative or occurrence order = %#v", finding)
	}
	development := buildFindings(
		inventory, &snapshot, nil,
		AdvisoryOptions{Usage: "development"}, snapshot.RetrievedAt,
	)
	if len(development.Findings) != 1 ||
		development.Findings[0].ManifestOccurrenceCount != 2 {
		t.Fatalf("occurrence-aware usage filter = %#v", development.Findings)
	}
}

func TestFindingsExposeUncoveredAndUnresolvableDeclarations(t *testing.T) {
	inventory := findingInventory(
		graph.DependencyDeclaration{
			Ecosystem: "npm", Package: "missing-version", Declared: "^1.0.0",
			Resolution: "constraint", Usage: "production", Evidence: findingEvidence("package.json", 3),
		},
		graph.DependencyDeclaration{
			Ecosystem: "unknown", Package: "native-lib", Declared: "1.0",
			Resolution: "exact", Usage: "production", Evidence: findingEvidence("deps.txt", 1),
		},
	)
	snapshot := fixtureSnapshot(inventory)
	response := buildFindings(inventory, &snapshot, nil, AdvisoryOptions{}, time.Now())
	if response.CheckState != "partial" || response.SkippedNoVersionCount != 1 ||
		len(response.UncoveredEcosystems) != 1 ||
		response.UncoveredEcosystems[0].Ecosystem != "unknown" ||
		response.TotalFindingCount != 0 || response.CheckedDeclarationCount != 0 {
		t.Fatalf("honesty response = %#v", response)
	}
}

func TestZeroFindingsDistinguishesCheckedFromUnavailable(t *testing.T) {
	inventory := findingInventory(graph.DependencyDeclaration{
		Ecosystem: "npm", Package: "safe", Resolved: "1.0.0",
		Resolution: "exact", Usage: "production", Evidence: findingEvidence("package-lock.json", 1),
	})
	unavailable := buildFindings(inventory, nil, nil, AdvisoryOptions{}, time.Now())
	snapshot := fixtureSnapshot(inventory)
	checked := buildFindings(inventory, &snapshot, nil, AdvisoryOptions{}, snapshot.RetrievedAt)
	if unavailable.CheckState != "unavailable" || checked.CheckState != "ready" ||
		checked.CheckedDeclarationCount != 1 || checked.TotalFindingCount != 0 {
		t.Fatalf("unavailable=%#v checked=%#v", unavailable, checked)
	}
}

func TestAdvisoryRefreshPersistsHydratedSnapshot(t *testing.T) {
	requests := 0
	client := &http.Client{Transport: dependencyRoundTripper(func(request *http.Request) (*http.Response, error) {
		requests++
		body := ""
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/v1/querybatch":
			body = `{"results":[{"vulns":[{"id":"GHSA-fixture","modified":"2026-07-27T00:00:00Z"}]}]}`
		case request.Method == http.MethodGet && request.URL.Path == "/v1/vulns/GHSA-fixture":
			body = `{"schema_version":"1.7.0","id":"GHSA-fixture","modified":"2026-07-27T00:00:00Z","summary":"fixture","affected":[{"package":{"ecosystem":"npm","name":"left-pad"},"ranges":[{"type":"ECOSYSTEM","events":[{"introduced":"0"},{"fixed":"2.0.0"}]}]}]}`
		default:
			t.Fatalf("unexpected OSV request: %s %s", request.Method, request.URL)
		}
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(body)), Request: request,
		}, nil
	})}
	service := NewService(context.Background(), &observationMemoryStore{}, client)
	if err := service.UseAdvisoryDirectory(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	service.advisoryBaseURL = "https://osv.example"
	inventory := findingInventory(graph.DependencyDeclaration{
		Ecosystem: "npm", Package: "left-pad", Resolved: "1.5.0",
		Resolution: "exact", Usage: "production", Evidence: findingEvidence("package-lock.json", 1),
	})
	if _, err := service.StartAdvisoryRefresh(inventory, true); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for service.AdvisoryProgress().State == "running" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	progress := service.AdvisoryProgress()
	if progress.State != "complete" || progress.PackageCompleted != 1 ||
		progress.AdvisoryCompleted != 1 || requests != 2 {
		t.Fatalf("progress=%#v requests=%d", progress, requests)
	}
	snapshot, err := service.readAdvisorySnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Version == "" || len(snapshot.Advisories) != 1 ||
		snapshot.Advisories[0].ID != "GHSA-fixture" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestFindingsSARIFRetainsRevisionAndAdvisoryEvidence(t *testing.T) {
	inventory := findingInventory(graph.DependencyDeclaration{
		Ecosystem: "npm", Package: "left-pad", Resolved: "1.5.0",
		Resolution: "exact", Usage: "production", Evidence: findingEvidence("package-lock.json", 1),
	})
	snapshot := fixtureSnapshot(inventory, fixtureAdvisory("GHSA-sarif", "left-pad", "0", "2.0.0"))
	findings := buildFindings(inventory, &snapshot, nil, AdvisoryOptions{}, snapshot.RetrievedAt)
	content, err := FindingsSARIF(findings, "0.67.0-dev")
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		`"version": "2.1.0"`, `"ruleId": "GHSA-sarif"`,
		`"revision": "abc123"`, `"repoKarta_advisory_only": true`,
		`"repoKarta_ci_gate": false`, `"manifest_evidence_url"`,
		`"snapshot_version"`,
	} {
		if !bytes.Contains(content, []byte(expected)) {
			t.Fatalf("SARIF lacks %s:\n%s", expected, content)
		}
	}
}

func findingInventory(declarations ...graph.DependencyDeclaration) graph.Snapshot {
	return graph.Snapshot{
		Repositories: []graph.Repository{{ID: 1, Name: "service", Revision: "abc123"}},
		Manifests: []graph.Manifest{{
			RepositoryID: 1, Repository: "service", Kind: "npm package",
			Path: "package.json", Declarations: declarations,
		}},
	}
}

func findingEvidence(path string, line int) graph.Evidence {
	return graph.Evidence{
		RepositoryID: 1, Repository: "service", Revision: "abc123",
		Path: path, Line: line, URL: "http://ui/source/service?rev=abc123&path=" + path,
	}
}

func fixtureSnapshot(inventory graph.Snapshot, advisories ...OSVAdvisory) AdvisorySnapshot {
	packages := advisoryPackages(normalizedDeclarations(inventory))
	snapshot := AdvisorySnapshot{
		SchemaVersion: AdvisorySnapshotSchema, Source: "OSV.dev", SourceURL: PublicOSVAPI,
		RetrievedAt: time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC),
		QueryDigest: advisoryQueryDigest(packages), Packages: packages, Advisories: advisories,
	}
	snapshot.Version = snapshotVersion(snapshot)
	return snapshot
}

func fixtureAdvisory(id, pkg, introduced, fixed string) OSVAdvisory {
	return OSVAdvisory{
		ID: id, Summary: id, DatabaseSpecific: map[string]any{"severity": "HIGH"},
		Affected: []OSVAffected{{
			Package: OSVPackage{Ecosystem: "npm", Name: pkg},
			Ranges: []OSVRange{{Type: "ECOSYSTEM", Events: []OSVEvent{
				{Introduced: introduced}, {Fixed: fixed},
			}}},
		}},
	}
}
