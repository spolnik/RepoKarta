package codeintel

import (
	"testing"

	"github.com/spolnik/RepoKarta/internal/catalog"
)

func TestCompleteQueryUsesOnlyVisibleRepositoryMetadata(t *testing.T) {
	revision := "0123456789012345678901234567890123456789"
	service := New(referenceTestStore{repository: catalog.Repository{
		ID:            7,
		Name:          "payments api",
		IndexedCommit: revision,
	}}, fixedResultSearcher{}, "")

	repositories, err := service.CompleteQuery(
		t.Context(),
		"repository:pay",
		len("repository:pay"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(repositories.Completions) != 1 ||
		repositories.Completions[0].InsertText != `repository:"payments api"` {
		t.Fatalf("repository completions = %#v", repositories)
	}

	revisions, err := service.CompleteQuery(t.Context(), "revision:012", len("revision:012"))
	if err != nil {
		t.Fatal(err)
	}
	if len(revisions.Completions) != 1 ||
		revisions.Completions[0].InsertText != "revision:"+revision {
		t.Fatalf("revision completions = %#v", revisions)
	}
}
