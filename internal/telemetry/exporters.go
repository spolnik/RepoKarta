package telemetry

import (
	"context"

	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type observedTraceExporter struct {
	sdktrace.SpanExporter
	state *deliveryState
}

func (exporter observedTraceExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	err := exporter.SpanExporter.ExportSpans(ctx, spans)
	exporter.state.record(len(spans), err)
	return err
}

type observedLogExporter struct {
	sdklog.Exporter
	state *deliveryState
}

func (exporter observedLogExporter) Export(ctx context.Context, records []sdklog.Record) error {
	err := exporter.Exporter.Export(ctx, records)
	exporter.state.record(len(records), err)
	return err
}

type observedMetricExporter struct {
	sdkmetric.Exporter
	state *deliveryState
}

func (exporter observedMetricExporter) Export(ctx context.Context, metrics *metricdata.ResourceMetrics) error {
	err := exporter.Exporter.Export(ctx, metrics)
	exporter.state.record(metricPointCount(metrics), err)
	return err
}

func metricPointCount(metrics *metricdata.ResourceMetrics) int {
	if metrics == nil {
		return 0
	}
	total := 0
	for _, scope := range metrics.ScopeMetrics {
		for _, current := range scope.Metrics {
			switch data := current.Data.(type) {
			case metricdata.Gauge[int64]:
				total += len(data.DataPoints)
			case metricdata.Gauge[float64]:
				total += len(data.DataPoints)
			case metricdata.Sum[int64]:
				total += len(data.DataPoints)
			case metricdata.Sum[float64]:
				total += len(data.DataPoints)
			case metricdata.Histogram[int64]:
				total += len(data.DataPoints)
			case metricdata.Histogram[float64]:
				total += len(data.DataPoints)
			case metricdata.ExponentialHistogram[int64]:
				total += len(data.DataPoints)
			case metricdata.ExponentialHistogram[float64]:
				total += len(data.DataPoints)
			case metricdata.Summary:
				total += len(data.DataPoints)
			}
		}
	}
	return total
}
