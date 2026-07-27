package graph

import (
	"testing"

	"github.com/spolnik/RepoKarta/internal/catalog"
)

func TestDistributedTopologyExtractsDirectedProtocolsAndResolvesServices(t *testing.T) {
	builder := newBuilder("http://127.0.0.1:7331")
	checkout := catalog.Repository{ID: 1, Name: "checkout", IndexedCommit: "aaaaaaaa"}
	orders := catalog.Repository{ID: 2, Name: "orders", IndexedCommit: "bbbbbbbb"}

	builder.addDistributedTopology(checkout, checkout.IndexedCommit, "README.md", map[string][]byte{
		"README.md": []byte("# Checkout"),
		"src/main/resources/application.yml": []byte(`
spring:
  application:
    name: checkout-service
  datasource:
    url: postgresql://db.internal:5432/checkout
`),
		"src/main/java/CheckoutClient.java": []byte(`
class CheckoutClient {
  WebClient orders = WebClient.create("http://orders-service:8080");
  void pricing() { grpc.Dial("pricing-service:9090"); }
  void publish() { kafkaTemplate.send("orders.created"); }
  @KafkaListener(topics = "payment.completed")
  void consume() {}
}
`),
		".mcp.json": []byte(`{
  "mcpServers": {
    "inventory-tools": {"url": "https://mcp.example.com/mcp"},
    "local-files": {"command": "files-mcp"}
  }
}`),
	})
	builder.addDistributedTopology(orders, orders.IndexedCommit, "README.md", map[string][]byte{
		"README.md": []byte("# Orders"),
		"src/main/resources/application.yml": []byte(`
spring:
  application:
    name: orders-service
`),
	})

	snapshot := builder.snapshot("topology-test")
	protocols := make(map[string]int)
	resolvedHTTP := false
	kafkaPublish, kafkaConsume := false, false
	for _, connection := range snapshot.Connections {
		protocols[connection.Protocol]++
		if connection.Protocol == "http" && connection.TargetResolved {
			target := componentByID(snapshot.Components, connection.Target)
			resolvedHTTP = target.Name == "orders"
		}
		if connection.Protocol == "kafka" && connection.Interaction == "publishes" {
			kafkaPublish = true
		}
		if connection.Protocol == "kafka" && connection.Interaction == "consumes" {
			kafkaConsume = true
		}
	}
	for _, protocol := range []string{"http", "grpc", "kafka", "database", "mcp"} {
		if protocols[protocol] == 0 {
			t.Fatalf("missing %s connection: %+v", protocol, snapshot.Connections)
		}
	}
	if !resolvedHTTP {
		t.Fatalf("HTTP peer was not resolved to orders service: %+v", snapshot.Connections)
	}
	if !kafkaPublish || !kafkaConsume {
		t.Fatalf("Kafka direction was not preserved: %+v", snapshot.Connections)
	}
}

func TestDistributedTopologyIgnoresTestsAndGenericUIEvents(t *testing.T) {
	builder := newBuilder("http://127.0.0.1:7331")
	repository := catalog.Repository{ID: 9, Name: "frontend", IndexedCommit: "dddddddd"}
	builder.addDistributedTopology(repository, repository.IndexedCommit, "README.md", map[string][]byte{
		"README.md": []byte("# Frontend"),
		"src/main.ts": []byte(`
button.addEventListener("click", submit);
window.on("resize", redraw);
channel.subscribe("change");
`),
		"src/client_test.go": []byte(`
func TestClient(t *testing.T) {
  http.Get("https://fictional-test-peer.example.com")
  kafkaTemplate.send("fictional.test.topic")
}
`),
	})
	snapshot := builder.snapshot("no-ui-events")
	if len(snapshot.Connections) != 0 {
		t.Fatalf("test fixtures or UI events became topology connections: %+v", snapshot.Connections)
	}
}

