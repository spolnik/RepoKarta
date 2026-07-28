package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"
)

// Status is a credential-free operator view of the telemetry pipeline.
type Status struct {
	Enabled     bool                    `json:"enabled"`
	ServiceName string                  `json:"service_name"`
	Version     string                  `json:"service_version"`
	InstanceID  string                  `json:"service_instance_id"`
	Signals     map[string]SignalStatus `json:"signals"`
}

// SignalStatus reports delivery state without exporter headers or URL secrets.
type SignalStatus struct {
	Enabled           bool       `json:"enabled"`
	Protocol          string     `json:"protocol,omitempty"`
	Endpoint          string     `json:"endpoint,omitempty"`
	QueueCapacity     int        `json:"queue_capacity,omitempty"`
	QueueDepth        int        `json:"queue_depth"`
	DroppedItems      uint64     `json:"dropped_items"`
	LastSuccessAt     *time.Time `json:"last_success_at,omitempty"`
	LastErrorAt       *time.Time `json:"last_error_at,omitempty"`
	LastError         string     `json:"last_error,omitempty"`
	FailedExportItems uint64     `json:"failed_export_items"`
}

type deliveryState struct {
	mu                sync.RWMutex
	config            SignalStatus
	lastSuccessAt     time.Time
	lastErrorAt       time.Time
	lastError         string
	failedExportItems uint64
	queueDepth        int
	droppedItems      uint64
}

func newDeliveryState(config SignalConfig, queueCapacity int) *deliveryState {
	return &deliveryState{config: SignalStatus{
		Enabled:       config.Enabled,
		Protocol:      config.Protocol,
		Endpoint:      config.Endpoint,
		QueueCapacity: queueCapacity,
	}}
}

func (state *deliveryState) record(count int, err error) {
	if state == nil {
		return
	}
	now := time.Now().UTC()
	state.mu.Lock()
	defer state.mu.Unlock()
	if err == nil {
		state.lastSuccessAt = now
		state.lastError = ""
		return
	}
	state.lastErrorAt = now
	state.lastError = deliveryError(err)
	if count > 0 {
		state.failedExportItems += uint64(count)
	}
}

func (state *deliveryState) snapshot() SignalStatus {
	state.mu.RLock()
	defer state.mu.RUnlock()
	result := state.config
	result.LastError = state.lastError
	result.FailedExportItems = state.failedExportItems
	result.QueueDepth = state.queueDepth
	result.DroppedItems = state.droppedItems
	if !state.lastSuccessAt.IsZero() {
		value := state.lastSuccessAt
		result.LastSuccessAt = &value
	}
	if !state.lastErrorAt.IsZero() {
		value := state.lastErrorAt
		result.LastErrorAt = &value
	}
	return result
}

func (state *deliveryState) queueCapacity() int {
	state.mu.RLock()
	defer state.mu.RUnlock()
	return state.config.QueueCapacity
}

func (state *deliveryState) queued(delta int) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.queueDepth += delta
	if state.queueDepth < 0 {
		state.queueDepth = 0
	}
}

func (state *deliveryState) dropped(count uint64) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.droppedItems += count
}

func sanitizeEndpoint(raw, protocol string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		if protocol == ProtocolHTTPProtobuf {
			return "http://localhost:4318"
		}
		return "localhost:4317"
	}
	parseTarget := raw
	if !strings.Contains(parseTarget, "://") {
		parseTarget = "otel://" + parseTarget
	}
	parsed, err := url.Parse(parseTarget)
	if err != nil {
		return "configured"
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	if parsed.Scheme == "otel" {
		return strings.TrimPrefix(parsed.String(), "otel://")
	}
	return parsed.String()
}

func boundedError(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 512 {
		value = value[:512] + "..."
	}
	return value
}

func deliveryError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "context canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "export deadline exceeded"
	default:
		return fmt.Sprintf("%s: export failed", reflect.TypeOf(err))
	}
}
