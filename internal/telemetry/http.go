package telemetry

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

type originalRequestTargetKey struct{}

type originalRequestTarget struct {
	path     string
	rawPath  string
	rawQuery string
}

type routeStateKey struct{}

type routeState struct {
	pattern string
}

// RouteHandler records the ServeMux pattern through intervening request
// context copies. Wrap the mux before authentication middleware.
func (system *System) RouteHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		next.ServeHTTP(response, request)
		if state, _ := request.Context().Value(routeStateKey{}).(*routeState); state != nil {
			state.pattern = normalizedRoute(request.Pattern)
		}
	})
}

// RoutePattern returns the bounded ServeMux pattern captured for a request.
func RoutePattern(ctx context.Context) string {
	state, _ := ctx.Value(routeStateKey{}).(*routeState)
	if state == nil {
		return ""
	}
	return state.pattern
}

// HTTPHandler adds current OpenTelemetry HTTP server conventions while hiding
// raw paths and queries from telemetry. The ServeMux route template is copied
// back after routing so bounded http.route dimensions remain available.
func (system *System) HTTPHandler(next http.Handler) http.Handler {
	if system == nil || !system.config.Enabled {
		return next
	}
	restoreTarget := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		target, _ := request.Context().Value(originalRequestTargetKey{}).(originalRequestTarget)
		restored := request.Clone(request.Context())
		restoredURL := cloneURL(request.URL)
		restoredURL.Path = target.path
		restoredURL.RawPath = target.rawPath
		restoredURL.RawQuery = target.rawQuery
		restored.URL = restoredURL
		next.ServeHTTP(response, restored)
		if state, _ := request.Context().Value(routeStateKey{}).(*routeState); state != nil {
			request.Pattern = state.pattern
			if state.pattern != "" {
				trace.SpanFromContext(request.Context()).SetAttributes(
					attribute.String("http.route", state.pattern),
				)
			}
		}
	})
	options := []otelhttp.Option{
		otelhttp.WithSpanNameFormatter(func(_ string, request *http.Request) string {
			if request.Pattern != "" {
				return request.Method + " " + request.Pattern
			}
			return request.Method
		}),
	}
	if system.traceProvider != nil {
		options = append(options, otelhttp.WithTracerProvider(system.traceProvider))
	}
	if system.meterProvider != nil {
		options = append(options, otelhttp.WithMeterProvider(system.meterProvider))
	}
	instrumented := otelhttp.NewHandler(restoreTarget, "HTTP", options...)
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		target := originalRequestTarget{
			path:     request.URL.Path,
			rawPath:  request.URL.RawPath,
			rawQuery: request.URL.RawQuery,
		}
		ctx := context.WithValue(request.Context(), originalRequestTargetKey{}, target)
		ctx = context.WithValue(ctx, routeStateKey{}, new(routeState))
		safe := request.Clone(ctx)
		safeURL := cloneURL(request.URL)
		safeURL.Path = "/"
		safeURL.RawPath = ""
		safeURL.RawQuery = ""
		safe.URL = safeURL
		instrumented.ServeHTTP(response, safe)
	})
}

// HTTPClient returns a timeout-bounded client with redacted OTLP client spans.
func (system *System) HTTPClient(timeout time.Duration, base http.RoundTripper) *http.Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if base == nil {
		base = http.DefaultTransport
	}
	if system == nil || !system.config.Enabled {
		return &http.Client{Timeout: timeout, Transport: base}
	}
	return &http.Client{Timeout: timeout, Transport: system.HTTPTransport(base)}
}

// HTTPTransport instruments a caller-provided transport without exposing raw
// package, repository, or query targets in client spans.
func (system *System) HTTPTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	if system == nil || !system.config.Enabled {
		return base
	}
	restoring := restoringTransport{base: base}
	options := []otelhttp.Option{}
	if system.traceProvider != nil {
		options = append(options, otelhttp.WithTracerProvider(system.traceProvider))
	}
	if system.meterProvider != nil {
		options = append(options, otelhttp.WithMeterProvider(system.meterProvider))
	}
	return privacyTransport{
		instrumented: otelhttp.NewTransport(restoring, options...),
	}
}

type originalURLKey struct{}

type privacyTransport struct {
	instrumented http.RoundTripper
}

func (transport privacyTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	original := cloneURL(request.URL)
	ctx := context.WithValue(request.Context(), originalURLKey{}, original)
	safe := request.Clone(ctx)
	safeURL := cloneURL(request.URL)
	safeURL.Path = "/"
	safeURL.RawPath = ""
	safeURL.RawQuery = ""
	safe.URL = safeURL
	return transport.instrumented.RoundTrip(safe)
}

type restoringTransport struct {
	base http.RoundTripper
}

func (transport restoringTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	original, _ := request.Context().Value(originalURLKey{}).(*url.URL)
	restored := request.Clone(request.Context())
	restored.URL = cloneURL(original)
	return transport.base.RoundTrip(restored)
}

func cloneURL(value *url.URL) *url.URL {
	if value == nil {
		return new(url.URL)
	}
	cloned := *value
	return &cloned
}

func normalizedRoute(pattern string) string {
	if index := strings.IndexByte(pattern, '/'); index >= 0 {
		return pattern[index:]
	}
	return ""
}