func TestExplicitTopologyProvidesHighConfidenceCorrectionLayer(t *testing.T) {
	builder := newBuilder("http://127.0.0.1:7331")
	repository := catalog.Repository{ID: 7, Name: "platform", IndexedCommit: "cccccccc"}
	builder.addDistributedTopology(repository, repository.IndexedCommit, "README.md", map[string][]byte{
		"README.md": []byte("# Platform"),
		".repokarta.yml": []byte(`
topology:
  components:
    - id: gateway
      name: API Gateway
      kind: service
      technology: Go
      path: cmd/gateway
      aliases: [gateway, edge]
      capabilities: [http_server, mcp_server]
  connections:
    - source: gateway
      target: customer-db
      protocol: database
      interaction: reads_writes
      transport: postgresql
`),
	})
	snapshot := builder.snapshot("declared-topology")
	var gateway SystemComponent
	for _, component := range snapshot.Components {
		if component.Name == "API Gateway" {
			gateway = component
		}
	}
	if gateway.ID == "" || !slicesContain(gateway.Capabilities, "mcp_server") {
		t.Fatalf("declared component missing: %+v", snapshot.Components)
	}
	found := false
	for _, connection := range snapshot.Connections {
		if connection.Source == gateway.ID && connection.Protocol == "database" &&
			connection.EvidenceOrigin == "declared" && connection.Confidence == "high" {
			found = true
		}
	}
	if !found {
		t.Fatalf("declared connection missing: %+v", snapshot.Connections)
	}
}

func TestTopologyPlaceholderResolvesFromIndexedInfrastructureWithDualEvidence(t *testing.T) {
	builder := newBuilder("http://127.0.0.1:7331")
	checkout := catalog.Repository{ID: 11, Name: "checkout", IndexedCommit: "11111111"}
	infrastructure := catalog.Repository{ID: 12, Name: "fleet-infra", IndexedCommit: "22222222"}
	billing := catalog.Repository{ID: 13, Name: "billing-service", IndexedCommit: "33333333"}

	builder.addDistributedTopology(checkout, checkout.IndexedCommit, "README.md", map[string][]byte{
		"README.md": []byte("# Checkout"),
		"src/main/resources/application.yml": []byte(`
routes:
  billing-service: ${BILLING_SERVICE_URL}
`),
	})
	builder.addDistributedTopology(infrastructure, infrastructure.IndexedCommit, "README.md", map[string][]byte{
		"README.md": []byte("# Fleet infrastructure"),
		"src/main/resources/application.yml": []byte(`
BILLING_SERVICE_URL: http://application-default-that-must-not-win
`),
		"deploy/production/deployment.yaml": []byte(`
env:
  BILLING_SERVICE_URL: "http://billing-service.production.svc.cluster.local:8080"
`),
	})
	builder.addDistributedTopology(billing, billing.IndexedCommit, "README.md", map[string][]byte{
		"README.md": []byte("# Billing"),
	})

	snapshot := builder.snapshot("placeholder-cross-repository")
	connection, found := placeholderConnection(snapshot.Connections, "BILLING_SERVICE_URL")
	if !found {
		t.Fatalf("placeholder connection missing: %+v", snapshot.Connections)
	}
	target := componentByID(snapshot.Components, connection.Target)
	if target.Name != "billing-service" || !connection.TargetResolved ||
		connection.ResolutionTier != "cross_repository_assignment" ||
		connection.Confidence != "high" {
		t.Fatalf("placeholder did not resolve to billing-service: %+v, target=%+v", connection, target)
	}
	if len(connection.Evidence) != 2 ||
		connection.Evidence[0].Repository != "checkout" ||
		connection.Evidence[1].Repository != "fleet-infra" {
		t.Fatalf("resolved edge does not carry consumption and assignment evidence: %+v", connection.Evidence)
	}
}

func TestTopologyPlaceholderUsesInFileDefaultBeforeIndexedAssignments(t *testing.T) {
	builder := newBuilder("http://127.0.0.1:7331")
	application := catalog.Repository{ID: 14, Name: "checkout", IndexedCommit: "44444444"}
	configuration := catalog.Repository{ID: 15, Name: "fleet-infra", IndexedCommit: "55555555"}
	billing := catalog.Repository{ID: 16, Name: "billing-service", IndexedCommit: "66666666"}
	builder.addDistributedTopology(application, application.IndexedCommit, "README.md", map[string][]byte{
		"README.md": []byte("# Checkout"),
		"config/routes.yaml": []byte(`
routes:
  billing-service: ${BILLING_SERVICE_URL:http://billing-service:8080}
`),
	})
	builder.addDistributedTopology(configuration, configuration.IndexedCommit, "README.md", map[string][]byte{
		"README.md": []byte("# Fleet infrastructure"),
		"deploy/production/values.yaml": []byte(`
BILLING_SERVICE_URL: https://api.example.net
`),
	})
	builder.addDistributedTopology(billing, billing.IndexedCommit, "README.md", map[string][]byte{
		"README.md": []byte("# Billing"),
	})

	snapshot := builder.snapshot("placeholder-in-file-default")
	connections := placeholderConnections(snapshot.Connections, "BILLING_SERVICE_URL")
	if len(connections) != 1 || connections[0].ResolutionTier != "in_file_default" ||
		connections[0].Confidence != "high" || !connections[0].TargetResolved ||
		componentByID(snapshot.Components, connections[0].Target).Name != "billing-service" {
		t.Fatalf("in-file default did not stop resolution: %+v", connections)
	}
}

