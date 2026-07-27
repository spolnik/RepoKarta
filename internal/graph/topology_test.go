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
