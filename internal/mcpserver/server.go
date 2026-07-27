// Package mcpserver exposes RepoKarta's deterministic, read-only code tools.
package mcpserver

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spolnik/RepoKarta/internal/access"
	"github.com/spolnik/RepoKarta/internal/agent"
	"github.com/spolnik/RepoKarta/internal/codeintel"
	"github.com/spolnik/RepoKarta/internal/contextscope"
	"github.com/spolnik/RepoKarta/internal/dependencies"
	"github.com/spolnik/RepoKarta/internal/docs"
	"github.com/spolnik/RepoKarta/internal/graph"
	"github.com/spolnik/RepoKarta/internal/insights"
)

const (
	maxCitations = 12
)

// Config controls the local MCP endpoint.
type Config struct {
	Version       string
	BaseURL       string
	Token         string
	Artifacts     ArtifactReader
	Insights      InsightReader
	Dependencies  DependencyReader
	ResolveViewer func(context.Context, string) (access.Viewer, error)
	AllowUnscoped func() bool
}

// DependencyReader joins mutable, timestamped observations onto immutable
// dependency artifacts without contacting external services on an MCP read.
type DependencyReader interface {
	Findings(context.Context, graph.Snapshot, dependencies.AdvisoryOptions) (dependencies.FindingResponse, error)
	Topology(context.Context, graph.Snapshot, graph.ArtifactProgress, dependencies.TopologyOptions) (dependencies.Topology, error)
}

// InsightReader exposes normalized evidence without invoking AI or scanners.
type InsightReader interface {
	Query(context.Context, insights.Filter) (insights.QueryResponse, error)
	Compare(context.Context, int64, string, string) (insights.Comparison, error)
	EvaluateThresholds(context.Context, int64) ([]insights.ThresholdEvaluation, error)
}

// Intelligence is the shared surface implemented by both the in-process
// service and the JSON API client.
type Intelligence interface {
	Repositories(context.Context) (codeintel.RepositoryList, error)
	ListNamedContexts(context.Context) (contextscope.NamedContextList, error)
	ResolveEffectiveContexts(context.Context, contextscope.EffectiveRequest) (contextscope.EffectiveResponse, error)
	Search(context.Context, codeintel.SearchRequest) (codeintel.SearchResponse, error)
	FindSymbol(context.Context, codeintel.SymbolRequest) (codeintel.SymbolResponse, error)
	FindReferences(context.Context, codeintel.ReferenceRequest) (codeintel.ReferenceResponse, error)
	SearchAST(context.Context, codeintel.ASTSearchRequest) (codeintel.ASTSearchResponse, error)
	GetFile(context.Context, codeintel.FileRequest) (codeintel.FileResponse, error)
	ListTree(context.Context, codeintel.TreeRequest) (codeintel.TreeResponse, error)
	GitLog(context.Context, codeintel.GitLogRequest) (codeintel.GitLogResponse, error)
	GitDiff(context.Context, codeintel.GitDiffRequest) (codeintel.GitDiffResponse, error)
}

// ArtifactReader exposes the higher-level, evidence-backed M3/M4 artifacts.
type ArtifactReader interface {
	RepositoryMap(context.Context, int64) (graph.Snapshot, error)
	DependencySnapshot(context.Context, int64) (graph.Snapshot, error)
	TopologySnapshot(context.Context, int64) (graph.Snapshot, graph.ArtifactProgress, error)
	GeneratedDocuments(context.Context, int64) (docs.Site, error)
	GeneratedDocument(context.Context, int64, string) (docs.Page, error)
}

// MapReader supplies commit-pinned structural maps.
type MapReader interface {
	Snapshot(context.Context, int64, bool) (graph.Snapshot, error)
	ReadDependencySnapshot(context.Context, int64) (graph.Snapshot, graph.ArtifactProgress, error)
	ReadTopologySnapshot(context.Context, int64) (graph.Snapshot, graph.ArtifactProgress, error)
}

// DocumentReader supplies generated repository pages.
type DocumentReader interface {
	Plan(context.Context, int64) (docs.Site, error)
	Page(context.Context, int64, string) (docs.Page, error)
}

// Artifacts adapts in-process map and documentation services to MCP tools.
type Artifacts struct {
	Maps      MapReader
	Documents DocumentReader
}

// RepositoryMap reads one cached or deterministically generated repository map.
func (a Artifacts) RepositoryMap(ctx context.Context, repositoryID int64) (graph.Snapshot, error) {
	return a.Maps.Snapshot(ctx, repositoryID, false)
}

// DependencySnapshot reads the cache-first dependency artifact path. A cold
// fleet build remains explicit in the returned snapshot rather than running a
// full synchronous graph build inside an MCP request.
func (a Artifacts) DependencySnapshot(ctx context.Context, repositoryID int64) (graph.Snapshot, error) {
	snapshot, _, err := a.Maps.ReadDependencySnapshot(ctx, repositoryID)
	return snapshot, err
}

func (a Artifacts) TopologySnapshot(
	ctx context.Context,
	repositoryID int64,
) (graph.Snapshot, graph.ArtifactProgress, error) {
	return a.Maps.ReadTopologySnapshot(ctx, repositoryID)
}

// GeneratedDocuments reads the persisted documentation plan and page metadata.
func (a Artifacts) GeneratedDocuments(ctx context.Context, repositoryID int64) (docs.Site, error) {
	return a.Documents.Plan(ctx, repositoryID)
}

// GeneratedDocument reads one persisted generated page.
func (a Artifacts) GeneratedDocument(ctx context.Context, repositoryID int64, slug string) (docs.Page, error) {
	return a.Documents.Page(ctx, repositoryID, slug)
}

// NewToken generates an unguessable bearer token for the loopback MCP endpoint.
func NewToken() (string, error) {
	var bytes [32]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}

// CitationTracker records exact source URLs returned by MCP tools.
type CitationTracker struct {
	mu       sync.Mutex
	byThread map[string]map[string]agent.Citation
}

// NewCitationTracker constructs an empty citation recorder.
func NewCitationTracker() *CitationTracker {
	return &CitationTracker{byThread: make(map[string]map[string]agent.Citation)}
}

// Record adds one exact citation to a conversation.
func (t *CitationTracker) Record(conversationID string, citation agent.Citation) {
	if t == nil || conversationID == "" || citation.URL == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.byThread[conversationID] == nil {
		t.byThread[conversationID] = make(map[string]agent.Citation)
	}
	t.byThread[conversationID][citation.URL] = citation
}