func TestTopologyAssignmentIndexRecognizesExactConfigurationKeyFormats(t *testing.T) {
	builder := newBuilder("http://127.0.0.1:7331")
	repository := catalog.Repository{ID: 17, Name: "fleet-infra", IndexedCommit: "77777777"}
	builder.addDistributedTopology(repository, repository.IndexedCommit, "README.md", map[string][]byte{
		"README.md": []byte("# Fleet infrastructure"),
		"deploy/values.yaml": []byte(`
YAML_SERVICE_URL: http://yaml-service
`),
		"deploy/config.json": []byte(`{
  "JSON_SERVICE_URL": "https://api.json.example.org"
}`),
		"deploy/docker-compose.yaml": []byte(`
services:
  checkout:
    environment:
      - COMPOSE_SERVICE_HOST=compose-service
`),
		"infra/main.tf": []byte(`
environment = {
  "TERRAFORM_SERVICE_URL" = "http://terraform-service"
}
`),
		"cdk.json": []byte(`{"app": "npx ts-node bin/app.ts"}`),
		"lib/billing-stack.ts": []byte(`
environment: {
  "CDK_SERVICE_URL": "http://cdk-service"
}
`),
		"k8s/deployment.yaml": []byte(`
env:
  - name: KUBERNETES_SERVICE_URL
    value: http://kubernetes-service
`),
	})

	snapshot := builder.snapshot("assignment-formats")
	variables := make(map[string]bool)
	for _, assignment := range snapshot.EnvironmentAssignments {
		variables[assignment.Variable] = true
		if assignment.Rank != environmentAssignmentInfrastructure ||
			assignment.Value == "" || assignment.Indirect {
			t.Fatalf("unexpected indexed assignment: %+v", assignment)
		}
	}
	for _, variable := range []string{
		"YAML_SERVICE_URL", "JSON_SERVICE_URL", "COMPOSE_SERVICE_HOST",
		"TERRAFORM_SERVICE_URL", "CDK_SERVICE_URL", "KUBERNETES_SERVICE_URL",
	} {
		if !variables[variable] {
			t.Fatalf("configuration key %s was not indexed: %+v", variable, snapshot.EnvironmentAssignments)
		}
	}
}

func TestTopologyAssignmentEnvironmentDerivesOverlayAndRegionNames(t *testing.T) {
	for filePath, expected := range map[string]string{
		"deploy/staging/values.yaml":         "staging",
		"helm/values-production.yaml":        "production",
		"overlays/eu-west-1/deployment.yaml": "eu-west-1",
		"helm/values-us-east-2.yaml":         "us-east-2",
	} {
		if actual := environmentFromAssignmentPath(filePath); actual != expected {
			t.Fatalf("environmentFromAssignmentPath(%q) = %q, want %q", filePath, actual, expected)
		}
	}
}

func TestTopologyPlaceholderExcludesTestAndSnapshotAssignments(t *testing.T) {
	builder := newBuilder("http://127.0.0.1:7331")
	application := catalog.Repository{ID: 21, Name: "checkout", IndexedCommit: "aaaaaaaa"}
	configuration := catalog.Repository{ID: 22, Name: "fleet-infra", IndexedCommit: "bbbbbbbb"}
	billing := catalog.Repository{ID: 23, Name: "billing-service", IndexedCommit: "cccccccc"}
	builder.addDistributedTopology(application, application.IndexedCommit, "README.md", map[string][]byte{
		"README.md": []byte("# Checkout"),
		"src/main/resources/application.yml": []byte(`
routes:
  billing-service: ${BILLING_SERVICE_URL}
`),
	})
	builder.addDistributedTopology(configuration, configuration.IndexedCommit, "README.md", map[string][]byte{
		"README.md": []byte("# Fleet infrastructure"),
		"tests/deployment.yaml": []byte(`
BILLING_SERVICE_URL: http://fictional-test-billing
`),
		"deploy/__snapshots__/values-production.yaml": []byte(`
	BILLING_SERVICE_URL: http://fictional-snapshot-billing
`),
	})
	builder.addDistributedTopology(billing, billing.IndexedCommit, "README.md", map[string][]byte{
		"README.md": []byte("# Billing"),
	})

	snapshot := builder.snapshot("placeholder-exclusions")
	connection, found := placeholderConnection(snapshot.Connections, "BILLING_SERVICE_URL")
	if !found || connection.ResolutionTier != "unresolved" ||
		connection.UnresolvedReason != "only_excluded_assignments" ||
		len(connection.Evidence) != 1 {
		t.Fatalf("excluded assignment resolved a placeholder: %+v", connection)
	}
}

