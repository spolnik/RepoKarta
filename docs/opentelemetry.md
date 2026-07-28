# OpenTelemetry operations

RepoKarta can export metrics, structured logs, and traces to any OTLP receiver.
The integration is vendor-neutral and disabled by default. A collector outage
does not affect `/healthz`, HTTP request handling, indexing, or background jobs.

## Enable OTLP

Use standard OpenTelemetry environment variables. This local example sends all
three signals over HTTP/protobuf to a collector bound on loopback:

```powershell
$env:OTEL_EXPORTER_OTLP_ENDPOINT = "http://127.0.0.1:4318"
$env:OTEL_EXPORTER_OTLP_PROTOCOL = "http/protobuf"
$env:OTEL_RESOURCE_ATTRIBUTES = "deployment.environment.name=development"
$env:OTEL_TRACES_SAMPLER = "parentbased_traceidratio"
$env:OTEL_TRACES_SAMPLER_ARG = "0.25"
.\repokarta.exe serve
```

Use port `4317` with `OTEL_EXPORTER_OTLP_PROTOCOL=grpc` for OTLP gRPC. Standard
per-signal endpoint, protocol, header, timeout, batch, sampling, TLS,
compression, and exporter variables are passed to the OpenTelemetry SDK. A
signal can be disabled independently:

```text
OTEL_TRACES_EXPORTER=otlp
OTEL_METRICS_EXPORTER=otlp
OTEL_LOGS_EXPORTER=none
```

`OTEL_SDK_DISABLED=true` takes precedence and makes no exporter connection.
Merely setting resource attributes does not enable telemetry. RepoKarta rejects
unsupported exporters/protocols and malformed queue, timeout, interval, or
sampling values during startup.

RepoKarta owns these non-secret resource attributes:

- `service.name=repokarta`
- `service.version` from the built executable
- a random `service.instance.id` for the running process

Add deployment and host metadata through `OTEL_RESOURCE_ATTRIBUTES`, for
example `deployment.environment.name=production,service.namespace=platform`.
Empty operator values cannot erase RepoKarta's owned service identity.

Local console output remains structured JSON. Set
`REPOKARTA_LOG_FORMAT=text` for development or `REPOKARTA_LOG_LEVEL=debug` for
more detail. OTLP logs use the same messages and include `request.id`,
`trace_id`, and `span_id` when present. The OTLP log bridge is isolated because
the OpenTelemetry Go log SDK and bridge remain pre-1.0.

## Local collector

The supplied [collector-debug.yaml](../deploy/otel/collector-debug.yaml) listens
only on loopback and prints received signals:

```powershell
docker run --rm `
  -v "${PWD}\deploy\otel\collector-debug.yaml:/etc/otelcol-contrib/config.yaml:ro" `
  -p 127.0.0.1:4317:4317 -p 127.0.0.1:4318:4318 `
  otel/opentelemetry-collector-contrib:latest
```

Pin the collector image digest in production. The debug exporter can print
payloads and should not be enabled on a shared or production collector.

## Datadog

RepoKarta never receives a Datadog API key. Put credentials in the Collector or
Datadog Agent environment and configure RepoKarta only with the local OTLP
receiver endpoint.

For the Collector Datadog exporter, start from
[collector-datadog.yaml](../deploy/otel/collector-datadog.yaml), supply
`DD_API_KEY` and `DD_SITE` to the Collector, and point RepoKarta at
`http://127.0.0.1:4318`. The example uses the Datadog connector for trace-derived
metrics and routes metrics, logs, and traces through the Datadog exporter.

For direct Datadog Agent ingestion, merge
[datadog-agent.yaml](../deploy/otel/datadog-agent.yaml) into `datadog.yaml`,
restart the Agent, and point RepoKarta at the Agent's loopback OTLP port. Change
the bind address only when the receiver is intentionally exposed and protected
by the deployment network.

## Signals and cardinality

HTTP spans and metrics use method, bounded route template, status, duration,
active request, and body-size conventions. Raw URLs and query strings are
removed before instrumentation. Outbound HTTP spans likewise omit the target
path and query.

| Instrument | Type | Purpose | Bounded attributes |
| --- | --- | --- | --- |
| `repokarta.operation.total` | monotonic counter | completed core jobs | operation, outcome, kind, trigger, provider |
| `repokarta.operation.active` | up/down counter | in-flight core jobs and backlog pressure | operation, kind, trigger, provider |
| `repokarta.operation.duration` | seconds histogram | job latency | operation, outcome, kind, trigger, provider |
| `repokarta.search.results` | result-count histogram | result size and truncation | kind, truncated |
| `repokarta.generation.tokens` | monotonic counter | provider-reported token usage | provider, token type |
| `repokarta.catalogue.repositories` | gauge | catalogue/index lifecycle counts | state |
| `repokarta.database.connections` | gauge | database pool state | state |

Operation values cover catalogue refresh, index build, search, repository
acquire/sync, chat and Wiki generation, dependency/advisory refresh, topology
build, SCIP build, maintenance, Git commands, and provider processes.

Metric attributes never include repository names, conversations, principals,
source paths, queries, prompts, source content, credentials, headers, tokens,
or database URLs. OTLP log attributes whose keys can carry those values are
replaced with `[redacted]`. Local console logs retain operational detail for
the machine operator; control their storage and access accordingly.

Metrics use cumulative sums and explicit histograms from the Go SDK. Preserve
temporality in the collector and allow the Datadog exporter to translate
cumulative counters and histogram buckets. Do not convert duration histograms
to gauges.

## Starter dashboards and alerts

- Availability: request rate plus `/healthz`; alert on health check failure.
- HTTP failures: rate of server requests with status `5xx`; warn above 2% for
  10 minutes.
- HTTP latency: p95 and p99 of `http.server.request.duration` by route; alert on
  a sustained route-specific SLO breach.
- Indexing: active `index.build` operations for pressure, plus error ratio and
  p95 duration.
- Generation: error ratio and p95 duration for `generation.chat` and
  `generation.wiki`; graph tokens separately by provider and token type.
- Sync and advisory freshness: completed/error rate and time since the latest
  successful `repository.sync` and `advisory.refresh` operations.
- Database saturation: in-use connections divided by open connections; warn
  above 80% for 10 minutes.
- Telemetry delivery: poll the administrator diagnostic endpoint and alert
  when failed export items increase or `last_success_at` becomes stale.

## Diagnostics and failure behavior

An administrator with `manage_security` permission can read
`GET /api/admin/telemetry`. It returns enabled signals, protocol, a sanitized
endpoint, configured queue capacity, last successful and failed export times,
current queue depth, counted queue-overflow drops, and failed export item
counts. Headers, user information, query parameters, and endpoint credentials
are never returned.

Exporter work is batched and queue-bounded by `OTEL_BSP_*` and `OTEL_BLRP_*`.
Metric export intervals and exporter timeouts use standard SDK settings. Export
errors go directly to the local console so they cannot recursively enter the
OTLP log pipeline. Shutdown has a five-second application deadline; pending
telemetry is flushed within it and then application shutdown continues.

If the receiver is unavailable, RepoKarta continues serving and processing.
Use diagnostics to distinguish an enabled-but-failing pipeline from disabled
telemetry. `/healthz` deliberately reports application health, not collector
availability.
