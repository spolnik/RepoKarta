// Package access carries the authenticated repository viewer through shared
// code-intelligence, map, Wiki, export, and MCP paths.
package access

import (
	"context"
	"strings"
)

const (
	VisibilityPrivate = "private"
	VisibilityShared  = "shared"
)

// Viewer is the stable application identity used by repository policy.
type Viewer struct {
	ID     string
	Groups []string
	Admin  bool
}

type viewerContextKey struct{}

// WithViewer attaches an authenticated viewer to an externally initiated
// operation. Trusted background work intentionally has no viewer.
func WithViewer(ctx context.Context, viewer Viewer) context.Context {
	viewer.ID = strings.TrimSpace(viewer.ID)
	viewer.Groups = normalizeValues(viewer.Groups)
	return context.WithValue(ctx, viewerContextKey{}, viewer)
}

// ViewerFromContext returns the external viewer, when the operation came from
// an authenticated HTTP or MCP request.
func ViewerFromContext(ctx context.Context) (Viewer, bool) {
	viewer, ok := ctx.Value(viewerContextKey{}).(Viewer)
	return viewer, ok
}

// IdentityID matches the durable conversation-author identity format.
func IdentityID(provider, identity string) string {
	provider = strings.TrimSpace(provider)
	identity = strings.TrimSpace(identity)
	if provider == "local" && identity == "admin" {
		return "local:admin"
	}
	if provider == "" {
		provider = "authenticated"
	}
	if identity == "" {
		identity = "anonymous"
	}
	return provider + ":" + identity
}

func normalizeValues(values []string) []string {
	output := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		output = append(output, value)
	}
	return output
}
