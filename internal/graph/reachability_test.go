package graph

import (
	"slices"
	"testing"
	"time"

	"github.com/spolnik/RepoKarta/internal/analysis"
)

func TestBuildReachabilityUsesFrameworkRootsAndCompletenessBeforeClassification(t *testing.T) {
	index := StructuralIndex{
		Version:     snapshotVersion,
		ID:          "commit-keyed",
		GeneratedAt: time.Unix(100, 0).UTC(),
		Scope: Scope{
			Kind:                 "repository",
			Complete:             true,
			TotalRepositories:    1,
			AnalyzedRepositories: 1,
		},
		Structure: []StructuralDocument{{
			RepositoryID:  7,
			Repository:    "payments",
			Revision:      "abc123",
			Path:          "src/Payments.java",
			Language:      "java",
			ParseComplete: true,
			Symbols: []analysis.Symbol{
				testReachabilitySymbol("Payments", "class", 0, 500, 1, []string{"Service"}, []string{"public"}),
				testReachabilitySymbol("handle", "method", 100, 180, 5, []string{"GetMapping"}, []string{"public"}),
				testReachabilitySymbol("helper", "method", 200, 260, 9, nil, []string{"private"}),
				testReachabilitySymbol("unused", "method", 280, 340, 13, nil, []string{"private"}),
				testReachabilitySymbol("extensionPoint", "method", 360, 420, 17, nil, []string{"public"}),
			},
			Relations: []analysis.Relation{{
				Kind:   "call",
				Target: "helper",
				Range:  analysis.Range{StartByte: 120, EndByte: 130, StartLine: 6},
			}},
		}},
	}

	report := buildReachability(index, "http://127.0.0.1:7070")
	if !report.Completeness.StaticAnalysisComplete {
		t.Fatalf("completeness = %#v, want complete static input", report.Completeness)
	}
	if report.Completeness.RuntimeComplete {
		t.Fatal("runtime completeness must remain false for static reachability")
	}
	for name, state := range map[string]string{
		"Payments":       ReachabilityStateReachable,
		"handle":         ReachabilityStateReachable,
		"helper":         ReachabilityStateReachable,
		"unused":         ReachabilityStateProbablyUnreachable,
		"extensionPoint": ReachabilityStateUnknown,
	} {
		symbol, found := reachabilitySymbolByName(report.Symbols, name)
		if !found || symbol.State != state {
			t.Fatalf("%s = %#v, want state %q", name, symbol, state)
		}
		if symbol.Evidence.Revision != "abc123" || symbol.Evidence.URL == "" {
			t.Fatalf("%s evidence = %#v, want pinned source URL", name, symbol.Evidence)
		}
	}
	helper, _ := reachabilitySymbolByName(report.Symbols, "helper")
	if len(helper.Witness) != 2 {
		t.Fatalf("helper witness = %#v, want root-to-helper path", helper.Witness)
	}
}

func TestBuildReachabilityLeavesPrivateSymbolsUnknownWhenInputIsIncomplete(t *testing.T) {
	index := StructuralIndex{
		Version: snapshotVersion,
		ID:      "partial",
		Scope: Scope{
			Kind:                 "repository",
			Complete:             true,
			TotalRepositories:    1,
			AnalyzedRepositories: 1,
		},
		Structure: []StructuralDocument{{
			RepositoryID:  7,
			Repository:    "payments",
			Revision:      "abc123",
			Path:          "src/Payments.java",
			Language:      "java",
			ParseComplete: false,
			Symbols: []analysis.Symbol{
				testReachabilitySymbol("unused", "method", 0, 50, 1, nil, []string{"private"}),
			},
		}},
	}

	report := buildReachability(index, "")
	if report.Completeness.StaticAnalysisComplete {
		t.Fatalf("completeness = %#v, want incomplete", report.Completeness)
	}
	symbol, found := reachabilitySymbolByName(report.Symbols, "unused")
	if !found || symbol.State != ReachabilityStateUnknown {
		t.Fatalf("unused = %#v, want unknown", symbol)
	}
}

func testReachabilitySymbol(
	name string,
	kind string,
	start int,
	end int,
	line int,
	annotations []string,
	modifiers []string,
) analysis.Symbol {
	return analysis.Symbol{
		Name:        name,
		Kind:        kind,
		Annotations: annotations,
		Modifiers:   modifiers,
		Range: analysis.Range{
			StartByte: start,
			EndByte:   end,
			StartLine: line,
		},
	}
}

func reachabilitySymbolByName(
	symbols []ReachabilitySymbol,
	name string,
) (ReachabilitySymbol, bool) {
	index := slices.IndexFunc(symbols, func(symbol ReachabilitySymbol) bool {
		return symbol.Name == name
	})
	if index < 0 {
		return ReachabilitySymbol{}, false
	}
	return symbols[index], true
}
