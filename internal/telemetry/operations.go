package telemetry

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Operation is a bounded, low-cardinality RepoKarta lifecycle.
type Operation string

const (
	OperationCatalogueRefresh  Operation = "catalogue.refresh"
	OperationIndexBuild        Operation = "index.build"
	OperationSearch            Operation = "search"
	OperationRepositorySync    Operation = "repository.sync"
	OperationRepositoryImport  Operation = "repository.acquire"
	OperationGenerationChat    Operation = "generation.chat"
	OperationGenerationWiki    Operation = "generation.wiki"
	OperationAdvisoryRefresh   Operation = "advisory.refresh"
	OperationDependencyRefresh Operation = "dependency.refresh"
	OperationTopologyBuild     Operation = "topology.build"
	OperationMaintenance       Operation = "maintenance"
	OperationSCIPBuild         Operation = "scip.build"
	OperationGitCommand        Operation = "git.command"
	OperationProviderProcess   Operation = "provider.process"
)

// Labels contains only bounded dimensions permitted on RepoKarta metrics.
type Labels struct {
	Provider string
	Kind     string
	Trigger  string
	State    string
}

type operationInstruments struct {
	tracer   trace.Tracer
	total    metric.Int64Counter
	active   metric.Int64UpDownCounter
	duration metric.Float64Histogram
	tokens   metric.Int64Counter
	results  metric.Int64Histogram
}

var currentInstruments atomic.Pointer[operationInstruments]

func initializeOperations() {
	meter := otel.Meter(instrumentationName)
	total, _ := meter.Int64Counter(
		"repokarta.operation.total",
		metric.WithDescription("Completed RepoKarta operations."),
		metric.WithUnit("{operation}"),
	)
	active, _ := meter.Int64UpDownCounter(
		"repokarta.operation.active",
		metric.WithDescription("Currently active RepoKarta operations."),
		metric.WithUnit("{operation}"),
	)
	duration, _ := meter.Float64Histogram(
		"repokarta.operation.duration",
		metric.WithDescription("RepoKarta operation duration."),
		metric.WithUnit("s"),
	)
	tokens, _ := meter.Int64Counter(
		"repokarta.generation.tokens",
		metric.WithDescription("AI generation tokens reported by providers."),
		metric.WithUnit("{token}"),
	)
	results, _ := meter.Int64Histogram(
		"repokarta.search.results",
		metric.WithDescription("Bounded result count returned by a search."),
		metric.WithUnit("{result}"),
	)
	currentInstruments.Store(&operationInstruments{
		tracer:   otel.Tracer(instrumentationName),
		total:    total,
		active:   active,
		duration: duration,
		tokens:   tokens,
		results:  results,
	})
}

// StartOperation begins a correlated span and common RED-style measurements.
// The returned completion function must be called once.
func StartOperation(ctx context.Context, operation Operation, labels Labels) (context.Context, func(error)) {
	instruments := currentInstruments.Load()
	if instruments == nil {
		return ctx, func(error) {}
	}
	attrs := operationAttributes(operation, labels)
	ctx, span := instruments.tracer.Start(
		ctx,
		"repokarta."+string(operation),
		trace.WithAttributes(attrs...),
	)
	options := metric.WithAttributes(attrs...)
	instruments.active.Add(ctx, 1, options)
	started := time.Now()
	var completed atomic.Bool
	return ctx, func(err error) {
		if !completed.CompareAndSwap(false, true) {
			return
		}
		outcome := operationOutcome(err)
		finalAttrs := append(append([]attribute.KeyValue(nil), attrs...), attribute.String("repokarta.outcome", outcome))
		finalOptions := metric.WithAttributes(finalAttrs...)
		instruments.active.Add(ctx, -1, options)
		instruments.total.Add(ctx, 1, finalOptions)
		instruments.duration.Record(ctx, time.Since(started).Seconds(), finalOptions)
		span.SetAttributes(attribute.String("repokarta.outcome", outcome))
		if err != nil {
			span.SetStatus(codes.Error, "")
			span.SetAttributes(attribute.String("error.type", reflect.TypeOf(err).String()))
		} else {
			span.SetStatus(codes.Ok, "")
		}
		span.End()
	}
}

// RecordGenerationTokens records provider totals without conversation identity.
func RecordGenerationTokens(ctx context.Context, provider string, input, output int64) {
	instruments := currentInstruments.Load()
	if instruments == nil {
		return
	}
	provider = boundedProvider(provider)
	if input > 0 {
		instruments.tokens.Add(ctx, input, metric.WithAttributes(
			attribute.String("repokarta.provider", provider),
			attribute.String("repokarta.token.type", "input"),
		))
	}
	if output > 0 {
		instruments.tokens.Add(ctx, output, metric.WithAttributes(
			attribute.String("repokarta.provider", provider),
			attribute.String("repokarta.token.type", "output"),
		))
	}
}

// RecordSearchResults records only the bounded count and truncation state.
func RecordSearchResults(ctx context.Context, count int, truncated bool, kind string) {
	instruments := currentInstruments.Load()
	if instruments == nil || count < 0 {
		return
	}
	instruments.results.Record(ctx, int64(count), metric.WithAttributes(
		attribute.Bool("repokarta.search.truncated", truncated),
		attribute.String("repokarta.kind", boundedKind(kind)),
	))
}

func operationAttributes(operation Operation, labels Labels) []attribute.KeyValue {
	attrs := []attribute.KeyValue{attribute.String("repokarta.operation", string(operation))}
	if value := boundedProvider(labels.Provider); value != "" {
		attrs = append(attrs, attribute.String("repokarta.provider", value))
	}
	if value := boundedKind(labels.Kind); value != "" {
		attrs = append(attrs, attribute.String("repokarta.kind", value))
	}
	if value := boundedTrigger(labels.Trigger); value != "" {
		attrs = append(attrs, attribute.String("repokarta.trigger", value))
	}
	if value := boundedState(labels.State); value != "" {
		attrs = append(attrs, attribute.String("repokarta.state", value))
	}
	return attrs
}

func operationOutcome(err error) string {
	switch {
	case err == nil:
		return "success"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return "error"
	}
}

func boundedProvider(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "", "anthropic", "anthropic-api", "claude", "codex", "github",
		"gitlab", "local", "scip-java":
		return value
	default:
		return "other"
	}
}

func boundedKind(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "", "chat", "deep_search", "turn", "quality", "discover", "remove",
		"plan", "execute", "clone", "fetch", "init", "remote", "rev-parse",
		"show-ref", "symbolic-ref", "ready", "empty", "error", "zoekt",
		"regex", "text", "code", "other":
		return value
	default:
		return "other"
	}
}

func boundedTrigger(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "", "background", "index", "request":
		return value
	default:
		return "other"
	}
}

func boundedState(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "", "total", "open", "in_use", "idle", "max_open", "waits",
		"scan.ready", "scan.empty", "scan.error", "index.pending",
		"index.indexing", "index.ready", "index.error":
		return value
	default:
		return "other"
	}
}
