package search

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/telemetry"
)

const maximumDerivedIndexConcurrency = 8

// CatalogueStore is the metadata surface needed by the indexing coordinator.
type CatalogueStore interface {
	SyncRepositories(context.Context, []catalog.Repository) error
	ListRepositories(context.Context) ([]catalog.Repository, error)
	UpdateIndexState(context.Context, int64, string, string, string) error
}

// Coordinator discovers repositories and incrementally indexes changed HEADs.
type Coordinator struct {
	root     string
	excludes []string
	store    CatalogueStore
	engine   Engine

	startOnce          sync.Once
	baseCtxMu          sync.RWMutex
	baseCtx            context.Context
	indexing           atomic.Bool
	indexedObserver    func(context.Context, int64) error
	indexedSignal      chan struct{}
	indexedMu          sync.Mutex
	indexedPending     []int64
	indexedQueued      map[int64]struct{}
	repositoryProvider func(context.Context) ([]catalog.Repository, error)
	artifactCollectors []func(context.Context, []catalog.Repository) error
}

// UseRepositoryProvider merges verified administrator-approved repositories
// into every authoritative catalogue refresh.
func (c *Coordinator) UseRepositoryProvider(provider func(context.Context) ([]catalog.Repository, error)) *Coordinator {
	c.repositoryProvider = provider
	return c
}

// UseArtifactGarbageCollector runs a bounded RepoKarta-owned artifact sweep
// after the durable catalogue has been synchronized and before replacement
// indexing begins.
func (c *Coordinator) UseArtifactGarbageCollector(
	collector func(context.Context, []catalog.Repository) error,
) *Coordinator {
	if collector != nil {
		c.artifactCollectors = append(c.artifactCollectors, collector)
	}
	return c
}

// NewCoordinator creates an indexing coordinator.
func NewCoordinator(root string, excludes []string, store CatalogueStore, engine Engine) *Coordinator {
	return &Coordinator{
		root:     root,
		excludes: append([]string(nil), excludes...),
		store:    store,
		engine:   engine,
	}
}

// UseIndexedObserver schedules one bounded background callback for every
// repository that is already ready or becomes ready after indexing. Derived
// indexes are CPU and disk bounded independently, so a small worker pool keeps
// a large fleet from being paced by the slowest repository.
func (c *Coordinator) UseIndexedObserver(observer func(context.Context, int64) error) *Coordinator {
	c.indexedObserver = observer
	if observer != nil {
		c.indexedSignal = make(chan struct{}, maximumDerivedIndexConcurrency)
		c.indexedQueued = make(map[int64]struct{})
	}
	return c
}

// Start performs the initial scan and starts incremental indexing in the
// background. Calling Start more than once is harmless.
func (c *Coordinator) Start(ctx context.Context) error {
	var startError error
	c.startOnce.Do(func() {
		c.baseCtxMu.Lock()
		c.baseCtx = ctx
		c.baseCtxMu.Unlock()
		if c.indexedObserver != nil {
			go c.observeIndexed(ctx)
		}
		startError = c.Refresh(ctx)
		if startError != nil || c.indexedObserver == nil {
			return
		}
		repositories, err := c.store.ListRepositories(ctx)
		if err != nil {
			startError = fmt.Errorf("load repositories for derived indexes: %w", err)
			return
		}
		for _, repository := range repositories {
			if repository.IndexState == "ready" && repository.IndexedCommit != "" {
				c.queueIndexed(ctx, repository.ID)
			}
		}
	})
	return startError
}

// Refresh manually rescans the configured root and queues changed repositories.
func (c *Coordinator) Refresh(ctx context.Context) (resultErr error) {
	ctx, finish := telemetry.StartOperation(ctx, telemetry.OperationCatalogueRefresh, telemetry.Labels{})
	defer func() { finish(resultErr) }()
	repositories, err := catalog.DiscoverWithOptions(c.root, catalog.DiscoverOptions{Exclude: c.excludes})
	if err != nil {
		return fmt.Errorf("discover repositories: %w", err)
	}
	if c.repositoryProvider != nil {
		managed, err := c.repositoryProvider(ctx)
		if err != nil {
			return fmt.Errorf("load managed repositories: %w", err)
		}
		repositories = append(repositories, managed...)
	}
	if err := c.store.SyncRepositories(ctx, repositories); err != nil {
		return fmt.Errorf("store repositories: %w", err)
	}
	if collector, ok := c.engine.(ArtifactGarbageCollector); ok || len(c.artifactCollectors) > 0 {
		liveRepositories, err := c.store.ListRepositories(ctx)
		if err != nil {
			return fmt.Errorf("load repositories for artifact cleanup: %w", err)
		}
		if ok {
			live := make(map[int64]struct{}, len(liveRepositories))
			for _, repository := range liveRepositories {
				live[repository.ID] = struct{}{}
			}
			if err := collector.PruneRepositories(ctx, live); err != nil {
				return fmt.Errorf("clean derived search artifacts: %w", err)
			}
		}
		for _, collect := range c.artifactCollectors {
			if err := collect(ctx, liveRepositories); err != nil {
				return fmt.Errorf("clean derived repository artifacts: %w", err)
			}
		}
	}
	c.queueIndexing()
	return nil
}

