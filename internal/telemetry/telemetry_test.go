package telemetry

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spolnik/RepoKarta/internal/audit"
	collectlogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collecttrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"
)

func TestConfigFromEnvIsDisabledByDefaultAndValidatesOptIn(t *testing.T) {
	clearTelemetryEnvironment(t)
	config, err := ConfigFromEnv("1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if config.Enabled || config.Traces.Enabled || config.Metrics.Enabled || config.Logs.Enabled {
		t.Fatalf("default telemetry config = %#v", config)
	}

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4318")
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", ProtocolHTTPProtobuf)
	t.Setenv("OTEL_LOGS_EXPORTER", "none")
	config, err = ConfigFromEnv("1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if !config.Enabled || !config.Traces.Enabled || !config.Metrics.Enabled || config.Logs.Enabled {
		t.Fatalf("opt-in telemetry config = %#v", config)
	}
	if config.Traces.Endpoint != "http://collector:4318" {
		t.Fatalf("sanitized endpoint = %q", config.Traces.Endpoint)
	}
}

func TestConfigFromEnvRejectsInvalidStandardSettings(t *testing.T) {
	for name, value := range map[string]string{
		"OTEL_EXPORTER_OTLP_PROTOCOL": "http/json",
		"OTEL_TRACES_EXPORTER":        "zipkin",
		"OTEL_BSP_MAX_QUEUE_SIZE":     "unbounded",
		"OTEL_TRACES_SAMPLER":         "custom",
	} {
		t.Run(name, func(t *testing.T) {
			clearTelemetryEnvironment(t)
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4318")
			t.Setenv(name, value)
			if _, err := ConfigFromEnv("test"); err == nil {
				t.Fatalf("%s=%q was accepted", name, value)
			}
		})
	}
}

func TestConfigFromEnvDoesNotEnableOtherSignalsFromExporterSelection(t *testing.T) {
	clearTelemetryEnvironment(t)
	t.Setenv("OTEL_LOGS_EXPORTER", "none")
	config, err := ConfigFromEnv("test")
	if err != nil {
		t.Fatal(err)
	}
	if config.Enabled || config.Traces.Enabled || config.Metrics.Enabled || config.Logs.Enabled {
		t.Fatalf("signal-specific opt-out enabled telemetry: %#v", config)
	}
}

func TestMetricDimensionsCollapseUnknownValues(t *testing.T) {
	if got := boundedProvider("private-repository"); got != "other" {
		t.Fatalf("unknown provider = %q", got)
	}
	if got := boundedKind("query-secret"); got != "other" {
		t.Fatalf("unknown kind = %q", got)
	}
	if got := boundedTrigger("principal@example.com"); got != "other" {
		t.Fatalf("unknown trigger = %q", got)
	}
	if got := boundedState(`C:\private\repository`); got != "other" {
		t.Fatalf("unknown state = %q", got)
	}
}

func TestUnavailableCollectorDoesNotBlockRequestsAndBecomesDiagnostic(t *testing.T) {
	clearTelemetryEnvironment(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "http://" + listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", endpoint+"/v1/traces")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", ProtocolHTTPProtobuf)
	t.Setenv("OTEL_METRICS_EXPORTER", "none")
	t.Setenv("OTEL_LOGS_EXPORTER", "none")
	t.Setenv("OTEL_TRACES_SAMPLER", "always_on")
	t.Setenv("OTEL_BSP_SCHEDULE_DELAY", "10")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_TIMEOUT", "50")

	config, err := ConfigFromEnv("test")
	if err != nil {
		t.Fatal(err)
	}
	config.ConsoleWriter = io.Discard
	system, err := New(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	handler := system.HTTPHandler(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusNoContent)
	}))
	started := time.Now()
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/private", nil))
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("request waited for unavailable collector: %s", elapsed)
	}

	deadline := time.Now().Add(2 * time.Second)
	for system.Status().Signals["traces"].LastErrorAt == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	status := system.Status().Signals["traces"]
	if status.LastErrorAt == nil || status.FailedExportItems == 0 {
		t.Fatalf("unavailable collector status = %#v", status)
	}
	if strings.Contains(status.LastError, endpoint) {
		t.Fatalf("diagnostic leaked endpoint: %q", status.LastError)
	}

	shutdownContext, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := system.Shutdown(shutdownContext); err != nil {
		t.Fatal(err)
	}
}

func TestDeliveryStatusCountsFailedItemsWithoutErrorDetail(t *testing.T) {
	state := newDeliveryState(SignalConfig{
		Enabled: true, Protocol: ProtocolHTTPProtobuf, Endpoint: "http://collector:4318",
	}, 8)
	state.record(3, errors.New("token=secret at http://collector/private"))
	status := state.snapshot()
	if status.FailedExportItems != 3 || status.LastErrorAt == nil {
		t.Fatalf("delivery failure status = %#v", status)
	}
	if strings.Contains(status.LastError, "secret") || strings.Contains(status.LastError, "private") {
		t.Fatalf("delivery failure leaked detail: %q", status.LastError)
	}
}

