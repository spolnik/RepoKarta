package codeintel

import (
	"context"
	"fmt"
	"strings"

	"github.com/spolnik/RepoKarta/internal/graph"
	"github.com/spolnik/RepoKarta/internal/querylang"
	"github.com/spolnik/RepoKarta/internal/search"
)

func ownershipFilters(parsed querylang.Query) (include, exclude []string) {
	for _, filter := range parsed.Filters {
		if filter.Field != querylang.FieldOwner {
			continue
		}
		if filter.Negative {
			exclude = append(exclude, filter.Value)
		} else {
			include = append(include, filter.Value)
		}
	}
	return include, exclude
}

func (s *Service) applyOwnership(
	ctx context.Context,
	response *SearchResponse,
	include []string,
	exclude []string,
) error {
	if s.structure == nil {
		if len(include)+len(exclude) > 0 {
			return fmt.Errorf("owner filters require a commit-pinned CODEOWNERS index")
		}
		return nil
	}
	if len(response.Matches) == 0 {
		return nil
	}
	index, err := s.structure.ReadStructure(ctx, 0)
	if err != nil {
		return err
	}
	byRepository := make(map[int64]graph.OwnershipIndex, len(index.Ownership))
	for _, ownership := range index.Ownership {
		byRepository[ownership.RepositoryID] = ownership
	}
	filtered := make([]SearchMatch, 0, len(response.Matches))
	matchCount := 0
	for _, match := range response.Matches {
		ownership, found := byRepository[match.RepositoryID]
		if !found {
			ownership = graph.OwnershipIndex{
				RepositoryID: match.RepositoryID,
				Repository:   match.Repository,
				Revision:     match.Revision,
				Available:    false,
			}
		}
		resolved := graph.ResolveOwners(ownership, match.Path)
		match.OwnerState = resolved.State
		match.Owners = append([]string(nil), resolved.Owners...)
		if resolved.Evidence.Path != "" {
			evidence := resolved.Evidence
			match.Ownership = &evidence
		}
		if !ownershipFiltersMatch(resolved, include, exclude) {
			continue
		}
		filtered = append(filtered, match)
		matchCount += max(1, len(match.Lines))
	}
	if len(filtered) != len(response.Matches) {
		response.Matches = filtered
		response.MatchCount = matchCount
		response.MatchingFiles = len(filtered)
		response.EstimatedTotalFiles = len(filtered)
		response.ReturnedFiles = len(filtered)
		response.ReturnedItems = len(filtered)
		response.TotalFilesExact = response.TotalFilesExact && !response.Truncated
		response.Warnings = append(response.Warnings, search.Warning{
			Code:    "ownership_filter_applied",
			Message: "Ownership filters were evaluated from commit-pinned CODEOWNERS metadata after deterministic source search.",
		})
	}
	return nil
}

func ownershipFiltersMatch(
	match graph.OwnershipMatch,
	include []string,
	exclude []string,
) bool {
	matches := func(value string) bool {
		value = strings.ToLower(strings.TrimSpace(value))
		switch value {
		case "owned":
			return match.State == "owned"
		case "unowned":
			return match.State == "unowned"
		case "unresolved", "unresolved-owner", "unresolved_owner":
			return match.State == "unresolved_owner"
		case "unavailable":
			return match.State == "unavailable"
		}
		for _, owner := range match.Owners {
			if strings.EqualFold(owner, value) {
				return true
			}
		}
		return false
	}
	for _, value := range include {
		if !matches(value) {
			return false
		}
	}
	for _, value := range exclude {
		if matches(value) {
			return false
		}
	}
	return true
}