// List returns a stable copy of citations observed for a conversation.
func (t *CitationTracker) List(conversationID string) []agent.Citation {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	values := make([]agent.Citation, 0, len(t.byThread[conversationID]))
	for _, citation := range t.byThread[conversationID] {
		values = append(values, citation)
	}
	sort.Slice(values, func(left, right int) bool {
		return values[left].URL < values[right].URL
	})
	if len(values) > maxCitations {
		values = values[:maxCitations]
	}
	return values
}

// Clear discards citations for one ephemeral turn.
func (t *CitationTracker) Clear(conversationID string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	delete(t.byThread, conversationID)
	t.mu.Unlock()
}

// NewHandler builds an authenticated Streamable HTTP MCP server.
func NewHandler(config Config, intelligence Intelligence, trackers ...*CitationTracker) http.Handler {
	var tracker *CitationTracker
	if len(trackers) > 0 {
		tracker = trackers[0]
	}
	handler := mcp.NewStreamableHTTPHandler(func(request *http.Request) *mcp.Server {
		return newServer(config, intelligence, tracker, request.URL.Query().Get("conversation_id"))
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	return bearerAuth(config, handler)
}

func newServer(config Config, intelligence Intelligence, tracker *CitationTracker, conversationID string) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "repokarta",
		Title:   "RepoKarta local code intelligence",
		Version: config.Version,
	}, nil)
	readOnly := &mcp.ToolAnnotations{
		ReadOnlyHint:   true,
		IdempotentHint: true,
		OpenWorldHint:  boolPointer(false),
	}

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_repositories",
		Title:       "List indexed repositories",
		Description: "List the local Git repositories RepoKarta can search. Every repository-specific tool takes the numeric repository_id returned here. Results include pinned indexed commits.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ listRepositoriesInput) (*mcp.CallToolResult, listRepositoriesOutput, error) {
		repositories, err := intelligence.Repositories(ctx)
		if err != nil {
			return nil, listRepositoriesOutput{}, err
		}
		return nil, repositories, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_named_contexts",
		Title:       "List reusable search contexts",
		Description: "List permission-checked personal and administrator-published search contexts. Each definition exposes its exact repository revisions, default scope, canonical URL, and current validity.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ listNamedContextsInput) (*mcp.CallToolResult, listNamedContextsOutput, error) {
		contexts, err := intelligence.ListNamedContexts(ctx)
		if err != nil {
			return nil, listNamedContextsOutput{}, err
		}
		return nil, contexts, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "resolve_effective_contexts",
		Title:       "Resolve effective search context",
		Description: "Expand explicit selectors, selected named contexts, and personal/administrator defaults into the exact fail-closed context set a search will use. Every item includes provenance and a canonical copyable URL.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input resolveEffectiveContextsInput) (*mcp.CallToolResult, resolveEffectiveContextsOutput, error) {
		result, err := intelligence.ResolveEffectiveContexts(ctx, contextscope.EffectiveRequest(input))
		if err != nil {
			return nil, resolveEffectiveContextsOutput{}, err
		}
		return nil, result, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_code",
		Title:       "Search indexed code",
		Description: "Search permission-filtered source and deterministic evidence. Prefer literal compact search for globally unique text and fleet discovery; use find_references when syntax precision matters, then get_file only for selected evidence. Completeness, parsed query provenance, warnings, and pinned URLs are explicit.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input searchCodeInput) (*mcp.CallToolResult, searchCodeOutput, error) {
		result, err := intelligence.Search(ctx, codeintel.SearchRequest{
			Query:              input.Query,
			RepositoryID:       input.RepositoryID,
			Language:           input.Language,
			Path:               input.Path,
			File:               input.File,
			Mode:               input.Mode,
			Limit:              input.Limit,
			Compact:            input.Compact,
			Contexts:           input.Contexts,
			NamedContextIDs:    input.NamedContextIDs,
			UseDefaultContexts: input.UseDefaultContexts,
		})
		if err != nil {
			return nil, searchCodeOutput{}, err
		}
		for _, match := range result.Matches {
			tracker.Record(conversationID, agent.Citation{Label: match.Citation, URL: match.SourceURL})
		}
		for _, item := range result.Items {
			if item.Citation != "" && item.SourceURL != "" {
				tracker.Record(conversationID, agent.Citation{Label: item.Citation, URL: item.SourceURL})
			}
		}
		return nil, result, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_file",
		Title:       "Get committed source",
		Description: "Read a bounded line range from a repository at its pinned indexed commit. Returns numbered source and a citation URL.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input openFileInput) (*mcp.CallToolResult, openFileOutput, error) {
		file, err := intelligence.GetFile(ctx, codeintel.FileRequest{
			RepositoryID: input.RepositoryID,
			Revision:     input.Revision,
			Path:         input.Path,
			StartLine:    input.StartLine,
			EndLine:      input.EndLine,
		})
		if err != nil {
			return nil, openFileOutput{}, err
		}
		tracker.Record(conversationID, agent.Citation{Label: file.Citation, URL: file.SourceURL})
		return nil, file, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "find_symbol",
		Title:       "Find indexed symbols",
		Description: "Find symbol definitions by exact name through the Zoekt/ctags index. Results are bounded, commit-pinned, and include explicit warnings when Universal Ctags symbol indexing is unavailable. Use find_references for syntax-backed call, import, and heritage sites.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input findSymbolInput) (*mcp.CallToolResult, findSymbolOutput, error) {
		result, err := intelligence.FindSymbol(ctx, codeintel.SymbolRequest{
			Symbol:             input.Symbol,
			RepositoryID:       input.RepositoryID,
			Language:           input.Language,
			Limit:              input.Limit,
			Compact:            input.Compact,
			Contexts:           input.Contexts,
			NamedContextIDs:    input.NamedContextIDs,
			UseDefaultContexts: input.UseDefaultContexts,
		})
		if err != nil {
			return nil, findSymbolOutput{}, err
		}
		for _, match := range result.Matches {
			tracker.Record(conversationID, agent.Citation{Label: match.Citation, URL: match.SourceURL})
		}
		return nil, result, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "find_references",
		Title:       "Find semantic or AST references",
		Description: "Find commit-pinned references from compiler-produced SCIP when complete exact-revision coverage resolves one symbol, otherwise from labeled persisted AST relations. A unique literal is cheaper through compact search_code. Set compact for fleet discovery without reopening every matched source blob, then use get_file selectively.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input findReferencesInput) (*mcp.CallToolResult, findReferencesOutput, error) {
		result, err := intelligence.FindReferences(ctx, codeintel.ReferenceRequest{
			Symbol:             input.Symbol,
			RepositoryID:       input.RepositoryID,
			Language:           input.Language,
			Path:               input.Path,
			File:               input.File,
			Limit:              input.Limit,
			Compact:            input.Compact,
			Contexts:           input.Contexts,
			NamedContextIDs:    input.NamedContextIDs,
			UseDefaultContexts: input.UseDefaultContexts,
		})
		if err != nil {
			return nil, findReferencesOutput{}, err
		}
		for _, match := range result.Matches {
			tracker.Record(conversationID, agent.Citation{Label: match.Citation, URL: match.SourceURL})
		}
		return nil, result, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_ast",
		Title:       "Search source structure",
		Description: "Run a bounded Tree-sitter query with named captures and predicates over Java or Go. Persisted node-kind inventories prune impossible files; cursors, index readiness, truncation, and completeness are explicit.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input searchASTInput) (*mcp.CallToolResult, searchASTOutput, error) {
		result, err := intelligence.SearchAST(ctx, codeintel.ASTSearchRequest(input))
		if err != nil {
			return nil, searchASTOutput{}, err
		}
		for _, match := range result.Matches {
			tracker.Record(conversationID, agent.Citation{Label: match.Citation, URL: match.SourceURL})
		}
		return nil, result, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_tree",
		Title:       "List repository tree",
		Description: "List files and directories at a repository's pinned indexed commit. Follow next_offset to traverse directories larger than one page. This never reads the worktree.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input listTreeInput) (*mcp.CallToolResult, listTreeOutput, error) {
		tree, err := intelligence.ListTree(ctx, codeintel.TreeRequest{
			RepositoryID: input.RepositoryID,
			Revision:     input.Revision,
			Path:         input.Path,
			Offset:       input.Offset,
		})
		if err != nil {
			return nil, listTreeOutput{}, err
		}
		return nil, tree, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "git_log",
		Title:       "Read repository history",
		Description: "List newest-first commits reachable from a repository's pinned indexed or HEAD commit. Optionally filter history to one path. Returns exact commit SHAs and explicit truncation metadata.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input gitLogInput) (*mcp.CallToolResult, gitLogOutput, error) {
		history, err := intelligence.GitLog(ctx, codeintel.GitLogRequest{
			RepositoryID: input.RepositoryID,
			Revision:     input.Revision,
			Path:         input.Path,
			Limit:        input.Limit,
		})
		if err != nil {
			return nil, gitLogOutput{}, err
		}
		return nil, history, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "git_diff",
		Title:       "Read a historical diff",
		Description: "Read a bounded unified patch between exact reachable commits. Omit to_revision for the indexed commit; omit from_revision to compare its first parent. Returns change counts and explicit patch truncation metadata.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input gitDiffInput) (*mcp.CallToolResult, gitDiffOutput, error) {
		diff, err := intelligence.GitDiff(ctx, codeintel.GitDiffRequest{
			RepositoryID: input.RepositoryID,
			FromRevision: input.FromRevision,
			ToRevision:   input.ToRevision,
			Path:         input.Path,
			ContextLines: input.ContextLines,
		})
		if err != nil {
			return nil, gitDiffOutput{}, err
		}
		return nil, diff, nil
	})

	if config.Artifacts != nil {
		mcp.AddTool(server, &mcp.Tool{
			Name:        "read_repository_map",
			Title:       "Read repository map",
			Description: "Read the complete deterministic, commit-pinned repository snapshot: languages, manifests and resolved dependency coordinates, parsed structure, packages, entry points, routes, architecture edges, and HTTP service calls. Scope and truncation are explicit, every graph fact has exact source evidence, and no AI is invoked.",
			Annotations: readOnly,
		}, func(ctx context.Context, _ *mcp.CallToolRequest, input readRepositoryMapInput) (*mcp.CallToolResult, readRepositoryMapOutput, error) {
			snapshot, err := config.Artifacts.RepositoryMap(ctx, input.RepositoryID)
			if err != nil {
				return nil, readRepositoryMapOutput{}, err
			}
			for _, node := range snapshot.Nodes {
				for _, evidence := range node.Evidence {
					tracker.Record(conversationID, agent.Citation{
						Label: evidence.Repository + "@" + shortRevision(evidence.Revision) + ":" + evidence.Path,
						URL:   evidence.URL,
					})
				}
			}
			return nil, snapshot, nil
		})

		mcp.AddTool(server, &mcp.Tool{
			Name:        "read_dependency_inventory",
			Title:       "Read dependency inventory",
			Description: "Read a compact, deterministic dependency inventory for one repository. Returns manifests, flattened declared coordinates with versions when statically available, outbound HTTP service calls, exact evidence, and explicit scope/truncation metadata. No AI is invoked.",
			Annotations: readOnly,
		}, func(ctx context.Context, _ *mcp.CallToolRequest, input readDependencyInventoryInput) (*mcp.CallToolResult, readDependencyInventoryOutput, error) {
			snapshot, err := config.Artifacts.DependencySnapshot(ctx, input.RepositoryID)
			if err != nil {
				return nil, readDependencyInventoryOutput{}, err
			}
			output := dependencyInventory(snapshot, input.RepositoryID)
			for _, manifest := range output.Manifests {
				recordEvidence(tracker, conversationID, manifest.Evidence)
			}
			for _, call := range output.ServiceCalls {
				for _, evidence := range call.Evidence {
					recordEvidence(tracker, conversationID, evidence)
				}
			}
			return nil, output, nil
		})

		mcp.AddTool(server, &mcp.Tool{
			Name:        "list_deep_wiki_pages",
			Title:       "List Deep Wiki pages",
			Description: "List the persisted Deep Wiki plan and page metadata for one repository, including slugs, hierarchy, generation status, revisions, models, supporting files, and evidence counts. Use a returned slug with read_generated_document. This never starts AI generation and omits page Markdown.",
			Annotations: readOnly,
		}, func(ctx context.Context, _ *mcp.CallToolRequest, input listDeepWikiPagesInput) (*mcp.CallToolResult, listDeepWikiPagesOutput, error) {
			site, err := config.Artifacts.GeneratedDocuments(ctx, input.RepositoryID)
			if err != nil {
				return nil, listDeepWikiPagesOutput{}, err
			}
			return nil, deepWikiIndex(site), nil
		})

		mcp.AddTool(server, &mcp.Tool{
			Name:        "read_generated_document",
			Title:       "Read generated documentation",
			Description: "Read one persisted Deep Wiki page by slug with its source revision, generation status, supporting files, exact citations, and Markdown. This never starts AI generation.",
			Annotations: readOnly,
		}, func(ctx context.Context, _ *mcp.CallToolRequest, input readGeneratedDocumentInput) (*mcp.CallToolResult, readGeneratedDocumentOutput, error) {
			page, err := config.Artifacts.GeneratedDocument(ctx, input.RepositoryID, input.Page)
			if err != nil {
				return nil, readGeneratedDocumentOutput{}, err
			}
			for _, evidence := range page.Citations {
				tracker.Record(conversationID, agent.Citation{
					Label: evidence.Repository + "@" + shortRevision(evidence.Revision) + ":" + evidence.Path,
					URL:   evidence.URL,
				})
			}
			return nil, page, nil
		})
	}

	if config.Artifacts != nil && config.Dependencies != nil {
		mcp.AddTool(server, &mcp.Tool{
			Name:        "read_system_topology",
			Title:       "Read distributed system topology",
			Description: "Read a directed component-level dependency graph across one repository or the visible fleet. Returns HTTP, gRPC, Kafka, database, MCP, and declared relationships; static source evidence and timestamped runtime observations remain distinct, with confirmed/static-only/runtime-only drift states and explicit unresolved peers. No external service is contacted.",
			Annotations: readOnly,
		}, func(ctx context.Context, _ *mcp.CallToolRequest, input readSystemTopologyInput) (*mcp.CallToolResult, dependencies.Topology, error) {
			options, err := topologyToolOptions(input)
			if err != nil {
				return nil, dependencies.Topology{}, err
			}
			snapshot, progress, err := config.Artifacts.TopologySnapshot(ctx, input.RepositoryID)
			if err != nil {
				return nil, dependencies.Topology{}, err
			}
			output, err := config.Dependencies.Topology(ctx, snapshot, progress, options)
			if err != nil {
				return nil, dependencies.Topology{}, err
			}
			for _, connection := range output.Connections {
				for _, evidence := range connection.Evidence {
					recordEvidence(tracker, conversationID, evidence)
				}
			}
			return nil, output, nil
		})

		mcp.AddTool(server, &mcp.Tool{
			Name:        "read_dependency_findings",
			Title:       "Read dependency advisory findings",
			Description: "Read compact, scope-aware OSV findings from the persisted local advisory snapshot. Returns IDs, versions, severity, usage, and evidence citations without advisory prose bodies. Findings are advisory evidence, never an enforced CI gate.",
			Annotations: readOnly,
		}, func(ctx context.Context, _ *mcp.CallToolRequest, input readDependencyFindingsInput) (*mcp.CallToolResult, readDependencyFindingsOutput, error) {
			if input.Limit < 0 || input.Limit > dependencies.MaximumFindingLimit {
				return nil, readDependencyFindingsOutput{}, fmt.Errorf(
					"limit must be between 1 and %d", dependencies.MaximumFindingLimit,
				)
			}
			snapshot, err := config.Artifacts.DependencySnapshot(ctx, input.RepositoryID)
			if err != nil {
				return nil, readDependencyFindingsOutput{}, err
			}
			result, err := config.Dependencies.Findings(ctx, snapshot, dependencies.AdvisoryOptions{
				Query: input.Query, Ecosystem: input.Ecosystem, Severity: input.Severity,
				Usage: input.Usage, Package: input.Package, Limit: input.Limit,
			})
			if err != nil {
				return nil, readDependencyFindingsOutput{}, err
			}
			output := compactDependencyFindings(result)
			for _, finding := range output.Findings {
				for _, evidence := range finding.Evidence {
					if tracker != nil && evidence.URL != "" {
						tracker.Record(conversationID, agent.Citation{
							Label: evidence.Label,
							URL:   evidence.URL,
						})
					}
				}
			}
			return nil, output, nil
		})
	}

	if config.Insights != nil {
		mcp.AddTool(server, &mcp.Tool{
			Name:        "read_code_insights",
			Title:       "Read normalized code insights",
			Description: "Read already-computed coverage metrics, static-analysis findings, deterministic indicators, run status, history, facets, and advisory threshold evaluations. Results are commit-pinned and permission-aware. This never invokes AI, tests, scanners, or repository code.",
			Annotations: readOnly,
		}, func(ctx context.Context, _ *mcp.CallToolRequest, input readCodeInsightsInput) (*mcp.CallToolResult, readCodeInsightsOutput, error) {
			result, err := config.Insights.Query(ctx, insights.Filter{
				RepositoryID:       input.RepositoryID,
				Revision:           input.Revision,
				Branch:             input.Branch,
				Directory:          input.Directory,
				File:               input.File,
				Language:           input.Language,
				Tool:               input.Tool,
				Rule:               input.Rule,
				Severity:           input.Severity,
				Owner:              input.Owner,
				Kind:               input.Kind,
				Limit:              input.Limit,
				IncludeQuarantined: input.IncludeQuarantined,
			})
			if err != nil {
				return nil, readCodeInsightsOutput{}, err
			}
			for _, observation := range result.Current {
				if observation.SourceURL != "" {
					tracker.Record(conversationID, agent.Citation{
						Label: observation.Repository + "@" + shortRevision(observation.Revision) + ":" + observation.Path,
						URL:   observation.SourceURL,
					})
				}
			}
			evaluations, err := config.Insights.EvaluateThresholds(ctx, input.RepositoryID)
			if err != nil {
				return nil, readCodeInsightsOutput{}, err
			}
			return nil, readCodeInsightsOutput{QueryResponse: result, Thresholds: evaluations}, nil
		})

		mcp.AddTool(server, &mcp.Tool{
			Name:        "compare_code_insights",
			Title:       "Compare code insight revisions",
			Description: "Compare two exact stored revisions for metric deltas and introduced or resolved findings. A missing side remains explicit; RepoKarta does not manufacture measurements or enforce a CI gate.",
			Annotations: readOnly,
		}, func(ctx context.Context, _ *mcp.CallToolRequest, input compareCodeInsightsInput) (*mcp.CallToolResult, compareCodeInsightsOutput, error) {
			result, err := config.Insights.Compare(ctx, input.RepositoryID, input.FromRevision, input.ToRevision)
			if err != nil {
				return nil, compareCodeInsightsOutput{}, err
			}
			for _, observation := range append(append([]insights.Observation{}, result.Introduced...), result.Resolved...) {
				if observation.SourceURL != "" {
					tracker.Record(conversationID, agent.Citation{
						Label: observation.Repository + "@" + shortRevision(observation.Revision) + ":" + observation.Path,
						URL:   observation.SourceURL,
					})
				}
			}
			return nil, result, nil
		})
	}

	return server
}

