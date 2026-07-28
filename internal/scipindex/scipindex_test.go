package scipindex

import (
	"bytes"
	"context"
	"testing"

	"github.com/scip-code/scip/bindings/go/scip"
	"google.golang.org/protobuf/proto"
)

const testSymbol = "scip-go gomod example.com/service v1.0.0 internal/store/Store#Save()."

func TestImportReadAndResolveReferences(t *testing.T) {
	definition := &scip.Occurrence{
		Symbol:      testSymbol,
		SymbolRoles: int32(scip.SymbolRole_Definition),
	}
	definition.SetSourceRange(scip.Range{
		Start: scip.Position{Line: 2, Character: 5},
		End:   scip.Position{Line: 2, Character: 9},
	})
	reference := &scip.Occurrence{
		Symbol:      testSymbol,
		SymbolRoles: int32(scip.SymbolRole_ReadAccess),
	}
	reference.SetSourceRange(scip.Range{
		Start: scip.Position{Line: 9, Character: 3},
		End:   scip.Position{Line: 9, Character: 7},
	})
	input := &scip.Index{
		Metadata: &scip.Metadata{
			ToolInfo: &scip.ToolInfo{Name: "scip-go", Version: "test"},
		},
		Documents: []*scip.Document{{
			RelativePath: "internal/store/store.go",
			Language:     "go",
			Symbols: []*scip.SymbolInformation{{
				Symbol:                 testSymbol,
				DisplayName:            "Save",
				Kind:                   scip.SymbolInformation_Method,
				Documentation:          []string{"Persists the record."},
				SignatureDocumentation: &scip.Signature{Language: "go", Text: "func (s *Store) Save() error"},
			}},
			Occurrences: []*scip.Occurrence{definition, reference},
		}},
	}
	content, err := proto.MarshalOptions{Deterministic: true}.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	summary, err := store.Import(context.Background(), 7, "abc123", "backend", bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if summary.Documents != 1 || summary.Symbols != 1 || summary.Occurrences != 2 {
		t.Fatalf("summary = %#v", summary)
	}
	if _, err := store.Import(context.Background(), 7, "abc123", "backend", bytes.NewReader(content)); err != nil {
		t.Fatalf("replace same-revision import: %v", err)
	}
	artifact, ok, err := store.Read(context.Background(), 7, "abc123")
	if err != nil || !ok || artifact.Documents[0].Path != "backend/internal/store/store.go" {
		t.Fatalf("Read() = %#v, %v, %v", artifact, ok, err)
	}
	resolution := ResolveReferences([]Artifact{artifact}, "Save")
	if resolution.State != "unique-name" ||
		resolution.Symbol != testSymbol ||
		len(resolution.References) != 1 ||
		len(resolution.Definitions) != 1 ||
		resolution.References[0].Line != 10 ||
		resolution.References[0].Kind != "read" ||
		resolution.Hover == nil ||
		resolution.Hover.Signature != "func (s *Store) Save() error" {
		t.Fatalf("resolution = %#v", resolution)
	}
	exact := ResolveReferences([]Artifact{artifact}, testSymbol)
	if exact.State != "exact" || len(exact.References) != 1 {
		t.Fatalf("exact resolution = %#v", exact)
	}
}

func TestResolveReferencesStitchesImplementationRelationshipsAcrossRepositories(t *testing.T) {
	const (
		apiSymbol  = "scip-java maven example:api 1.0.0 example/api/Store#save()."
		implSymbol = "scip-java maven example:impl 1.0.0 example/impl/SqlStore#save()."
	)
	artifacts := []Artifact{
		{
			RepositoryID: 1, Revision: "one",
			Symbols: []Symbol{{ID: apiSymbol, DisplayName: "save"}},
			Documents: []Document{{
				Path: "Store.java", Language: "java",
				Occurrences: []Occurrence{{
					Symbol: apiSymbol, SymbolRoles: int32(scip.SymbolRole_Definition), StartLine: 2,
				}},
			}},
		},
		{
			RepositoryID: 2, Revision: "two",
			Symbols: []Symbol{{
				ID: implSymbol, DisplayName: "save",
				Relationships: []Relationship{{
					Symbol: apiSymbol, Reference: true, Implementation: true,
				}},
			}},
			Documents: []Document{{
				Path: "SqlStore.java", Language: "java",
				Occurrences: []Occurrence{
					{Symbol: implSymbol, SymbolRoles: int32(scip.SymbolRole_Definition), StartLine: 6},
					{Symbol: implSymbol, SymbolRoles: int32(scip.SymbolRole_ReadAccess), StartLine: 20},
				},
			}},
		},
	}
	resolution := ResolveReferences(artifacts, apiSymbol)
	if !resolution.Stitched || len(resolution.Implementations) != 1 ||
		resolution.Implementations[0].RepositoryID != 2 ||
		len(resolution.References) != 1 ||
		resolution.References[0].RepositoryID != 2 {
		t.Fatalf("stitched resolution = %#v", resolution)
	}
}

func TestResolveReferencesRejectsAmbiguousDisplayName(t *testing.T) {
	artifacts := []Artifact{
		{Symbols: []Symbol{{ID: "scip-go gomod one v1 A#Save().", DisplayName: "Save"}}},
		{Symbols: []Symbol{{ID: "scip-go gomod two v1 B#Save().", DisplayName: "Save"}}},
	}
	resolution := ResolveReferences(artifacts, "Save")
	if resolution.State != "ambiguous" || len(resolution.Candidates) != 2 {
		t.Fatalf("resolution = %#v", resolution)
	}
}

func TestImportRejectsNonCanonicalDocumentPath(t *testing.T) {
	input := &scip.Index{
		Metadata:  &scip.Metadata{},
		Documents: []*scip.Document{{RelativePath: "../outside.go"}},
	}
	content, err := proto.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Import(
		context.Background(),
		1,
		"abc123",
		"../outside",
		bytes.NewReader(content),
	); err == nil {
		t.Fatal("expected invalid source root to be rejected")
	}
	if _, err := store.Import(context.Background(), 1, "abc123", ".", bytes.NewReader(content)); err == nil {
		t.Fatal("expected invalid document path to be rejected")
	}
}
