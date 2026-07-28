package graph

import (
	"reflect"
	"strings"
	"testing"

	"github.com/spolnik/RepoKarta/internal/catalog"
)

func TestTopologyMapKeyCandidateUsesRegisteredServiceWithoutAssignment(t *testing.T) {
	builder := newBuilder("http://127.0.0.1:7331")
	consumer := catalog.Repository{
		ID: 81, Name: "checkout-service", IndexedCommit: "81818181",
	}
	target := catalog.Repository{
		ID: 82, Name: "some-service", IndexedCommit: "82828282",
	}
	targetNodeID := "repository:82"
	builder.registerServiceTarget(target.Name, targetNodeID)
	builder.addNode(Node{
		ID: targetNodeID, Kind: "repository", Label: target.Name,
		RepositoryID: target.ID, Repository: target.Name,
	})
	builder.addDistributedTopology(
		consumer, consumer.IndexedCommit, "README.md", map[string][]byte{
			"README.md": []byte("# Checkout"),
			"src/main/resources/application.yml": []byte(`
application:
  routes:
    some-service: ${SOME_SERVICE_URL}
    not-registered: ${UNROUTABLE_URL}
`),
		},
	)

	snapshot := builder.snapshot("map-key-registry-only")
	if len(snapshot.Connections) != 1 {
		t.Fatalf("connections = %+v, want exactly one internal edge", snapshot.Connections)
	}
	connection := snapshot.Connections[0]
	targetComponent := componentByID(snapshot.Components, connection.Target)
	if targetComponent.Name != target.Name || targetComponent.External ||
		!connection.TargetResolved || connection.Confidence != "high" ||
		connection.ResolutionTier != "map_key_registry" ||
		len(connection.Evidence) != 1 {
		t.Fatalf("registered map-key target was not resolved immediately: connection=%+v target=%+v", connection, targetComponent)
	}
	if len(snapshot.UnresolvedTopology) != 1 ||
		snapshot.UnresolvedTopology[0].Variable != "UNROUTABLE_URL" ||
		snapshot.UnresolvedTopology[0].Candidate != "not-registered" {
		t.Fatalf("unmatched map-key placeholder was not retained: %+v", snapshot.UnresolvedTopology)
	}
	if componentByID(snapshot.Components, "system:81").Name != consumer.Name {
		t.Fatalf("consumer source component missing: %+v", snapshot.Components)
	}
}

func TestTopologyLiteralURLInServiceMapAlwaysCreatesExternalComponent(t *testing.T) {
	builder := newBuilder("http://127.0.0.1:7331")
	consumer := catalog.Repository{
		ID: 83, Name: "orders-service", IndexedCommit: "83838383",
	}
	builder.addDistributedTopology(
		consumer, consumer.IndexedCommit, "README.md", map[string][]byte{
			"README.md": []byte("# Orders"),
			"src/main/resources/application.yml": []byte(`
application:
  routes:
    vendor: https://api.example-vendor.com/v2
    fallback: ${FALLBACK_VENDOR_URL:https://api.fallback-vendor.com/v1}
`),
		},
	)

	snapshot := builder.snapshot("literal-url-in-service-map")
	if len(snapshot.UnresolvedTopology) != 0 {
		t.Fatalf("literal URL became unresolved topology: %+v", snapshot.UnresolvedTopology)
	}
	component := topologyComponentNamed(snapshot.Components, "example-vendor.com")
	if component.ID == "" || !component.External ||
		!slicesContain(component.Aliases, "api.example-vendor.com") {
		t.Fatalf("literal URL external component missing or malformed: %+v", snapshot.Components)
	}
	fallback := topologyComponentNamed(snapshot.Components, "fallback-vendor.com")
	fallbackConnection, found := placeholderConnection(
		snapshot.Connections, "FALLBACK_VENDOR_URL",
	)
	if fallback.ID == "" || !found ||
		fallbackConnection.Target != fallback.ID ||
		fallbackConnection.ResolutionTier != "in_file_default" ||
		fallbackConnection.Confidence != "medium" {
		t.Fatalf("placeholder literal default did not contribute both paths: component=%+v connection=%+v", fallback, fallbackConnection)
	}
	if len(snapshot.Connections) != 2 {
		t.Fatalf("literal URL edge missing: %+v", snapshot.Connections)
	}
}