// RunStdio serves the same tools over stdio, normally backed by the JSON API.
func RunStdio(ctx context.Context, config Config, intelligence Intelligence) error {
	if artifacts, ok := intelligence.(ArtifactReader); ok {
		config.Artifacts = artifacts
	}
	err := newServer(config, intelligence, nil, "").Run(ctx, &mcp.StdioTransport{})
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

type listRepositoriesInput struct{}
type listRepositoriesOutput = codeintel.RepositoryList

type listNamedContextsInput struct{}
type listNamedContextsOutput = contextscope.NamedContextList

type resolveEffectiveContextsInput contextscope.EffectiveRequest
type resolveEffectiveContextsOutput = contextscope.EffectiveResponse

type searchCodeInput struct {
	Query              string                  `json:"query" jsonschema:"required,Source or deterministic evidence text using fields content repository revision language path file symbol_kind result_type and owner; prefix a field with minus to exclude it. Result types are content file_path repository symbol_definition reference implementation dependency route commit diff wiki_page and code_insight."`
	RepositoryID       int64                   `json:"repository_id,omitempty" jsonschema:"Optional repository ID returned by list_repositories. Omit to search every indexed repository."`
	Language           string                  `json:"language,omitempty" jsonschema:"Optional programming language filter."`
	Path               string                  `json:"path,omitempty" jsonschema:"Optional substring required in the path."`
	File               string                  `json:"file,omitempty" jsonschema:"Optional substring required in the filename."`
	Mode               string                  `json:"mode,omitempty" jsonschema:"Search mode: literal regex zoekt or references. References uses persisted AST relations."`
	Limit              int                     `json:"limit,omitempty" jsonschema:"Maximum files to return from 1 to 500. Defaults to 100."`
	Compact            bool                    `json:"compact,omitempty" jsonschema:"Return compact discovery evidence: repositories revisions paths line numbers citations and typed reference metadata without snippet bodies ranking facets or actions."`
	Contexts           []contextscope.Selector `json:"contexts,omitempty" jsonschema:"Optional structured repository file directory or symbol contexts. Each uses a stable repository ID and pinned revision; path and symbol identity fields are required by their context kind."`
	NamedContextIDs    []string                `json:"named_context_ids,omitempty" jsonschema:"Optional IDs returned by list_named_contexts. Definitions are permission rechecked and expanded at their pinned revisions."`
	UseDefaultContexts *bool                   `json:"use_default_contexts,omitempty" jsonschema:"Apply personal and administrator defaults. Defaults to true; set false for an explicitly unscoped search."`
}
type searchCodeOutput = codeintel.SearchResponse

type readCodeInsightsInput struct {
	RepositoryID       int64  `json:"repository_id,omitempty" jsonschema:"Optional repository ID. Omit for the accessible fleet."`
	Revision           string `json:"revision,omitempty" jsonschema:"Optional exact analyzed Git revision."`
	Branch             string `json:"branch,omitempty" jsonschema:"Optional reported branch."`
	Directory          string `json:"directory,omitempty" jsonschema:"Optional repository-relative directory prefix."`
	File               string `json:"file,omitempty" jsonschema:"Optional path substring."`
	Language           string `json:"language,omitempty" jsonschema:"Optional language filter."`
	Tool               string `json:"tool,omitempty" jsonschema:"Optional exact tool name."`
	Rule               string `json:"rule,omitempty" jsonschema:"Optional exact rule or metric key."`
	Severity           string `json:"severity,omitempty" jsonschema:"Optional normalized severity."`
	Owner              string `json:"owner,omitempty" jsonschema:"Optional owner filter."`
	Kind               string `json:"kind,omitempty" jsonschema:"Optional kind: metric or finding."`
	Limit              int    `json:"limit,omitempty" jsonschema:"Maximum observations from 1 to 5000."`
	IncludeQuarantined bool   `json:"include_quarantined,omitempty" jsonschema:"Include reports that do not reconcile to the indexed revision."`
}

type readCodeInsightsOutput struct {
	insights.QueryResponse
	Thresholds []insights.ThresholdEvaluation `json:"threshold_evaluations,omitempty"`
}

type compareCodeInsightsInput struct {
	RepositoryID int64  `json:"repository_id" jsonschema:"required,Repository ID returned by list_repositories."`
	FromRevision string `json:"from_revision" jsonschema:"required,Exact baseline revision already present in insight history."`
	ToRevision   string `json:"to_revision" jsonschema:"required,Exact target revision already present in insight history."`
}

type compareCodeInsightsOutput = insights.Comparison

type readRepositoryMapInput struct {
	RepositoryID int64 `json:"repository_id" jsonschema:"required,Repository ID returned by list_repositories."`
}

type readRepositoryMapOutput = graph.Snapshot

type readDependencyInventoryInput struct {
	RepositoryID int64 `json:"repository_id" jsonschema:"required,Repository ID returned by list_repositories."`
}

type readDependencyFindingsInput struct {
	RepositoryID int64  `json:"repository_id,omitempty" jsonschema:"Optional repository ID returned by list_repositories. Omit for the accessible fleet."`
	Query        string `json:"query,omitempty" jsonschema:"Optional advisory ID, alias, package, repository, or manifest substring."`
	Ecosystem    string `json:"ecosystem,omitempty" jsonschema:"Optional ecosystem: maven, npm, pypi, go, cargo, or nuget."`
	Severity     string `json:"severity,omitempty" jsonschema:"Optional severity: critical, high, medium, low, or unknown."`
	Usage        string `json:"usage,omitempty" jsonschema:"Optional inventory usage such as production, implementation, test, development, or build."`
	Package      string `json:"package,omitempty" jsonschema:"Optional package-name substring."`
	Limit        int    `json:"limit,omitempty" jsonschema:"Maximum compact findings from 1 to 500. Defaults to 100."`
}

type compactDependencyEvidence struct {
	Kind            string `json:"kind"`
	Label           string `json:"label"`
	URL             string `json:"url"`
	Revision        string `json:"revision,omitempty"`
	SnapshotVersion string `json:"snapshot_version,omitempty"`
}

type compactDependencyFinding struct {
	ID              string                      `json:"id"`
	AdvisoryID      string                      `json:"advisory_id"`
	Aliases         []string                    `json:"aliases,omitempty"`
	Ecosystem       string                      `json:"ecosystem"`
	Package         string                      `json:"package"`
	Version         string                      `json:"version"`
	Severity        string                      `json:"severity"`
	Usage           string                      `json:"usage"`
	DeclaredScope   string                      `json:"declared_scope,omitempty"`
	MatchBasis      string                      `json:"match_basis"`
	MatchConfidence string                      `json:"match_confidence"`
	FixedVersion    string                      `json:"fixed_version,omitempty"`
	LatestStable    string                      `json:"latest_stable,omitempty"`
	RepositoryID    int64                       `json:"repository_id"`
	Repository      string                      `json:"repository"`
	Revision        string                      `json:"revision"`
	ManifestPath    string                      `json:"manifest_path"`
	Evidence        []compactDependencyEvidence `json:"evidence"`
}

type readDependencyFindingsOutput struct {
	CheckState                 string                              `json:"check_state"`
	CheckMessage               string                              `json:"check_message,omitempty"`
	AdvisoryOnly               bool                                `json:"advisory_only"`
	Snapshot                   dependencies.AdvisorySnapshotStatus `json:"snapshot"`
	CheckedDeclarationCount    int                                 `json:"checked_declaration_count"`
	SkippedNoVersionCount      int                                 `json:"skipped_no_version_count"`
	SkippedInvalidVersionCount int                                 `json:"skipped_invalid_version_count"`
	NotInSnapshotCount         int                                 `json:"not_in_snapshot_count"`
	UncoveredEcosystems        []dependencies.AdvisoryGap          `json:"uncovered_ecosystems,omitempty"`
	SkippedDeclarations        []dependencies.DependencyCheckGap   `json:"skipped_declarations,omitempty"`
	TotalFindingCount          int                                 `json:"total_finding_count"`
	ReturnedCount              int                                 `json:"returned_count"`
	HasMore                    bool                                `json:"has_more"`
	Findings                   []compactDependencyFinding          `json:"findings"`
}

type readSystemTopologyInput struct {
	RepositoryID int64  `json:"repository_id,omitempty" jsonschema:"Optional repository ID returned by list_repositories. Omit for the bounded visible fleet."`
	Query        string `json:"query,omitempty" jsonschema:"Optional component connection protocol or state substring."`
	Protocol     string `json:"protocol,omitempty" jsonschema:"Optional protocol: http grpc kafka database mcp amqp or unknown."`
	Origin       string `json:"origin,omitempty" jsonschema:"Optional evidence filter: static runtime or confirmed."`
	Environment  string `json:"environment,omitempty" jsonschema:"Optional exact runtime environment such as prod."`
	Provider     string `json:"provider,omitempty" jsonschema:"Optional exact runtime provider such as tempo or datadog."`
	ObservedFrom string `json:"observed_from,omitempty" jsonschema:"Optional RFC3339 runtime window start. Defaults to 24 hours before observed_to."`
	ObservedTo   string `json:"observed_to,omitempty" jsonschema:"Optional RFC3339 runtime window end. Defaults to now."`
}

func topologyToolOptions(input readSystemTopologyInput) (dependencies.TopologyOptions, error) {
	options := dependencies.TopologyOptions{
		Query: strings.TrimSpace(input.Query), Protocol: strings.ToLower(strings.TrimSpace(input.Protocol)),
		Origin:      strings.ToLower(strings.TrimSpace(input.Origin)),
		Environment: strings.TrimSpace(input.Environment), Provider: strings.TrimSpace(input.Provider),
	}
	for _, timestamp := range []struct {
		value  string
		target *time.Time
	}{
		{input.ObservedFrom, &options.ObservedFrom},
		{input.ObservedTo, &options.ObservedTo},
	} {
		value := strings.TrimSpace(timestamp.value)
		if value == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return dependencies.TopologyOptions{}, errors.New("runtime topology window must use RFC3339 timestamps")
		}
		*timestamp.target = parsed.UTC()
	}
	return options, nil
}

