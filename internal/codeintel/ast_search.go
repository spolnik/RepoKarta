package codeintel

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/spolnik/RepoKarta/internal/analysis"
	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/graph"
	"github.com/spolnik/RepoKarta/internal/source"
)

const (
	DefaultASTSearchLimit           = 50
	MaximumASTSearchLimit           = 200
	MaximumASTCandidateFilesPerPage = 32
)

// ASTSearchRequest is a language-specific Tree-sitter query over persisted
// structural candidates. PathPrefix is a normalized repository-relative path.
type ASTSearchRequest struct {
	RepositoryID int64  `json:"repository_id,omitempty"`
	Language     string `json:"language"`
	Query        string `json:"query"`
	PathPrefix   string `json:"path_prefix,omitempty"`
	Limit        int    `json:"limit,omitempty"`
	Cursor       string `json:"cursor,omitempty"`
}

// ASTSearchIndex exposes artifact readiness separately from result pagination.
type ASTSearchIndex struct {
	Provider              string `json:"provider"`
	State                 string `json:"state"`
	ID                    string `json:"id"`
	RequestedRepositories int    `json:"requested_repositories"`
	ReadyRepositories     int    `json:"ready_repositories"`
	PendingRepositories   int    `json:"pending_repositories"`
	ArtifactTruncated     bool   `json:"artifact_truncated"`
}

// ASTSearchMatch is one Tree-sitter pattern match and all of its named captures.
type ASTSearchMatch struct {
	RepositoryID int64                   `json:"repository_id"`
	Repository   string                  `json:"repository"`
	Revision     string                  `json:"revision"`
	Path         string                  `json:"path"`
	Language     string                  `json:"language"`
	PatternIndex int                     `json:"pattern_index"`
	Captures     []analysis.QueryCapture `json:"captures"`
	Citation     string                  `json:"citation,omitempty"`
	SourceURL    string                  `json:"source_url,omitempty"`
}

// ASTSearchResponse distinguishes pagination from evidence completeness:
// NextCursor means more exact results may exist, while Complete reports whether
// the available artifact and every scanned file were processed without loss.
type ASTSearchResponse struct {
	Language          string           `json:"language"`
	Query             string           `json:"query"`
	Resolution        string           `json:"resolution"`
	RequiredRootKinds []string         `json:"required_root_kinds,omitempty"`
	Index             ASTSearchIndex   `json:"index"`
	CandidateFiles    int              `json:"candidate_files"`
	ScannedFiles      int              `json:"scanned_files"`
	SkippedFiles      int              `json:"skipped_files"`
	Matches           []ASTSearchMatch `json:"matches"`
	NextCursor        string           `json:"next_cursor,omitempty"`
	Truncated         bool             `json:"truncated"`
	Complete          bool             `json:"complete"`
	Warnings          []string         `json:"warnings,omitempty"`
	DurationMillis    int64            `json:"duration_ms"`
}

type astSearchCursor struct {
	Version     int    `json:"v"`
	IndexID     string `json:"i"`
	RequestHash string `json:"q"`
	Document    int    `json:"d"`
	Match       int    `json:"m"`
}

type astCandidate struct {
	document   graph.StructuralDocument
	repository catalog.Repository
}

