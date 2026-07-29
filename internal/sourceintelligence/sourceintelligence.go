// Package sourceintelligence joins route and topology artifacts for source views.
package sourceintelligence

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"

	"github.com/spolnik/RepoKarta/internal/dependencies"
	"github.com/spolnik/RepoKarta/internal/graph"
)

const maximumRoutes = 24

// RouteStore supplies commit-pinned route and topology artifacts.
type RouteStore interface {
	ReadRouteSnapshot(context.Context, int64) (graph.Snapshot, graph.ArtifactProgress, error)
	ReadTopologySnapshot(context.Context, int64) (graph.Snapshot, graph.ArtifactProgress, error)
}

// TopologyService resolves system topology from graph artifacts.
type TopologyService interface {
	Topology(context.Context, graph.Snapshot, graph.ArtifactProgress, dependencies.TopologyOptions) (dependencies.Topology, error)
}

// Request identifies the source window whose intelligence should be assembled.
type Request struct {
	RepositoryID int64
	Revision     string
	FilePath     string
	StartLine    int
	EndLine      int
}

// View is the source-facing route and caller projection.
type View struct {
	Routes        []Route
	RouteCount    int
	OmittedRoutes int
	Callers       []Caller
	State         string
	Message       string
	Partial       bool
	TopologyURL   string
}

// Route describes an endpoint declared by the selected source file.
type Route struct {
	Label         string
	Line          int
	URL           string
	VisibleWindow bool
	Callers       []Caller
}

// Caller describes service-level inbound evidence.
type Caller struct {
	Name       string
	State      string
	Confidence string
	Evidence   graph.Evidence
}

// Build joins commit-pinned route evidence with inbound system topology.
func Build(
	ctx context.Context,
	routes RouteStore,
	topologies TopologyService,
	request Request,
) View {
	view := View{
		Routes:  []Route{},
		Callers: []Caller{},
		State:   "ready",
		TopologyURL: fmt.Sprintf(
			"/dependencies?repository=%d&protocol=http&direction=inbound&depth=1",
			request.RepositoryID,
		),
	}
	if routes == nil {
		view.State = "unavailable"
		view.Message = "Route and caller artifacts are unavailable in this runtime."
		return view
	}

	routeSnapshot, routeProgress, err := routes.ReadRouteSnapshot(ctx, request.RepositoryID)
	if err != nil {
		view.State = "unavailable"
		view.Message = "Route artifacts could not be read."
		return view
	}
	for _, node := range routeSnapshot.Nodes {
		if node.Kind != "route" {
			continue
		}
		evidence, ok := routeEvidenceForFile(node, request.RepositoryID, request.FilePath)
		if !ok {
			continue
		}
		if evidence.URL == "" {
			evidenceRevision := evidence.Revision
			if evidenceRevision == "" {
				evidenceRevision = request.Revision
			}
			evidence.URL = sourceEvidenceURL(
				request.RepositoryID,
				evidenceRevision,
				request.FilePath,
				evidence.Line,
			)
		}
		view.Routes = append(view.Routes, Route{
			Label:         node.Label,
			Line:          max(1, evidence.Line),
			URL:           evidence.URL,
			VisibleWindow: evidence.Line >= request.StartLine && evidence.Line <= request.EndLine,
		})
	}
	sort.Slice(view.Routes, func(left, right int) bool {
		if view.Routes[left].VisibleWindow != view.Routes[right].VisibleWindow {
			return view.Routes[left].VisibleWindow
		}
		if view.Routes[left].Line != view.Routes[right].Line {
			return view.Routes[left].Line < view.Routes[right].Line
		}
		return view.Routes[left].Label < view.Routes[right].Label
	})
	view.RouteCount = len(view.Routes)
	if len(view.Routes) > maximumRoutes {
		view.OmittedRoutes = len(view.Routes) - maximumRoutes
		view.Routes = view.Routes[:maximumRoutes]
	}
	if routeProgress.State == "building" || !routeSnapshot.Scope.Complete {
		view.Partial = true
		view.State = "building"
	}
	if len(view.Routes) == 0 {
		if view.Partial {
			view.Message = "Route artifacts are still building; endpoints in this file may not be available yet."
		} else {
			view.Message = "No supported HTTP route declaration was detected in this file."
		}
		return view
	}
	if topologies == nil {
		view.State = "unavailable"
		view.Message = "Routes were detected, but caller topology is unavailable in this runtime."
		return view
	}

	topologySnapshot, topologyProgress, err := routes.ReadTopologySnapshot(ctx, request.RepositoryID)
	if err != nil {
		view.State = "unavailable"
		view.Message = "Routes were detected, but caller topology could not be read."
		return view
	}
	topology, err := topologies.Topology(
		ctx,
		topologySnapshot,
		topologyProgress,
		dependencies.TopologyOptions{Protocol: "http", Direction: "both", Depth: 1},
	)
	if err != nil {
		view.State = "unavailable"
		view.Message = "Routes were detected, but inbound callers could not be resolved."
		return view
	}
	routeComponentIDs := sourceComponentIDs(topology.Components, request.RepositoryID, request.FilePath)
	seenCallers := make(map[string]bool)
	routeCallers := make([]map[string]bool, len(view.Routes))
	for routeIndex := range routeCallers {
		routeCallers[routeIndex] = make(map[string]bool)
	}
	for _, connection := range topology.Connections {
		if !strings.EqualFold(connection.Protocol, "http") ||
			!routeComponentIDs[connection.Target] ||
			connection.Source == connection.Target {
			continue
		}
		caller := Caller{
			Name:       connection.SourceName,
			State:      connection.State,
			Confidence: connection.Confidence,
		}
		if len(connection.Evidence) > 0 {
			caller.Evidence = connection.Evidence[0]
		}
		key := strings.ToLower(caller.Name) + "\x00" + caller.State + "\x00" + caller.Evidence.URL
		if !seenCallers[key] {
			seenCallers[key] = true
			view.Callers = append(view.Callers, caller)
		}
		for routeIndex := range view.Routes {
			if routeMatchesCallerEvidence(view.Routes[routeIndex].Label, connection.Evidence) &&
				!routeCallers[routeIndex][key] {
				routeCallers[routeIndex][key] = true
				view.Routes[routeIndex].Callers = append(
					view.Routes[routeIndex].Callers,
					caller,
				)
			}
		}
	}
	sort.Slice(view.Callers, func(left, right int) bool {
		return strings.ToLower(view.Callers[left].Name) < strings.ToLower(view.Callers[right].Name)
	})
	for routeIndex := range view.Routes {
		sort.Slice(view.Routes[routeIndex].Callers, func(left, right int) bool {
			return strings.ToLower(view.Routes[routeIndex].Callers[left].Name) <
				strings.ToLower(view.Routes[routeIndex].Callers[right].Name)
		})
	}
	if topology.Partial || topologyProgress.State == "building" {
		view.Partial = true
		view.State = "building"
	}
	if len(view.Callers) == 0 {
		view.Message = "No inbound HTTP caller evidence is currently indexed for this service."
	} else {
		view.Message = "Callers are attributed at service level; route badges require a matching commit-pinned URL path."
	}
	return view
}