type declaredDependency struct {
	Coordinate string           `json:"coordinate"`
	DeclaredIn []string         `json:"declared_in"`
	Evidence   []graph.Evidence `json:"evidence"`
}

type readDependencyInventoryOutput struct {
	RepositoryID     int64                `json:"repository_id"`
	Repository       string               `json:"repository"`
	Revision         string               `json:"revision"`
	ManifestCount    int                  `json:"manifest_count"`
	DependencyCount  int                  `json:"dependency_count"`
	ServiceCallCount int                  `json:"service_call_count"`
	Manifests        []graph.Manifest     `json:"manifests"`
	Dependencies     []declaredDependency `json:"dependencies"`
	ServiceCalls     []graph.Edge         `json:"service_calls"`
	Truncated        bool                 `json:"truncated"`
	Scope            graph.Scope          `json:"scope"`
}

type listDeepWikiPagesInput struct {
	RepositoryID int64 `json:"repository_id" jsonschema:"required,Repository ID returned by list_repositories."`
}

type deepWikiPage struct {
	Slug            string    `json:"slug"`
	Title           string    `json:"title"`
	Summary         string    `json:"summary"`
	Order           int       `json:"order"`
	Number          string    `json:"number"`
	ParentSlug      string    `json:"parent_slug,omitempty"`
	Depth           int       `json:"depth"`
	Status          string    `json:"status"`
	Revision        string    `json:"revision,omitempty"`
	Provider        string    `json:"provider,omitempty"`
	Model           string    `json:"model,omitempty"`
	SupportingFiles []string  `json:"supporting_files"`
	CitationCount   int       `json:"citation_count"`
	UpdatedAt       time.Time `json:"updated_at,omitempty"`
}