func TestTopologyPlaceholderCollapsesEquivalentEnvironmentAssignments(t *testing.T) {
	builder := newBuilder("http://127.0.0.1:7331")
	application := catalog.Repository{ID: 31, Name: "checkout", IndexedCommit: "aaaaaaaa"}
	configuration := catalog.Repository{ID: 32, Name: "fleet-infra", IndexedCommit: "bbbbbbbb"}
	billing := catalog.Repository{ID: 33, Name: "billing-service", IndexedCommit: "cccccccc"}
	builder.addDistributedTopology(application, application.IndexedCommit, "README.md", map[string][]byte{
		"README.md": []byte("# Checkout"),
		"src/main/resources/application.yml": []byte(`
routes:
  billing-service: ${BILLING_SERVICE_URL}
`),
	})
	builder.addDistributedTopology(configuration, configuration.IndexedCommit, "README.md", map[string][]byte{
		"README.md": []byte("# Fleet infrastructure"),
		"deploy/staging/values.yaml": []byte(`
BILLING_SERVICE_URL: http://billing-service.staging.svc.cluster.local
`),
		"deploy/production/values.yaml": []byte(`
BILLING_SERVICE_URL: http://billing-service.production.svc.cluster.local
`),
	})
	builder.addDistributedTopology(billing, billing.IndexedCommit, "README.md", map[string][]byte{
		"README.md": []byte("# Billing"),
	})

	snapshot := builder.snapshot("placeholder-equivalent-environments")
	connections := placeholderConnections(snapshot.Connections, "BILLING_SERVICE_URL")
	if len(connections) != 1 || connections[0].ResolutionDivergent ||
		connections[0].Environment != "" || !connections[0].TargetResolved ||
		len(connections[0].Evidence) != 2 {
		t.Fatalf("equivalent environment assignments did not collapse: %+v", connections)
	}
}

func TestTopologyPlaceholderEmitsEnvironmentQualifiedDivergentExternalTargets(t *testing.T) {
	builder := newBuilder("http://127.0.0.1:7331")
	application := catalog.Repository{ID: 41, Name: "checkout", IndexedCommit: "aaaaaaaa"}
	configuration := catalog.Repository{ID: 42, Name: "fleet-infra", IndexedCommit: "bbbbbbbb"}
	builder.addDistributedTopology(application, application.IndexedCommit, "README.md", map[string][]byte{
		"README.md": []byte("# Checkout"),
		"src/main/resources/application.yml": []byte(`
routes:
  billing-service: ${BILLING_SERVICE_URL}
`),
	})
	builder.addDistributedTopology(configuration, configuration.IndexedCommit, "README.md", map[string][]byte{
		"README.md": []byte("# Fleet infrastructure"),
		"deploy/staging/values.yaml": []byte(`
BILLING_SERVICE_URL: https://api.staging.stripe.com
`),
		"deploy/production/values.yaml": []byte(`
BILLING_SERVICE_URL: https://api.paypal.com
`),
	})

	snapshot := builder.snapshot("placeholder-divergent-environments")
	connections := placeholderConnections(snapshot.Connections, "BILLING_SERVICE_URL")
	if len(connections) != 2 {
		t.Fatalf("divergent assignments = %+v, want two connections", connections)
	}
	environments, targets := map[string]bool{}, map[string]bool{}
	for _, connection := range connections {
		environments[connection.Environment] = true
		targets[componentByID(snapshot.Components, connection.Target).Name] = true
		if !connection.ResolutionDivergent || connection.ResolutionTier != "cross_repository_assignment" ||
			len(connection.Evidence) != 2 {
			t.Fatalf("divergence metadata or evidence missing: %+v", connection)
		}
	}
	if !environments["staging"] || !environments["production"] ||
		!targets["stripe.com"] || !targets["paypal.com"] ||
		targets["api"] {
		t.Fatalf("environment or registrable-domain naming mismatch: environments=%v targets=%v", environments, targets)
	}
}