func TestTraceQueueOverflowIsBoundedAndCounted(t *testing.T) {
	clearTelemetryEnvironment(t)
	exportStarted := make(chan struct{})
	releaseExport := make(chan struct{})
	receiver := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		select {
		case <-exportStarted:
		default:
			close(exportStarted)
		}
		<-releaseExport
		response.Header().Set("Content-Type", "application/x-protobuf")
		payload, _ := proto.Marshal(new(collecttrace.ExportTraceServiceResponse))
		_, _ = response.Write(payload)
	}))
	defer receiver.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", receiver.URL+"/v1/traces")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", ProtocolHTTPProtobuf)
	t.Setenv("OTEL_METRICS_EXPORTER", "none")
	t.Setenv("OTEL_LOGS_EXPORTER", "none")
	t.Setenv("OTEL_TRACES_SAMPLER", "always_on")
	t.Setenv("OTEL_BSP_MAX_QUEUE_SIZE", "1")
	t.Setenv("OTEL_BSP_MAX_EXPORT_BATCH_SIZE", "1")
	t.Setenv("OTEL_BSP_SCHEDULE_DELAY", "1")

	config, err := ConfigFromEnv("test")
	if err != nil {
		t.Fatal(err)
	}
	config.ConsoleWriter = io.Discard
	system, err := New(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	_, finish := StartOperation(context.Background(), OperationSearch, Labels{Kind: "code"})
	finish(nil)
	select {
	case <-exportStarted:
	case <-time.After(time.Second):
		t.Fatal("trace export did not start")
	}

	started := time.Now()
	for range 100 {
		_, finish = StartOperation(context.Background(), OperationSearch, Labels{Kind: "code"})
		finish(nil)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("queue overflow blocked producers: %s", elapsed)
	}
	status := system.Status().Signals["traces"]
	if status.QueueCapacity != 1 || status.QueueDepth > 1 || status.DroppedItems == 0 {
		t.Fatalf("bounded queue status = %#v", status)
	}

	close(releaseExport)
	shutdownContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := system.Shutdown(shutdownContext); err != nil {
		t.Fatal(err)
	}
}

func TestOTLPHTTPExportsCorrelatedRedactedSignals(t *testing.T) {
	clearTelemetryEnvironment(t)
	receiver := newOTLPReceiver(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", receiver.server.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", ProtocolHTTPProtobuf)
	t.Setenv("OTEL_TRACES_SAMPLER", "always_on")
	t.Setenv("OTEL_BSP_SCHEDULE_DELAY", "10")
	t.Setenv("OTEL_BLRP_SCHEDULE_DELAY", "10")
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "20")
	t.Setenv("OTEL_EXPORTER_OTLP_TIMEOUT", "1000")
	t.Setenv(
		"OTEL_RESOURCE_ATTRIBUTES",
		"service.name=misleading,service.version=misleading,service.instance.id=misleading,deployment.environment.name=test",
	)

	config, err := ConfigFromEnv("1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	var console bytes.Buffer
	config.ConsoleWriter = &console
	config.InstanceID = "11111111-2222-4333-8444-555555555555"
	system, err := New(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = system.Shutdown(shutdownContext)
	}()
	if err := system.RegisterRuntimeSnapshot(func(context.Context) (RuntimeSnapshot, error) {
		return RuntimeSnapshot{
			Repositories: map[string]int64{"total": 3, "index.ready": 2},
			Database:     map[string]int64{"open": 1},
		}, nil
	}); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /projects/{repository}", func(response http.ResponseWriter, request *http.Request) {
		operationContext, finish := StartOperation(request.Context(), OperationSearch, Labels{Kind: "code"})
		slog.InfoContext(
			operationContext,
			"handled project request",
			"query", "supersecret-query",
			"operation", "read",
		)
		RecordSearchResults(operationContext, 2, false, "code")
		finish(nil)
		response.WriteHeader(http.StatusNoContent)
	})
	handler := system.HTTPHandler(system.RouteHandler(mux))
	request := httptest.NewRequest(
		http.MethodGet,
		"http://repokarta.test/projects/private-repository?query=needle",
		nil,
	)
	request = request.WithContext(audit.WithCorrelationID(request.Context(), "request-123"))
	request.Header.Set(
		"traceparent",
		"00-0102030405060708090a0b0c0d0e0f10-0102030405060708-01",
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("HTTP status = %d", response.Code)
	}

	shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := system.Shutdown(shutdownContext); err != nil {
		t.Fatal(err)
	}

	receiver.assertNoPayloadContains(t, "needle", "private-repository", "supersecret-query")
	traces := receiver.traces(t)
	logs := receiver.logs(t)
	metrics := receiver.metrics(t)
	assertResourceIdentity(t, traces[0].ResourceSpans[0].Resource)
	assertTraceRoute(t, traces, "/projects/{repository}")
	assertTraceParent(t, traces, "/projects/{repository}", "0102030405060708090a0b0c0d0e0f10")
	assertCorrelatedLog(t, logs)
	assertMetricNames(t, metrics,
		"http.server.request.duration",
		"repokarta.operation.total",
		"repokarta.search.results",
		"repokarta.catalogue.repositories",
		"repokarta.database.connections",
	)
	status := system.Status()
	for signal, current := range status.Signals {
		if !current.Enabled || current.LastSuccessAt == nil || current.FailedExportItems != 0 {
			t.Fatalf("%s delivery status = %#v", signal, current)
		}
	}
	if !strings.Contains(console.String(), `"trace_id"`) ||
		!strings.Contains(console.String(), `"request.id":"request-123"`) {
		t.Fatalf("structured console log lacks correlation: %s", console.String())
	}
}

