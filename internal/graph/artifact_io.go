package graph

import (
	"context"
	"sync"

	"github.com/spolnik/RepoKarta/internal/catalog"
)

type repositoryArtifact[T any] struct {
	Repository catalog.Repository
	Value      T
	Ready      bool
}

func readRepositoryArtifacts[T any](
	ctx context.Context,
	repositories []catalog.Repository,
	read func(catalog.Repository) (T, bool),
) ([]repositoryArtifact[T], error) {
	results := make([]repositoryArtifact[T], len(repositories))
	workers := make(chan struct{}, maximumStructuralReadConcurrency)
	var wait sync.WaitGroup
	for index, repository := range repositories {
		wait.Add(1)
		go func() {
			defer wait.Done()
			select {
			case workers <- struct{}{}:
				defer func() { <-workers }()
			case <-ctx.Done():
				return
			}
			value, ready := read(repository)
			results[index] = repositoryArtifact[T]{
				Repository: repository,
				Value:      value,
				Ready:      ready,
			}
		}()
	}
	wait.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return results, nil
}
