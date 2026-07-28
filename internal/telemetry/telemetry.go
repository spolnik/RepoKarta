package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	otelruntime "go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	oteltrace "go.opentelemetry.io/otel/trace"
)

const (
	instrumentationName = "github.com/spolnik/RepoKarta"
	traceQueueDefault   = 2048
	logQueueDefault     = 2048
)

// System owns RepoKarta's process-wide OpenTelemetry lifecycle.
type System struct {
	config Config

	traceProvider  *trace.TracerProvider
	meterProvider  *metric.MeterProvider
	loggerProvider *log.LoggerProvider

	traceState  *deliveryState
	metricState *deliveryState
	logState    *deliveryState

	previousLogger *slog.Logger
	previousTracer oteltrace.TracerProvider
	previousMeter  otelmetric.MeterProvider
	previousLogs   otellog.LoggerProvider
	previousProp   propagation.TextMapPropagator
	previousErrors otel.ErrorHandler
	shutdownOnce   sync.Once
	shutdownError  error
}

// New configures structured console logging and optional OTLP exporters.
func New(ctx context.Context, config Config) (*System, error) {
	config = normalizeConfig(config)
	if config.InstanceID == "" {
		instanceID, err := randomInstanceID()
		if err != nil {
			return nil, fmt.Errorf("create telemetry service instance ID: %w", err)
		}
		config.InstanceID = instanceID
	}
	console := consoleHandler(config)
	system := &System{
		config:         config,
		traceState:     newDeliveryState(config.Traces, envPositiveInt("OTEL_BSP_MAX_QUEUE_SIZE", traceQueueDefault)),
		metricState:    newDeliveryState(config.Metrics, 1),
		logState:       newDeliveryState(config.Logs, envPositiveInt("OTEL_BLRP_MAX_QUEUE_SIZE", logQueueDefault)),
		previousLogger: slog.Default(),
		previousTracer: otel.GetTracerProvider(),
		previousMeter:  otel.GetMeterProvider(),
		previousLogs:   global.GetLoggerProvider(),
		previousProp:   otel.GetTextMapPropagator(),
		previousErrors: otel.GetErrorHandler(),
	}
	if !config.Enabled {
		currentInstruments.Store(nil)
		slog.SetDefault(slog.New(contextHandler{next: console}))
		return system, nil
	}

	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	otel.SetErrorHandler(errorHandler{console: console, system: system})

	res, err := resource.New(
		ctx,
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithOS(),
		resource.WithProcessRuntimeDescription(),
		resource.WithAttributes(
			semconv.ServiceName(config.ServiceName),
			semconv.ServiceVersion(config.Version),
			semconv.ServiceInstanceID(config.InstanceID),
		),
	)
	if err != nil {
		_ = system.Shutdown(context.Background())
		return nil, fmt.Errorf("detect OpenTelemetry resource: %w", err)
	}

	if config.Traces.Enabled {
		exporter, err := newTraceExporter(ctx, config.Traces)
		if err != nil {
			_ = system.Shutdown(context.Background())
			return nil, fmt.Errorf("initialize OTLP trace exporter: %w", err)
		}
		observed := observedTraceExporter{
			SpanExporter: exporter,
			state:        system.traceState,
		}
		system.traceProvider = trace.NewTracerProvider(
			trace.WithResource(res),
			trace.WithSpanProcessor(newTraceBatchProcessor(observed, system.traceState)),
		)
		otel.SetTracerProvider(system.traceProvider)
	}

	if config.Metrics.Enabled {
		exporter, err := newMetricExporter(ctx, config.Metrics)
		if err != nil {
			_ = system.Shutdown(context.Background())
			return nil, fmt.Errorf("initialize OTLP metric exporter: %w", err)
		}
		reader := metric.NewPeriodicReader(observedMetricExporter{
			Exporter: exporter,
			state:    system.metricState,
		})
		system.meterProvider = metric.NewMeterProvider(
			metric.WithResource(res),
			metric.WithReader(reader),
			metric.WithCardinalityLimit(256),
		)
		otel.SetMeterProvider(system.meterProvider)
		if err := otelruntime.Start(otelruntime.WithMeterProvider(system.meterProvider)); err != nil {
			_ = system.Shutdown(context.Background())
			return nil, fmt.Errorf("start Go runtime metrics: %w", err)
		}
	}

	logHandlers := []slog.Handler{console}
	if config.Logs.Enabled {
		exporter, err := newLogExporter(ctx, config.Logs)
		if err != nil {
			_ = system.Shutdown(context.Background())
			return nil, fmt.Errorf("initialize OTLP log exporter: %w", err)
		}
		system.loggerProvider = log.NewLoggerProvider(
			log.WithResource(res),
			log.WithProcessor(newLogBatchProcessor(observedLogExporter{
				Exporter: exporter,
				state:    system.logState,
			}, system.logState)),
		)
		global.SetLoggerProvider(system.loggerProvider)
		logHandlers = append(logHandlers, redactingHandler{next: otelslog.NewHandler(
			instrumentationName,
			otelslog.WithLoggerProvider(system.loggerProvider),
			otelslog.WithVersion(config.Version),
		)})
	}
	slog.SetDefault(slog.New(contextHandler{next: fanoutHandler{handlers: logHandlers}}))
	initializeOperations()
	return system, nil
}

