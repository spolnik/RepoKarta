# Distributed dependency topology

RepoKarta's Dependencies landing view models deployable components and their
directed communication. Package imports remain in **Package inventory** because
using a library is not the same fact as one running component calling another.

## Read model

A component is a service or infrastructure resource:

- service or external service;
- database;
- queue or broker;
- MCP server.

A connection always has:

- source and target component identities;
- protocol (`http`, `grpc`, `kafka`, `database`, `mcp`, `amqp`, or `unknown`);
- interaction such as `calls`, `publishes`, `consumes`, `reads_writes`, or
  `invokes`;
- optional transport, such as `https`, `postgresql`, `kafka`, `stdio`, or
  `streamable_http`;
- confidence and peer-resolution state;
- an evidence origin;
- immutable source evidence and/or timestamped runtime metrics.

Arrows follow communication flow. Kafka is therefore represented as:

```text
producer service -> topic -> consumer service
```

MCP remains a first-class protocol. A remote MCP server using Streamable HTTP
is an `mcp` relationship with `streamable_http` transport, not a generic HTTP
dependency. A local configured MCP process uses the `stdio` transport.

## Static discovery

Static facts are extracted only from the indexed Git revision. RepoKarta does
not run repository code. Test, fixture, mock, and testdata paths are excluded
so sample endpoints and UI event listeners do not become production topology.

The current extractors recognize:

- Spring application identities and HTTP clients;
- common source-level HTTP and gRPC client construction;
- Kafka producer/consumer and topic declarations;
- PostgreSQL, MySQL, MongoDB, Redis, Cassandra, Neo4j, and SQL Server URLs;
- JSON MCP server configuration and MCP server capabilities in source;
- Docker Compose services and `depends_on`;
- Backstage component `dependsOn` and `consumesApis` relations;
- explicit `.repokarta.yml` topology declarations.

Only aliases with one kind-compatible target are reconciled. A database or
Kafka topic called `orders` never resolves to an application service called
`orders`. Ambiguous service aliases remain external and unresolved.

## Explicit component model

Use `.repokarta.yml` when source heuristics cannot express a deployment
boundary or when a connection target is supplied indirectly at runtime:

```yaml
topology:
  components:
    - id: gateway
      name: API Gateway
      kind: service
      technology: Go
      path: cmd/gateway
      aliases: [gateway, edge]
      capabilities: [http_server, mcp_server]

    - id: worker
      name: Order Worker
      kind: service
      technology: Kotlin
      path: services/order-worker
      aliases: [order-worker]

  connections:
    - source: gateway
      target: order-worker
      protocol: grpc
      interaction: calls
      transport: grpc

    - source: worker
      target: orders
      protocol: kafka
      interaction: publishes
      transport: kafka

    - source: gateway
      target: catalog-tools
      protocol: mcp
      interaction: invokes
      transport: streamable_http
```

Declared relationships have high confidence and `declared` evidence origin.
The declaration remains tied to its exact committed line.

## Runtime observations

Runtime observations are imported through an administrator-controlled API.
They are aggregate service-graph evidence, not source facts. RepoKarta keeps
provider, environment, time window, request count, error count, p95 latency,
and import time for 90 days.

```http
POST /api/dependencies/topology/observations
Content-Type: application/json

{
  "provider": "Grafana Tempo",
  "environment": "prod",
  "observations": [
    {
      "source_name": "checkout",
      "source_kind": "service",
      "target_name": "orders",
      "target_kind": "service",
      "protocol": "http",
      "interaction": "calls",
      "transport": "https",
      "observed_from": "2026-07-27T10:00:00Z",
      "observed_to": "2026-07-27T11:00:00Z",
      "request_count": 12542,
      "error_count": 18,
      "latency_p95_ms": 84.2
    },
    {
      "source_name": "orders",
      "source_kind": "service",
      "target_name": "orders.created",
      "target_kind": "queue",
      "protocol": "kafka",
      "interaction": "publishes",
      "transport": "kafka",
      "observed_from": "2026-07-27T10:00:00Z",
      "observed_to": "2026-07-27T11:00:00Z",
      "request_count": 8701,
      "error_count": 4,
      "latency_p95_ms": 12.8
    }
  ]
}
```

An import contains 1 to 5,000 observations. One observation window may span at
most 31 days. Error count cannot exceed request count. Imports require the
artifact-management permission and never mutate repositories.

The read endpoint defaults to the last 24 hours:

```text
GET /api/dependencies/topology
GET /api/dependencies/topology?repository=42&protocol=mcp
GET /api/dependencies/topology?origin=confirmed&environment=prod
GET /api/dependencies/topology?observed_from=2026-07-27T10:00:00Z&observed_to=2026-07-27T11:00:00Z
```

The read-only MCP equivalent is `read_system_topology`.

## Architecture drift states

- `confirmed`: the same directed endpoints, protocol, interaction, and
  transport exist in static and runtime evidence;
- `static_only`: source or declaration evidence exists, but no matching runtime
  observation is present in the selected window;
- `runtime_only`: traffic was observed, but no matching static relationship was
  extracted;
- unresolved peer: a static service or MCP target could not be matched to one
  unambiguous known component.

Static-only does not necessarily mean unused, and runtime-only does not
necessarily mean undocumented. Sampling, traffic windows, conditional paths,
and incomplete instrumentation can all explain a difference. The UI exposes
the raw evidence needed to make that judgment.

## Reference design choices

The topology combines complementary ideas from established systems:

- Datadog-style inferred databases, queues, and external services from outbound
  peer evidence;
- Grafana Tempo-style directed runtime service graphs and RED metrics;
- C4/Structurizr-style stable components, technology, explicit relationships,
  and focused views;
- Backstage-style catalog identities and declared relations.

RepoKarta differs by keeping immutable static evidence and mutable runtime
observations in separate stores and joining them only in the read model.

Further reading:

- [Datadog Service Map](https://docs.datadoghq.com/tracing/services/services_map/)
- [Datadog inferred services](https://docs.datadoghq.com/tracing/services/inferred_services/)
- [Grafana Tempo service graphs](https://grafana.com/docs/tempo/latest/metrics-from-traces/service_graphs/)
- [Structurizr DSL relationships](https://docs.structurizr.com/dsl/language)
- [Backstage catalog graph](https://backstage.io/docs/features/software-catalog/creating-the-catalog-graph/)
- [MCP architecture](https://modelcontextprotocol.io/docs/learn/architecture)
