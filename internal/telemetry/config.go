package telemetry

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
)

const (
	serviceNameDefault = "repokarta"

	ProtocolGRPC         = "grpc"
	ProtocolHTTPProtobuf = "http/protobuf"
)

// SignalConfig describes one OTLP signal without retaining exporter headers.
type SignalConfig struct {
	Enabled  bool
	Protocol string
	Endpoint string
}

// Config is the validated, secret-free telemetry configuration.
//
// OTLP exporters still read the standard OTEL_* environment variables
// directly. This value records only the bounded choices RepoKarta needs for
// lifecycle decisions, diagnostics, and tests.
type Config struct {
	Enabled       bool
	ServiceName   string
	Version       string
	InstanceID    string
	Traces        SignalConfig
	Metrics       SignalConfig
	Logs          SignalConfig
	ConsoleFormat string
	ConsoleLevel  slog.Level
	ConsoleWriter io.Writer
}

// ConfigFromEnv validates the standard OpenTelemetry exporter selection.
//
// RepoKarta is deliberately disabled by default. Setting a general/per-signal
// OTLP endpoint or selecting the otlp exporter for a signal opts in. Resource
// attributes alone never cause network traffic.
func ConfigFromEnv(version string) (Config, error) {
	config := Config{
		ServiceName:   serviceNameDefault,
		Version:       strings.TrimSpace(version),
		ConsoleFormat: strings.ToLower(strings.TrimSpace(os.Getenv("REPOKARTA_LOG_FORMAT"))),
		ConsoleLevel:  slog.LevelInfo,
	}
	if config.ConsoleFormat == "" {
		config.ConsoleFormat = "json"
	}
	if config.ConsoleFormat != "json" && config.ConsoleFormat != "text" {
		return Config{}, errors.New("REPOKARTA_LOG_FORMAT must be json or text")
	}
	if value := strings.TrimSpace(os.Getenv("REPOKARTA_LOG_LEVEL")); value != "" {
		if err := config.ConsoleLevel.UnmarshalText([]byte(value)); err != nil {
			return Config{}, fmt.Errorf("parse REPOKARTA_LOG_LEVEL: %w", err)
		}
	}

	disabled, err := strictBoolEnv("OTEL_SDK_DISABLED")
	if err != nil {
		return Config{}, err
	}
	if disabled {
		return config, nil
	}

	generalEndpointConfigured := anyEnvSet("OTEL_EXPORTER_OTLP_ENDPOINT")
	config.Traces, err = signalFromEnv(
		"TRACES",
		generalEndpointConfigured || anyEnvSet("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"),
	)
	if err != nil {
		return Config{}, err
	}
	config.Metrics, err = signalFromEnv(
		"METRICS",
		generalEndpointConfigured || anyEnvSet("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"),
	)
	if err != nil {
		return Config{}, err
	}
	config.Logs, err = signalFromEnv(
		"LOGS",
		generalEndpointConfigured || anyEnvSet("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"),
	)
	if err != nil {
		return Config{}, err
	}
	if err := validateSDKEnvironment(); err != nil {
		return Config{}, err
	}
	config.Enabled = config.Traces.Enabled || config.Metrics.Enabled || config.Logs.Enabled
	return config, nil
}

func signalFromEnv(signal string, configured bool) (SignalConfig, error) {
	exporterName := "OTEL_" + signal + "_EXPORTER"
	exporter := strings.ToLower(strings.TrimSpace(os.Getenv(exporterName)))
	enabled := configured
	if exporter != "" {
		enabled = false
		values := strings.Split(exporter, ",")
		for _, value := range values {
			switch strings.TrimSpace(value) {
			case "otlp":
				enabled = true
			case "none":
				if len(values) != 1 {
					return SignalConfig{}, fmt.Errorf("%s cannot combine none with another exporter", exporterName)
				}
			case "":
				return SignalConfig{}, fmt.Errorf("%s contains an empty exporter", exporterName)
			default:
				return SignalConfig{}, fmt.Errorf(
					"%s supports only otlp or none in RepoKarta", exporterName,
				)
			}
		}
	}

	protocolName := "OTEL_EXPORTER_OTLP_" + signal + "_PROTOCOL"
	protocol := strings.ToLower(strings.TrimSpace(os.Getenv(protocolName)))
	if protocol == "" {
		protocol = strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")))
	}
	if protocol == "" {
		protocol = ProtocolGRPC
	}
	if protocol != ProtocolGRPC && protocol != ProtocolHTTPProtobuf {
		return SignalConfig{}, fmt.Errorf(
			"%s supports only grpc or http/protobuf", protocolName,
		)
	}

	endpoint := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_" + signal + "_ENDPOINT"))
	if endpoint == "" {
		endpoint = strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	}
	return SignalConfig{
		Enabled:  enabled,
		Protocol: protocol,
		Endpoint: sanitizeEndpoint(endpoint, protocol),
	}, nil
}

func strictBoolEnv(name string) (bool, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return false, nil
	}
	if !strings.EqualFold(value, "true") && !strings.EqualFold(value, "false") {
		return false, fmt.Errorf("%s must be true or false", name)
	}
	parsed, _ := strconv.ParseBool(value)
	return parsed, nil
}

func anyEnvSet(names ...string) bool {
	for _, name := range names {
		if value, ok := os.LookupEnv(name); ok && strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

func validateSDKEnvironment() error {
	for _, name := range []string{
		"OTEL_BSP_EXPORT_TIMEOUT", "OTEL_BSP_MAX_EXPORT_BATCH_SIZE",
		"OTEL_BSP_MAX_QUEUE_SIZE", "OTEL_BSP_SCHEDULE_DELAY",
		"OTEL_BLRP_EXPORT_TIMEOUT", "OTEL_BLRP_MAX_EXPORT_BATCH_SIZE",
		"OTEL_BLRP_MAX_QUEUE_SIZE", "OTEL_BLRP_SCHEDULE_DELAY",
		"OTEL_METRIC_EXPORT_INTERVAL", "OTEL_METRIC_EXPORT_TIMEOUT",
		"OTEL_EXPORTER_OTLP_TIMEOUT", "OTEL_EXPORTER_OTLP_TRACES_TIMEOUT",
		"OTEL_EXPORTER_OTLP_METRICS_TIMEOUT", "OTEL_EXPORTER_OTLP_LOGS_TIMEOUT",
	} {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			continue
		}
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed <= 0 {
			return fmt.Errorf("%s must be a positive integer number of milliseconds or items", name)
		}
	}
	sampler := strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_TRACES_SAMPLER")))
	switch sampler {
	case "", "always_on", "always_off", "traceidratio",
		"parentbased_always_on", "parentbased_always_off", "parentbased_traceidratio":
	default:
		return fmt.Errorf("OTEL_TRACES_SAMPLER %q is not supported by RepoKarta", sampler)
	}
	if sampler == "traceidratio" || sampler == "parentbased_traceidratio" {
		value := strings.TrimSpace(os.Getenv("OTEL_TRACES_SAMPLER_ARG"))
		ratio, err := strconv.ParseFloat(value, 64)
		if err != nil || ratio < 0 || ratio > 1 {
			return errors.New("OTEL_TRACES_SAMPLER_ARG must be a number from 0 through 1")
		}
	}
	return nil
}