func TestTopologyRejectsInvalidExternalHostNamesAtCreationChokePoint(t *testing.T) {
	builder := newBuilder("http://127.0.0.1:7331")
	for _, value := range []string{"0", "2m", "30s", "...)", "10"} {
		builder.externalSystemComponent(
			"service", value, "HTTP", []string{value},
		)
	}

	if len(builder.components) != 0 {
		t.Fatalf("invalid external names became components: %+v", builder.components)
	}
	snapshot := builder.snapshot("rejected-external-hosts")
	if snapshot.RejectedExternalCount != 5 {
		t.Fatalf("external component rejection counter = %d, want 5", snapshot.RejectedExternalCount)
	}
}

func TestTopologyDatabaseURLRejectsMalformedHostComponents(t *testing.T) {
	builder := newBuilder("http://127.0.0.1:7331")
	repository := catalog.Repository{
		ID: 105, Name: "reporting-service", IndexedCommit: "10510510",
	}
	builder.addDistributedTopology(
		repository, repository.IndexedCommit, "README.md", map[string][]byte{
			"README.md": []byte("# Reporting"),
			"src/main/resources/application.yml": []byte(`
spring:
  datasource:
    malformed: jdbc:postgresql://db)
    numeric: postgresql://0
    truncated: postgresql://...)
`),
		},
	)

	snapshot := builder.snapshot("malformed-database-hosts")
	for _, component := range snapshot.Components {
		if component.Kind == "database" {
			t.Fatalf("malformed database host became component: %+v", component)
		}
	}
	if snapshot.RejectedComponentCounts[componentRejectionInvalidName] != 3 ||
		snapshot.RejectedExternalCount != 3 {
		t.Fatalf(
			"database rejection diagnostics = reasons=%v external=%d, want invalid=3 external=3",
			snapshot.RejectedComponentCounts, snapshot.RejectedExternalCount,
		)
	}
	if snapshot.RejectedComponentConnections != 3 {
		t.Fatalf(
			"rejected database connections = %d, want 3",
			snapshot.RejectedComponentConnections,
		)
	}
}

func TestTopologyComposeServiceNamesUseComponentNameBlocklist(t *testing.T) {
	builder := newBuilder("http://127.0.0.1:7331")
	repository := catalog.Repository{
		ID: 106, Name: "billing-platform", IndexedCommit: "10610610",
	}
	builder.addDistributedTopology(
		repository, repository.IndexedCommit, "src/main.go", map[string][]byte{
			"src/main.go": []byte("package main"),
			"deploy/docker-compose.yml": []byte(`
services:
  app:
    depends_on:
      - billing-worker
  billing-worker:
    depends_on:
      - app
`),
		},
	)

	snapshot := builder.snapshot("compose-component-blocklist")
	if app := topologyComponentNamed(snapshot.Components, "app"); app.ID != "" {
		t.Fatalf("blocklisted compose service became component: %+v", app)
	}
	if worker := topologyComponentNamed(snapshot.Components, "billing-worker"); worker.ID == "" {
		t.Fatalf("valid compose service was suppressed: %+v", snapshot.Components)
	}
	if len(snapshot.Connections) != 0 ||
		snapshot.RejectedComponentConnections != 2 {
		t.Fatalf(
			"connections referencing rejected compose component survived: connections=%+v rejected=%d",
			snapshot.Connections, snapshot.RejectedComponentConnections,
		)
	}
	if snapshot.RejectedComponentCounts[componentRejectionGenericLabel] != 1 {
		t.Fatalf(
			"compose rejection diagnostics = %v, want generic_label=1",
			snapshot.RejectedComponentCounts,
		)
	}
}

