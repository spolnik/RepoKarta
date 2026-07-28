package graph

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
)

const (
	DefaultGraphQueryDepth = 2
	MaximumGraphQueryDepth = 6
	DefaultGraphQueryLimit = 200
	MaximumGraphQueryLimit = 1000
)

// QueryRequest selects a bounded traversal or shortest evidenced connection.
// Selectors accept an exact node ID, repository ID/name, repository-relative
// file path, or symbol name.
type QueryRequest struct {
	Mode      string   `json:"mode"`
	Start     string   `json:"start"`
	End       string   `json:"end,omitempty"`
	Direction string   `json:"direction,omitempty"`
	Depth     int      `json:"depth,omitempty"`
	Kinds     []string `json:"relation_kinds,omitempty"`
	Limit     int      `json:"limit,omitempty"`
}

type QueryNode struct {
	Node
	Distance int `json:"distance"`
}

type QueryPath struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

// QueryResult never hides bounding or ambiguous selector state.
type QueryResult struct {
	Mode       string      `json:"mode"`
	Start      Node        `json:"start"`
	End        *Node       `json:"end,omitempty"`
	Nodes      []QueryNode `json:"nodes"`
	Edges      []Edge      `json:"edges"`
	Path       *QueryPath  `json:"path,omitempty"`
	Truncated  bool        `json:"truncated"`
	Complete   bool        `json:"complete"`
	Warnings   []string    `json:"warnings,omitempty"`
	Candidates []Node      `json:"candidates,omitempty"`
}

// QueryGraph executes only over the supplied immutable snapshot.
func QueryGraph(snapshot Snapshot, request QueryRequest) (QueryResult, error) {
	request.Mode = strings.ToLower(strings.TrimSpace(request.Mode))
	if request.Mode == "" {
		request.Mode = "impact"
	}
	if !slices.Contains([]string{"impact", "shortest_path"}, request.Mode) {
		return QueryResult{}, errors.New("graph mode must be impact or shortest_path")
	}
	request.Direction = strings.ToLower(strings.TrimSpace(request.Direction))
	if request.Direction == "" {
		request.Direction = "both"
	}
	if !slices.Contains([]string{"upstream", "downstream", "both"}, request.Direction) {
		return QueryResult{}, errors.New("graph direction must be upstream, downstream, or both")
	}
	if request.Depth <= 0 {
		request.Depth = DefaultGraphQueryDepth
	}
	if request.Depth > MaximumGraphQueryDepth {
		return QueryResult{}, fmt.Errorf("graph depth must not exceed %d", MaximumGraphQueryDepth)
	}
	if request.Limit <= 0 {
		request.Limit = DefaultGraphQueryLimit
	}
	if request.Limit > MaximumGraphQueryLimit {
		return QueryResult{}, fmt.Errorf("graph limit must not exceed %d", MaximumGraphQueryLimit)
	}
	nodes, edges := queryGraph(snapshot)
	start, candidates := resolveGraphSelector(nodes, request.Start)
	if len(candidates) != 1 {
		return QueryResult{Mode: request.Mode, Complete: false, Candidates: candidates},
			fmt.Errorf("graph start selector resolved to %d nodes", len(candidates))
	}
	start = candidates[0]
	result := QueryResult{
		Mode:      request.Mode,
		Start:     start,
		Nodes:     []QueryNode{},
		Edges:     []Edge{},
		Complete:  !snapshot.Truncated && !snapshot.StructureTruncated && snapshot.Scope.Complete,
		Warnings:  []string{},
		Truncated: snapshot.Truncated || snapshot.StructureTruncated || !snapshot.Scope.Complete,
	}
	if result.Truncated {
		result.Warnings = append(result.Warnings, "the source graph snapshot is partial")
	}
	if request.Mode == "shortest_path" {
		end, endCandidates := resolveGraphSelector(nodes, request.End)
		if len(endCandidates) != 1 {
			result.Candidates = endCandidates
			return result, fmt.Errorf("graph end selector resolved to %d nodes", len(endCandidates))
		}
		end = endCandidates[0]
		result.End = &end
		path, found, limitReached := shortestGraphPath(nodes, edges, start.ID, end.ID, request)
		if limitReached {
			result.Truncated = true
			result.Complete = false
			result.Warnings = append(result.Warnings, "the path search reached the result limit")
		}
		if found {
			result.Path = &path
			result.Nodes = make([]QueryNode, 0, len(path.Nodes))
			for distance, node := range path.Nodes {
				result.Nodes = append(result.Nodes, QueryNode{Node: node, Distance: distance})
			}
			result.Edges = append(result.Edges, path.Edges...)
		} else {
			result.Warnings = append(
				result.Warnings,
				"no path was found within the requested depth and relation filters",
			)
		}
		return result, nil
	}
	result.Nodes, result.Edges, result.Truncated = traverseGraph(nodes, edges, start.ID, request, result.Truncated)
	result.Complete = result.Complete && !result.Truncated
	return result, nil
}

