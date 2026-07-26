package search

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/spolnik/RepoKarta/internal/catalog"
)

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

	startOnce       sync.Once
	baseCtx         context.Context
	indexing        atomic.Bool
	indexedObserver func(context.Context, int64) error
	indexedQueue    chan int64
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

// UseIndexedObserver schedules one bounded, sequential background callback for
// every repository that is already ready or becomes ready after indexing. It
// is intended for deterministic derived indexes such as structural maps.
func (c *Coordinator) UseIndexedObserver(observer func(context.Context, int64) error) *Coordinator {
	c.indexedObserver = observer
	if observer != nil {
		c.indexedQueue = make(chan int64, 128)
	}
	return c
}

// Start performs the initial scan and starts incremental indexing in the
// background. Calling Start more than once is harmless.
func (c *Coordinator) Start(ctx context.Context) error {
	var startError error
	c.startOnce.Do(func() {
		c.baseCtx = ctx
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
func (c *Coordinator) Refresh(ctx context.Context) error {
	repositories, err := catalog.DiscoverWithOptions(c.root, catalog.DiscoverOptions{Exclude: c.excludes})
	if err != nil {
		return fmt.Errorf("discover repositories: %w", err)
	}
	if err := c.store.SyncRepositories(ctx, repositories); err != nil {
		return fmt.Errorf("store repositories: %w", err)
	}
	c.queueIndexing()
	return nil
}

func (c *Coordinator) queueIndexing() {
	ctx := c.baseCtx
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
			if err := c.store.UpdateIndexState(ctx, repository.ID, "indexing", repository.IndexedCommit, ""); err != nil {
				slog.Error("record indexing start", "repository", repository.Name, "error", err)
				continue
			}

			_, indexError := c.engine.Index(ctx, repository)
			if indexError != nil {
				slog.Error("index repository", "repository", repository.Name, "error", indexError)
				_ = c.store.UpdateIndexState(ctx, repository.ID, "error", repository.IndexedCommit, indexError.Error())
				continue
			}
			if err := c.store.UpdateIndexState(ctx, repository.ID, "ready", repository.HeadCommit, ""); err != nil {
				slog.Error("record indexing completion", "repository", repository.Name, "error", err)
				continue
			}
			c.queueIndexed(ctx, repository.ID)
		}

		if !indexedAny {
			return
		}
	}
}

func (c *Coordinator) queueIndexed(ctx context.Context, repositoryID int64) {
	if c.indexedObserver == nil || c.indexedQueue == nil || repositoryID <= 0 {
		return
	}
	select {
	case c.indexedQueue <- repositoryID:
	case <-ctx.Done():
	default:
		slog.Warn("derived structural index queue is full", "repository_id", repositoryID)
	}
}

func (c *Coordinator) observeIndexed(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case repositoryID := <-c.indexedQueue:
			if err := c.indexedObserver(ctx, repositoryID); err != nil && ctx.Err() == nil {
				slog.Warn("build derived structural index", "repository_id", repositoryID, "error", err)
			}
		}
	}
}
