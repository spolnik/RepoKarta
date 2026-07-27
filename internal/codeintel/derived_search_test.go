package codeintel

import (
	"context"
	"strings"
	"testing"

	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/search"
)

type capturingDerivedSearcher struct {
	request DerivedEvidenceRequest
	result  DerivedEvidenceResult
}

func (s *capturingDerivedSearcher) SearchDerivedEvidence(
	_ context.Context,
	request DerivedEvidenceRequest,
) (DerivedEvidenceResult, error) {
	s.request = request
	return s.result, nil
}

func TestDerivedSearchPassesOnlyVisibleRepositoriesAndRevalidatesItems(t *testing.T) {
	revision := strings.Repeat("a", 40)
	repository := catalog.Repository{
		ID: 7, Name: "payments", Path: t.TempDir(),
		IndexedCommit: revision, IndexState: "ready",
	}
	derived := &capturingDerivedSearcher{result: DerivedEvidenceResult{
		Items: []SearchItem{
			{ResultType: "dependency", RepositoryID: 7, Title: "visible"},
			{ResultType: "dependency", RepositoryID: 99, Title: "private"},
		},
		TotalExact: true,
	}}
	sourceSearcher := &capturingSearcher{}
	service := New(
		visibleRepositoryStore{repository: repository},
		sourceSearcher,
		"http://ui",
	).UseDerivedEvidence(derived)
	result, err := service.Search(t.Context(), SearchRequest{
		Query: "mux result_type:dependency repository:payments",
		Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(derived.request.Repositories) != 1 ||
		derived.request.Repositories[0].ID != repository.ID ||
		derived.request.Repositories[0].Revision != revision {
		t.Fatalf("provider repositories = %#v", derived.request.Repositories)
	}
	if len(result.Items) != 1 || result.Items[0].Title != "visible" ||
		result.Items[0].Repository != repository.Name ||
		!result.Truncated || result.TotalFilesExact ||
		len(result.Warnings) != 1 ||
		result.Warnings[0].Code != "unauthorized_derived_evidence" {
		t.Fatalf("derived response = %#v", result)
	}
	if sourceSearcher.query.Text != "" {
		t.Fatalf("derived search reached source index: %#v", sourceSearcher.query)
	}
}

func TestDerivedSearchReturnsExplicitEmptyEvidenceWithoutCallingProvider(t *testing.T) {
	derived := &capturingDerivedSearcher{result: DerivedEvidenceResult{
		Warnings: []search.Warning{{Code: "unexpected", Message: "provider called"}},
	}}
	service := New(symbolTestStore{}, &capturingSearcher{}, "").UseDerivedEvidence(derived)
	result, err := service.Search(t.Context(), SearchRequest{
		Query: "result_type:route",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 0 || len(result.Warnings) != 0 ||
		len(derived.request.Repositories) != 0 ||
		result.ResultType != "route" ||
		!result.TotalFilesExact {
		t.Fatalf("empty response = %#v; provider request = %#v", result, derived.request)
	}
}