type otlpReceiver struct {
	server *httptest.Server
	mu     sync.Mutex
	bodies map[string][][]byte
}

func newOTLPReceiver(t *testing.T) *otlpReceiver {
	t.Helper()
	receiver := &otlpReceiver{bodies: make(map[string][][]byte)}
	receiver.server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(response, err.Error(), http.StatusBadRequest)
			return
		}
		receiver.mu.Lock()
		receiver.bodies[request.URL.Path] = append(receiver.bodies[request.URL.Path], body)
		receiver.mu.Unlock()
		response.Header().Set("Content-Type", "application/x-protobuf")
		var payload []byte
		switch request.URL.Path {
		case "/v1/traces":
			payload, _ = proto.Marshal(new(collecttrace.ExportTraceServiceResponse))
		case "/v1/metrics":
			payload, _ = proto.Marshal(new(collectmetrics.ExportMetricsServiceResponse))
		case "/v1/logs":
			payload, _ = proto.Marshal(new(collectlogs.ExportLogsServiceResponse))
		default:
			http.NotFound(response, request)
			return
		}
		_, _ = response.Write(payload)
	}))
	t.Cleanup(receiver.server.Close)
	return receiver
}

func (receiver *otlpReceiver) assertNoPayloadContains(t *testing.T, values ...string) {
	t.Helper()
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	for path, bodies := range receiver.bodies {
		for _, body := range bodies {
			for _, value := range values {
				if bytes.Contains(body, []byte(value)) {
					t.Fatalf("%s OTLP payload contains %q", path, value)
				}
			}
		}
	}
}

func (receiver *otlpReceiver) traces(t *testing.T) []*collecttrace.ExportTraceServiceRequest {
	t.Helper()
	return decodeRequests(t, receiver, "/v1/traces", func() *collecttrace.ExportTraceServiceRequest {
		return new(collecttrace.ExportTraceServiceRequest)
	})
}

func (receiver *otlpReceiver) logs(t *testing.T) []*collectlogs.ExportLogsServiceRequest {
	t.Helper()
	return decodeRequests(t, receiver, "/v1/logs", func() *collectlogs.ExportLogsServiceRequest {
		return new(collectlogs.ExportLogsServiceRequest)
	})
}

func (receiver *otlpReceiver) metrics(t *testing.T) []*collectmetrics.ExportMetricsServiceRequest {
	t.Helper()
	return decodeRequests(t, receiver, "/v1/metrics", func() *collectmetrics.ExportMetricsServiceRequest {
		return new(collectmetrics.ExportMetricsServiceRequest)
	})
}

func decodeRequests[T proto.Message](
	t *testing.T,
	receiver *otlpReceiver,
	path string,
	create func() T,
) []T {
	t.Helper()
	receiver.mu.Lock()
	bodies := append([][]byte(nil), receiver.bodies[path]...)
	receiver.mu.Unlock()
	if len(bodies) == 0 {
		t.Fatalf("no OTLP requests received at %s", path)
	}
	output := make([]T, 0, len(bodies))
	for _, body := range bodies {
		request := create()
		if err := proto.Unmarshal(body, request); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		output = append(output, request)
	}
	return output
}

func assertResourceIdentity(t *testing.T, resource *resourcepb.Resource) {
	t.Helper()
	attrs := keyValues(resource.Attributes)
	for key, expected := range map[string]string{
		"service.name":                "repokarta",
		"service.version":             "1.2.3",
		"service.instance.id":         "11111111-2222-4333-8444-555555555555",
		"deployment.environment.name": "test",
	} {
		if attrs[key] != expected {
			t.Fatalf("resource %s = %q, want %q", key, attrs[key], expected)
		}
	}
}