func TestTopologyInfrastructureHostSuffixesAreRejectedAtComponentCreation(t *testing.T) {
	builder := newBuilder("http://127.0.0.1:7331")
	repository := catalog.Repository{
		ID: 107, Name: "routing-service", IndexedCommit: "10710710",
	}
	builder.addDistributedTopology(
		repository, repository.IndexedCommit, "README.md", map[string][]byte{
			"README.md": []byte("# Routing"),
			"src/main/resources/application.yml": []byte(`
upstreams:
  service-url: http://something.svc.cluster.local
  cluster-url: http://cluster.local
`),
		},
	)

	snapshot := builder.snapshot("infrastructure-host-suffixes")
	if len(snapshot.Components) != 1 ||
		snapshot.Components[0].Name != repository.Name {
		t.Fatalf("infrastructure hosts became components: %+v", snapshot.Components)
	}
	if snapshot.RejectedExternalCount != 2 {
		t.Fatalf(
			"infrastructure rejection counter = %d, want 2",
			snapshot.RejectedExternalCount,
		)
	}
	if snapshot.RejectedComponentCounts[componentRejectionInfrastructure] != 2 ||
		snapshot.RejectedComponentConnections != 2 {
		t.Fatalf(
			"infrastructure rejection diagnostics = reasons=%v connections=%d",
			snapshot.RejectedComponentCounts, snapshot.RejectedComponentConnections,
		)
	}
}

func TestGenericExternalHostLabelsBlocklistIsExhaustive(t *testing.T) {
	expected := map[string]bool{
		"api": true, "auth": true, "www": true, "app": true, "gateway": true,
		"service": true, "internal": true, "prod": true, "staging": true,
	}
	if !reflect.DeepEqual(genericExternalHostLabels, expected) {
		t.Fatalf("generic external host labels = %#v, want %#v", genericExternalHostLabels, expected)
	}
	builder := newBuilder("http://127.0.0.1:7331")
	for label := range expected {
		builder.externalSystemComponent(
			"service", label, "HTTP", []string{label},
		)
	}
	if len(builder.components) != 0 ||
		builder.rejectedExternalCount != len(expected) {
		t.Fatalf(
			"blocklisted labels bypassed external creation gate: components=%+v rejected=%d",
			builder.components, builder.rejectedExternalCount,
		)
	}
}

func TestTopologyFiltersKubernetesInfrastructureOnlyHosts(t *testing.T) {
	builder := newBuilder("http://127.0.0.1:7331")
	repository := catalog.Repository{
		ID: 100, Name: "configuration-service", IndexedCommit: "10010010",
	}
	builder.addDistributedTopology(
		repository, repository.IndexedCommit, "README.md", map[string][]byte{
			"README.md": []byte("# Configuration"),
			"src/main/resources/application.yml": []byte(`
infrastructure:
  cluster: http://cluster.local
  service-dns: http://svc.cluster.local
`),
		},
	)

	snapshot := builder.snapshot("kubernetes-infrastructure-hosts")
	if len(snapshot.Components) != 1 || snapshot.Components[0].Name != repository.Name {
		t.Fatalf("infrastructure-only hosts became components: %+v", snapshot.Components)
	}
	if len(snapshot.Connections) != 0 {
		t.Fatalf("infrastructure-only hosts became connections: %+v", snapshot.Connections)
	}
}

func TestDeploymentOnlyRepositoryDropsConnectionsFromSuppressedSource(t *testing.T) {
	builder := newBuilder("http://127.0.0.1:7331")
	repository := catalog.Repository{
		ID: 104, Name: "deployment-config", IndexedCommit: "10410410",
	}
	builder.addDistributedTopology(
		repository, repository.IndexedCommit, "README.md", map[string][]byte{
			"README.md": []byte("# Deployment configuration"),
			"infrastructure/deploy/production/deployment.yaml": []byte(`
orders:
  vendor-url: https://api.example-vendor.com/v2
`),
		},
	)

	snapshot := builder.snapshot("deployment-only-suppressed-source")
	if len(snapshot.Components) != 0 {
		t.Fatalf("deployment-only repository emitted components: %+v", snapshot.Components)
	}
	if len(snapshot.Connections) != 0 {
		t.Fatalf("deployment-only repository emitted connections: %+v", snapshot.Connections)
	}
	if snapshot.SuppressedSourceEdges != 1 {
		t.Fatalf(
			"suppressed source edges = %d, want 1",
			snapshot.SuppressedSourceEdges,
		)
	}
}