type listDeepWikiPagesOutput struct {
	Version      int            `json:"version"`
	RepositoryID int64          `json:"repository_id"`
	Repository   string         `json:"repository"`
	Revision     string         `json:"revision"`
	Profile      string         `json:"profile,omitempty"`
	ProfilePages string         `json:"profile_pages,omitempty"`
	SurveyReady  bool           `json:"survey_ready"`
	SurveyStale  bool           `json:"survey_stale"`
	SurveyStatus string         `json:"survey_status,omitempty"`
	PlanReady    bool           `json:"plan_ready"`
	PlanStale    bool           `json:"plan_stale"`
	PlanRevision string         `json:"plan_revision,omitempty"`
	PlanProvider string         `json:"plan_provider,omitempty"`
	PlanModel    string         `json:"plan_model,omitempty"`
	Ready        int            `json:"ready"`
	Stale        int            `json:"stale"`
	Pending      int            `json:"pending"`
	Failed       int            `json:"failed"`
	Pages        []deepWikiPage `json:"pages"`
}

type readGeneratedDocumentInput struct {
	RepositoryID int64  `json:"repository_id" jsonschema:"required,Repository ID returned by list_repositories."`
	Page         string `json:"page" jsonschema:"required,Page slug from the documentation plan such as overview architecture or dependencies."`
}