func TestTopologyPlaceholderKeepsVaultIndirectionUnresolved(t *testing.T) {
	builder := newBuilder("http://127.0.0.1:7331")
	application := catalog.Repository{ID: 51, Name: "checkout", IndexedCommit: "aaaaaaaa"}
	configuration := catalog.Repository{ID: 52, Name: "fleet-infra", IndexedCommit: "bbbbbbbb"}
	builder.addDistributedTopology(application, application.IndexedCommit, "README.md", map[string][]byte{
		"README.md": []byte("# Checkout"),
		"src/main/resources/application.yml": []byte(`
routes:
  billing-service: ${BILLING_SERVICE_URL}
`),
	})
	builder.addDistributedTopology(configuration, configuration.IndexedCommit, "README.md", map[string][]byte{
		"README.md": []byte("# Fleet infrastructure"),
		"deploy/production/values.yaml": []byte(`
BILLING_SERVICE_URL: vault://services/billing/url
DATABASE_PASSWORD: correct-horse-battery-staple
`),
	})

	snapshot := builder.snapshot("placeholder-vault")
	connection, found := placeholderConnection(snapshot.Connections, "BILLING_SERVICE_URL")
	if !found || connection.ResolutionTier != "unresolved" ||
		connection.UnresolvedReason != "secret_or_vault_indirection" ||
		connection.TargetResolved {
		t.Fatalf("vault indirection was not kept unresolved: %+v", connection)
	}
	if len(snapshot.EnvironmentAssignments) != 1 ||
		snapshot.EnvironmentAssignments[0].Variable != "BILLING_SERVICE_URL" ||
		snapshot.EnvironmentAssignments[0].Value != "" ||
		!snapshot.EnvironmentAssignments[0].Indirect {
		t.Fatalf("artifact retained unrelated or indirect secret values: %+v", snapshot.EnvironmentAssignments)
	}
}

func TestTopologyPlaceholderFallsBackToKnownServiceNameShape(t *testing.T) {
	builder := newBuilder("http://127.0.0.1:7331")
	application := catalog.Repository{ID: 61, Name: "checkout", IndexedCommit: "aaaaaaaa"}
	billing := catalog.Repository{ID: 62, Name: "billing-service", IndexedCommit: "bbbbbbbb"}
	builder.addDistributedTopology(application, application.IndexedCommit, "README.md", map[string][]byte{
		"README.md": []byte("# Checkout"),
		"src/main/resources/application.yml": []byte(`
routes:
  billing-service: ${BILLING_SERVICE_BASE_URL}
`),
	})
	builder.addDistributedTopology(billing, billing.IndexedCommit, "README.md", map[string][]byte{
		"README.md": []byte("# Billing"),
	})

	snapshot := builder.snapshot("placeholder-name-shape")
	connection, found := placeholderConnection(snapshot.Connections, "BILLING_SERVICE_BASE_URL")
	if !found || connection.ResolutionTier != "name_shape_heuristic" ||
		connection.Confidence != "low" || !connection.TargetResolved ||
		componentByID(snapshot.Components, connection.Target).Name != "billing-service" {
		t.Fatalf("name-shape fallback did not resolve at low confidence: %+v", connection)
	}
}

func placeholderConnection(
	connections []SystemConnection,
	variable string,
) (SystemConnection, bool) {
	for _, connection := range connections {
		if connection.EnvironmentVariable == variable {
			return connection, true
		}
	}
	return SystemConnection{}, false
}

func placeholderConnections(
	connections []SystemConnection,
	variable string,
) []SystemConnection {
	output := make([]SystemConnection, 0)
	for _, connection := range connections {
		if connection.EnvironmentVariable == variable {
			output = append(output, connection)
		}
	}
	return output
}

func componentByID(components []SystemComponent, id string) SystemComponent {
	for _, component := range components {
		if component.ID == id {
			return component
		}
	}
	return SystemComponent{}
}

func slicesContain(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
