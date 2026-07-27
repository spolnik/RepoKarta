package codeintel

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/contextscope"
)

type memoryNamedContextStore struct {
	records []contextscope.NamedContextRecord
}

func (s *memoryNamedContextStore) ListNamedContextRecords(context.Context) ([]contextscope.NamedContextRecord, error) {
	return append([]contextscope.NamedContextRecord(nil), s.records...), nil
}

func (s *memoryNamedContextStore) GetNamedContextRecord(_ context.Context, id string) (contextscope.NamedContextRecord, error) {
	for _, record := range s.records {
		if record.ID == id {
			return record, nil
		}
	}
	return contextscope.NamedContextRecord{}, contextscope.ErrNamedContextNotFound
}

func (s *memoryNamedContextStore) CreateNamedContextRecord(
	_ context.Context,
	record contextscope.NamedContextRecord,
) (contextscope.NamedContextRecord, error) {
	record.ID = "created"
	record.OwnerID = "local:admin"
	record.CreatedAt = time.Now().UTC()
	record.UpdatedAt = record.CreatedAt
	s.records = append(s.records, record)
	return record, nil
}

func (s *memoryNamedContextStore) UpdateNamedContextRecord(
	_ context.Context,
	id string,
	record contextscope.NamedContextRecord,
) (contextscope.NamedContextRecord, error) {
	for index := range s.records {
		if s.records[index].ID == id {
			record.ID = id
			s.records[index] = record
			return record, nil
		}
	}
	return contextscope.NamedContextRecord{}, contextscope.ErrNamedContextNotFound
}

func (s *memoryNamedContextStore) DeleteNamedContextRecord(_ context.Context, id string) error {
	for index := range s.records {
		if s.records[index].ID == id {
			s.records = append(s.records[:index], s.records[index+1:]...)
			return nil
		}
	}
	return contextscope.ErrNamedContextNotFound
}