// SearchAST executes a bounded structural query over source files selected by
// the persisted syntax inventory. It never builds structural artifacts on the
// interactive request path.
func (s *Service) SearchAST(ctx context.Context, request ASTSearchRequest) (ASTSearchResponse, error) {
	started := time.Now()
	if s.structure == nil {
		return ASTSearchResponse{}, errors.New("AST structural search is not configured")
	}
	request.Language = strings.ToLower(strings.TrimSpace(request.Language))
	request.Query = strings.TrimSpace(request.Query)
	request.PathPrefix = strings.TrimSpace(strings.ReplaceAll(request.PathPrefix, "\\", "/"))
	if request.RepositoryID < 0 {
		return ASTSearchResponse{}, errors.New("repository_id must be positive when provided")
	}
	if request.PathPrefix != "" {
		clean := path.Clean(request.PathPrefix)
		if clean != request.PathPrefix || clean == "." || path.IsAbs(clean) ||
			clean == ".." || strings.HasPrefix(clean, "../") {
			return ASTSearchResponse{}, errors.New("path_prefix must be a safe repository-relative path")
		}
	}
	limit := request.Limit
	if limit == 0 {
		limit = DefaultASTSearchLimit
	}
	if limit < 1 || limit > MaximumASTSearchLimit {
		return ASTSearchResponse{}, fmt.Errorf("limit must be an integer from 1 to %d", MaximumASTSearchLimit)
	}
	compiled, err := analysis.CompileStructuralQuery(request.Language, request.Query)
	if err != nil {
		return ASTSearchResponse{}, err
	}
	requiredRoots := analysis.RequiredRootKinds(request.Query)
	index, err := s.structure.ReadStructure(ctx, request.RepositoryID)
	if err != nil {
		return ASTSearchResponse{}, fmt.Errorf("load AST structural index: %w", err)
	}
	repositories, err := s.store.ListRepositories(ctx)
	if err != nil {
		return ASTSearchResponse{}, err
	}
	repositoriesByID := make(map[int64]catalog.Repository, len(repositories))
	for _, repository := range repositories {
		repositoriesByID[repository.ID] = repository
	}
	candidates := make([]astCandidate, 0, len(index.Structure))
	for _, document := range index.Structure {
		if document.Language != request.Language ||
			(request.RepositoryID > 0 && document.RepositoryID != request.RepositoryID) ||
			(request.PathPrefix != "" && document.Path != request.PathPrefix &&
				!strings.HasPrefix(document.Path, request.PathPrefix+"/")) ||
			!documentCanMatchRoots(document, requiredRoots) {
			continue
		}
		repository, visible := repositoriesByID[document.RepositoryID]
		if !visible {
			continue
		}
		candidates = append(candidates, astCandidate{document: document, repository: repository})
	}
	sort.Slice(candidates, func(left, right int) bool {
		if candidates[left].document.RepositoryID != candidates[right].document.RepositoryID {
			return candidates[left].document.RepositoryID < candidates[right].document.RepositoryID
		}
		return candidates[left].document.Path < candidates[right].document.Path
	})

	requestHash := astRequestHash(request)
	position, err := decodeASTCursor(request.Cursor, index.ID, requestHash, len(candidates))
	if err != nil {
		return ASTSearchResponse{}, err
	}
	response := ASTSearchResponse{
		Language:          request.Language,
		Query:             request.Query,
		Resolution:        "tree-sitter-query",
		RequiredRootKinds: requiredRoots,
		Index: ASTSearchIndex{
			Provider:              "tree-sitter",
			State:                 "ready",
			ID:                    index.ID,
			RequestedRepositories: index.Scope.TotalRepositories,
			ReadyRepositories:     index.Scope.AnalyzedRepositories,
			PendingRepositories:   index.Scope.OmittedRepositories,
			ArtifactTruncated:     index.StructureTruncated,
		},
		CandidateFiles: len(candidates),
		Matches:        make([]ASTSearchMatch, 0, limit),
	}
	if !index.Scope.Complete {
		response.Index.State = "building"
	}
	attemptedFiles := 0
	for documentIndex := position.Document; documentIndex < len(candidates); documentIndex++ {
		if err := ctx.Err(); err != nil {
			return ASTSearchResponse{}, err
		}
		if attemptedFiles >= MaximumASTCandidateFilesPerPage {
			response.NextCursor, err = encodeASTCursor(astSearchCursor{
				Version:     1,
				IndexID:     index.ID,
				RequestHash: requestHash,
				Document:    documentIndex,
			})
			if err != nil {
				return ASTSearchResponse{}, err
			}
			response.Complete = astSearchComplete(index, response)
			response.DurationMillis = time.Since(started).Milliseconds()
			return response, nil
		}
		attemptedFiles++
		candidate := candidates[documentIndex]
		content, readErr := source.ReadFileContent(
			ctx,
			candidate.repository,
			candidate.document.Revision,
			candidate.document.Path,
		)
		if readErr != nil {
			response.SkippedFiles++
			response.Warnings = appendWarning(response.Warnings, fmt.Sprintf(
				"%s: source could not be read at the indexed revision",
				candidate.document.Path,
			))
			position.Match = 0
			continue
		}
		result, executeErr := compiled.Execute(content.Bytes, analysis.QueryOptions{})
		if executeErr != nil {
			response.SkippedFiles++
			response.Warnings = appendWarning(response.Warnings, fmt.Sprintf(
				"%s: structural query could not be executed",
				candidate.document.Path,
			))
			position.Match = 0
			continue
		}
		response.ScannedFiles++
		response.Truncated = response.Truncated || result.Truncated
		matchStart := 0
		if documentIndex == position.Document {
			matchStart = position.Match
		}
		if matchStart > len(result.Matches) {
			return ASTSearchResponse{}, errors.New("AST search cursor is no longer valid")
		}
		for matchIndex := matchStart; matchIndex < len(result.Matches); matchIndex++ {
			match := result.Matches[matchIndex]
			startLine, endLine := astMatchLineRange(match)
			response.Matches = append(response.Matches, ASTSearchMatch{
				RepositoryID: candidate.document.RepositoryID,
				Repository:   candidate.document.Repository,
				Revision:     candidate.document.Revision,
				Path:         candidate.document.Path,
				Language:     candidate.document.Language,
				PatternIndex: match.PatternIndex,
				Captures:     match.Captures,
				Citation: Citation(
					candidate.document.Repository,
					candidate.document.Revision,
					candidate.document.Path,
					startLine,
					endLine,
				),
				SourceURL: s.SourceURL(
					candidate.document.RepositoryID,
					candidate.document.Revision,
					candidate.document.Path,
					startLine,
					endLine,
				),
			})
			if len(response.Matches) == limit {
				nextDocument, nextMatch := documentIndex, matchIndex+1
				if nextMatch >= len(result.Matches) {
					nextDocument, nextMatch = documentIndex+1, 0
				}
				if nextDocument < len(candidates) {
					response.NextCursor, err = encodeASTCursor(astSearchCursor{
						Version:     1,
						IndexID:     index.ID,
						RequestHash: requestHash,
						Document:    nextDocument,
						Match:       nextMatch,
					})
					if err != nil {
						return ASTSearchResponse{}, err
					}
				}
				response.Complete = astSearchComplete(index, response)
				response.DurationMillis = time.Since(started).Milliseconds()
				return response, nil
			}
		}
		position.Match = 0
	}
	response.Complete = astSearchComplete(index, response)
	response.DurationMillis = time.Since(started).Milliseconds()
	return response, nil
}

