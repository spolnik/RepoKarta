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

	startOnce sync.Once
	baseCtx   context.Context
	indexing  atomic.Bool
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

// Start performs the initial scan and starts incremental indexing in the
// background. Calling Start more than once is harmless.
func (c *Coordinator) Start(ctx context.Context) error {
	var startError error
	c.startOnce.Do(func() {
		c.baseCtx = ctx
		startError = c.Refresh(ctx)
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
			}
		}

		if !indexedAny {
			return
		}
	}
}