type readGeneratedDocumentOutput = docs.Page

type findSymbolInput struct {
	Symbol             string                  `json:"symbol" jsonschema:"required,Exact symbol name to find."`
	RepositoryID       int64                   `json:"repository_id,omitempty" jsonschema:"Optional repository ID returned by list_repositories. Omit to search every indexed repository."`
	Language           string                  `json:"language,omitempty" jsonschema:"Optional programming language filter."`
	Limit              int                     `json:"limit,omitempty" jsonschema:"Maximum files to return from 1 to 500. Defaults to 100."`
	Compact            bool                    `json:"compact,omitempty" jsonschema:"Return paths line numbers and pinned citations without snippet bodies ranking facets or actions."`
	Contexts           []contextscope.Selector `json:"contexts,omitempty" jsonschema:"Optional structured repository file directory or symbol contexts."`
	NamedContextIDs    []string                `json:"named_context_ids,omitempty" jsonschema:"Optional IDs returned by list_named_contexts."`
	UseDefaultContexts *bool                   `json:"use_default_contexts,omitempty" jsonschema:"Apply personal and administrator defaults. Defaults to true."`
}

type findSymbolOutput = codeintel.SymbolResponse

type findReferencesInput struct {
	Symbol             string                  `json:"symbol" jsonschema:"required,Full SCIP symbol identity or exact source-level name. Bare names use SCIP only when unambiguous and otherwise fall back to AST relations."`
	RepositoryID       int64                   `json:"repository_id,omitempty" jsonschema:"Optional repository ID returned by list_repositories. Omit to search every repository covered by the bounded structural snapshot."`
	Language           string                  `json:"language,omitempty" jsonschema:"Optional parser language filter."`
	Path               string                  `json:"path,omitempty" jsonschema:"Optional substring required in the repository-relative path."`
	File               string                  `json:"file,omitempty" jsonschema:"Optional substring required in the filename."`
	Limit              int                     `json:"limit,omitempty" jsonschema:"Maximum files to return from 1 to 500. Defaults to 100."`
	Compact            bool                    `json:"compact,omitempty" jsonschema:"Read only cached structural relations and return paths line numbers citations and relation metadata without reopening source blobs for snippets."`
	Contexts           []contextscope.Selector `json:"contexts,omitempty" jsonschema:"Optional structured repository file directory or symbol contexts."`
	NamedContextIDs    []string                `json:"named_context_ids,omitempty" jsonschema:"Optional IDs returned by list_named_contexts."`
	UseDefaultContexts *bool                   `json:"use_default_contexts,omitempty" jsonschema:"Apply personal and administrator defaults. Defaults to true."`
}

