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