func queryGraph(snapshot Snapshot) (map[string]Node, []Edge) {
	nodes := make(map[string]Node, len(snapshot.Nodes)+len(snapshot.Structure)*2)
	for _, node := range snapshot.Nodes {
		nodes[node.ID] = node
	}
	edges := append([]Edge(nil), snapshot.Edges...)
	for _, document := range snapshot.Structure {
		repositoryID := "repository:" + strconv.FormatInt(document.RepositoryID, 10)
		fileID := "file:" + strconv.FormatInt(document.RepositoryID, 10) + ":" + document.Path
		evidence := Evidence{
			RepositoryID: document.RepositoryID,
			Repository:   document.Repository,
			Revision:     document.Revision,
			Path:         document.Path,
			Line:         1,
			Label:        document.Path,
		}
		nodes[fileID] = Node{
			ID: fileID, Kind: "file", Label: document.Path, Layer: "Files",
			RepositoryID: document.RepositoryID, Repository: document.Repository,
			Path: document.Path, Evidence: []Evidence{evidence},
		}
		if _, found := nodes[repositoryID]; found {
			edges = append(edges, Edge{
				ID: repositoryID + "->" + fileID, Source: repositoryID, Target: fileID,
				Kind: "contains", Label: "contains", Confidence: "exact", Evidence: []Evidence{evidence},
			})
		}
		for _, symbol := range document.Symbols {
			symbolID := fmt.Sprintf(
				"symbol:%d:%s:%s:%s:%d",
				document.RepositoryID, document.Path, symbol.Kind, symbol.Name, symbol.Range.StartLine,
			)
			symbolEvidence := evidence
			symbolEvidence.Line = max(1, symbol.Range.StartLine)
			symbolEvidence.Label = symbol.Name
			nodes[symbolID] = Node{
				ID: symbolID, Kind: "symbol", Label: symbol.Name, Subtitle: symbol.Kind,
				Layer: "Symbols", RepositoryID: document.RepositoryID,
				Repository: document.Repository, Path: document.Path,
				Evidence: []Evidence{symbolEvidence},
			}
			edges = append(edges, Edge{
				ID: fileID + "->" + symbolID, Source: fileID, Target: symbolID,
				Kind: "defines", Label: "defines", Confidence: symbol.Confidence,
				Evidence: []Evidence{symbolEvidence},
			})
		}
	}
	sort.Slice(edges, func(left, right int) bool { return edges[left].ID < edges[right].ID })
	return nodes, edges
}

func resolveGraphSelector(nodes map[string]Node, selector string) (Node, []Node) {
	selector = strings.TrimSpace(selector)
	if node, found := nodes[selector]; found {
		return node, []Node{node}
	}
	lower := strings.ToLower(selector)
	var candidates []Node
	for _, node := range nodes {
		if strings.EqualFold(node.Label, selector) ||
			strings.EqualFold(node.Path, selector) ||
			(node.Kind == "repository" && strings.TrimPrefix(node.ID, "repository:") == selector) ||
			strings.Contains(strings.ToLower(node.Label), lower) {
			candidates = append(candidates, node)
		}
	}
	sort.Slice(candidates, func(left, right int) bool { return candidates[left].ID < candidates[right].ID })
	if len(candidates) == 1 {
		return candidates[0], candidates
	}
	return Node{}, candidates
}