type findReferencesOutput = codeintel.ReferenceResponse

type searchASTInput struct {
	RepositoryID int64  `json:"repository_id,omitempty" jsonschema:"Optional repository ID returned by list_repositories. Omit for the accessible fleet."`
	Language     string `json:"language" jsonschema:"required,Tree-sitter language: java or go."`
	Query        string `json:"query" jsonschema:"required,Tree-sitter S-expression query with named captures and optional predicates such as #eq? #match? #has-parent? or #has-ancestor?."`
	PathPrefix   string `json:"path_prefix,omitempty" jsonschema:"Optional repository-relative directory or exact file prefix."`
	Limit        int    `json:"limit,omitempty" jsonschema:"Maximum pattern matches from 1 to 200. Defaults to 50."`
	Cursor       string `json:"cursor,omitempty" jsonschema:"Opaque next_cursor from the preceding response for the identical query and index."`
}

type searchASTOutput = codeintel.ASTSearchResponse

type openFileInput struct {
	RepositoryID int64  `json:"repository_id" jsonschema:"required,Repository ID returned by list_repositories."`
	Path         string `json:"path" jsonschema:"required,Repository-relative source path."`
	Revision     string `json:"revision,omitempty" jsonschema:"Pinned indexed commit. Omit to use the current indexed commit."`
	StartLine    int    `json:"start_line,omitempty" jsonschema:"First one-based line to return."`
	EndLine      int    `json:"end_line,omitempty" jsonschema:"Last one-based line to return. At most 500 lines."`
}

type openFileOutput = codeintel.FileResponse

type listTreeInput struct {
	RepositoryID int64  `json:"repository_id" jsonschema:"required,Repository ID returned by list_repositories."`
	Path         string `json:"path,omitempty" jsonschema:"Optional repository-relative directory."`
	Revision     string `json:"revision,omitempty" jsonschema:"Pinned indexed commit. Omit to use the current indexed commit."`
	Offset       int    `json:"offset,omitempty" jsonschema:"Zero-based page offset. Use next_offset to traverse directories with more than 500 entries."`
}

type listTreeOutput = codeintel.TreeResponse

type gitLogInput struct {
	RepositoryID int64  `json:"repository_id" jsonschema:"required,Repository ID returned by list_repositories."`
	Revision     string `json:"revision,omitempty" jsonschema:"Exact reachable commit SHA to start from. Omit to use the indexed commit."`
	Path         string `json:"path,omitempty" jsonschema:"Optional repository-relative file or directory whose history should be returned."`
	Limit        int    `json:"limit,omitempty" jsonschema:"Maximum commits to return from 1 to 200. Defaults to 50."`
}

type gitLogOutput = codeintel.GitLogResponse

type gitDiffInput struct {
	RepositoryID int64  `json:"repository_id" jsonschema:"required,Repository ID returned by list_repositories."`
	FromRevision string `json:"from_revision,omitempty" jsonschema:"Exact reachable base commit SHA. Omit to use the first parent of to_revision."`
	ToRevision   string `json:"to_revision,omitempty" jsonschema:"Exact reachable target commit SHA. Omit to use the indexed commit."`
	Path         string `json:"path,omitempty" jsonschema:"Optional repository-relative file or directory to diff."`
	ContextLines int    `json:"context_lines,omitempty" jsonschema:"Unified diff context lines from 1 to 20. Defaults to 3."`
}

type gitDiffOutput = codeintel.GitDiffResponse

func dependencyInventory(snapshot graph.Snapshot, repositoryID int64) readDependencyInventoryOutput {
	output := readDependencyInventoryOutput{
		RepositoryID: repositoryID,
		Manifests:    append([]graph.Manifest(nil), snapshot.Manifests...),
		Dependencies: []declaredDependency{},
		ServiceCalls: []graph.Edge{},
		Truncated:    snapshot.Truncated || snapshot.StructureTruncated,
		Scope:        snapshot.Scope,
	}
	if len(snapshot.Repositories) > 0 {
		output.Repository = snapshot.Repositories[0].Name
		output.Revision = snapshot.Repositories[0].Revision
	}

	evidenceByDeclaration := make(map[string]graph.Evidence)
	for _, edge := range snapshot.Edges {
		if edge.Kind == "service_call" {
			output.ServiceCalls = append(output.ServiceCalls, edge)
		}
		if edge.Kind != "dependency" {
			continue
		}
		for _, evidence := range edge.Evidence {
			evidenceByDeclaration[evidence.Path+"\x00"+evidence.Label] = evidence
		}
	}

	byCoordinate := make(map[string]*declaredDependency)
	for _, manifest := range output.Manifests {
		for _, coordinate := range manifest.Dependencies {
			dependency := byCoordinate[coordinate]
			if dependency == nil {
				dependency = &declaredDependency{
					Coordinate: coordinate,
					DeclaredIn: []string{},
					Evidence:   []graph.Evidence{},
				}
				byCoordinate[coordinate] = dependency
			}
			dependency.DeclaredIn = append(dependency.DeclaredIn, manifest.Path)
			if evidence, ok := evidenceByDeclaration[manifest.Path+"\x00"+coordinate]; ok {
				dependency.Evidence = append(dependency.Evidence, evidence)
			} else {
				dependency.Evidence = append(dependency.Evidence, manifest.Evidence)
			}
		}
	}
	coordinates := make([]string, 0, len(byCoordinate))
	for coordinate := range byCoordinate {
		coordinates = append(coordinates, coordinate)
	}
	sort.Strings(coordinates)
	for _, coordinate := range coordinates {
		dependency := byCoordinate[coordinate]
		sort.Strings(dependency.DeclaredIn)
		output.Dependencies = append(output.Dependencies, *dependency)
	}
	sort.Slice(output.ServiceCalls, func(left, right int) bool {
		if output.ServiceCalls[left].Source != output.ServiceCalls[right].Source {
			return output.ServiceCalls[left].Source < output.ServiceCalls[right].Source
		}
		return output.ServiceCalls[left].Target < output.ServiceCalls[right].Target
	})
	output.ManifestCount = len(output.Manifests)
	output.DependencyCount = len(output.Dependencies)
	output.ServiceCallCount = len(output.ServiceCalls)
	return output
}