func (c *Coordinator) queueIndexing() {
	c.baseCtxMu.RLock()
	ctx := c.baseCtx
	c.baseCtxMu.RUnlock()
	if ctx == nil || !c.indexing.CompareAndSwap(false, true) {
		return
	}
	go c.indexPending(ctx)
}

func (c *Coordinator) indexPending(ctx context.Context) {
	defer func() {
		c.indexing.Store(false)
		if ctx.Err() != nil {
			return
		}
		repositories, err := c.store.ListRepositories(ctx)
		if err != nil {
			return
		}
		for _, repository := range repositories {
			if repository.IndexState == "pending" && repository.ScanState == "ready" {
				c.queueIndexing()
				return
			}
		}
	}()

	for {
		repositories, err := c.store.ListRepositories(ctx)
		if err != nil {
			slog.Error("load repositories for indexing", "error", err)
			return
		}

		indexedAny := false
		for _, repository := range repositories {
			if ctx.Err() != nil {
				return
			}
			if repository.ScanState != "ready" ||
				repository.HeadCommit == "" ||
				repository.IndexState != "pending" {
				continue
			}

			indexedAny = true
			if err := c.indexRepository(ctx, repository); err != nil {
				slog.ErrorContext(ctx, "index repository", "repository", repository.Name, "error", err)
			}
		}

		if !indexedAny {
			return
		}
	}
}

func (c *Coordinator) indexRepository(ctx context.Context, repository catalog.Repository) (resultErr error) {
	ctx, finish := telemetry.StartOperation(ctx, telemetry.OperationIndexBuild, telemetry.Labels{
		Kind: repository.ScanState,
	})
	defer func() { finish(resultErr) }()
	if err := c.store.UpdateIndexState(ctx, repository.ID, "indexing", repository.IndexedCommit, ""); err != nil {
		return fmt.Errorf("record indexing start: %w", err)
	}
	_, err := c.engine.Index(ctx, repository)
	if err != nil {
		_ = c.store.UpdateIndexState(ctx, repository.ID, "error", repository.IndexedCommit, err.Error())
		return err
	}
	if err := c.store.UpdateIndexState(ctx, repository.ID, "ready", repository.HeadCommit, ""); err != nil {
		return fmt.Errorf("record indexing completion: %w", err)
	}
	c.queueIndexed(ctx, repository.ID)
	return nil
}

func (c *Coordinator) queueIndexed(ctx context.Context, repositoryID int64) {
	if c.indexedObserver == nil || c.indexedSignal == nil || repositoryID <= 0 {
		return
	}
	c.indexedMu.Lock()
	if _, queued := c.indexedQueued[repositoryID]; queued {
		c.indexedMu.Unlock()
		return
	}
	c.indexedQueued[repositoryID] = struct{}{}
	c.indexedPending = append(c.indexedPending, repositoryID)
	c.indexedMu.Unlock()
	select {
	case c.indexedSignal <- struct{}{}:
	case <-ctx.Done():
	default:
	}
}

func (c *Coordinator) observeIndexed(ctx context.Context) {
	var workers sync.WaitGroup
	workers.Add(maximumDerivedIndexConcurrency)
	for range maximumDerivedIndexConcurrency {
		go func() {
			defer workers.Done()
			c.observeIndexedWorker(ctx)
		}()
	}
	workers.Wait()
}

func (c *Coordinator) observeIndexedWorker(ctx context.Context) {
	for {
		repositoryID, ok := c.nextIndexed()
		if ok {
			if err := c.indexedObserver(ctx, repositoryID); err != nil && ctx.Err() == nil {
				slog.Warn("build derived structural index", "repository_id", repositoryID, "error", err)
			}
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-c.indexedSignal:
		}
	}
}

func (c *Coordinator) nextIndexed() (int64, bool) {
	c.indexedMu.Lock()
	defer c.indexedMu.Unlock()
	if len(c.indexedPending) == 0 {
		return 0, false
	}
	repositoryID := c.indexedPending[0]
	c.indexedPending = c.indexedPending[1:]
	delete(c.indexedQueued, repositoryID)
	return repositoryID, true
}