type graphStep struct {
	node string
	edge Edge
}

func graphNeighbors(edges []Edge, node string, request QueryRequest) []graphStep {
	kinds := make(map[string]struct{}, len(request.Kinds))
	for _, kind := range request.Kinds {
		kinds[strings.ToLower(strings.TrimSpace(kind))] = struct{}{}
	}
	allowedKind := func(kind string) bool {
		if len(kinds) == 0 {
			return true
		}
		_, ok := kinds[strings.ToLower(kind)]
		return ok
	}
	var output []graphStep
	for _, edge := range edges {
		if !allowedKind(edge.Kind) {
			continue
		}
		if request.Direction != "upstream" && edge.Source == node {
			output = append(output, graphStep{node: edge.Target, edge: edge})
		}
		if request.Direction != "downstream" && edge.Target == node {
			output = append(output, graphStep{node: edge.Source, edge: edge})
		}
	}
	sort.Slice(output, func(left, right int) bool {
		if output[left].node != output[right].node {
			return output[left].node < output[right].node
		}
		return output[left].edge.ID < output[right].edge.ID
	})
	return output
}

func traverseGraph(
	nodes map[string]Node,
	edges []Edge,
	start string,
	request QueryRequest,
	alreadyTruncated bool,
) ([]QueryNode, []Edge, bool) {
	distance := map[string]int{start: 0}
	queue := []string{start}
	usedEdges := make(map[string]Edge)
	truncated := alreadyTruncated
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if distance[current] >= request.Depth {
			continue
		}
		for _, next := range graphNeighbors(edges, current, request) {
			if _, seen := distance[next.node]; seen {
				usedEdges[next.edge.ID] = next.edge
				continue
			}
			if len(distance) >= request.Limit {
				truncated = true
				continue
			}
			distance[next.node] = distance[current] + 1
			usedEdges[next.edge.ID] = next.edge
			queue = append(queue, next.node)
		}
	}
	outputNodes := make([]QueryNode, 0, len(distance))
	for id, depth := range distance {
		if node, found := nodes[id]; found {
			outputNodes = append(outputNodes, QueryNode{Node: node, Distance: depth})
		}
	}
	sort.Slice(outputNodes, func(left, right int) bool {
		if outputNodes[left].Distance != outputNodes[right].Distance {
			return outputNodes[left].Distance < outputNodes[right].Distance
		}
		return outputNodes[left].ID < outputNodes[right].ID
	})
	outputEdges := make([]Edge, 0, len(usedEdges))
	for _, edge := range usedEdges {
		outputEdges = append(outputEdges, edge)
	}
	sort.Slice(outputEdges, func(left, right int) bool { return outputEdges[left].ID < outputEdges[right].ID })
	return outputNodes, outputEdges, truncated
}

func shortestGraphPath(
	nodes map[string]Node,
	edges []Edge,
	start string,
	end string,
	request QueryRequest,
) (QueryPath, bool, bool) {
	queue := []string{start}
	distance := map[string]int{start: 0}
	previous := make(map[string]graphStep)
	limitReached := false
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if current == end {
			break
		}
		if distance[current] >= request.Depth {
			continue
		}
		for _, next := range graphNeighbors(edges, current, request) {
			if _, seen := distance[next.node]; seen {
				continue
			}
			if len(distance) >= request.Limit {
				limitReached = true
				continue
			}
			distance[next.node] = distance[current] + 1
			previous[next.node] = graphStep{node: current, edge: next.edge}
			queue = append(queue, next.node)
		}
	}
	if _, found := distance[end]; !found {
		return QueryPath{}, false, limitReached
	}
	nodeIDs := []string{end}
	var pathEdges []Edge
	for current := end; current != start; {
		step := previous[current]
		pathEdges = append(pathEdges, step.edge)
		current = step.node
		nodeIDs = append(nodeIDs, current)
	}
	slices.Reverse(nodeIDs)
	slices.Reverse(pathEdges)
	pathNodes := make([]Node, 0, len(nodeIDs))
	for _, id := range nodeIDs {
		pathNodes = append(pathNodes, nodes[id])
	}
	return QueryPath{Nodes: pathNodes, Edges: pathEdges}, true, limitReached
}