func compactDependencyFindings(result dependencies.FindingResponse) readDependencyFindingsOutput {
	output := readDependencyFindingsOutput{
		CheckState: result.CheckState, CheckMessage: result.CheckMessage,
		AdvisoryOnly: result.AdvisoryOnly, Snapshot: result.Snapshot,
		CheckedDeclarationCount:    result.CheckedDeclarationCount,
		SkippedNoVersionCount:      result.SkippedNoVersionCount,
		SkippedInvalidVersionCount: result.SkippedInvalidVersionCount,
		NotInSnapshotCount:         result.NotInSnapshotCount,
		UncoveredEcosystems:        append([]dependencies.AdvisoryGap(nil), result.UncoveredEcosystems...),
		SkippedDeclarations:        append([]dependencies.DependencyCheckGap(nil), result.SkippedDeclarations...),
		TotalFindingCount:          result.TotalFindingCount, ReturnedCount: result.ReturnedCount,
		HasMore: result.HasMore, Findings: make([]compactDependencyFinding, 0, len(result.Findings)),
	}
	for _, finding := range result.Findings {
		output.Findings = append(output.Findings, compactDependencyFinding{
			ID: finding.ID, AdvisoryID: finding.AdvisoryID,
			Aliases:   append([]string(nil), finding.Aliases...),
			Ecosystem: finding.Ecosystem, Package: finding.Package, Version: finding.Version,
			Severity: finding.Severity, Usage: finding.Usage, DeclaredScope: finding.DeclaredScope,
			MatchBasis: finding.MatchBasis, MatchConfidence: finding.MatchConfidence,
			FixedVersion: finding.FixedVersion, LatestStable: finding.LatestStable,
			RepositoryID: finding.RepositoryID, Repository: finding.Repository,
			Revision: finding.Revision, ManifestPath: finding.ManifestPath,
			Evidence: []compactDependencyEvidence{
				{
					Kind:  "manifest",
					Label: finding.Repository + "@" + shortRevision(finding.Revision) + ":" + finding.ManifestPath,
					URL:   finding.ManifestEvidence.URL, Revision: finding.Revision,
				},
				{
					Kind: "advisory_snapshot", Label: finding.AdvisoryID,
					URL:             finding.AdvisoryEvidence.AdvisoryURL,
					SnapshotVersion: finding.AdvisoryEvidence.SnapshotVersion,
				},
			},
		})
	}
	return output
}

func deepWikiIndex(site docs.Site) listDeepWikiPagesOutput {
	output := listDeepWikiPagesOutput{
		Version:      site.Version,
		RepositoryID: site.RepositoryID,
		Repository:   site.Repository,
		Revision:     site.Revision,
		Profile:      site.Profile,
		ProfilePages: site.ProfilePages,
		SurveyReady:  site.SurveyReady,
		SurveyStale:  site.SurveyStale,
		SurveyStatus: site.SurveyStatus,
		PlanReady:    site.PlanReady,
		PlanStale:    site.PlanStale,
		PlanRevision: site.PlanRevision,
		PlanProvider: site.PlanProvider,
		PlanModel:    site.PlanModel,
		Ready:        site.Ready,
		Stale:        site.Stale,
		Pending:      site.Pending,
		Failed:       site.Failed,
		Pages:        make([]deepWikiPage, 0, len(site.Pages)),
	}
	for _, page := range site.Pages {
		output.Pages = append(output.Pages, deepWikiPage{
			Slug:            page.Slug,
			Title:           page.Title,
			Summary:         page.Summary,
			Order:           page.Order,
			Number:          page.Number,
			ParentSlug:      page.ParentSlug,
			Depth:           page.Depth,
			Status:          page.Status,
			Revision:        page.Revision,
			Provider:        page.Provider,
			Model:           page.Model,
			SupportingFiles: append([]string(nil), page.SupportingFiles...),
			CitationCount:   len(page.Citations),
			UpdatedAt:       page.UpdatedAt,
		})
	}
	return output
}

func recordEvidence(tracker *CitationTracker, conversationID string, evidence graph.Evidence) {
	tracker.Record(conversationID, agent.Citation{
		Label: evidence.Repository + "@" + shortRevision(evidence.Revision) + ":" + evidence.Path,
		URL:   evidence.URL,
	})
}

func bearerAuth(config Config, next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		expected := "Bearer " + config.Token
		actual := request.Header.Get("Authorization")
		if config.Token == "" || len(actual) != len(expected) || subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) != 1 {
			response.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(response, "Unauthorized", http.StatusUnauthorized)
			return
		}
		ctx := request.Context()
		if config.ResolveViewer != nil {
			conversationID := strings.TrimSpace(request.URL.Query().Get("conversation_id"))
			if conversationID == "" {
				if config.AllowUnscoped == nil || !config.AllowUnscoped() {
					http.Error(response, "A conversation-scoped MCP endpoint is required in shared mode", http.StatusForbidden)
					return
				}
				ctx = access.WithViewer(ctx, access.Viewer{ID: "local:admin", Admin: true})
			} else {
				viewer, err := config.ResolveViewer(ctx, conversationID)
				if err != nil {
					http.Error(response, "Conversation-scoped MCP authorization failed", http.StatusForbidden)
					return
				}
				ctx = access.WithViewer(ctx, viewer)
			}
		}
		next.ServeHTTP(response, request.WithContext(ctx))
	})
}

func boolPointer(value bool) *bool {
	return &value
}

func shortRevision(value string) string {
	if len(value) > 8 {
		return value[:8]
	}
	return value
}
