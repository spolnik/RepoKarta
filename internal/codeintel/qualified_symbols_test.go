package codeintel

import (
	"strings"
	"testing"

	"github.com/scip-code/scip/bindings/go/scip"
	"github.com/spolnik/RepoKarta/internal/analysis"
	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/graph"
	"github.com/spolnik/RepoKarta/internal/scipindex"
)

func TestQualifiedSymbolSearchPrefersCompilerMetadataAndSupportsModes(t *testing.T) {
	revision := strings.Repeat("a", 40)
	repository := catalog.Repository{
		ID: 7, Name: "payments", Path: t.TempDir(),
		IndexedCommit: revision, IndexState: "ready",
	}
	const semanticSave = "scip-java maven com.acme:payments 1.0.0 com/acme/store/PaymentStore#save()."
	structure := referenceTestStructure{index: graph.StructuralIndex{
		Scope: graph.Scope{Complete: true, TotalRepositories: 1, AnalyzedRepositories: 1},
		Structure: []graph.StructuralDocument{{
			RepositoryID: 7, Repository: "payments", Revision: revision,
			Path: "src/main/java/com/acme/store/PaymentStore.java", Language: "java",
			Symbols: []analysis.Symbol{{
				Name: "save", Kind: "method", Confidence: "syntax",
				Range: analysis.Range{StartLine: 12, EndLine: 14},
			}},
		}},
	}}
	service := New(visibleRepositoryStore{repository: repository}, &capturingSearcher{}, "http://localhost").
		UseStructure(structure).
		UseSCIP(referenceTestSCIP{artifact: scipindex.Artifact{
			RepositoryID: 7, Revision: revision,
			Symbols: []scipindex.Symbol{{
				ID: semanticSave, DisplayName: "save", Kind: "Method",
				Signature:     "void save(Payment payment)",
				Documentation: []string{"Persists one payment."},
			}},
			Documents: []scipindex.Document{{
				Path: "src/main/java/com/acme/store/PaymentStore.java", Language: "java",
				Occurrences: []scipindex.Occurrence{{
					Symbol: semanticSave, SymbolRoles: int32(scip.SymbolRole_Definition),
					StartLine: 11,
				}},
			}},
		}})
	result, err := service.Search(t.Context(), SearchRequest{
		Query:        `save result_type:symbol_definition package:com.acme:payments method:sa match:prefix`,
		RepositoryID: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Matches) != 1 || result.Matches[0].Symbol == nil ||
		result.Matches[0].Symbol.Confidence != "compiler" ||
		result.Matches[0].Symbol.Signature != "void save(Payment payment)" ||
		result.Matches[0].Lines[0].Number != 12 {
		t.Fatalf("qualified result = %#v", result)
	}
}

func TestQualifiedSymbolSearchLabelsSyntaxFallback(t *testing.T) {
	revision := strings.Repeat("b", 40)
	repository := catalog.Repository{
		ID: 8, Name: "ledger", Path: t.TempDir(),
		IndexedCommit: revision, IndexState: "ready",
	}
	service := New(visibleRepositoryStore{repository: repository}, &capturingSearcher{}, "").
		UseStructure(referenceTestStructure{index: graph.StructuralIndex{
			Scope: graph.Scope{Complete: true, TotalRepositories: 1, AnalyzedRepositories: 1},
			Structure: []graph.StructuralDocument{{
				RepositoryID: 8, Repository: "ledger", Revision: revision,
				Path: "internal/ledger/store.go", Language: "go",
				Symbols: []analysis.Symbol{{
					Name: "Store", Kind: "type", Confidence: "syntax",
					Range: analysis.Range{StartLine: 4},
				}},
			}},
		}})
	result, err := service.Search(t.Context(), SearchRequest{
		Query:        "full_name:internal.ledger.Store result_type:symbol_definition",
		RepositoryID: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Matches) != 1 || result.Matches[0].Symbol.Confidence != "syntax" ||
		len(result.Warnings) != 1 ||
		result.Warnings[0].Code != "qualified_symbol_syntax_fallback" {
		t.Fatalf("syntax result = %#v", result)
	}
}