func TestTopologyThreeRepositoryRegressionFixture(t *testing.T) {
	builder := newBuilder("http://127.0.0.1:7331")
	consumer := catalog.Repository{
		ID: 101, Name: "consumer-service", IndexedCommit: "10110110",
	}
	deployment := catalog.Repository{
		ID: 102, Name: "deployment-config", IndexedCommit: "10210210",
	}
	target := catalog.Repository{
		ID: 103, Name: "registered-service", IndexedCommit: "10310310",
	}
	builder.registerServiceTarget(target.Name, "repository:103")
	builder.addNode(Node{
		ID: "repository:103", Kind: "repository", Label: target.Name,
		RepositoryID: target.ID, Repository: target.Name,
	})
	builder.addDistributedTopology(
		consumer, consumer.IndexedCommit, "README.md", map[string][]byte{
			"README.md": []byte("# Consumer"),
			"src/main/resources/application.yml": []byte(`
application:
  routes:
    registered-service: ${REGISTERED_SERVICE_URL}
    missing-service: ${MISSING_SERVICE_URL}
    vendor: https://api.example-vendor.com/v2
`),
		},
	)
	builder.addDistributedTopology(
		deployment, deployment.IndexedCommit, "README.md", map[string][]byte{
			"README.md": []byte("# Deployment"),
			"deploy/production/values.yaml": []byte(`
REGISTERED_SERVICE_URL: https://registered-service.production.svc.cluster.local
`),
		},
	)
	builder.addDistributedTopology(
		target, target.IndexedCommit, "README.md",
		map[string][]byte{"README.md": []byte("# Registered service")},
	)

	snapshot := builder.snapshot("three-repository-regression")
	names := make(map[string]SystemComponent)
	for _, component := range snapshot.Components {
		names[component.Name] = component
		lower := strings.ToLower(component.Name)
		if strings.Contains(component.Name, "${") ||
			topologyDigitsOnly(component.Name) ||
			genericExternalHostLabels[lower] {
			t.Fatalf("forbidden topology component: %+v", component)
		}
	}
	for _, expected := range []string{
		consumer.Name, target.Name, "example-vendor.com",
	} {
		if names[expected].ID == "" {
			t.Fatalf("missing component %q: %+v", expected, snapshot.Components)
		}
	}
	if len(snapshot.Components) != 3 {
		t.Fatalf("components = %+v, want exactly consumer, target, and vendor", snapshot.Components)
	}
	if len(snapshot.Connections) != 2 {
		t.Fatalf("connections = %+v, want internal and vendor edges", snapshot.Connections)
	}
	placeholder, found := placeholderConnection(
		snapshot.Connections, "REGISTERED_SERVICE_URL",
	)
	if !found || placeholder.Target != names[target.Name].ID ||
		placeholder.Confidence != "high" || !placeholder.TargetResolved ||
		len(placeholder.Evidence) != 2 {
		t.Fatalf("registered placeholder edge = %+v", placeholder)
	}
	if len(snapshot.UnresolvedTopology) != 1 ||
		snapshot.UnresolvedTopology[0].Variable != "MISSING_SERVICE_URL" ||
		snapshot.UnresolvedTopology[0].Candidate != "missing-service" {
		t.Fatalf("unresolved topology = %+v, want missing service only", snapshot.UnresolvedTopology)
	}
}

func topologyComponentNamed(
	components []SystemComponent,
	name string,
) SystemComponent {
	for _, component := range components {
		if component.Name == name {
			return component
		}
	}
	return SystemComponent{}
}

func topologyDigitsOnly(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}
