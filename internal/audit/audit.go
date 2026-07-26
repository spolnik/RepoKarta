// Package audit defines RepoKarta's redacted, append-only security evidence.
package audit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"
)

const (
	DefaultLimit = 100
	MaximumLimit = 500
)

// Event is one immutable security-relevant observation.
type Event struct {
	ID            int64             `json:"id"`
	ActorID       string            `json:"actor_id"`
	ActorName     string            `json:"actor_name,omitempty"`
	Action        string            `json:"action"`
	TargetType    string            `json:"target_type"`
	TargetID      string            `json:"target_id,omitempty"`
	Outcome       string            `json:"outcome"`
	Provider      string            `json:"authentication_provider"`
	CorrelationID string            `json:"correlation_id"`
	Metadata      map[string]string `json:"metadata,omitempty"`
	CreatedAt     time.Time         `json:"timestamp"`
}

// Filter bounds an audit search. BeforeID supports stable descending pagination.
type Filter struct {
	Query    string
	ActorID  string
	Action   string
	Outcome  string
	Since    time.Time
	Until    time.Time
	BeforeID int64
	Limit    int
}

// Retention describes both configured limits and the retained evidence window.
type Retention struct {
	Days          int       `json:"days"`
	MaxEvents     int       `json:"max_events"`
	OldestEventAt time.Time `json:"oldest_event_at,omitempty"`
	NewestEventAt time.Time `json:"newest_event_at,omitempty"`
	EventCount    int64     `json:"event_count"`
	CompleteSince time.Time `json:"complete_since,omitempty"`
}

// Page is a bounded audit result with explicit retention and pagination state.
type Page struct {
	Events     []Event   `json:"events"`
	NextBefore int64     `json:"next_before,omitempty"`
	Truncated  bool      `json:"truncated"`
	Retention  Retention `json:"retention"`
}

// Recorder accepts already-redacted events.
type Recorder interface {
	AppendAuditEvent(context.Context, Event) error
}

type correlationContextKey struct{}

// WithCorrelationID attaches a request correlation ID.
func WithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationContextKey{}, strings.TrimSpace(id))
}

// CorrelationID returns the request correlation ID, if present.
func CorrelationID(ctx context.Context) string {
	value, _ := ctx.Value(correlationContextKey{}).(string)
	return value
}

// NewCorrelationID creates a non-secret, URL-safe request identifier.
func NewCorrelationID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "request-" + time.Now().UTC().Format("20060102T150405.000000000")
	}
	return hex.EncodeToString(value[:])
}

// RedactMetadata drops secret-bearing fields and bounds retained values.
func RedactMetadata(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	output := make(map[string]string, len(input))
	for key, value := range input {
		key = strings.TrimSpace(key)
		lower := strings.ToLower(key)
		if key == "" || strings.Contains(lower, "token") ||
			strings.Contains(lower, "secret") || strings.Contains(lower, "password") ||
			strings.Contains(lower, "credential") || strings.Contains(lower, "cookie") ||
			strings.Contains(lower, "prompt") || strings.Contains(lower, "source") ||
			strings.Contains(lower, "content") || strings.Contains(lower, "assertion") {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) > 512 {
			value = value[:512]
		}
		output[key] = value
	}
	if len(output) == 0 {
		return nil
	}
	return output
}

// Normalize makes an event safe and complete before persistence.
func Normalize(event Event) Event {
	event.ActorID = bounded(event.ActorID, 256)
	event.ActorName = bounded(event.ActorName, 256)
	event.Action = bounded(event.Action, 128)
	event.TargetType = bounded(event.TargetType, 64)
	event.TargetID = bounded(event.TargetID, 512)
	event.Outcome = bounded(event.Outcome, 32)
	event.Provider = bounded(event.Provider, 64)
	event.CorrelationID = bounded(event.CorrelationID, 128)
	event.Metadata = RedactMetadata(event.Metadata)
	if event.ActorID == "" {
		event.ActorID = "unknown"
	}
	if event.Action == "" {
		event.Action = "unknown"
	}
	if event.TargetType == "" {
		event.TargetType = "application"
	}
	if event.Outcome == "" {
		event.Outcome = "success"
	}
	if event.Provider == "" {
		event.Provider = "unknown"
	}
	if event.CorrelationID == "" {
		event.CorrelationID = NewCorrelationID()
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	} else {
		event.CreatedAt = event.CreatedAt.UTC()
	}
	return event
}

func bounded(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) > limit {
		return value[:limit]
	}
	return value
}