func assertTraceParent(
	t *testing.T,
	requests []*collecttrace.ExportTraceServiceRequest,
	route string,
	expectedTraceID string,
) {
	t.Helper()
	for _, request := range requests {
		for _, resources := range request.ResourceSpans {
			for _, scope := range resources.ScopeSpans {
				for _, span := range scope.Spans {
					if keyValues(span.Attributes)["http.route"] != route {
						continue
					}
					if got := hex.EncodeToString(span.TraceId); got != expectedTraceID {
						t.Fatalf("server trace ID = %q, want propagated %q", got, expectedTraceID)
					}
					return
				}
			}
		}
	}
	t.Fatalf("server span for route %q was not exported", route)
}

func assertTraceRoute(t *testing.T, requests []*collecttrace.ExportTraceServiceRequest, route string) {
	t.Helper()
	var exported []string
	for _, request := range requests {
		for _, resources := range request.ResourceSpans {
			for _, scope := range resources.ScopeSpans {
				for _, span := range scope.Spans {
					attributes := keyValues(span.Attributes)
					exported = append(exported, span.Name+" "+attributes["http.route"])
					if attributes["http.route"] == route {
						return
					}
				}
			}
		}
	}
	t.Fatalf("trace route %q was not exported; spans = %#v", route, exported)
}

func assertCorrelatedLog(t *testing.T, requests []*collectlogs.ExportLogsServiceRequest) {
	t.Helper()
	for _, request := range requests {
		for _, resources := range request.ResourceLogs {
			for _, scope := range resources.ScopeLogs {
				for _, record := range scope.LogRecords {
					if record.Body.GetStringValue() != "handled project request" {
						continue
					}
					attrs := keyValues(record.Attributes)
					if attrs["query"] != "[redacted]" || attrs["request.id"] != "request-123" {
						t.Fatalf("redacted log attributes = %#v", attrs)
					}
					if len(record.TraceId) == 0 || len(record.SpanId) == 0 {
						t.Fatal("OTLP log lacks trace/span correlation")
					}
					return
				}
			}
		}
	}
	t.Fatal("correlated OTLP log was not exported")
}

func assertMetricNames(
	t *testing.T,
	requests []*collectmetrics.ExportMetricsServiceRequest,
	expected ...string,
) {
	t.Helper()
	found := make(map[string]bool)
	for _, request := range requests {
		for _, resources := range request.ResourceMetrics {
			for _, scope := range resources.ScopeMetrics {
				for _, current := range scope.Metrics {
					found[current.Name] = true
				}
			}
		}
	}
	for _, name := range expected {
		if !found[name] {
			t.Errorf("metric %q missing from %#v", name, found)
		}
	}
}

func keyValues(values []*commonpb.KeyValue) map[string]string {
	output := make(map[string]string, len(values))
	for _, value := range values {
		output[value.Key] = value.Value.GetStringValue()
	}
	return output
}

func clearTelemetryEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"OTEL_SDK_DISABLED",
		"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_PROTOCOL",
		"OTEL_EXPORTER_OTLP_TIMEOUT", "OTEL_EXPORTER_OTLP_HEADERS",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL",
		"OTEL_EXPORTER_OTLP_TRACES_TIMEOUT", "OTEL_EXPORTER_OTLP_TRACES_HEADERS",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL",
		"OTEL_EXPORTER_OTLP_METRICS_TIMEOUT", "OTEL_EXPORTER_OTLP_METRICS_HEADERS",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "OTEL_EXPORTER_OTLP_LOGS_PROTOCOL",
		"OTEL_EXPORTER_OTLP_LOGS_TIMEOUT", "OTEL_EXPORTER_OTLP_LOGS_HEADERS",
		"OTEL_TRACES_EXPORTER", "OTEL_METRICS_EXPORTER", "OTEL_LOGS_EXPORTER",
		"OTEL_TRACES_SAMPLER", "OTEL_TRACES_SAMPLER_ARG",
		"OTEL_BSP_EXPORT_TIMEOUT", "OTEL_BSP_MAX_EXPORT_BATCH_SIZE",
		"OTEL_BSP_MAX_QUEUE_SIZE", "OTEL_BSP_SCHEDULE_DELAY",
		"OTEL_BLRP_EXPORT_TIMEOUT", "OTEL_BLRP_MAX_EXPORT_BATCH_SIZE",
		"OTEL_BLRP_MAX_QUEUE_SIZE", "OTEL_BLRP_SCHEDULE_DELAY",
		"OTEL_METRIC_EXPORT_INTERVAL", "OTEL_METRIC_EXPORT_TIMEOUT",
		"OTEL_RESOURCE_ATTRIBUTES", "OTEL_SERVICE_NAME",
		"REPOKARTA_LOG_FORMAT", "REPOKARTA_LOG_LEVEL",
	} {
		t.Setenv(name, "")
	}
}
