package graph

import (
	"strings"
	"testing"
)

func TestQueryGraphTraversesImpactAndFindsShortestPath(t *testing.T) {
	snapshot := Snapshot{
		Scope: Scope{Complete: true},
		Nodes: []Node{
			{ID: "repository:1", Kind: "repository", Label: "api"},
			{ID: "repository:2", Kind: "repository", Label: "billing"},
			{ID: "repository:3", Kind: "repository", Label: "ledger"},
		},
		Edges: []Edge{
			{ID: "api-billing", Source: "repository:1", Target: "repository:2", Kind: "calls"},
			{ID: "billing-ledger", Source: "repository:2", Target: "repository:3", Kind: "writes"},
		},
	}
	impact, err := QueryGraph(snapshot, QueryRequest{
		Mode: "impact", Start: "api", Direction: "downstream", Depth: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(impact.Nodes) != 3 || len(impact.Edges) != 2 || !impact.Complete {
		t.Fatalf("impact = %#v", impact)
	}
	path, err := QueryGraph(snapshot, QueryRequest{
		Mode: "shortest_path", Start: "api", End: "ledger",
		Direction: "downstream", Depth: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if path.Path == nil || len(path.Path.Nodes) != 3 || len(path.Path.Edges) != 2 {
		t.Fatalf("path = %#v", path)
	}
}

func TestQueryGraphReportsAmbiguousSelectorsAndBounds(t *testing.T) {
	snapshot := Snapshot{
		Scope: Scope{Complete: false},
		Nodes: []Node{
			{ID: "symbol:1", Kind: "symbol", Label: "Start"},
			{ID: "symbol:2", Kind: "symbol", Label: "Start"},
		},
	}
	result, err := QueryGraph(snapshot, QueryRequest{Start: "Start"})
	if err == nil || len(result.Candidates) != 2 || result.Complete {
		t.Fatalf("ambiguous result = %#v, %v", result, err)
	}
	if _, err := QueryGraph(snapshot, QueryRequest{
		Start: "symbol:1", Depth: MaximumGraphQueryDepth + 1,
	}); err == nil {
		t.Fatal("unbounded depth accepted")
	}
}

func TestQueryGraphLimitsNeverReturnDanglingEdges(t *testing.T) {
	snapshot := Snapshot{
		Scope: Scope{Complete: true},
		Nodes: []Node{
			{ID: "repository:1", Kind: "repository", Label: "api"},
			{ID: "repository:2", Kind: "repository", Label: "billing"},
			{ID: "repository:3", Kind: "repository", Label: "ledger"},
		},
		Edges: []Edge{
			{ID: "api-billing", Source: "repository:1", Target: "repository:2", Kind: "calls"},
			{ID: "api-ledger", Source: "repository:1", Target: "repository:3", Kind: "calls"},
		},
	}
	result, err := QueryGraph(snapshot, QueryRequest{
		Mode: "impact", Start: "api", Direction: "downstream", Depth: 2, Limit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	returned := make(map[string]struct{}, len(result.Nodes))
	for _, node := range result.Nodes {
		returned[node.ID] = struct{}{}
	}
	for _, edge := range result.Edges {
		if _, found := returned[edge.Source]; !found {
			t.Fatalf("edge source %q is not in the bounded node result", edge.Source)
		}
		if _, found := returned[edge.Target]; !found {
			t.Fatalf("edge target %q is not in the bounded node result", edge.Target)
		}
	}
	if !result.Truncated || result.Complete {
		t.Fatalf("bounded result = %#v", result)
	}
}

func TestShortestPathReportsDepthAndResultBounds(t *testing.T) {
	snapshot := Snapshot{
		Scope: Scope{Complete: true},
		Nodes: []Node{
			{ID: "repository:1", Kind: "repository", Label: "api"},
			{ID: "repository:2", Kind: "repository", Label: "billing"},
			{ID: "repository:3", Kind: "repository", Label: "ledger"},
		},
		Edges: []Edge{
			{ID: "api-billing", Source: "repository:1", Target: "repository:2", Kind: "calls"},
			{ID: "billing-ledger", Source: "repository:2", Target: "repository:3", Kind: "writes"},
		},
	}
	depthBound, err := QueryGraph(snapshot, QueryRequest{
		Mode: "shortest_path", Start: "api", End: "ledger",
		Direction: "downstream", Depth: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if depthBound.Path != nil ||
		!warningsContain(depthBound.Warnings, "requested depth") {
		t.Fatalf("depth-bounded path = %#v", depthBound)
	}
	limitBound, err := QueryGraph(snapshot, QueryRequest{
		Mode: "shortest_path", Start: "api", End: "ledger",
		Direction: "downstream", Depth: 3, Limit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if limitBound.Path != nil || !limitBound.Truncated || limitBound.Complete ||
		!warningsContain(limitBound.Warnings, "result limit") {
		t.Fatalf("limit-bounded path = %#v", limitBound)
	}
}

func warningsContain(warnings []string, fragment string) bool {
	for _, warning := range warnings {
		if strings.Contains(warning, fragment) {
			return true
		}
	}
	return false
}