func documentCanMatchRoots(document graph.StructuralDocument, roots []string) bool {
	if len(roots) == 0 || len(document.NodeKinds) == 0 {
		return true
	}
	for _, root := range roots {
		index := sort.SearchStrings(document.NodeKinds, root)
		if index < len(document.NodeKinds) && document.NodeKinds[index] == root {
			return true
		}
	}
	return false
}

func astRequestHash(request ASTSearchRequest) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf(
		"%d\x00%s\x00%s\x00%s",
		request.RepositoryID,
		request.Language,
		request.PathPrefix,
		request.Query,
	)))
	return hex.EncodeToString(digest[:16])
}

func decodeASTCursor(value, indexID, requestHash string, candidateCount int) (astSearchCursor, error) {
	if strings.TrimSpace(value) == "" {
		return astSearchCursor{Version: 1}, nil
	}
	if len(value) > 2048 {
		return astSearchCursor{}, errors.New("AST search cursor is invalid")
	}
	content, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return astSearchCursor{}, errors.New("AST search cursor is invalid")
	}
	var cursor astSearchCursor
	if json.Unmarshal(content, &cursor) != nil ||
		cursor.Version != 1 ||
		cursor.IndexID != indexID ||
		cursor.RequestHash != requestHash ||
		cursor.Document < 0 ||
		cursor.Document > candidateCount ||
		cursor.Match < 0 ||
		(cursor.Document == candidateCount && cursor.Match != 0) {
		return astSearchCursor{}, errors.New("AST search cursor is stale or invalid")
	}
	return cursor, nil
}

func encodeASTCursor(cursor astSearchCursor) (string, error) {
	content, err := json.Marshal(cursor)
	if err != nil {
		return "", fmt.Errorf("encode AST search cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(content), nil
}

func astMatchLineRange(match analysis.QueryMatch) (int, int) {
	start, end := 0, 0
	for _, capture := range match.Captures {
		if start == 0 || capture.Range.StartLine < start {
			start = capture.Range.StartLine
		}
		if capture.Range.EndLine > end {
			end = capture.Range.EndLine
		}
	}
	return max(1, start), max(1, end)
}

func astSearchComplete(index graph.StructuralIndex, response ASTSearchResponse) bool {
	return index.Scope.Complete &&
		!index.StructureTruncated &&
		response.SkippedFiles == 0 &&
		!response.Truncated
}

func appendWarning(warnings []string, warning string) []string {
	const maximumWarnings = 20
	if len(warnings) >= maximumWarnings {
		return warnings
	}
	return append(warnings, warning)
}
