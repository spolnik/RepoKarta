package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spolnik/RepoKarta/internal/analysis"
	"github.com/spolnik/RepoKarta/internal/catalog"
)

type graphStore struct {
	repository catalog.Repository
}

func (s graphStore) ListRepositories(context.Context) ([]catalog.Repository, error) {
	return []catalog.Repository{s.repository}, nil
}

type multiGraphStore struct {
	repositories []catalog.Repository
}

func (s multiGraphStore) ListRepositories(context.Context) ([]catalog.Repository, error) {
	return s.repositories, nil
}

func (s multiGraphStore) RepositoryByID(_ context.Context, id int64) (catalog.Repository, error) {
	for _, repository := range s.repositories {
		if repository.ID == id {
			return repository, nil
		}
	}
	return catalog.Repository{}, os.ErrNotExist
}

func (s graphStore) RepositoryByID(_ context.Context, id int64) (catalog.Repository, error) {
	if id == s.repository.ID {
		return s.repository, nil
	}
	return catalog.Repository{}, os.ErrNotExist
}

func TestEmptyRepositoryIsNotReportedAsPendingArtifactWork(t *testing.T) {
	repository := catalog.Repository{
		ID:         23,
		Name:       "empty",
		ScanState:  "empty",
		ScanError:  catalog.EmptyRepositoryReason,
		IndexState: "empty",
		IndexError: catalog.EmptyRepositoryReason,
	}
	service, err := New(
		graphStore{repository: repository},
		filepath.Join(t.TempDir(), "maps"),
		"http://127.0.0.1:7331",
	)
	if err != nil {
		t.Fatal(err)
	}

	progress, err := service.StructureProgress(t.Context(), repository.ID)
	if err != nil {
		t.Fatal(err)
	}
	if progress.State != "ready" ||
		progress.RequestedRepositories != 0 ||
		progress.PendingRepositories != 0 {
		t.Fatalf("empty repository artifact progress = %#v", progress)
	}

	_, dependencyProgress, err := service.ReadDependencySnapshot(t.Context(), repository.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dependencyProgress.RequestedRepositories != 0 ||
		dependencyProgress.PendingRepositories != 0 {
		t.Fatalf("empty repository dependency progress = %#v", dependencyProgress)
	}
	_, routeProgress, err := service.ReadRouteSnapshot(t.Context(), repository.ID)
	if err != nil {
		t.Fatal(err)
	}
	if routeProgress.RequestedRepositories != 0 ||
		routeProgress.PendingRepositories != 0 {
		t.Fatalf("empty repository route progress = %#v", routeProgress)
	}
}

func TestReadRouteSnapshotUsesOnlyPreparedArtifacts(t *testing.T) {
	revision := strings.Repeat("a", 40)
	repository := catalog.Repository{
		ID: 31, Name: "routes", Path: t.TempDir(),
		HeadCommit: revision, IndexedCommit: revision, IndexState: "ready",
	}
	directory := filepath.Join(t.TempDir(), "maps")
	service, err := New(graphStore{repository: repository}, directory, "http://127.0.0.1:7331")
	if err != nil {
		t.Fatal(err)
	}
	pending, progress, err := service.ReadRouteSnapshot(t.Context(), repository.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending.Nodes) != 0 || progress.PendingRepositories != 1 {
		t.Fatalf("unprepared route evidence = %#v, progress = %#v", pending, progress)
	}
	snapshot := Snapshot{
		Version: snapshotVersion,
		Repositories: []Repository{{
			ID: repository.ID, Name: repository.Name, Revision: revision,
		}},
		Nodes: []Node{
			{
				Kind: "route", Label: "GET /ready", RepositoryID: repository.ID,
				Evidence: []Evidence{{
					RepositoryID: repository.ID, Repository: repository.Name,
					Revision: revision, Path: "main.go", Line: 12,
				}},
			},
			{Kind: "component", Label: "server", RepositoryID: repository.ID},
		},
		Scope: Scope{Complete: true, TotalRepositories: 1, AnalyzedRepositories: 1},
	}
	content, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(service.repositorySnapshotPath(repository), content, 0o600); err != nil {
		t.Fatal(err)
	}
	ready, progress, err := service.ReadRouteSnapshot(t.Context(), repository.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ready.Nodes) != 1 || ready.Nodes[0].Kind != "route" ||
		progress.State != "ready" || progress.PendingRepositories != 0 ||
		ready.Nodes[0].Evidence[0].URL == "" {
		t.Fatalf("prepared route evidence = %#v, progress = %#v", ready, progress)
	}
}

func TestReadTopologySnapshotResolvesPlaceholderAcrossPreparedRepositoryArtifacts(t *testing.T) {
	repositories := []catalog.Repository{
		{ID: 71, Name: "checkout", IndexedCommit: strings.Repeat("a", 40)},
		{ID: 72, Name: "fleet-infra", IndexedCommit: strings.Repeat("b", 40)},
		{ID: 73, Name: "billing-service", IndexedCommit: strings.Repeat("c", 40)},
	}
	directory := filepath.Join(t.TempDir(), "maps")
	service, err := New(
		multiGraphStore{repositories: repositories},
		directory,
		"http://127.0.0.1:7331",
	)
	if err != nil {
		t.Fatal(err)
	}

	builders := []*builder{
		newBuilder("http://127.0.0.1:7331"),
		newBuilder("http://127.0.0.1:7331"),
		newBuilder("http://127.0.0.1:7331"),
	}
	builders[0].addDistributedTopology(
		repositories[0], repositories[0].IndexedCommit, "README.md",
		map[string][]byte{
			"README.md": []byte("# Checkout"),
			"src/main/resources/application.yml": []byte(`
routes:
  billing-service: ${BILLING_SERVICE_URL}
`),
		},
	)
	builders[1].addDistributedTopology(
		repositories[1], repositories[1].IndexedCommit, "README.md",
		map[string][]byte{
			"README.md": []byte("# Fleet infrastructure"),
			"deploy/production/values.yaml": []byte(`
BILLING_SERVICE_URL: http://billing-service.production.svc.cluster.local
`),
		},
	)
	builders[2].addDistributedTopology(
		repositories[2], repositories[2].IndexedCommit, "README.md",
		map[string][]byte{"README.md": []byte("# Billing")},
	)
	for index, builder := range builders {
		snapshot := builder.snapshot("repository-artifact")
		snapshot.Scope = Scope{
			Kind: "repository", Complete: true,
			TotalRepositories: 1, AnalyzedRepositories: 1,
			RequestedRepositoryID: repositories[index].ID,
		}
		content, marshalErr := json.Marshal(snapshot)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if writeErr := os.WriteFile(
			service.repositorySnapshotPath(repositories[index]), content, 0o600,
		); writeErr != nil {
			t.Fatal(writeErr)
		}
	}

	snapshot, progress, err := service.ReadTopologySnapshot(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	connection, found := placeholderConnection(snapshot.Connections, "BILLING_SERVICE_URL")
	if !found || connection.ResolutionTier != "map_key_registry" ||
		connection.Confidence != "high" || !connection.TargetResolved ||
		componentByID(snapshot.Components, connection.Target).Name != "billing-service" ||
		len(connection.Evidence) != 2 || progress.PendingRepositories != 0 {
		t.Fatalf("fleet artifact resolution = %+v, progress = %+v", connection, progress)
	}

	selected, selectedProgress, err := service.ReadTopologySnapshot(
		t.Context(), repositories[0].ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	selectedConnection, found := placeholderConnection(
		selected.Connections, "BILLING_SERVICE_URL",
	)
	if !found || selectedConnection.ResolutionTier != "map_key_registry" ||
		!selectedConnection.TargetResolved ||
		componentByID(selected.Components, selectedConnection.Target).Name != "billing-service" ||
		selected.Scope.TotalRepositories != 1 ||
		selectedProgress.RequestedRepositories != 1 {
		t.Fatalf(
			"repository-scoped fleet resolution = %+v, scope = %+v, progress = %+v",
			selectedConnection, selected.Scope, selectedProgress,
		)
	}
}

func TestStructuralIndexIsPreparedInBackgroundAndReadWithoutBuilding(t *testing.T) {
	root, revision := javaGraphFixture(t, map[string]string{
		"src/main/java/com/acme/PaymentJob.java": `package com.acme;
import com.acme.JobTimeGuard;
class PaymentJob {
    private final JobTimeGuard guard;
    void run() { guard.check(); }
}`,
	})
	repository := catalog.Repository{
		ID:            17,
		Name:          "payment-service",
		Path:          root,
		HeadCommit:    revision,
		IndexedCommit: revision,
		IndexState:    "ready",
	}
	directory := filepath.Join(t.TempDir(), "maps")
	service, err := New(graphStore{repository: repository}, directory, "http://127.0.0.1:7331")
	if err != nil {
		t.Fatal(err)
	}

	pending, err := service.ReadStructure(t.Context(), repository.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Scope.Complete || pending.Scope.AnalyzedRepositories != 0 ||
		pending.Scope.OmittedRepositories != 1 || len(pending.Structure) != 0 {
		t.Fatalf("unprepared structural index = %#v", pending)
	}
	if matches, err := filepath.Glob(filepath.Join(directory, "repository-*.json")); err != nil || len(matches) != 0 {
		t.Fatalf("read-only lookup built a map: matches=%v err=%v", matches, err)
	}

	if err := service.PrepareStructure(t.Context(), repository.ID); err != nil {
		t.Fatal(err)
	}
	ready, err := service.ReadStructure(t.Context(), repository.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ready.Scope.Complete || ready.Scope.AnalyzedRepositories != 1 ||
		ready.Scope.OmittedRepositories != 0 || len(ready.Structure) == 0 {
		t.Fatalf("prepared structural index = %#v", ready)
	}
	if !slices.ContainsFunc(ready.Structure[0].Relations, func(relation analysis.Relation) bool {
		return relation.Kind == "type" && relation.Target == "JobTimeGuard"
	}) {
		t.Fatalf("prepared relations = %#v", ready.Structure[0].Relations)
	}
	if !slices.ContainsFunc(ready.Structure[0].Symbols, func(symbol analysis.Symbol) bool {
		return symbol.Name == "PaymentJob"
	}) {
		t.Fatalf("prepared symbols = %#v", ready.Structure[0].Symbols)
	}
}

func TestStructuralIndexReadsEveryPreparedRepositoryInLargeFleet(t *testing.T) {
	const repositoryCount = 300
	repositories := make([]catalog.Repository, 0, repositoryCount)
	for index := 0; index < repositoryCount; index++ {
		repositories = append(repositories, catalog.Repository{
			ID:            int64(index + 1),
			Name:          fmt.Sprintf("service-%03d", index),
			IndexedCommit: fmt.Sprintf("%040d", index+1),
			IndexState:    "ready",
		})
	}
	service, err := New(
		multiGraphStore{repositories: repositories},
		filepath.Join(t.TempDir(), "maps"),
		"http://127.0.0.1:7331",
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, repository := range repositories {
		signature := snapshotSignature([]catalog.Repository{repository})
		if err := service.writeStructuralIndex(StructuralIndex{
			Version: snapshotVersion,
			ID:      signature,
			Structure: []StructuralDocument{{
				RepositoryID: repository.ID,
				Repository:   repository.Name,
				Revision:     repository.IndexedCommit,
				Path:         "src/Job.java",
				Language:     "java",
				Relations: []analysis.Relation{{
					Kind:   "type",
					Target: "JobTimeGuard",
					Range:  analysis.Range{StartLine: 1},
				}},
			}},
			Scope: Scope{
				Kind:                  "repository",
				Complete:              true,
				TotalRepositories:     1,
				AnalyzedRepositories:  1,
				RequestedRepositoryID: repository.ID,
			},
		}); err != nil {
			t.Fatal(err)
		}
	}

	index, err := service.ReadStructure(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !index.Scope.Complete || index.Scope.AnalyzedRepositories != repositoryCount ||
		index.Scope.OmittedRepositories != 0 || len(index.Structure) != repositoryCount {
		t.Fatalf("fleet structural index scope = %#v, documents = %d", index.Scope, len(index.Structure))
	}
}

func TestSnapshotBuildsEvidenceBackedInventoryAndDependencyGraph(t *testing.T) {
	root := t.TempDir()
	writeGraphFixture(t, root, "go.mod", `module example.com/acme

go 1.26

require github.com/google/uuid v1.6.0
`)
	writeGraphFixture(t, root, "cmd/server/main.go", `package main

import (
	"net/http"
	"example.com/acme/internal/service"
)

func main() {
	http.HandleFunc("/healthz", func(http.ResponseWriter, *http.Request) {})
	service.Run()
}
`)
	writeGraphFixture(t, root, "internal/service/service.go", `package service

func Run() {}
`)
	writeGraphFixture(t, root, "web/package.json", `{
  "name": "@acme/web",
  "dependencies": {
    "htmx.org": "2.0.10"
  },
  "devDependencies": {
    "vitest": "^4.0.0"
  },
  "optionalDependencies": {
    "fsevents": "2.3.3"
  },
  "peerDependencies": {
    "react": "^19.0.0"
  }
}
`)
	writeGraphFixture(t, root, "package-lock.json", `{
  "lockfileVersion": 3,
  "packages": {
    "": {},
    "node_modules/htmx.org": {"version": "2.0.10"},
    "node_modules/vitest": {"version": "4.0.1"}
  }
}
`)
	writeGraphFixture(t, root, "README.md", "# Acme\n")
	runGraphGit(t, root, "init")
	runGraphGit(t, root, "config", "user.email", "graph@example.com")
	runGraphGit(t, root, "config", "user.name", "Graph Test")
	runGraphGit(t, root, "add", ".")
	runGraphGit(t, root, "commit", "-m", "fixture")
	revision := strings.TrimSpace(runGraphGit(t, root, "rev-parse", "HEAD"))

	snapshotDirectory := filepath.Join(t.TempDir(), "maps")
	service, err := New(graphStore{repository: catalog.Repository{
		ID:            7,
		Name:          "acme",
		Path:          root,
		HeadCommit:    revision,
		IndexedCommit: revision,
		IndexState:    "ready",
	}}, snapshotDirectory, "http://127.0.0.1:7331")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.Snapshot(context.Background(), 7, false)
	if err != nil {
		t.Fatal(err)
	}

	if len(snapshot.Repositories) != 1 || snapshot.Repositories[0].Name != "acme" {
		t.Fatalf("repositories = %#v", snapshot.Repositories)
	}
	if snapshot.FileCount != 6 || len(snapshot.Languages) == 0 {
		t.Fatalf("inventory = files %d, languages %#v", snapshot.FileCount, snapshot.Languages)
	}
	if len(snapshot.Manifests) != 2 {
		t.Fatalf("manifests = %#v", snapshot.Manifests)
	}
	if !slices.ContainsFunc(snapshot.Manifests, func(manifest Manifest) bool {
		return manifest.Path == "go.mod" &&
			slices.ContainsFunc(manifest.Declarations, func(declaration DependencyDeclaration) bool {
				return declaration.Ecosystem == "go" &&
					declaration.Package == "github.com/google/uuid" &&
					declaration.Declared == "v1.6.0" &&
					declaration.Resolution == "exact" &&
					declaration.Resolved == "v1.6.0" &&
					declaration.ResolutionSource == "go.mod" &&
					declaration.Evidence.Line == 5
			})
	}) || !slices.ContainsFunc(snapshot.Manifests, func(manifest Manifest) bool {
		return manifest.Path == "web/package.json" &&
			slices.ContainsFunc(manifest.Declarations, func(declaration DependencyDeclaration) bool {
				return declaration.Ecosystem == "npm" &&
					declaration.Package == "htmx.org" &&
					declaration.Declared == "2.0.10" &&
					declaration.Resolution == "exact" &&
					declaration.Resolved == "2.0.10" &&
					declaration.ResolutionSource == "package-lock.json" &&
					declaration.Usage == "production" &&
					declaration.Relationship == "required" &&
					declaration.DeclaredScope == "dependencies" &&
					declaration.Evidence.Line == 4
			}) &&
			slices.ContainsFunc(manifest.Declarations, func(declaration DependencyDeclaration) bool {
				return declaration.Package == "vitest" &&
					declaration.Usage == "development" &&
					declaration.Relationship == "required" &&
					declaration.DeclaredScope == "devDependencies"
			}) &&
			slices.ContainsFunc(manifest.Declarations, func(declaration DependencyDeclaration) bool {
				return declaration.Package == "fsevents" &&
					declaration.Usage == "production" &&
					declaration.Relationship == "optional"
			}) &&
			slices.ContainsFunc(manifest.Declarations, func(declaration DependencyDeclaration) bool {
				return declaration.Package == "react" &&
					declaration.Usage == "production" &&
					declaration.Relationship == "peer"
			})
	}) {
		t.Fatalf("normalized declarations = %#v", snapshot.Manifests)
	}
	assertGraphNode(t, snapshot, "repository", "acme")
	assertGraphNode(t, snapshot, "package", "server")
	assertGraphNode(t, snapshot, "package", "service")
	assertGraphNode(t, snapshot, "dependency", "github.com/google/uuid")
	assertGraphNode(t, snapshot, "dependency", "htmx.org")
	assertGraphNode(t, snapshot, "route", "/healthz")
	assertGraphEdge(t, snapshot, "import", "imports")
	assertGraphEdge(t, snapshot, "route", "serves")
	if snapshot.Version != ArtifactVersion {
		t.Fatalf("snapshot version = %d, want %d", snapshot.Version, ArtifactVersion)
	}
	if !snapshot.Scope.Complete || snapshot.Scope.Kind != "repository" ||
		snapshot.Scope.TotalRepositories != 1 || snapshot.Scope.AnalyzedRepositories != 1 {
		t.Fatalf("repository scope = %#v", snapshot.Scope)
	}
	if snapshot.StructureTruncated {
		t.Fatal("small structural inventory was reported truncated")
	}
	if len(snapshot.Structure) != 2 {
		t.Fatalf("structural documents = %#v", snapshot.Structure)
	}
	if !slices.ContainsFunc(snapshot.Structure, func(document StructuralDocument) bool {
		return document.RepositoryID == 7 &&
			document.Revision == revision &&
			document.Path == "cmd/server/main.go" &&
			document.Language == "go" &&
			document.ParseComplete &&
			slices.ContainsFunc(document.Symbols, func(symbol analysis.Symbol) bool {
				return symbol.Kind == "function" && symbol.Name == "main"
			})
	}) {
		t.Fatalf("main structural document missing: %#v", snapshot.Structure)
	}

	for _, node := range snapshot.Nodes {
		if len(node.Evidence) == 0 || node.Evidence[0].Revision != revision || node.Evidence[0].URL == "" {
			t.Fatalf("node has incomplete evidence: %#v", node)
		}
	}
	for _, edge := range snapshot.Edges {
		if len(edge.Evidence) == 0 || edge.Evidence[0].Revision != revision || edge.Evidence[0].URL == "" {
			t.Fatalf("edge has incomplete evidence: %#v", edge)
		}
	}
	entries, err := os.ReadDir(snapshotDirectory)
	if err != nil || len(entries) != 2 ||
		!slices.ContainsFunc(entries, func(entry os.DirEntry) bool {
			return strings.HasPrefix(entry.Name(), "repository-7-") && filepath.Ext(entry.Name()) == ".json"
		}) ||
		!slices.ContainsFunc(entries, func(entry os.DirEntry) bool {
			return strings.HasPrefix(entry.Name(), "structure-repository-7-") && filepath.Ext(entry.Name()) == ".json"
		}) {
		t.Fatalf("snapshot files = %#v, err = %v", entries, err)
	}

	cached, err := service.Snapshot(context.Background(), 7, false)
	if err != nil {
		t.Fatal(err)
	}
	if cached.ID != snapshot.ID || !cached.GeneratedAt.Equal(snapshot.GeneratedAt) {
		t.Fatalf("snapshot was not loaded from cache: first %#v, cached %#v", snapshot.GeneratedAt, cached.GeneratedAt)
	}
}

func TestSnapshotRegeneratesUnsupportedCachedArtifact(t *testing.T) {
	root := t.TempDir()
	writeGraphFixture(t, root, "go.mod", "module example.com/upgrade\n\ngo 1.26\n")
	writeGraphFixture(t, root, "main.go", "package main\n\nfunc main() {}\n")
	runGraphGit(t, root, "init")
	runGraphGit(t, root, "config", "user.email", "graph@example.com")
	runGraphGit(t, root, "config", "user.name", "Graph Test")
	runGraphGit(t, root, "add", ".")
	runGraphGit(t, root, "commit", "-m", "fixture")
	revision := strings.TrimSpace(runGraphGit(t, root, "rev-parse", "HEAD"))
	directory := filepath.Join(t.TempDir(), "maps")
	service, err := New(graphStore{repository: catalog.Repository{
		ID: 15, Name: "upgrade", Path: root, HeadCommit: revision,
		IndexedCommit: revision, IndexState: "ready",
	}}, directory, "http://127.0.0.1:7331")
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.Snapshot(context.Background(), 15, false)
	if err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(directory, "repository-15-*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("cached files = %#v, error = %v", files, err)
	}
	unsupported := bytes.ReplaceAll(
		mustReadGraphFile(t, files[0]),
		[]byte(fmt.Sprintf(`"version": %d`, ArtifactVersion)),
		[]byte(`"version": 999`),
	)
	unsupported = append(unsupported, []byte("\nunsupported-marker")...)
	if err := os.WriteFile(files[0], unsupported, 0o600); err != nil {
		t.Fatal(err)
	}
	recovered, err := service.Snapshot(context.Background(), 15, false)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Version != ArtifactVersion || recovered.ID != first.ID {
		t.Fatalf("recovered snapshot = %+v", recovered)
	}
	content := mustReadGraphFile(t, files[0])
	if bytes.Contains(content, []byte("unsupported-marker")) ||
		!bytes.Contains(content, []byte(fmt.Sprintf(`"version": %d`, ArtifactVersion))) {
		t.Fatalf("unsupported cache was not regenerated: %s", content)
	}
}

func mustReadGraphFile(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func TestSnapshotExtractsGradleSpringRoutesAndServiceCalls(t *testing.T) {
	paymentRoot, paymentRevision := javaGraphFixture(t, map[string]string{
		"settings.gradle": `rootProject.name = "payment-service"`,
		"build.gradle": `plugins {
    id 'java'
}

dependencies {
    implementation 'org.springframework.boot:spring-boot-starter-web:4.0.1'
    implementation("com.fasterxml.jackson.core:jackson-databind:2.18.4")
    implementation(group = "net.javacrumbs.shedlock", name = "shedlock-spring", version = "6.3.0")
    implementation(libs.kafka.clients)
    testImplementation project(':contract-tests')
}`,
		"gradle/libs.versions.toml": `[versions]
kafka = "3.9.0"

[libraries]
kafka-clients = { module = "org.apache.kafka:kafka-clients", version.ref = "kafka" }
`,
		"src/main/java/com/acme/payment/PaymentController.java": `package com.acme.payment;

import org.springframework.web.bind.annotation.*;

@RestController
@RequestMapping("/payments")
public class PaymentController {
    @GetMapping("/{id}")
    public String get() { return "ok"; }

    @PostMapping
    public void create() {}
}`,
		"src/main/java/com/acme/payment/InventoryClient.java": `package com.acme.payment;

import org.springframework.cloud.openfeign.FeignClient;

@FeignClient(name = "inventory-service")
public interface InventoryClient {}
`,
		"src/main/java/com/acme/payment/ShippingClients.java": `/*
 * Licensed under the Apache License, Version 2.0
 * http://www.apache.org/licenses/LICENSE-2.0
 */
package com.acme.payment;

class ShippingClients {
    WebClient shipping() {
        return WebClient.builder().baseUrl("http://shipping-api:8080").build();
    }

    RestTemplate pricing(RestTemplateBuilder builder) {
        return builder.rootUri("http://${pricing.host}/api").build();
    }
}
`,
	})
	inventoryRoot, inventoryRevision := javaGraphFixture(t, map[string]string{
		"settings.gradle.kts": `rootProject.name = "inventory-service"`,
		"build.gradle.kts": `dependencies {
    implementation("org.springframework.boot:spring-boot-starter-web:4.0.1")
}`,
		"src/main/java/com/acme/inventory/Routes.java": `package com.acme.inventory;

class Routes {
    RouterFunction<?> routes() {
        return RouterFunctions.route(GET("/inventory/{sku}"), handler);
    }
}`,
	})
	// The shipping repository directory is not named after the service, so the
	// WebClient host can only resolve through spring.application.name.
	shippingRoot, shippingRevision := javaGraphFixture(t, map[string]string{
		"build.gradle": `dependencies {
    implementation 'org.springframework.boot:spring-boot-starter-web:4.0.1'
}`,
		"src/main/resources/application.yml": `server:
  port: 8080
spring:
  application:
    name: shipping-api
`,
	})
	store := multiGraphStore{repositories: []catalog.Repository{
		{
			ID:            11,
			Name:          "payment-service",
			Path:          paymentRoot,
			HeadCommit:    paymentRevision,
			IndexedCommit: paymentRevision,
			IndexState:    "ready",
		},
		{
			ID:            12,
			Name:          "inventory-service",
			Path:          inventoryRoot,
			HeadCommit:    inventoryRevision,
			IndexedCommit: inventoryRevision,
			IndexState:    "ready",
		},
		{
			ID:            13,
			Name:          "acme-shipping",
			Path:          shippingRoot,
			HeadCommit:    shippingRevision,
			IndexedCommit: shippingRevision,
			IndexState:    "ready",
		},
	}}
	service, err := New(store, filepath.Join(t.TempDir(), "maps"), "http://127.0.0.1:7331")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.Snapshot(context.Background(), 0, true)
	if err != nil {
		t.Fatal(err)
	}

	assertGraphNode(t, snapshot, "dependency", "com.fasterxml.jackson.core:jackson-databind")
	assertGraphNode(t, snapshot, "dependency", "net.javacrumbs.shedlock:shedlock-spring")
	assertGraphNode(t, snapshot, "dependency", "org.apache.kafka:kafka-clients")
	assertGraphNode(t, snapshot, "dependency", "project:contract-tests")
	assertGraphNode(t, snapshot, "route", "GET /payments/{id}")
	assertGraphNode(t, snapshot, "route", "POST /payments")
	assertGraphNode(t, snapshot, "route", "GET /inventory/{sku}")
	// The Feign interface in payment-service declares a consumed endpoint, so
	// only inventory-service may serve it.
	for _, node := range snapshot.Nodes {
		if node.Kind == "route" && node.Label == "GET /inventory/{sku}" &&
			node.RepositoryID != 12 {
			t.Fatalf("consumed endpoint became a served route: %#v", node)
		}
	}
	assertGraphEdge(t, snapshot, "service_call", "calls over HTTP")

	gradleManifest := manifestByPath(t, snapshot, "build.gradle")
	for _, coordinate := range []string{
		"com.fasterxml.jackson.core:jackson-databind:2.18.4",
		"net.javacrumbs.shedlock:shedlock-spring:6.3.0",
		"org.apache.kafka:kafka-clients:3.9.0",
		"org.springframework.boot:spring-boot-starter-web:4.0.1",
		"project:contract-tests",
	} {
		if !slices.Contains(gradleManifest.Dependencies, coordinate) {
			t.Fatalf("Gradle dependencies %v do not contain %q", gradleManifest.Dependencies, coordinate)
		}
	}
	for _, expected := range []struct {
		pkg  string
		path string
		line int
	}{
		{
			pkg:  "org.apache.kafka:kafka-clients",
			path: "gradle/libs.versions.toml",
			line: 5,
		},
		{
			pkg:  "com.fasterxml.jackson.core:jackson-databind",
			path: "build.gradle",
			line: 7,
		},
	} {
		declarationIndex := slices.IndexFunc(
			gradleManifest.Declarations,
			func(declaration DependencyDeclaration) bool {
				return declaration.Package == expected.pkg
			},
		)
		if declarationIndex < 0 {
			t.Fatalf("missing declaration for %s: %#v", expected.pkg, gradleManifest.Declarations)
		}
		evidence := gradleManifest.Declarations[declarationIndex].Evidence
		if evidence.Path != expected.path || evidence.Line != expected.line {
			t.Fatalf(
				"%s evidence = %s:%d, want %s:%d",
				expected.pkg, evidence.Path, evidence.Line, expected.path, expected.line,
			)
		}
	}
	calls := make(map[string]Evidence)
	for _, edge := range snapshot.Edges {
		if edge.Kind != "service_call" {
			continue
		}
		if edge.Source != "repository:11" {
			t.Fatalf("unexpected service call source in %#v", edge)
		}
		if len(edge.Evidence) != 1 {
			t.Fatalf("service call evidence = %#v", edge.Evidence)
		}
		calls[edge.Target] = edge.Evidence[0]
	}
	for target, wanted := range map[string]string{
		"repository:12": "src/main/java/com/acme/payment/InventoryClient.java",
		"repository:13": "src/main/java/com/acme/payment/ShippingClients.java",
	} {
		evidence, ok := calls[target]
		if !ok {
			t.Fatalf("missing service call to %s in %v", target, calls)
		}
		if evidence.Path != wanted {
			t.Fatalf("service call to %s cites %q, want %q", target, evidence.Path, wanted)
		}
		if evidence.Revision != paymentRevision || evidence.Line <= 0 {
			t.Fatalf("service call evidence to %s = %#v", target, evidence)
		}
	}
	// The Apache license URL and the interpolated RestTemplate host must not
	// invent additional relationships.
	if len(calls) != 2 {
		t.Fatalf("service calls = %v", calls)
	}
}

func javaGraphFixture(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		writeGraphFixture(t, root, name, content)
	}
	runGraphGit(t, root, "init")
	runGraphGit(t, root, "config", "user.email", "graph@example.com")
	runGraphGit(t, root, "config", "user.name", "Graph Test")
	runGraphGit(t, root, "add", ".")
	runGraphGit(t, root, "commit", "-m", "java fixture")
	return root, strings.TrimSpace(runGraphGit(t, root, "rev-parse", "HEAD"))
}

func manifestByPath(t *testing.T, snapshot Snapshot, filePath string) Manifest {
	t.Helper()
	for _, manifest := range snapshot.Manifests {
		if manifest.Path == filePath {
			return manifest
		}
	}
	t.Fatalf("missing manifest %q in %#v", filePath, snapshot.Manifests)
	return Manifest{}
}

func assertGraphNode(t *testing.T, snapshot Snapshot, kind, label string) {
	t.Helper()
	for _, node := range snapshot.Nodes {
		if node.Kind == kind && node.Label == label {
			return
		}
	}
	t.Fatalf("missing %s node %q in %#v", kind, label, snapshot.Nodes)
}

func assertGraphEdge(t *testing.T, snapshot Snapshot, kind, label string) {
	t.Helper()
	for _, edge := range snapshot.Edges {
		if edge.Kind == kind && edge.Label == label {
			return
		}
	}
	t.Fatalf("missing %s edge %q in %#v", kind, label, snapshot.Edges)
}

func writeGraphFixture(t *testing.T, root, name, content string) {
	t.Helper()
	filePath := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runGraphGit(t *testing.T, root string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, arguments...)...)
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
	return string(output)
}

func TestParseGradleDependenciesReadsGroovyAndKotlinSyntax(t *testing.T) {
	groovy := []byte(`plugins {
    id 'org.springframework.boot' version '4.0.1'
}

ext {
    lombokVersion = '1.18.42'
}

dependencies {
    implementation platform('org.springframework.boot:spring-boot-dependencies:4.0.1')
    implementation enforcedPlatform("com.fasterxml.jackson:jackson-bom:2.18.4")
    implementation 'org.springframework.boot:spring-boot-starter-web'
    implementation group: 'org.projectlombok', name: 'lombok', version: '1.18.42'
    compileOnly name: 'shaded-tools', group: 'com.acme.tools'
    runtimeOnly "org.postgresql:postgresql:${postgresVersion}"
    annotationProcessor "org.projectlombok:lombok:${lombokVersion}"
    testFixturesImplementation 'org.assertj:assertj-core:3.27.6'
    testImplementation project(path: ':contract-tests')
    implementation project(':shared:domain')
}`)
	versionVariables := parseGradleVersionVariables(map[string][]byte{
		"build.gradle":      groovy,
		"gradle.properties": []byte("postgresVersion=42.7.5\n"),
	})
	dependencies := parseGradleDependencies(groovy, nil, versionVariables)
	coordinates := make([]string, 0, len(dependencies))
	for _, dependency := range dependencies {
		coordinates = append(coordinates, dependency.coordinate)
		if dependency.line <= 0 {
			t.Fatalf("dependency %q has no line evidence", dependency.coordinate)
		}
	}
	for _, wanted := range []string{
		"com.acme.tools:shaded-tools",
		"com.fasterxml.jackson:jackson-bom:2.18.4",
		"org.assertj:assertj-core:3.27.6",
		"org.postgresql:postgresql:42.7.5",
		"org.projectlombok:lombok:1.18.42",
		"org.springframework.boot:spring-boot-dependencies:4.0.1",
		"org.springframework.boot:spring-boot-starter-web",
		"project:contract-tests",
		"project:shared/domain",
	} {
		if !slices.Contains(coordinates, wanted) {
			t.Fatalf("Groovy coordinates %v do not contain %q", coordinates, wanted)
		}
	}
	for _, unwanted := range []string{
		"org.postgresql:postgresql:${postgresVersion}",
		"org.springframework.boot:4.0.1",
	} {
		if slices.Contains(coordinates, unwanted) {
			t.Fatalf("Groovy coordinates %v must not contain %q", coordinates, unwanted)
		}
	}

	kotlin := []byte(`dependencies {
    implementation(platform("org.springframework.boot:spring-boot-dependencies:4.0.1"))
    implementation(enforcedPlatform(libs.jackson.bom))
    implementation(group = "net.javacrumbs.shedlock", name = "shedlock-spring", version = "6.3.0")
    implementation(name = "guava", group = "com.google.guava")
    ksp(libs.moshi.codegen)
    testImplementation(project(":contract-tests"))
}`)
	catalog := map[string]gradleCatalogReference{
		"jackson.bom": {
			coordinate: "com.fasterxml.jackson:jackson-bom:2.18.4",
			path:       "gradle/libs.versions.toml",
			line:       4,
		},
		"moshi.codegen": {
			coordinate: "com.squareup.moshi:moshi-kotlin-codegen:1.15.2",
			path:       "gradle/libs.versions.toml",
			line:       5,
		},
	}
	dependencies = parseGradleDependencies(kotlin, catalog, nil)
	coordinates = coordinates[:0]
	for _, dependency := range dependencies {
		coordinates = append(coordinates, dependency.coordinate)
	}
	for _, wanted := range []string{
		"com.fasterxml.jackson:jackson-bom:2.18.4",
		"com.google.guava:guava",
		"com.squareup.moshi:moshi-kotlin-codegen:1.15.2",
		"net.javacrumbs.shedlock:shedlock-spring:6.3.0",
		"org.springframework.boot:spring-boot-dependencies:4.0.1",
		"project:contract-tests",
	} {
		if !slices.Contains(coordinates, wanted) {
			t.Fatalf("Kotlin coordinates %v do not contain %q", coordinates, wanted)
		}
	}
	for _, dependency := range dependencies {
		switch dependency.coordinate {
		case "project:contract-tests":
			if dependency.configuration != "testImplementation" ||
				gradleDependencyUsage(dependency.configuration) != "test" {
				t.Fatalf("test dependency metadata = %#v", dependency)
			}
		case "com.squareup.moshi:moshi-kotlin-codegen:1.15.2":
			if dependency.configuration != "ksp" ||
				gradleDependencyUsage(dependency.configuration) != "build" {
				t.Fatalf("build dependency metadata = %#v", dependency)
			}
		}
	}
}

func TestParseGradleVersionCatalogResolvesAccessorForms(t *testing.T) {
	contents := map[string][]byte{
		"gradle/libs.versions.toml": []byte(`[libraries]
# libraries deliberately precede versions
kafka-clients = { module = "org.apache.kafka:kafka-clients", version.ref = "kafka" }
jackson-databind = { group = "com.fasterxml.jackson.core", name = "jackson-databind", version = "2.18.4" }
assertj = "org.assertj:assertj-core:3.27.6" # inline comment
shedlock-spring = { module = "net.javacrumbs.shedlock:shedlock-spring", version = { ref = "shedlock" } }
spring-boot-bom = { module = "org.springframework.boot:spring-boot-dependencies" }

[versions]
kafka = "3.9.0"
shedlock = "6.3.0"
`),
	}
	catalog := parseGradleVersionCatalogs(contents)
	for alias, wanted := range map[string]string{
		"kafka.clients":    "org.apache.kafka:kafka-clients:3.9.0",
		"jackson.databind": "com.fasterxml.jackson.core:jackson-databind:2.18.4",
		"assertj":          "org.assertj:assertj-core:3.27.6",
		"shedlock.spring":  "net.javacrumbs.shedlock:shedlock-spring:6.3.0",
		"spring.boot.bom":  "org.springframework.boot:spring-boot-dependencies",
	} {
		if catalog[alias] != wanted {
			t.Fatalf("catalog[%q] = %q, want %q (catalog %v)", alias, catalog[alias], wanted, catalog)
		}
	}

	declared := catalogDependencies(contents["gradle/libs.versions.toml"])
	for _, dependency := range declared {
		if dependency.line <= 1 {
			t.Fatalf("catalog dependency %q resolved to line %d", dependency.coordinate, dependency.line)
		}
	}
	if len(declared) != 5 {
		t.Fatalf("declared catalog libraries = %#v", declared)
	}
}

func TestParseMavenDeclarationsPreservesUsageAndProperties(t *testing.T) {
	declarations := parseMavenDeclarations([]byte(`<project>
  <properties><junit.version>5.12.2</junit.version></properties>
  <dependencies>
    <dependency>
      <groupId>org.springframework</groupId>
      <artifactId>spring-context</artifactId>
      <version>6.2.1</version>
    </dependency>
    <dependency>
      <groupId>org.junit.jupiter</groupId>
      <artifactId>junit-jupiter</artifactId>
      <version>${junit.version}</version>
      <scope>test</scope>
    </dependency>
    <dependency>
      <groupId>com.example</groupId>
      <artifactId>optional-agent</artifactId>
      <version>1.0.0</version>
      <optional>true</optional>
    </dependency>
  </dependencies>
</project>`))
	if len(declarations) != 3 {
		t.Fatalf("Maven declarations = %#v", declarations)
	}
	if !slices.ContainsFunc(declarations, func(declaration DependencyDeclaration) bool {
		return declaration.Package == "org.junit.jupiter:junit-jupiter" &&
			declaration.Declared == "5.12.2" &&
			declaration.Usage == "test" &&
			declaration.DeclaredScope == "test"
	}) {
		t.Fatalf("Maven test declaration missing from %#v", declarations)
	}
	if !slices.ContainsFunc(declarations, func(declaration DependencyDeclaration) bool {
		return declaration.Package == "com.example:optional-agent" &&
			declaration.Usage == "production" &&
			declaration.Relationship == "optional"
	}) {
		t.Fatalf("Maven optional declaration missing from %#v", declarations)
	}
}

func TestCargoPythonAndNuGetDeclarationsPreserveResolvedVersions(t *testing.T) {
	cargoLock := map[string][]string{
		"serde":    {"1.0.219"},
		"tempfile": {"3.20.0"},
	}
	cargo := parseCargoDeclarations([]byte(`[dependencies]
serde = { version = "1", optional = true }

[dev-dependencies]
tempfile = "3"

[build-dependencies]
cc = { git = "https://example.com/cc.git" }
`), cargoLock, "Cargo.lock")
	for _, wanted := range []struct {
		name, usage, resolved, relationship string
	}{
		{"serde", "production", "1.0.219", "optional"},
		{"tempfile", "test", "3.20.0", "required"},
		{"cc", "build", "", "required"},
	} {
		if !slices.ContainsFunc(cargo, func(declaration DependencyDeclaration) bool {
			return declaration.Package == wanted.name &&
				declaration.Usage == wanted.usage &&
				declaration.Resolved == wanted.resolved &&
				declaration.Relationship == wanted.relationship
		}) {
			t.Fatalf("Cargo declaration %q missing from %#v", wanted.name, cargo)
		}
	}

	pythonLock := map[string][]string{
		"fastapi": {"0.116.1"},
		"pytest":  {"8.4.1"},
	}
	python := parsePyprojectDeclarations([]byte(`[project]
dependencies = [
  "fastapi>=0.115",
]

[project.optional-dependencies]
test = ["pytest>=8"]
`), pythonLock, "uv.lock")
	if !slices.ContainsFunc(python, func(declaration DependencyDeclaration) bool {
		return declaration.Package == "fastapi" && declaration.Usage == "production" &&
			declaration.Resolved == "0.116.1"
	}) || !slices.ContainsFunc(python, func(declaration DependencyDeclaration) bool {
		return declaration.Package == "pytest" && declaration.Usage == "test" &&
			declaration.Resolved == "8.4.1"
	}) {
		t.Fatalf("Python declarations = %#v", python)
	}

	nuget := parseNuGetDeclarations(
		"tests/Service.Tests.csproj",
		[]byte(`<Project><ItemGroup>
  <PackageReference Include="Microsoft.NET.Test.Sdk" Version="17.13.0" />
  <PackageReference Include="xunit"><Version>2.9.3</Version></PackageReference>
</ItemGroup></Project>`),
		map[string][]string{"Microsoft.NET.Test.Sdk": {"17.13.0"}, "xunit": {"2.9.3"}},
		"tests/packages.lock.json",
	)
	if len(nuget) != 2 || !slices.ContainsFunc(nuget, func(declaration DependencyDeclaration) bool {
		return declaration.Package == "xunit" && declaration.Usage == "test" &&
			declaration.Resolved == "2.9.3" && declaration.Ecosystem == "nuget"
	}) {
		t.Fatalf("NuGet declarations = %#v", nuget)
	}
}

func TestLockfileReadersSelectUnambiguousDirectVersions(t *testing.T) {
	versions := tomlPackageLockVersions([]byte(`version = 4

[[package]]
name = "serde"
version = "1.0.219"

[[package]]
name = "shared"
version = "1.0.0"

[[package]]
name = "shared"
version = "2.0.0"
`))
	if got := selectLockedVersion(versions["serde"], "^1"); got != "1.0.219" {
		t.Fatalf("serde resolved = %q", got)
	}
	if got := selectLockedVersion(versions["shared"], "2.0.0"); got != "2.0.0" {
		t.Fatalf("shared resolved = %q", got)
	}
	if got := selectLockedVersion(versions["shared"], "^1"); got != "" {
		t.Fatalf("ambiguous shared resolved = %q", got)
	}

	pnpm := pnpmLockVersions([]byte(`lockfileVersion: '9.0'
importers:
  .:
    dependencies:
      react:
        specifier: ^19.0.0
        version: 19.1.1
    devDependencies:
      vitest:
        specifier: ^3.0.0
        version: 3.2.4(@types/node@22.15.0)
  apps/admin:
    dependencies:
      '@acme/ui':
        specifier: workspace:*
        version: link:../../packages/ui
      zod:
        specifier: ^3.0.0
        version: 3.25.67
`), "apps/admin/package.json", "pnpm-lock.yaml")
	if pnpm["zod"] != "3.25.67" || pnpm["@acme/ui"] != "" {
		t.Fatalf("pnpm resolved versions = %#v", pnpm)
	}

	requirements := parsePythonDeclarations(
		"requirements-dev.txt",
		[]byte("zope_interface==7.2\n"),
		map[string][]string{"zope-interface": {"7.2"}},
		"uv.lock",
	)
	if len(requirements) != 1 ||
		requirements[0].Usage != "development" ||
		requirements[0].Resolved != "7.2" {
		t.Fatalf("development requirements = %#v", requirements)
	}
}

func TestGoDependencyUsageDistinguishesProductionAndTests(t *testing.T) {
	declarations := []DependencyDeclaration{
		{Package: "github.com/acme/runtime", Usage: "unknown"},
		{Package: "github.com/acme/testkit", Usage: "unknown"},
		{Package: "github.com/acme/unused", Usage: "unknown"},
	}
	classifyGoDependencyUsage(declarations, map[string][]byte{
		"main.go": []byte(`package main
import "github.com/acme/runtime/client"
`),
		"main_test.go": []byte(`package main
import "github.com/acme/testkit/assert"
`),
	})
	if declarations[0].Usage != "production" ||
		declarations[1].Usage != "test" ||
		declarations[2].Usage != "unknown" {
		t.Fatalf("Go dependency usage = %#v", declarations)
	}
}

func TestSpringClientTargetsRequireClientEvidence(t *testing.T) {
	license := []byte(`/*
 * Licensed under the Apache License, Version 2.0
 * http://www.apache.org/licenses/LICENSE-2.0
 */
package com.acme.payment;

class PaymentTotals {}
`)
	if targets := springClientTargets(license); len(targets) != 0 {
		t.Fatalf("license header produced client targets %#v", targets)
	}

	clients := []byte(`package com.acme.payment;

class Clients {
    WebClient inventory() {
        return WebClient.builder().baseUrl("http://inventory-service:8080").build();
    }

    RestClient shipping() {
        return RestClient.builder().baseUrl("lb://shipping-service").build();
    }

    WebClient clusterInfrastructure() {
        return WebClient.builder().baseUrl("http://cluster.local").build();
    }

    WebClient serviceDNSInfrastructure() {
        return WebClient.builder().baseUrl("http://svc.cluster.local").build();
    }

    WebClient inventoryInKubernetes() {
        return WebClient.builder().baseUrl("http://inventory-service.default.svc.cluster.local").build();
    }

    RestTemplate unresolved(RestTemplateBuilder builder) {
        return builder.rootUri("http://${pricing.host}/api").build();
    }
}
`)
	targets := springClientTargets(clients)
	names := make([]string, 0, len(targets))
	for _, target := range targets {
		names = append(names, target.name)
		if target.line <= 0 {
			t.Fatalf("client target %q has no line evidence", target.name)
		}
	}
	if !slices.Equal(names, []string{"inventory-service", "shipping-service"}) {
		t.Fatalf("client targets = %v", names)
	}
}

func TestSpringRoutesIgnoreDeclarativeClientInterfaces(t *testing.T) {
	// @HttpExchange and @FeignClient types declare endpoints a service calls,
	// not endpoints it serves, so the Routes layer must leave them out.
	for name, content := range map[string][]byte{
		"HTTP interface": []byte(`package com.acme.payment;

@HttpExchange("/payments")
public interface PaymentApi {
    @GetExchange("/{id}")
    Payment get(String id);
}
`),
		"Feign client": []byte(`package com.acme.payment;

@FeignClient(name = "inventory-api")
public interface InventoryClient {
    @GetMapping("/inventory/{sku}")
    String stock(String sku);
}
`),
	} {
		if routes := springRoutes(content); len(routes) != 0 {
			t.Fatalf("%s produced served routes %#v", name, routes)
		}
	}

	served := []byte(`package com.acme.payment;

@RestController
@RequestMapping("/payments")
public class PaymentController {
    @GetMapping("/{id}")
    public String get() { return "ok"; }

    @RequestMapping(value = "/{id}/refunds", method = RequestMethod.PUT)
    public void refund() {}
}
`)
	labels := make([]string, 0)
	for _, route := range springRoutes(served) {
		labels = append(labels, route.label)
	}
	if !slices.Equal(labels, []string{"GET /payments/{id}", "PUT /payments/{id}/refunds"}) {
		t.Fatalf("controller routes = %v", labels)
	}
}

func TestSpringApplicationNameReadsNestedYAMLAndProperties(t *testing.T) {
	yaml := []byte(`server:
  port: 8080
spring:
  application:
    name: inventory-service
  datasource:
    name: primary
`)
	if name := springApplicationName("src/main/resources/application.yml", yaml); name != "inventory-service" {
		t.Fatalf("nested YAML application name = %q", name)
	}
	flat := []byte("spring.application.name=payment-service\n")
	if name := springApplicationName("src/main/resources/application.properties", flat); name != "payment-service" {
		t.Fatalf("property application name = %q", name)
	}
	unrelated := []byte("name: build\njobs:\n  test:\n    name: unit\n")
	if name := springApplicationName("src/main/resources/application.yaml", unrelated); name != "" {
		t.Fatalf("unrelated YAML produced application name %q", name)
	}
}

func TestServiceConfigurationAndMainSourceEvidenceOutrankTests(t *testing.T) {
	targets := serviceConfigurationTargets([]byte(`clients:
  inventory:
    base-url: ${INVENTORY_URL:http://inventory-service:8080}
  pricing:
    base-url: ${PRICING_HOST:pricing-service:9090}
docs:
  url: https://docs.example.com
infrastructure:
  cluster: http://cluster.local
  service-dns: http://svc.cluster.local
`))
	if len(targets) != 2 ||
		targets[0].name != "inventory-service" || targets[0].line != 3 ||
		targets[1].name != "pricing-service" || targets[1].line != 5 {
		t.Fatalf("configuration targets = %#v", targets)
	}
	if sourceConfidence("src/main/java/com/acme/InventoryClient.java") != "high" ||
		sourceConfidence("src/test/java/com/acme/ManualInventoryClientTest.java") != "low" ||
		sourceConfidence("tools/ManualInventoryClientTest.kt") != "low" {
		t.Fatal("source confidence did not distinguish production and test clients")
	}

	builder := newBuilder("http://127.0.0.1:7331")
	builder.registerServiceTarget("inventory-service", "repository:2")
	builder.clientReferences = []clientReference{
		{
			sourceRepositoryID: 1,
			target:             "inventory-service",
			confidence:         "low",
			evidence: Evidence{
				RepositoryID: 1,
				Path:         "src/test/java/com/acme/ManualInventoryClientTest.java",
				Line:         35,
			},
		},
		{
			sourceRepositoryID: 1,
			target:             "inventory-service",
			confidence:         "high",
			evidence: Evidence{
				RepositoryID: 1,
				Path:         "src/main/resources/application.yml",
				Line:         3,
			},
		},
	}
	builder.resolveClientReferences()
	edge := builder.edges[edgeID("repository:1", "repository:2", "service-call")]
	if edge.Confidence != "high" || edge.Label != "calls over HTTP" ||
		len(edge.Evidence) != 1 ||
		edge.Evidence[0].Path != "src/main/resources/application.yml" {
		t.Fatalf("resolved production edge = %#v", edge)
	}

	testOnly := newBuilder("http://127.0.0.1:7331")
	testOnly.registerServiceTarget("inventory-service", "repository:2")
	testOnly.clientReferences = builder.clientReferences[:1]
	testOnly.resolveClientReferences()
	edge = testOnly.edges[edgeID("repository:1", "repository:2", "service-call")]
	if edge.Confidence != "low" || edge.Label != "calls over HTTP (test-only)" {
		t.Fatalf("resolved test-only edge = %#v", edge)
	}
}

func TestCollectionSnapshotIsBoundedAndReportsTruncation(t *testing.T) {
	// A large collection must not analyze every repository before rendering.
	root, revision := javaGraphFixture(t, map[string]string{
		"build.gradle": `dependencies {
    implementation 'org.springframework.boot:spring-boot-starter-web:4.0.1'
}`,
	})
	repositories := make([]catalog.Repository, 0, maximumCollectionRepositories+5)
	for index := range maximumCollectionRepositories + 5 {
		repositories = append(repositories, catalog.Repository{
			ID:            int64(index + 1),
			Name:          fmt.Sprintf("service-%02d", index),
			Path:          root,
			HeadCommit:    revision,
			IndexedCommit: revision,
			IndexState:    "ready",
		})
	}
	service, err := New(
		multiGraphStore{repositories: repositories},
		filepath.Join(t.TempDir(), "maps"),
		"http://127.0.0.1:7331",
	)
	if err != nil {
		t.Fatal(err)
	}

	collection, err := service.Snapshot(context.Background(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(collection.Repositories) != maximumCollectionRepositories {
		t.Fatalf("collection analyzed %d repositories, want %d",
			len(collection.Repositories), maximumCollectionRepositories)
	}
	if !collection.Truncated {
		t.Fatal("bounded collection did not report truncation")
	}
	if collection.Scope.Kind != "collection" || collection.Scope.Complete ||
		collection.Scope.TotalRepositories != maximumCollectionRepositories+5 ||
		collection.Scope.AnalyzedRepositories != maximumCollectionRepositories ||
		collection.Scope.OmittedRepositories != 5 ||
		collection.Scope.RepositoryLimit != maximumCollectionRepositories {
		t.Fatalf("collection scope = %#v", collection.Scope)
	}

	// Selecting one repository stays complete.
	single, err := service.Snapshot(context.Background(), 3, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(single.Repositories) != 1 || single.Truncated {
		t.Fatalf("single repository snapshot = %d repositories, truncated=%v",
			len(single.Repositories), single.Truncated)
	}
	if !single.Scope.Complete || single.Scope.Kind != "repository" ||
		single.Scope.RequestedRepositoryID != 3 {
		t.Fatalf("single repository scope = %#v", single.Scope)
	}
}

func TestExtractionIgnoresCommentsButKeepsStringLiterals(t *testing.T) {
	content := []byte(`/*
 * Licensed under the Apache License, Version 2.0
 * http://www.apache.org/licenses/LICENSE-2.0
 */
package com.acme.payment;

@RestController
@RequestMapping("/payments")
public class PaymentController {

	/**
	 * Called before each and every @RequestMapping annotated method.
	 * See also @GetMapping("/documented-but-not-real").
	 */
	void helper() {}

	// @DeleteMapping("/commented-out")
	@GetMapping("/{id}")
	public String get() { return "ok"; }

	WebClient client() {
		// the base URL below is real code, not a comment
		return WebClient.builder().baseUrl("http://inventory-service:8080").build();
	}
}
`)
	labels := make([]string, 0)
	for _, route := range springRoutes(content) {
		labels = append(labels, route.label)
	}
	if !slices.Equal(labels, []string{"GET /payments/{id}"}) {
		t.Fatalf("routes = %v; documented or commented-out annotations must not count", labels)
	}

	targets := make([]string, 0)
	for _, target := range springClientTargets(content) {
		targets = append(targets, target.name)
	}
	if !slices.Equal(targets, []string{"inventory-service"}) {
		t.Fatalf("client targets = %v; a URL in a string literal must survive and a license URL must not", targets)
	}

	// Byte offsets must be preserved so evidence keeps exact line numbers.
	stripped := stripJavaComments(content)
	if len(stripped) != len(content) {
		t.Fatalf("stripped length %d, want %d", len(stripped), len(content))
	}
	if bytes.Count(stripped, []byte("\n")) != bytes.Count(content, []byte("\n")) {
		t.Fatal("comment stripping changed the line count")
	}
}
