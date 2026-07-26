package search

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/spolnik/RepoKarta/internal/catalog"
)

type observerStore struct {
	mu         sync.Mutex
	repository catalog.Repository
}

func (*observerStore) SyncRepositories(context.Context, []catalog.Repository) error {
	return nil
}

func (s *observerStore) ListRepositories(context.Context) ([]catalog.Repository, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return []catalog.Repository{s.repository}, nil
}

func (s *observerStore) UpdateIndexState(_ context.Context, _ int64, state, commit, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repository.IndexState = state
	s.repository.IndexedCommit = commit
	s.repository.IndexError = message
	return nil
}

type observerEngine struct{}

func (observerEngine) Index(context.Context, catalog.Repository) (bool, error) {
	return true, nil
}

func (observerEngine) Search(context.Context, Query) (Result, error) {
	return Result{}, nil
}

func (observerEngine) Close() error { return nil }

func TestIndexCompletionQueuesDerivedStructuralIndex(t *testing.T) {
	store := &observerStore{repository: catalog.Repository{
		ID:         7,
		Name:       "payments",
		HeadCommit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ScanState:  "ready",
		IndexState: "pending",
	}}
	observed := make(chan int64, 1)
	coordinator := NewCoordinator("", nil, store, observerEngine{}).
		UseIndexedObserver(func(_ context.Context, repositoryID int64) error {
			observed <- repositoryID
			return nil
		})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	coordinator.baseCtx = ctx
	go coordinator.observeIndexed(ctx)

	coordinator.indexPending(ctx)

	select {
	case repositoryID := <-observed:
		if repositoryID != 7 {
			t.Fatalf("observed repository = %d, want 7", repositoryID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("derived structural index was not queued")
	}
	repositories, err := store.ListRepositories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if repositories[0].IndexState != "ready" ||
		repositories[0].IndexedCommit != repositories[0].HeadCommit {
		t.Fatalf("indexed repository = %#v", repositories[0])
	}
}

func TestDerivedStructuralIndexQueueDoesNotDropLargeFleets(t *testing.T) {
	const repositories = 300
	observed := make(chan int64, repositories)
	coordinator := NewCoordinator("", nil, &observerStore{}, observerEngine{}).
		UseIndexedObserver(func(_ context.Context, repositoryID int64) error {
			observed <- repositoryID
			return nil
		})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for repositoryID := int64(1); repositoryID <= repositories; repositoryID++ {
		coordinator.queueIndexed(ctx, repositoryID)
	}
	go coordinator.observeIndexed(ctx)

	seen := make(map[int64]struct{}, repositories)
	timeout := time.After(2 * time.Second)
	for len(seen) < repositories {
		select {
		case repositoryID := <-observed:
			seen[repositoryID] = struct{}{}
		case <-timeout:
			t.Fatalf("observed %d of %d queued repositories", len(seen), repositories)
		}
	}
}

type providerStore struct {
	repositories []catalog.Repository
}

func (s *providerStore) SyncRepositories(_ context.Context, repositories []catalog.Repository) error {
	s.repositories = append([]catalog.Repository(nil), repositories...)
	return nil
}

func (s *providerStore) ListRepositories(context.Context) ([]catalog.Repository, error) {
	return append([]catalog.Repository(nil), s.repositories...), nil
}

func (*providerStore) UpdateIndexState(context.Context, int64, string, string, string) error {
	return nil
}

func TestRefreshMergesAdministratorManagedRepositories(t *testing.T) {
	store := &providerStore{}
	managed := catalog.Repository{
		Name:       "managed",
		Path:       "C:/repokarta-owned/github/acme/managed",
		HeadCommit: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		ScanState:  "ready",
		IndexState: "pending",
	}
	coordinator := NewCoordinator(t.TempDir(), nil, store, observerEngine{}).
		UseRepositoryProvider(func(context.Context) ([]catalog.Repository, error) {
			return []catalog.Repository{managed}, nil
		})
	if err := coordinator.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.repositories) != 1 || store.repositories[0].Path != managed.Path {
		t.Fatalf("refreshed repositories = %#v", store.repositories)
	}
}