func sourceComponentIDs(
	components []dependencies.TopologyComponent,
	repositoryID int64,
	filePath string,
) map[string]bool {
	filePath = strings.Trim(strings.ReplaceAll(filePath, "\\", "/"), "/")
	selected := make(map[string]bool)
	longestRoot := -1
	for _, component := range components {
		if component.RepositoryID != repositoryID {
			continue
		}
		root := strings.Trim(strings.ReplaceAll(component.Path, "\\", "/"), "/")
		if root == "" || root == "." {
			root = ""
		}
		if root != "" && filePath != root && !strings.HasPrefix(filePath, root+"/") {
			continue
		}
		if len(root) < longestRoot {
			continue
		}
		if len(root) > longestRoot {
			clear(selected)
			longestRoot = len(root)
		}
		selected[component.ID] = true
	}
	return selected
}

func routeEvidenceForFile(
	node graph.Node,
	repositoryID int64,
	filePath string,
) (graph.Evidence, bool) {
	for _, evidence := range node.Evidence {
		if evidence.RepositoryID == repositoryID && evidence.Path == filePath {
			return evidence, true
		}
	}
	if node.RepositoryID == repositoryID && node.Path == filePath {
		if len(node.Evidence) > 0 {
			return node.Evidence[0], true
		}
		return graph.Evidence{
			RepositoryID: repositoryID,
			Path:         filePath,
			Line:         1,
			Label:        node.Label,
		}, true
	}
	return graph.Evidence{}, false
}

func sourceEvidenceURL(repositoryID int64, revision, filePath string, line int) string {
	line = max(1, line)
	values := url.Values{
		"rev":   []string{revision},
		"path":  []string{filePath},
		"focus": []string{fmt.Sprintf("%d-%d", line, line)},
	}
	return fmt.Sprintf("/source/%d?%s#L%d", repositoryID, values.Encode(), line)
}

// RouteMatchesCallerEvidence reports whether caller evidence addresses a route.
func RouteMatchesCallerEvidence(routeLabel string, evidence []graph.Evidence) bool {
	return routeMatchesCallerEvidence(routeLabel, evidence)
}

func routeMatchesCallerEvidence(routeLabel string, evidence []graph.Evidence) bool {
	routePath := endpointPath(routeLabel)
	if routePath == "" {
		return false
	}
	for _, item := range evidence {
		if routePathMatches(routePath, endpointPath(item.Label)) {
			return true
		}
	}
	return false
}

func endpointPath(value string) string {
	value = strings.TrimSpace(value)
	if fields := strings.Fields(value); len(fields) > 1 &&
		strings.HasPrefix(fields[len(fields)-1], "/") {
		value = fields[len(fields)-1]
	}
	if parsed, err := url.Parse(value); err == nil && parsed.Path != "" {
		value = parsed.Path
	}
	if !strings.HasPrefix(value, "/") {
		return ""
	}
	value = strings.TrimSuffix(path.Clean(value), "/")
	if value == "" {
		return "/"
	}
	return value
}

func routePathMatches(routePath, evidencePath string) bool {
	if routePath == "" || evidencePath == "" {
		return false
	}
	routeParts := strings.Split(strings.Trim(routePath, "/"), "/")
	evidenceParts := strings.Split(strings.Trim(evidencePath, "/"), "/")
	if len(routeParts) != len(evidenceParts) {
		return false
	}
	for index, routePart := range routeParts {
		if (strings.HasPrefix(routePart, "{") && strings.HasSuffix(routePart, "}")) ||
			strings.HasPrefix(routePart, ":") ||
			routePart == "*" {
			continue
		}
		if routePart != evidenceParts[index] {
			return false
		}
	}
	return true
}
