package graph

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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
	if snapshot.FileCount != 5 || len(snapshot.Languages) == 0 {
		t.Fatalf("inventory = files %d, languages %#v", snapshot.FileCount, snapshot.Languages)
	}
	if len(snapshot.Manifests) != 2 {
		t.Fatalf("manifests = %#v", snapshot.Manifests)
	}
	assertGraphNode(t, snapshot, "repository", "acme")
	assertGraphNode(t, snapshot, "package", "server")
	assertGraphNode(t, snapshot, "package", "service")
	assertGraphNode(t, snapshot, "dependency", "github.com/google/uuid")
	assertGraphNode(t, snapshot, "dependency", "htmx.org")
	assertGraphNode(t, snapshot, "route", "/healthz")
	assertGraphEdge(t, snapshot, "import", "imports")
	assertGraphEdge(t, snapshot, "route", "serves")

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
	if err != nil || len(entries) != 1 || filepath.Ext(entries[0].Name()) != ".json" {
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
	dependencies := parseGradleDependencies(groovy, nil)
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
		"org.postgresql:postgresql",
		"org.projectlombok:lombok",
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
	catalog := map[string]string{
		"jackson.bom":   "com.fasterxml.jackson:jackson-bom:2.18.4",
		"moshi.codegen": "com.squareup.moshi:moshi-kotlin-codegen:1.15.2",
	}
	dependencies = parseGradleDependencies(kotlin, catalog)
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

	// Selecting one repository stays complete.
	single, err := service.Snapshot(context.Background(), 3, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(single.Repositories) != 1 || single.Truncated {
		t.Fatalf("single repository snapshot = %d repositories, truncated=%v",
			len(single.Repositories), single.Truncated)
	}
}
