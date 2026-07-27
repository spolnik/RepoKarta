package codeintel

import (
	"context"
	"fmt"

	"github.com/spolnik/RepoKarta/internal/querylang"
)

// CompleteQuery returns permission-checked grammar and value completions.
func (s *Service) CompleteQuery(
	ctx context.Context,
	raw string,
	cursor int,
) (querylang.CompletionList, error) {
	repositories, err := s.store.ListRepositories(ctx)
	if err != nil {
		return querylang.CompletionList{}, err
	}
	options := querylang.CompletionOptions{
		Repositories: make([]querylang.Option, 0, len(repositories)*2),
		Revisions:    make([]querylang.Option, 0, len(repositories)),
	}
	seenRevisions := make(map[string]struct{}, len(repositories))
	for _, repository := range repositories {
		label := repository.Name
		if repository.IndexedCommit != "" {
			label += " · " + shortRevision(repository.IndexedCommit)
		}
		options.Repositories = append(options.Repositories, querylang.Option{
			Value: repository.Name,
			Label: label,
		})
		options.Repositories = append(options.Repositories, querylang.Option{
			Value: fmt.Sprintf("%d", repository.ID),
			Label: label + " · stable ID",
		})
		if repository.IndexedCommit == "" {
			continue
		}
		if _, duplicate := seenRevisions[repository.IndexedCommit]; duplicate {
			continue
		}
		seenRevisions[repository.IndexedCommit] = struct{}{}
		options.Revisions = append(options.Revisions, querylang.Option{
			Value: repository.IndexedCommit,
			Label: repository.Name + " · " + shortRevision(repository.IndexedCommit),
		})
	}
	return querylang.Complete(raw, cursor, options), nil
}
