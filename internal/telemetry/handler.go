package telemetry

import (
	"context"
	"log/slog"
	"strings"

	"github.com/spolnik/RepoKarta/internal/audit"
	"go.opentelemetry.io/otel/trace"
)

type fanoutHandler struct {
	handlers []slog.Handler
}

func (handler fanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, child := range handler.handlers {
		if child.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (handler fanoutHandler) Handle(ctx context.Context, record slog.Record) error {
	var firstError error
	for _, child := range handler.handlers {
		if !child.Enabled(ctx, record.Level) {
			continue
		}
		if err := child.Handle(ctx, record.Clone()); err != nil && firstError == nil {
			firstError = err
		}
	}
	return firstError
}

func (handler fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	children := make([]slog.Handler, 0, len(handler.handlers))
	for _, child := range handler.handlers {
		children = append(children, child.WithAttrs(attrs))
	}
	return fanoutHandler{handlers: children}
}

func (handler fanoutHandler) WithGroup(name string) slog.Handler {
	children := make([]slog.Handler, 0, len(handler.handlers))
	for _, child := range handler.handlers {
		children = append(children, child.WithGroup(name))
	}
	return fanoutHandler{handlers: children}
}

type contextHandler struct {
	next slog.Handler
}

func (handler contextHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return handler.next.Enabled(ctx, level)
}

func (handler contextHandler) Handle(ctx context.Context, record slog.Record) error {
	if requestID := audit.CorrelationID(ctx); requestID != "" {
		record.AddAttrs(slog.String("request.id", requestID))
	}
	spanContext := trace.SpanContextFromContext(ctx)
	if spanContext.IsValid() {
		record.AddAttrs(
			slog.String("trace_id", spanContext.TraceID().String()),
			slog.String("span_id", spanContext.SpanID().String()),
		)
	}
	return handler.next.Handle(ctx, record)
}

func (handler contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{next: handler.next.WithAttrs(attrs)}
}

func (handler contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{next: handler.next.WithGroup(name)}
}

type redactingHandler struct {
	next slog.Handler
}

func (handler redactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return handler.next.Enabled(ctx, level)
}

func (handler redactingHandler) Handle(ctx context.Context, record slog.Record) error {
	safe := slog.NewRecord(record.Time, record.Level, record.Message, record.PC)
	record.Attrs(func(attr slog.Attr) bool {
		safe.AddAttrs(redactAttr(attr))
		return true
	})
	return handler.next.Handle(ctx, safe)
}

func (handler redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	safe := make([]slog.Attr, 0, len(attrs))
	for _, attr := range attrs {
		safe = append(safe, redactAttr(attr))
	}
	return redactingHandler{next: handler.next.WithAttrs(safe)}
}

func (handler redactingHandler) WithGroup(name string) slog.Handler {
	return redactingHandler{next: handler.next.WithGroup(name)}
}

func redactAttr(attr slog.Attr) slog.Attr {
	attr.Value = attr.Value.Resolve()
	if sensitiveTelemetryKey(attr.Key) {
		return slog.String(attr.Key, "[redacted]")
	}
	if attr.Value.Kind() == slog.KindGroup {
		group := attr.Value.Group()
		safe := make([]slog.Attr, 0, len(group))
		for _, child := range group {
			safe = append(safe, redactAttr(child))
		}
		return slog.Group(attr.Key, attrsToAny(safe)...)
	}
	return attr
}

func attrsToAny(attrs []slog.Attr) []any {
	values := make([]any, 0, len(attrs))
	for _, attr := range attrs {
		values = append(values, attr)
	}
	return values
}

func sensitiveTelemetryKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	for _, marker := range []string{
		"authorization", "cookie", "credential", "database_url", "directory",
		"email", "error", "header", "password", "path", "prompt", "query",
		"repository", "root", "secret", "source", "token", "url", "user",
	} {
		if strings.Contains(key, marker) {
			return true
		}
	}
	return false
}