func normalizeConfig(config Config) Config {
	if config.ServiceName == "" {
		config.ServiceName = serviceNameDefault
	}
	if config.Version == "" {
		config.Version = "dev"
	}
	if config.ConsoleFormat == "" {
		config.ConsoleFormat = "json"
	}
	if config.ConsoleWriter == nil {
		config.ConsoleWriter = os.Stderr
	}
	return config
}

func consoleHandler(config Config) slog.Handler {
	options := &slog.HandlerOptions{Level: config.ConsoleLevel}
	if config.ConsoleFormat == "text" {
		return slog.NewTextHandler(config.ConsoleWriter, options)
	}
	return slog.NewJSONHandler(config.ConsoleWriter, options)
}

func newTraceExporter(ctx context.Context, config SignalConfig) (trace.SpanExporter, error) {
	if config.Protocol == ProtocolHTTPProtobuf {
		return otlptracehttp.New(ctx)
	}
	return otlptracegrpc.New(ctx)
}

func newMetricExporter(ctx context.Context, config SignalConfig) (metric.Exporter, error) {
	if config.Protocol == ProtocolHTTPProtobuf {
		return otlpmetrichttp.New(ctx)
	}
	return otlpmetricgrpc.New(ctx)
}

func newLogExporter(ctx context.Context, config SignalConfig) (log.Exporter, error) {
	if config.Protocol == ProtocolHTTPProtobuf {
		return otlploghttp.New(ctx)
	}
	return otlploggrpc.New(ctx)
}

// Status returns a secret-free snapshot suitable for an administrator API.
func (system *System) Status() Status {
	if system == nil {
		return Status{Signals: map[string]SignalStatus{}}
	}
	return Status{
		Enabled:     system.config.Enabled,
		ServiceName: system.config.ServiceName,
		Version:     system.config.Version,
		InstanceID:  system.config.InstanceID,
		Signals: map[string]SignalStatus{
			"traces":  system.traceState.snapshot(),
			"metrics": system.metricState.snapshot(),
			"logs":    system.logState.snapshot(),
		},
	}
}

// Shutdown flushes each configured provider once and restores local logging.
func (system *System) Shutdown(ctx context.Context) error {
	if system == nil {
		return nil
	}
	system.shutdownOnce.Do(func() {
		var failures []error
		if system.loggerProvider != nil {
			failures = appendFailure(failures, system.loggerProvider.Shutdown(ctx))
		}
		if system.meterProvider != nil {
			failures = appendFailure(failures, system.meterProvider.Shutdown(ctx))
		}
		if system.traceProvider != nil {
			failures = appendFailure(failures, system.traceProvider.Shutdown(ctx))
		}
		if system.previousLogger != nil {
			slog.SetDefault(system.previousLogger)
		}
		otel.SetTracerProvider(system.previousTracer)
		otel.SetMeterProvider(system.previousMeter)
		global.SetLoggerProvider(system.previousLogs)
		otel.SetTextMapPropagator(system.previousProp)
		otel.SetErrorHandler(system.previousErrors)
		currentInstruments.Store(nil)
		system.shutdownError = errors.Join(failures...)
	})
	return system.shutdownError
}

func appendFailure(failures []error, err error) []error {
	if err != nil {
		return append(failures, err)
	}
	return failures
}

type errorHandler struct {
	console slog.Handler
	system  *System
}

func (handler errorHandler) Handle(err error) {
	record := slog.NewRecord(time.Now(), slog.LevelError, "OpenTelemetry delivery failure", 0)
	record.AddAttrs(slog.String("error", deliveryError(err)))
	_ = handler.console.Handle(context.Background(), record)
}

func randomInstanceID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" +
		encoded[16:20] + "-" + encoded[20:32], nil
}

func envPositiveInt(name string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(name))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}