func TestResolveEffectiveContextsMergesDefaultsNamedAndExplicitProvenance(t *testing.T) {
	revision := "0123456789012345678901234567890123456789"
	repository := catalog.Repository{
		ID: 7, Name: "payments", IndexedCommit: revision, IndexState: "ready",
	}
	selector := contextscope.Selector{
		Kind: contextscope.KindRepository, RepositoryID: repository.ID, Revision: revision,
	}
	namedStore := &memoryNamedContextStore{records: []contextscope.NamedContextRecord{{
		ID:           "fleet",
		Title:        "Platform fleet",
		Category:     contextscope.CategoryServiceFleet,
		Visibility:   contextscope.VisibilityShared,
		DefaultScope: contextscope.DefaultAdministrator,
		OwnerID:      "local:admin",
		Managed:      true,
		Selectors:    []contextscope.Selector{selector},
	}}}
	service := New(referenceTestStore{repository: repository}, fixedResultSearcher{}, "https://repo.example.com").
		UseNamedContexts(namedStore)
	effective, err := service.ResolveEffectiveContexts(t.Context(), contextscope.EffectiveRequest{
		Contexts:        []contextscope.Selector{selector},
		NamedContextIDs: []string{"fleet"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(effective.Contexts) != 1 {
		t.Fatalf("effective contexts = %#v", effective.Contexts)
	}
	context := effective.Contexts[0]
	if context.URL != "https://repo.example.com/contexts?kind=repository&repository=7&revision="+revision {
		t.Fatalf("context URL = %q", context.URL)
	}
	if len(context.Sources) != 3 ||
		context.Sources[0].Kind != contextscope.SourceAdministratorDefault ||
		context.Sources[1].Kind != contextscope.SourceNamed ||
		context.Sources[2].Kind != contextscope.SourceExplicit {
		t.Fatalf("context sources = %#v", context.Sources)
	}
	if len(effective.NamedContexts) != 1 ||
		effective.NamedContexts[0].URL != "https://repo.example.com/contexts/fleet" {
		t.Fatalf("named contexts = %#v", effective.NamedContexts)
	}
}

func TestNamedContextValidationPinsCurrentRevisionAndFailsClosedWhenStale(t *testing.T) {
	revision := "0123456789012345678901234567890123456789"
	repository := catalog.Repository{
		ID: 11, Name: "catalogue", IndexedCommit: revision, IndexState: "ready",
	}
	namedStore := &memoryNamedContextStore{}
	service := New(referenceTestStore{repository: repository}, fixedResultSearcher{}, "").
		UseNamedContexts(namedStore)
	created, err := service.CreateNamedContext(t.Context(), contextscope.NamedContextInput{
		Title:        "Release 42",
		Category:     contextscope.CategoryRelease,
		Visibility:   contextscope.VisibilityPersonal,
		DefaultScope: contextscope.DefaultPersonal,
		Selectors: []contextscope.Selector{{
			Kind: contextscope.KindRepository, RepositoryID: repository.ID,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.State != "ready" || len(namedStore.records) != 1 ||
		namedStore.records[0].Selectors[0].Revision != revision {
		t.Fatalf("created named context = %#v, records = %#v", created, namedStore.records)
	}
	namedStore.records[0].Selectors[0].Revision = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	_, err = service.ResolveEffectiveContexts(t.Context(), contextscope.EffectiveRequest{})
	var resolution *contextscope.ResolutionError
	if !errors.As(err, &resolution) || len(resolution.Issues) != 1 ||
		resolution.Issues[0].Code != "stale" {
		t.Fatalf("stale default error = %#v", err)
	}
	useDefaults := false
	unscoped, err := service.ResolveEffectiveContexts(t.Context(), contextscope.EffectiveRequest{
		UseDefaults: &useDefaults,
	})
	if err != nil || len(unscoped.Contexts) != 0 {
		t.Fatalf("explicit default opt-out = %#v, %v", unscoped, err)
	}
}

func TestNamedContextDoesNotLeakUnavailableRepositoryIdentity(t *testing.T) {
	namedStore := &memoryNamedContextStore{records: []contextscope.NamedContextRecord{{
		ID:           "restricted",
		Title:        "Restricted fleet",
		Category:     contextscope.CategoryServiceFleet,
		Visibility:   contextscope.VisibilityShared,
		DefaultScope: contextscope.DefaultAdministrator,
		Selectors: []contextscope.Selector{{
			Kind:         contextscope.KindRepository,
			RepositoryID: 9876,
			Revision:     "0123456789012345678901234567890123456789",
		}},
	}}}
	service := New(referenceTestStore{}, fixedResultSearcher{}, "").UseNamedContexts(namedStore)
	contexts, err := service.ListNamedContexts(t.Context())
	if err != nil || len(contexts.NamedContexts) != 1 {
		t.Fatalf("named contexts = %#v, %v", contexts, err)
	}
	issues := contexts.NamedContexts[0].Issues
	if len(issues) != 1 || issues[0].Selector.RepositoryID != 0 ||
		issues[0].Selector.Revision != "" ||
		issues[0].Message == "" {
		t.Fatalf("sanitized issues = %#v", issues)
	}
	_, err = service.ResolveEffectiveContexts(t.Context(), contextscope.EffectiveRequest{})
	var resolution *contextscope.ResolutionError
	if !errors.As(err, &resolution) || resolution.Issues[0].Selector.RepositoryID != 0 {
		t.Fatalf("effective error = %#v", err)
	}
}

func TestLegacyRepositorySelectorOverridesDefaultContexts(t *testing.T) {
	revision := "0123456789012345678901234567890123456789"
	repository := catalog.Repository{
		ID: 19, Name: "legacy", IndexedCommit: revision, IndexState: "ready",
	}
	namedStore := &memoryNamedContextStore{records: []contextscope.NamedContextRecord{{
		ID:           "default",
		Title:        "Personal default",
		Category:     contextscope.CategoryPersonalTask,
		Visibility:   contextscope.VisibilityPersonal,
		DefaultScope: contextscope.DefaultPersonal,
		OwnerID:      "local:admin",
		Selectors: []contextscope.Selector{{
			Kind:         contextscope.KindRepository,
			RepositoryID: repository.ID,
			Revision:     revision,
		}},
	}}}
	service := New(referenceTestStore{repository: repository}, fixedResultSearcher{}, "").
		UseNamedContexts(namedStore)

	response, err := service.Search(t.Context(), SearchRequest{
		Query:        "needle",
		RepositoryID: repository.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Contexts) != 0 || len(response.NamedContexts) != 0 {
		t.Fatalf("legacy selector unexpectedly used defaults: %#v", response)
	}
}
