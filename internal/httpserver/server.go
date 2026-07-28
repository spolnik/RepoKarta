package httpserver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"path"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/spolnik/RepoKarta/internal/acquisition"
	"github.com/spolnik/RepoKarta/internal/agent"
	"github.com/spolnik/RepoKarta/internal/audit"
	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/codeintel"
	"github.com/spolnik/RepoKarta/internal/contextscope"
	"github.com/spolnik/RepoKarta/internal/dependencies"
	"github.com/spolnik/RepoKarta/internal/docs"
	"github.com/spolnik/RepoKarta/internal/graph"
	"github.com/spolnik/RepoKarta/internal/identity"
	"github.com/spolnik/RepoKarta/internal/insights"
	"github.com/spolnik/RepoKarta/internal/maintenance"
	"github.com/spolnik/RepoKarta/internal/mcpserver"
	"github.com/spolnik/RepoKarta/internal/scipjava"
	"github.com/spolnik/RepoKarta/internal/search"
	"github.com/spolnik/RepoKarta/internal/searchworkspace"
	"github.com/spolnik/RepoKarta/internal/security"
	"github.com/spolnik/RepoKarta/internal/source"
	"github.com/spolnik/RepoKarta/internal/store"
	"github.com/spolnik/RepoKarta/web"
)

const (
	maximumSourceLines      = 500
	maximumSourceRoutes     = 24
	maximumChatRequestBytes = (agent.MaximumImagesPerTurn * agent.MaximumImageBytes * 4 / 3) + (1 << 20)
	eventPollInterval       = time.Second
)

// CatalogueRefresher manually rediscovers and queues repositories.
type CatalogueRefresher interface {
	Refresh(context.Context) error
}

// SCIPJavaService exposes the optional compiler indexer without coupling HTTP
// requests to build execution.
type SCIPJavaService interface {
	ProviderStatus() scipjava.ProviderStatus
	Retry(context.Context, int64) error
}

// ConversationService supplies provider status and streamed read-only turns.
type ConversationService interface {
	Statuses(context.Context) []agent.Status
	Send(context.Context, agent.TurnRequest, func(agent.Event) error) error
	Interrupt(context.Context, string) error
}

type ConversationRetryService interface {
	Retry(context.Context, agent.RetryRequest, func(agent.Event) error) error
}

// ConversationHistoryService supplies durable titled transcripts.
type ConversationHistoryService interface {
	ListConversations(context.Context, agent.ConversationFilter) ([]agent.Conversation, error)
	GetConversation(context.Context, string) (agent.Conversation, error)
	RenameConversation(context.Context, string, string) error
	DeleteConversation(context.Context, string) error
}

// MapService supplies deterministic, commit-pinned repository maps.
type MapService interface {
	Snapshot(context.Context, int64, bool) (graph.Snapshot, error)
	ReadDependencySnapshot(context.Context, int64) (graph.Snapshot, graph.ArtifactProgress, error)
	ReadTopologySnapshot(context.Context, int64) (graph.Snapshot, graph.ArtifactProgress, error)
	ReadRouteSnapshot(context.Context, int64) (graph.Snapshot, graph.ArtifactProgress, error)
	StructureProgress(context.Context, int64) (graph.ArtifactProgress, error)
}

// DependencyService owns cached registry observations and refresh work.
type DependencyService interface {
	Inventory(context.Context, graph.Snapshot, dependencies.Options) (dependencies.Inventory, error)
	StartRefresh(graph.Snapshot, dependencies.Options, bool) (dependencies.RefreshProgress, error)
	Progress() dependencies.RefreshProgress
	Findings(context.Context, graph.Snapshot, dependencies.AdvisoryOptions) (dependencies.FindingResponse, error)
	StartAdvisoryRefresh(graph.Snapshot, bool) (dependencies.AdvisoryRefreshProgress, error)
	AdvisoryProgress() dependencies.AdvisoryRefreshProgress
	Topology(context.Context, graph.Snapshot, graph.ArtifactProgress, dependencies.TopologyOptions) (dependencies.Topology, error)
	ImportTopologyObservations(context.Context, dependencies.TopologyImportRequest) (dependencies.TopologyImportResult, error)
}

// DocumentationService supplies durable, commit-aware repository pages.
type DocumentationService interface {
	Plan(context.Context, int64) (docs.Site, error)
	Generate(context.Context, docs.GenerateRequest) (docs.Site, error)
	Page(context.Context, int64, string) (docs.Page, error)
	Export(context.Context, int64) ([]byte, string, error)
}

// RepositoryAccessService owns administrator-managed repository visibility.
type RepositoryAccessService interface {
	ListRepositoryAccess(context.Context) ([]store.RepositoryAccess, error)
	SetRepositoryAccess(context.Context, store.RepositoryAccess) error
}

// EnterpriseStore supplies M10 identity administration and audit evidence.
type EnterpriseStore interface {
	identity.Store
	audit.Recorder
	AuditEvents(context.Context, audit.Filter) (audit.Page, error)
	AuditRetention(context.Context) (audit.Retention, error)
	SetAuditRetention(context.Context, int, int) (int64, error)
}

// InsightService supplies normalized, already-computed quality evidence.
type InsightService interface {
	Query(context.Context, insights.Filter) (insights.QueryResponse, error)
	Import(context.Context, insights.ImportRequest) (insights.Run, error)
	Derive(context.Context, int64) (insights.Run, error)
	Compare(context.Context, int64, string, string) (insights.Comparison, error)
	Thresholds(context.Context, int64) ([]insights.Threshold, error)
	SetThreshold(context.Context, insights.Threshold) (insights.Threshold, error)
	EvaluateThresholds(context.Context, int64) ([]insights.ThresholdEvaluation, error)
	ConfigureSonar(context.Context, insights.SonarConnection) (insights.SonarConnection, error)
	SonarConnections(context.Context) ([]insights.SonarConnection, error)
	SyncSonar(context.Context, int64) (insights.Run, error)
}

// RepositoryAcquisitionService owns administrator-managed repository intake.
type RepositoryAcquisitionService interface {
	List(context.Context) ([]acquisition.Repository, error)
	Discover(context.Context, acquisition.DiscoverRequest) ([]acquisition.Candidate, error)
	Acquire(context.Context, acquisition.Candidate, string) (acquisition.Repository, error)
	Sync(context.Context, int64) (acquisition.Repository, error)
	Remove(context.Context, int64) (string, error)
}

// Config controls the local HTTP server.
type Config struct {
	Address               string
	RepositoryRoot        string
	Version               string
	OpenBrowser           bool
	MCPHandler            http.Handler
	MCPToken              string
	MCPBaseURL            string
	MCPCommand            string
	Conversations         ConversationService
	ConversationShares    agent.ConversationShareStore
	Maps                  MapService
	Docs                  DocumentationService
	Security              *security.Manager
	Maintenance           *maintenance.Service
	RepositoryAccess      RepositoryAccessService
	RepositoryAcquisition RepositoryAcquisitionService
	Enterprise            EnterpriseStore
	SCIMHandler           http.Handler
	Insights              InsightService
	Dependencies          DependencyService
	SCIPJava              SCIPJavaService
}

// Server hosts RepoKarta's loopback interface.
type Server struct {
	config                Config
	server                *http.Server
	templates             *template.Template
	intelligence          *codeintel.Service
	refresher             CatalogueRefresher
	agents                ConversationService
	history               ConversationHistoryService
	conversationShares    agent.ConversationShareStore
	maps                  MapService
	docs                  DocumentationService
	security              *security.Manager
	maintenance           *maintenance.Service
	repositoryAccess      RepositoryAccessService
	repositoryAcquisition RepositoryAcquisitionService
	enterprise            EnterpriseStore
	insights              InsightService
	dependencies          DependencyService
	scipJava              SCIPJavaService
}

type pageData struct {
	Version        string
	RepositoryRoot string
	Repositories   []catalog.Repository
	// RepositoryLabels disambiguates repositories that share a name so every
	// picker identifies exactly one repository.
	RepositoryLabels    map[int64]string
	ReadyCount          int
	PendingCount        int
	ErrorCount          int
	EmptyCount          int
	IndexableCount      int
	ArtifactProgress    graph.ArtifactProgress
	ActivePage          string
	ChatEnabled         bool
	WikiEnabled         bool
	DependenciesEnabled bool
	MCPEnabled          bool
	InsightsEnabled     bool
	Search              searchData
	AuthMode            string
	UserLabel           string
	AdminEnabled        bool
	CanAdminister       bool
	CanManageArtifacts  bool
	MCP                 mcpPageData
	SCIPJava            scipjava.ProviderStatus
	SCIPJavaEnabled     bool
}

type mcpPageData struct {
	Endpoint       string
	Token          string
	TokenPreview   string
	HTTPConfig     string
	HTTPConfigView string
	StdioConfig    string
	Tools          []mcpToolView
	Shared         bool
}

type mcpToolView struct {
	Name        string
	Description string
}

type searchData struct {
	Query search.Query
	// SelectedRepositoryID keeps the repository filter bound to a stable ID so
	// repositories that share a name stay distinguishable in the picker.
	SelectedRepositoryID int64
	Performed            bool
	Error                string
	Duration             string
	MatchCount           int
	FileCount            int
	EstimatedFiles       int
	ReturnedFiles        int
	ReturnedItems        int
	Limit                int
	TotalFilesExact      bool
	FilesSkipped         int
	ShardsSkipped        int
	Warnings             []search.Warning
	Truncated            bool
	Matches              []searchMatchView
	Items                []codeintel.SearchItem
	Facets               []codeintel.SearchFacet
	FacetCoverage        codeintel.SearchFacetCoverage
	ResultType           string
}

type searchMatchView struct {
	ResultType   string
	RepositoryID int64
	Repository   string
	Revision     string
	Path         string
	Language     string
	FocusLine    int
	Lines        []search.LineMatch
	Ranking      []codeintel.RankingSignal
	Actions      []codeintel.SearchAction
}

type sourcePageData struct {
	Version       string
	ChatEnabled   bool
	File          source.File
	ProjectURL    string
	Breadcrumbs   []projectBreadcrumbView
	TreeEntries   []projectEntryView
	RemoteURL     string
	Citation      string
	PreviousStart int
	PreviousEnd   int
	NextStart     int
	NextEnd       int
	FocusStart    int
	FocusEnd      int
	Intelligence  sourceIntelligenceView
}

type sourceIntelligenceView struct {
	Routes        []sourceRouteView
	RouteCount    int
	OmittedRoutes int
	Callers       []sourceCallerView
	State         string
	Message       string
	Partial       bool
	TopologyURL   string
}

type sourceRouteView struct {
	Label         string
	Line          int
	URL           string
	VisibleWindow bool
	Callers       []sourceCallerView
}

type sourceCallerView struct {
	Name       string
	State      string
	Confidence string
	Evidence   graph.Evidence
}

type projectPageData struct {
	pageData
	Repository  catalog.Repository
	Revision    string
	Path        string
	Breadcrumbs []projectBreadcrumbView
	Entries     []projectEntryView
	PreviousURL string
	NextURL     string
	FirstEntry  int
	LastEntry   int
}

type projectBreadcrumbView struct {
	Label string
	URL   string
}

type projectEntryView struct {
	Name   string
	Path   string
	Type   string
	URL    string
	Active bool
}

type contextPageData struct {
	pageData
	Title        string
	Description  string
	Category     string
	DefaultScope string
	Contexts     []contextscope.Context
	Issues       []contextscope.Issue
	ShareURL     string
	UseURL       string
}

type dependencyPageData struct {
	pageData
	Topology             dependencies.Topology
	TopologyView         bool
	Inventory            dependencies.Inventory
	Findings             dependencies.FindingResponse
	AdvisoryOptions      dependencies.AdvisoryOptions
	FindingsView         bool
	SelectedRepositoryID int64
	PreviousURL          string
	NextURL              string
	APIURL               string
	SARIFURL             string
	RefreshURL           string
	AdvisoryRefreshURL   string
	FirstRow             int
	LastRow              int
	RefreshProgress      dependencies.RefreshProgress
	AdvisoryProgress     dependencies.AdvisoryRefreshProgress
}

// New builds the local HTTP server and parses embedded templates.
func New(config Config, intelligence *codeintel.Service, refresher CatalogueRefresher) (*Server, error) {
	functions := template.FuncMap{
		"formatTime":     formatTime,
		"highlightLine":  highlightLine,
		"fragmentRanges": fragmentRanges,
		"nextLine":       func(line int) int { return line + 1 },
		"previousLine":   func(line int) int { return max(1, line-1) },
		"sourceWindowStart": func(line int) int {
			start, _ := codeintel.SourceWindow(line, line)
			return start
		},
		"sourceWindowEnd": func(line int) int {
			_, end := codeintel.SourceWindow(line, line)
			return end
		},
		"lineFocused": func(line, start, end int) bool {
			return start > 0 && line >= start && line <= end
		},
		"shortCommit":      shortCommit,
		"statusLabel":      statusLabel,
		"scipStatusLabel":  scipStatusLabel,
		"scipFailureLabel": scipFailureLabel,
		"nextSearchLimit":  nextSearchLimit,
		"indexProgress":    indexProgress,
		"formatBytes":      formatBytes,
		"formatAgeSeconds": func(seconds int64) string {
			return formatDuration(time.Duration(seconds) * time.Second)
		},
		"formatMetric": func(value *float64) string {
			if value == nil {
				return "—"
			}
			return strconv.FormatFloat(*value, 'f', 2, 64)
		},
	}
	templates, err := template.New("repokarta").Funcs(functions).ParseFS(web.Files, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}

	dist, err := fs.Sub(web.Files, "dist")
	if err != nil {
		return nil, fmt.Errorf("load embedded frontend: %w", err)
	}

	server := &Server{
		config:                config,
		templates:             templates,
		intelligence:          intelligence,
		refresher:             refresher,
		agents:                config.Conversations,
		conversationShares:    config.ConversationShares,
		maps:                  config.Maps,
		docs:                  config.Docs,
		security:              config.Security,
		maintenance:           config.Maintenance,
		repositoryAccess:      config.RepositoryAccess,
		repositoryAcquisition: config.RepositoryAcquisition,
		enterprise:            config.Enterprise,
		insights:              config.Insights,
		dependencies:          config.Dependencies,
		scipJava:              config.SCIPJava,
	}
	server.history, _ = config.Conversations.(ConversationHistoryService)

	mux := http.NewServeMux()
	mux.Handle("GET /assets/", staticAssets(dist))
	mux.HandleFunc("GET /{$}", server.home)
	mux.HandleFunc("GET /repositories", server.repositoryList)
	mux.HandleFunc("POST /repositories/refresh", server.controlled(
		identity.PermissionAcquireRepositories, "repository.refresh", "repository-catalogue", server.refreshRepositories,
	))
	mux.HandleFunc("GET /search", server.search)
	mux.HandleFunc("GET /contexts", server.contextPage)
	mux.HandleFunc("GET /contexts/{contextID}", server.namedContextPage)
	mux.HandleFunc("GET /projects/{repositoryID}", server.project)
	mux.HandleFunc("GET /source/{repositoryID}", server.source)
	mux.HandleFunc("GET /api/search", server.apiSearch)
	mux.HandleFunc("POST /api/search", server.apiSearchJSON)
	mux.HandleFunc("GET /api/search/query-completions", server.apiQueryCompletions)
	mux.HandleFunc("GET /api/searches", server.apiSearchWorkspace)
	mux.HandleFunc("POST /api/searches", server.createSavedSearch)
	mux.HandleFunc("PUT /api/searches/{searchID}", server.updateSavedSearch)
	mux.HandleFunc("DELETE /api/searches/{searchID}", server.deleteSavedSearch)
	mux.HandleFunc("PUT /api/searches/{searchID}/monitor", server.configureSearchMonitor)
	mux.HandleFunc("POST /api/search-monitors/{monitorID}/run", server.runSearchMonitor)
	mux.HandleFunc("GET /api/contexts/suggest", server.apiContextSuggestions)
	mux.HandleFunc("POST /api/contexts/resolve", server.apiContextResolution)
	mux.HandleFunc("GET /api/contexts/named", server.apiNamedContexts)
	mux.HandleFunc("POST /api/contexts/named", server.createNamedContext)
	mux.HandleFunc("GET /api/contexts/named/{contextID}", server.apiNamedContext)
	mux.HandleFunc("PUT /api/contexts/named/{contextID}", server.updateNamedContext)
	mux.HandleFunc("DELETE /api/contexts/named/{contextID}", server.deleteNamedContext)
	mux.HandleFunc("GET /api/symbol", server.apiSymbol)
	mux.HandleFunc("POST /api/symbol", server.apiSymbolJSON)
	mux.HandleFunc("POST /api/ast/search", server.apiASTSearch)
	mux.HandleFunc("GET /api/repositories", server.apiRepositories)
	if server.scipJava != nil {
		mux.HandleFunc("GET /api/scip/java", server.apiSCIPJava)
		mux.HandleFunc("POST /api/scip/java/retry/{repositoryID}", server.controlled(
			identity.PermissionManageArtifacts,
			"scip.java.retry",
			"repository-scip-index",
			server.retrySCIPJava,
		))
		mux.HandleFunc("POST /repositories/{repositoryID}/scip/java/retry", server.controlled(
			identity.PermissionManageArtifacts,
			"scip.java.retry",
			"repository-scip-index",
			server.retrySCIPJava,
		))
	}
	mux.HandleFunc("GET /api/whoami", server.apiWhoAmI)
	mux.HandleFunc("GET /api/file/{repository}", server.apiFile)
	mux.HandleFunc("GET /api/tree/{repository}", server.apiTree)
	mux.HandleFunc("GET /api/git/log/{repository}", server.apiGitLog)
	mux.HandleFunc("GET /api/git/diff/{repository}", server.apiGitDiff)
	mux.HandleFunc("GET /events", server.events)
	mux.HandleFunc("GET /healthz", server.health)
	if server.security != nil {
		mux.HandleFunc("GET /admin/login", server.adminLoginPage)
		mux.HandleFunc("POST /admin/login", server.adminLogin)
		mux.HandleFunc("GET /admin", server.adminPage)
		mux.HandleFunc("POST /admin/security", server.updateSecurity)
		if server.repositoryAccess != nil {
			mux.HandleFunc("POST /admin/repositories/access", server.updateRepositoryAccess)
		}
		if server.enterprise != nil {
			mux.HandleFunc("POST /admin/identities/role", server.updateUserRole)
			mux.HandleFunc("POST /admin/groups/role", server.updateGroupRole)
			mux.HandleFunc("POST /admin/role-mappings", server.addRoleMapping)
			mux.HandleFunc("POST /admin/role-mappings/delete", server.deleteRoleMapping)
			mux.HandleFunc("POST /admin/audit/retention", server.updateAuditRetention)
			mux.HandleFunc("GET /admin/audit/export", server.exportBootstrapAudit)
			mux.HandleFunc("GET /api/admin/audit", server.controlled(
				identity.PermissionViewAudit, "audit.search", "audit-log", server.apiAudit,
			))
			mux.HandleFunc("GET /api/admin/audit/export", server.controlled(
				identity.PermissionViewAudit, "audit.export", "audit-log", server.exportAudit,
			))
			mux.HandleFunc("GET /api/admin/identities", server.controlled(
				identity.PermissionManageRoles, "administration.identities.list", "identity-directory", server.apiIdentityAdministration,
			))
			mux.HandleFunc("PATCH /api/admin/identities/{userID}/role", server.controlled(
				identity.PermissionManageRoles, "role.user.assign", "identity", server.apiUpdateUserRole,
			))
			mux.HandleFunc("PATCH /api/admin/groups/{groupID}/role", server.controlled(
				identity.PermissionManageRoles, "role.group.assign", "identity-group", server.apiUpdateGroupRole,
			))
			mux.HandleFunc("GET /api/admin/role-mappings", server.controlled(
				identity.PermissionManageRoles, "role.mapping.list", "role-mapping", server.apiRoleMappings,
			))
			mux.HandleFunc("POST /api/admin/role-mappings", server.controlled(
				identity.PermissionManageRoles, "role.mapping.set", "role-mapping", server.apiRoleMappings,
			))
			mux.HandleFunc("DELETE /api/admin/role-mappings/{mappingID}", server.controlled(
				identity.PermissionManageRoles, "role.mapping.delete", "role-mapping", server.apiDeleteRoleMapping,
			))
			mux.HandleFunc("GET /api/admin/security", server.controlled(
				identity.PermissionManageSecurity, "security.settings.read", "security-configuration", server.apiSecuritySettings,
			))
			mux.HandleFunc("PUT /api/admin/security", server.controlled(
				identity.PermissionManageSecurity, "security.settings.update", "security-configuration", server.apiSecuritySettings,
			))
			mux.HandleFunc("GET /api/admin/audit/retention", server.controlled(
				identity.PermissionManageAuditRetention, "audit.retention.read", "audit-log", server.apiAuditRetention,
			))
			mux.HandleFunc("PUT /api/admin/audit/retention", server.controlled(
				identity.PermissionManageAuditRetention, "audit.retention.update", "audit-log", server.apiAuditRetention,
			))
		}
		if server.repositoryAcquisition != nil {
			mux.HandleFunc("POST /admin/repositories/discover", server.discoverRepositories)
			mux.HandleFunc("POST /admin/repositories/acquire", server.acquireRepository)
			mux.HandleFunc("POST /admin/repositories/sync", server.syncAcquiredRepository)
			mux.HandleFunc("POST /admin/repositories/remove", server.removeAcquiredRepository)
		}
		if server.maintenance != nil {
			mux.HandleFunc("POST /admin/storage/preview", server.previewCleanup)
			mux.HandleFunc("POST /admin/storage/cleanup", server.executeCleanup)
			mux.HandleFunc("GET /admin/diagnostics", server.exportDiagnostics)
		}
		mux.HandleFunc("POST /admin/logout", server.adminLogout)
		mux.HandleFunc("POST /auth/logout", server.authLogout)
		mux.Handle("/saml/", server.security.SAMLHandler())
	}
	if server.maps != nil {
		mux.HandleFunc("GET /maps", server.mapPage)
		mux.HandleFunc("GET /api/maps", server.apiMap)
		mux.HandleFunc("POST /api/graph/query", server.apiGraphQuery)
		mux.HandleFunc("GET /api/artifacts/progress", server.apiArtifactProgress)
		mux.HandleFunc("GET /api/maps/export", server.controlled(
			identity.PermissionExportArtifacts, "artifact.export", "map", server.exportMap,
		))
		mux.HandleFunc("GET /dependencies", server.dependencyPage)
		mux.HandleFunc("GET /api/dependencies", server.apiDependencies)
		mux.HandleFunc("GET /api/dependencies/topology", server.apiDependencyTopology)
		if server.dependencies != nil {
			mux.HandleFunc("POST /api/dependencies/topology/observations", server.controlled(
				identity.PermissionManageArtifacts,
				"dependency.topology.import",
				"runtime-topology",
				server.importDependencyTopology,
			))
			mux.HandleFunc("POST /api/dependencies/refresh", server.controlled(
				identity.PermissionManageArtifacts,
				"dependency.refresh",
				"dependency-registry",
				server.refreshDependencies,
			))
			mux.HandleFunc("GET /api/dependencies/progress", server.dependencyRefreshProgress)
			mux.HandleFunc("GET /api/dependencies/findings", server.apiDependencyFindings)
			mux.HandleFunc("POST /api/dependencies/advisories/refresh", server.controlled(
				identity.PermissionManageArtifacts,
				"dependency.advisories.refresh",
				"osv-advisories",
				server.refreshDependencyAdvisories,
			))
			mux.HandleFunc("GET /api/dependencies/advisories/progress", server.dependencyAdvisoryProgress)
			mux.HandleFunc("GET /api/dependencies/findings.sarif", server.controlled(
				identity.PermissionExportArtifacts,
				"artifact.export",
				"dependency-findings-sarif",
				server.exportDependencyFindingsSARIF,
			))
		}
	}
	if server.docs != nil {
		mux.HandleFunc("GET /wiki", server.wikiPage)
		mux.HandleFunc("GET /api/wiki", server.apiWiki)
		mux.HandleFunc("POST /api/wiki/generate", server.controlled(
			identity.PermissionManageArtifacts, "generation.wiki", "wiki", server.generateWiki,
		))
		mux.HandleFunc("GET /api/wiki/export", server.controlled(
			identity.PermissionExportArtifacts, "artifact.export", "wiki", server.exportWiki,
		))
		mux.HandleFunc("GET /api/wiki/{repositoryID}/{page}", server.apiWikiPage)
	}
	if server.insights != nil {
		mux.HandleFunc("GET /insights", server.insightsPage)
		mux.HandleFunc("POST /insights/import", server.controlled(
			identity.PermissionManageArtifacts, "insight.import", "insight-run", server.importInsights,
		))
		mux.HandleFunc("POST /insights/derive", server.controlled(
			identity.PermissionManageArtifacts, "insight.derive", "insight-run", server.deriveInsights,
		))
		mux.HandleFunc("POST /insights/sonar/sync", server.syncSonar)
		mux.HandleFunc("POST /insights/sonar", server.configureSonar)
		mux.HandleFunc("POST /insights/threshold", server.setInsightThreshold)
		mux.HandleFunc("GET /api/insights", server.apiInsights)
		mux.HandleFunc("GET /api/insights/compare", server.compareInsights)
		mux.HandleFunc("POST /api/insights/import", server.controlled(
			identity.PermissionManageArtifacts, "insight.import", "insight-run", server.importInsights,
		))
		mux.HandleFunc("POST /api/insights/derive", server.controlled(
			identity.PermissionManageArtifacts, "insight.derive", "insight-run", server.deriveInsights,
		))
		mux.HandleFunc("GET /api/insights/thresholds", server.insightThresholds)
		mux.HandleFunc("PUT /api/insights/thresholds", server.setInsightThreshold)
		mux.HandleFunc("GET /api/insights/sonar", server.sonarConnections)
		mux.HandleFunc("PUT /api/insights/sonar", server.configureSonar)
		mux.HandleFunc("POST /api/insights/sonar/sync", server.syncSonar)
	}
	if config.MCPHandler != nil {
		mux.HandleFunc("GET /mcp/setup", server.controlled(
			identity.PermissionGenerateAI, "mcp.setup.read", "mcp-configuration", server.mcpPage,
		))
		mux.Handle("/mcp", config.MCPHandler)
	}
	if config.SCIMHandler != nil {
		mux.Handle("/scim/v2/", config.SCIMHandler)
	}
	if config.Conversations != nil {
		mux.HandleFunc("GET /chat", server.chatPage)
		mux.HandleFunc("GET /api/providers", server.providerStatuses)
		mux.HandleFunc("POST /api/chat", server.controlled(
			identity.PermissionGenerateAI, "generation.chat", "conversation", server.chat,
		))
		mux.HandleFunc("POST /api/chat/{conversationID}/interrupt", server.controlled(
			identity.PermissionGenerateAI, "generation.interrupt", "conversation", server.interruptChat,
		))
		mux.HandleFunc("POST /api/chat/{conversationID}/retry", server.controlled(
			identity.PermissionGenerateAI, "generation.retry", "conversation", server.retryChat,
		))
		if server.history != nil {
			mux.HandleFunc("GET /api/conversations", server.listConversations)
			mux.HandleFunc("GET /api/conversations/{conversationID}", server.getConversation)
			mux.HandleFunc("PATCH /api/conversations/{conversationID}", server.renameConversation)
			mux.HandleFunc("DELETE /api/conversations/{conversationID}", server.controlled(
				identity.PermissionReadRepositories, "owned-data.conversation.delete", "conversation", server.deleteConversation,
			))
		}
		if server.conversationShares != nil {
			mux.HandleFunc("POST /api/chat/{conversationID}/share", server.controlled(
				identity.PermissionReadRepositories, "conversation.share", "conversation", server.shareConversation,
			))
			mux.HandleFunc("DELETE /api/chat/shares/{token}", server.controlled(
				identity.PermissionReadRepositories, "conversation.share.revoke", "conversation", server.revokeConversationShare,
			))
			mux.HandleFunc("GET /api/shared/deep/{token}", server.sharedDeepSearch)
		}
	}

	handler := http.Handler(mux)
	if server.security != nil {
		handler = server.security.Middleware(mux)
	}
	handler = correlationMiddleware(handler)
	handler = securityHeaders(handler)
	server.server = &http.Server{
		Addr:              config.Address,
		Handler:           requestLog(handler),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	return server, nil
}

func (s *Server) listConversations(response http.ResponseWriter, request *http.Request) {
	viewer := s.conversationViewer(request.Context())
	conversations, err := s.history.ListConversations(request.Context(), agent.ConversationFilter{
		AuthorID: viewer.Author.ID,
	})
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, err)
		return
	}
	if conversations == nil {
		conversations = []agent.Conversation{}
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"conversations": conversations,
		"viewer":        viewer.Author,
		"can_view_all":  false,
		"scope":         "own",
	})
}

func (s *Server) getConversation(response http.ResponseWriter, request *http.Request) {
	conversation, err := s.history.GetConversation(
		request.Context(),
		strings.TrimSpace(request.PathValue("conversationID")),
	)
	if err != nil {
		writeConversationError(response, err)
		return
	}
	if err := s.authorizeConversation(request.Context(), conversation); err != nil {
		s.recordCrossAuthorConversation(request, conversation, "conversation.cross-author.read", "denied")
		writeConversationError(response, err)
		return
	}
	s.recordCrossAuthorConversation(request, conversation, "conversation.cross-author.read", "success")
	writeJSON(response, http.StatusOK, conversation)
}

func (s *Server) renameConversation(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, 4<<10)
	var input struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("invalid conversation title"))
		return
	}
	conversationID := strings.TrimSpace(request.PathValue("conversationID"))
	conversation, err := s.history.GetConversation(request.Context(), conversationID)
	if err != nil {
		writeConversationError(response, err)
		return
	}
	if err := s.authorizeConversation(request.Context(), conversation); err != nil {
		s.recordCrossAuthorConversation(request, conversation, "conversation.cross-author.rename", "denied")
		writeConversationError(response, err)
		return
	}
	s.recordCrossAuthorConversation(request, conversation, "conversation.cross-author.rename", "success")
	if err := s.history.RenameConversation(
		request.Context(),
		conversationID,
		input.Title,
	); err != nil {
		writeConversationError(response, err)
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteConversation(response http.ResponseWriter, request *http.Request) {
	conversationID := strings.TrimSpace(request.PathValue("conversationID"))
	conversation, err := s.history.GetConversation(request.Context(), conversationID)
	if err != nil {
		writeConversationError(response, err)
		return
	}
	if err := s.authorizeConversation(request.Context(), conversation); err != nil {
		s.recordCrossAuthorConversation(request, conversation, "conversation.cross-author.delete", "denied")
		writeConversationError(response, err)
		return
	}
	s.recordCrossAuthorConversation(request, conversation, "conversation.cross-author.delete", "success")
	if err := s.history.DeleteConversation(
		request.Context(),
		conversationID,
	); err != nil {
		writeConversationError(response, err)
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func writeConversationError(response http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, agent.ErrConversationNotFound), errors.Is(err, sql.ErrNoRows):
		writeAPIError(response, http.StatusNotFound, errors.New("conversation not found"))
	case errors.Is(err, agent.ErrConversationForbidden):
		writeAPIError(response, http.StatusForbidden, err)
	case errors.Is(err, agent.ErrInvalidInput):
		writeAPIError(response, http.StatusBadRequest, err)
	default:
		writeAPIError(response, http.StatusInternalServerError, err)
	}
}

func (s *Server) providerStatuses(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(map[string]any{
		"providers": s.agents.Statuses(request.Context()),
	})
}

func (s *Server) chat(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, maximumChatRequestBytes)
	var turn agent.TurnRequest
	if err := json.NewDecoder(request.Body).Decode(&turn); err != nil {
		http.Error(response, "Invalid conversation request", http.StatusBadRequest)
		return
	}
	turn.Message = strings.TrimSpace(turn.Message)
	turn.Provider = strings.TrimSpace(turn.Provider)
	turn.Model = strings.TrimSpace(turn.Model)
	turn.Effort = strings.TrimSpace(turn.Effort)
	turn.ConversationID = strings.TrimSpace(turn.ConversationID)
	effective, err := s.intelligence.ResolveEffectiveContexts(
		request.Context(),
		contextscope.EffectiveRequest{
			Contexts:        turn.ContextSelectors,
			NamedContextIDs: turn.NamedContextIDs,
			UseDefaults:     turn.UseDefaultContexts,
		},
	)
	if err != nil {
		writeContextOrAPIError(response, err)
		return
	}
	turn.Contexts = effective.Contexts
	viewer := s.conversationViewer(request.Context())
	turn.Author = viewer.Author
	for index := range turn.Images {
		turn.Images[index].Name = strings.TrimSpace(turn.Images[index].Name)
		turn.Images[index].MediaType = strings.ToLower(strings.TrimSpace(turn.Images[index].MediaType))
	}
	if (turn.Message == "" && len(turn.Images) == 0) || (turn.ConversationID == "" && turn.Provider == "") {
		http.Error(response, "Provider and message or image are required", http.StatusBadRequest)
		return
	}
	if err := agent.ValidateImages(turn.Images); err != nil {
		http.Error(response, err.Error(), http.StatusBadRequest)
		return
	}

	flusher, ok := response.(http.Flusher)
	if !ok {
		http.Error(response, "Streaming is not supported", http.StatusInternalServerError)
		return
	}
	response.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("X-Accel-Buffering", "no")
	encoder := json.NewEncoder(response)
	emit := func(event agent.Event) error {
		if err := encoder.Encode(event); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	if err := s.agents.Send(request.Context(), turn, emit); err != nil {
		slog.Warn("conversation turn failed", "provider", turn.Provider, "error", err)
		_ = emit(agent.Event{Type: agent.EventError, ConversationID: turn.ConversationID, Text: err.Error()})
	}
}

func (s *Server) retryChat(response http.ResponseWriter, request *http.Request) {
	retrier, ok := s.agents.(ConversationRetryService)
	if !ok || s.history == nil {
		writeAPIError(response, http.StatusServiceUnavailable, errors.New("conversation retry is unavailable"))
		return
	}
	var input struct {
		Strategy       string `json:"strategy"`
		TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
		TokenBudget    int64  `json:"token_budget,omitempty"`
		ToolCallBudget int    `json:"tool_call_budget,omitempty"`
	}
	if err := decodeBoundedJSON(response, request, &input); err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("invalid retry request"))
		return
	}
	conversationID := strings.TrimSpace(request.PathValue("conversationID"))
	conversation, err := s.history.GetConversation(request.Context(), conversationID)
	if err != nil {
		writeConversationError(response, err)
		return
	}
	if err := s.authorizeConversation(request.Context(), conversation); err != nil {
		writeConversationError(response, err)
		return
	}
	flusher, ok := response.(http.Flusher)
	if !ok {
		writeAPIError(response, http.StatusInternalServerError, errors.New("streaming is not supported"))
		return
	}
	response.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("X-Accel-Buffering", "no")
	encoder := json.NewEncoder(response)
	emit := func(event agent.Event) error {
		if err := encoder.Encode(event); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	viewer := s.conversationViewer(request.Context())
	err = retrier.Retry(request.Context(), agent.RetryRequest{
		ConversationID: conversationID,
		Author:         viewer.Author,
		Strategy:       input.Strategy,
		TimeoutSeconds: input.TimeoutSeconds,
		TokenBudget:    input.TokenBudget,
		ToolCallBudget: input.ToolCallBudget,
	}, emit)
	if err != nil {
		_ = emit(agent.Event{Type: agent.EventError, ConversationID: conversationID, Text: err.Error()})
	}
}

func (s *Server) shareConversation(response http.ResponseWriter, request *http.Request) {
	conversationID := strings.TrimSpace(request.PathValue("conversationID"))
	conversation, err := s.history.GetConversation(request.Context(), conversationID)
	if err != nil {
		writeConversationError(response, err)
		return
	}
	if err := s.authorizeConversation(request.Context(), conversation); err != nil {
		writeConversationError(response, err)
		return
	}
	if conversation.Mode != "deep_search" {
		writeAPIError(response, http.StatusUnprocessableEntity, errors.New("only Deep Search conversations can be shared"))
		return
	}
	viewer := s.conversationViewer(request.Context())
	share, err := s.conversationShares.CreateConversationShare(
		request.Context(), conversation.ID, viewer.Author.ID,
	)
	if err != nil {
		writeConversationError(response, err)
		return
	}
	writeJSON(response, http.StatusCreated, map[string]any{
		"share": share,
		"url":   "/api/shared/deep/" + url.PathEscape(share.Token),
	})
}

func (s *Server) revokeConversationShare(response http.ResponseWriter, request *http.Request) {
	viewer := s.conversationViewer(request.Context())
	if err := s.conversationShares.RevokeConversationShare(
		request.Context(), request.PathValue("token"), viewer.Author.ID,
	); err != nil {
		writeConversationError(response, err)
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func (s *Server) sharedDeepSearch(response http.ResponseWriter, request *http.Request) {
	share, conversation, err := s.conversationShares.GetConversationShare(
		request.Context(), request.PathValue("token"),
	)
	if err != nil || conversation.Mode != "deep_search" {
		writeAPIError(response, http.StatusNotFound, agent.ErrConversationNotFound)
		return
	}
	if err := s.revalidateConversationSources(request.Context(), conversation); err != nil {
		writeAPIError(
			response,
			http.StatusForbidden,
			errors.New("shared Deep Search contains a source that is not visible to the current viewer"),
		)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"share": map[string]any{
			"token":      share.Token,
			"created_at": share.CreatedAt,
		},
		"conversation": map[string]any{
			"id":            conversation.ID,
			"title":         conversation.Title,
			"mode":          conversation.Mode,
			"provider":      conversation.Provider,
			"model":         conversation.Model,
			"created_at":    conversation.CreatedAt,
			"updated_at":    conversation.UpdatedAt,
			"input_tokens":  conversation.InputTokens,
			"output_tokens": conversation.OutputTokens,
			"messages":      conversation.Messages,
		},
		"permission_revalidated": true,
	})
}

func (s *Server) revalidateConversationSources(
	ctx context.Context,
	conversation agent.Conversation,
) error {
	checked := make(map[int64]struct{})
	check := func(repositoryID int64) error {
		if repositoryID <= 0 {
			return nil
		}
		if _, ok := checked[repositoryID]; ok {
			return nil
		}
		if _, err := s.intelligence.RepositoryByID(ctx, repositoryID); err != nil {
			return err
		}
		checked[repositoryID] = struct{}{}
		return nil
	}
	for _, message := range conversation.Messages {
		for _, structured := range message.Contexts {
			if err := check(structured.RepositoryID); err != nil {
				return err
			}
		}
		for _, citation := range message.Sources {
			repositoryID, ok := sourceRepositoryID(citation.URL)
			if ok {
				if err := check(repositoryID); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func sourceRepositoryID(raw string) (int64, bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return 0, false
	}
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	for index := 0; index+1 < len(segments); index++ {
		switch segments[index] {
		case "source", "projects", "wiki":
		default:
			continue
		}
		id, err := strconv.ParseInt(segments[index+1], 10, 64)
		if err == nil && id > 0 {
			return id, true
		}
	}
	for _, key := range []string{"repo", "repository_id"} {
		id, err := strconv.ParseInt(parsed.Query().Get(key), 10, 64)
		if err == nil && id > 0 {
			return id, true
		}
	}
	return 0, false
}

func (s *Server) interruptChat(response http.ResponseWriter, request *http.Request) {
	conversationID := strings.TrimSpace(request.PathValue("conversationID"))
	if conversationID == "" {
		http.Error(response, "Conversation is required", http.StatusBadRequest)
		return
	}
	if s.history == nil {
		writeAPIError(response, http.StatusServiceUnavailable, errors.New("conversation authorization is unavailable"))
		return
	}
	conversation, err := s.history.GetConversation(request.Context(), conversationID)
	if err != nil {
		writeConversationError(response, err)
		return
	}
	if err := s.authorizeConversation(request.Context(), conversation); err != nil {
		s.recordCrossAuthorConversation(request, conversation, "conversation.cross-author.interrupt", "denied")
		writeConversationError(response, err)
		return
	}
	s.recordCrossAuthorConversation(request, conversation, "conversation.cross-author.interrupt", "success")
	if err := s.agents.Interrupt(request.Context(), conversationID); err != nil {
		switch {
		case errors.Is(err, agent.ErrConversationNotFound):
			http.Error(response, err.Error(), http.StatusNotFound)
		case errors.Is(err, agent.ErrNoActiveTurn):
			http.Error(response, err.Error(), http.StatusConflict)
		default:
			slog.Warn("interrupt conversation turn", "conversation_id", conversationID, "error", err)
			http.Error(response, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

type conversationViewer struct {
	Author agent.ConversationAuthor
	Admin  bool
}

func (s *Server) conversationViewer(ctx context.Context) conversationViewer {
	principal, ok := security.PrincipalFromContext(ctx)
	if !ok {
		return conversationViewer{
			Author: agent.ConversationAuthor{
				ID:       "local:admin",
				Name:     "Local administrator",
				Provider: string(security.ModeLocal),
			},
			Admin: true,
		}
	}
	provider := strings.TrimSpace(principal.Provider)
	if provider == "" {
		provider = "authenticated"
	}
	identity := strings.TrimSpace(principal.ID)
	if identity == "" {
		identity = strings.ToLower(strings.TrimSpace(principal.Email))
	}
	if identity == "" {
		identity = "anonymous"
	}
	name := strings.TrimSpace(principal.Name)
	if name == "" {
		name = strings.TrimSpace(principal.Email)
	}
	if name == "" {
		name = identity
	}
	return conversationViewer{
		Author: agent.ConversationAuthor{
			ID:       provider + ":" + identity,
			Name:     name,
			Email:    strings.TrimSpace(principal.Email),
			Provider: provider,
			Groups:   append([]string(nil), principal.Groups...),
		},
		Admin: principal.Admin,
	}
}

func (s *Server) authorizeConversation(ctx context.Context, conversation agent.Conversation) error {
	viewer := s.conversationViewer(ctx)
	if conversation.Author.ID == viewer.Author.ID {
		return nil
	}
	return agent.ErrConversationForbidden
}

func (s *Server) apiSearch(response http.ResponseWriter, request *http.Request) {
	limit, err := apiSearchLimit(request.URL.Query().Get("limit"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	compact, err := optionalBool(request.URL.Query().Get("compact"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("compact must be a boolean"))
		return
	}
	repositoryID, repositoryName := repositorySelector(request.URL.Query().Get("repo"))
	mode := strings.TrimSpace(request.URL.Query().Get("mode"))
	if mode == "" {
		mode = "zoekt"
	}
	input := codeintel.SearchRequest{
		Query:        request.URL.Query().Get("q"),
		RepositoryID: repositoryID,
		Repository:   repositoryName,
		Language:     request.URL.Query().Get("lang"),
		Path:         request.URL.Query().Get("path"),
		File:         request.URL.Query().Get("file"),
		Mode:         mode,
		Limit:        limit,
		Compact:      compact,
	}
	result, err := s.intelligence.Search(request.Context(), input)
	if err != nil {
		writeContextOrAPIError(response, err)
		return
	}
	if err := s.intelligence.RecordRecentSearch(request.Context(), input, result); err != nil {
		slog.Warn("record recent search", "error", err)
	}
	writeSearchJSON(response, result)
}

func (s *Server) apiSearchJSON(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, 1<<20)
	var input codeintel.SearchRequest
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("invalid structured search request"))
		return
	}
	result, err := s.intelligence.Search(request.Context(), input)
	if err != nil {
		writeContextOrAPIError(response, err)
		return
	}
	if err := s.intelligence.RecordRecentSearch(request.Context(), input, result); err != nil {
		slog.Warn("record recent structured search", "error", err)
	}
	writeSearchJSON(response, result)
}

func (s *Server) apiSearchWorkspace(response http.ResponseWriter, request *http.Request) {
	workspace, err := s.intelligence.ListSearchWorkspace(request.Context())
	if err != nil {
		writeSearchWorkspaceError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, workspace)
}

func (s *Server) createSavedSearch(response http.ResponseWriter, request *http.Request) {
	var input codeintel.SavedSearchInput
	if err := decodeBoundedJSON(response, request, &input); err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("invalid saved search"))
		return
	}
	saved, err := s.intelligence.CreateSavedSearch(request.Context(), input)
	if err != nil {
		writeSearchWorkspaceError(response, err)
		return
	}
	writeJSON(response, http.StatusCreated, saved)
}

func (s *Server) updateSavedSearch(response http.ResponseWriter, request *http.Request) {
	var input codeintel.SavedSearchInput
	if err := decodeBoundedJSON(response, request, &input); err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("invalid saved search"))
		return
	}
	saved, err := s.intelligence.UpdateSavedSearch(
		request.Context(), request.PathValue("searchID"), input,
	)
	if err != nil {
		writeSearchWorkspaceError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, saved)
}

func (s *Server) deleteSavedSearch(response http.ResponseWriter, request *http.Request) {
	if err := s.intelligence.DeleteSavedSearch(
		request.Context(), request.PathValue("searchID"),
	); err != nil {
		writeSearchWorkspaceError(response, err)
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func (s *Server) configureSearchMonitor(response http.ResponseWriter, request *http.Request) {
	var input codeintel.SearchMonitorInput
	if err := decodeBoundedJSON(response, request, &input); err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("invalid search monitor"))
		return
	}
	monitor, err := s.intelligence.ConfigureSearchMonitor(
		request.Context(), request.PathValue("searchID"), input,
	)
	if err != nil {
		writeSearchWorkspaceError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, monitor)
}

func (s *Server) runSearchMonitor(response http.ResponseWriter, request *http.Request) {
	run, err := s.intelligence.RunSearchMonitor(
		request.Context(), request.PathValue("monitorID"),
	)
	if err != nil {
		writeSearchWorkspaceError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, run)
}

func decodeBoundedJSON(response http.ResponseWriter, request *http.Request, value any) error {
	request.Body = http.MaxBytesReader(response, request.Body, 1<<20)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	return decoder.Decode(value)
}

func (s *Server) apiQueryCompletions(response http.ResponseWriter, request *http.Request) {
	raw := request.URL.Query().Get("q")
	if len(raw) > 8192 {
		writeAPIError(response, http.StatusRequestURITooLong, errors.New("query is too long"))
		return
	}
	cursor, err := strconv.Atoi(request.URL.Query().Get("cursor"))
	if err != nil || cursor < 0 {
		writeAPIError(response, http.StatusBadRequest, errors.New("cursor must be a non-negative integer"))
		return
	}
	result, err := s.intelligence.CompleteQuery(request.Context(), raw, cursor)
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, errors.New("query completions are unavailable"))
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func (s *Server) apiContextSuggestions(response http.ResponseWriter, request *http.Request) {
	repositoryID, err := optionalRepositoryID(request.URL.Query().Get("repository_id"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	limit, err := apiSearchLimit(request.URL.Query().Get("limit"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	result, err := s.intelligence.SuggestContexts(request.Context(), codeintel.ContextSuggestionRequest{
		Kind:         request.URL.Query().Get("kind"),
		Query:        request.URL.Query().Get("q"),
		RepositoryID: repositoryID,
		Limit:        limit,
	})
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func (s *Server) apiContextResolution(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, 64<<10)
	var input contextscope.EffectiveRequest
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("invalid structured context request"))
		return
	}
	effective, err := s.intelligence.ResolveEffectiveContexts(request.Context(), input)
	if err != nil {
		writeContextOrAPIError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, effective)
}

func (s *Server) apiNamedContexts(response http.ResponseWriter, request *http.Request) {
	contexts, err := s.intelligence.ListNamedContexts(request.Context())
	if err != nil {
		writeNamedContextError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, contexts)
}

func (s *Server) apiNamedContext(response http.ResponseWriter, request *http.Request) {
	context, err := s.intelligence.GetNamedContext(request.Context(), request.PathValue("contextID"))
	if err != nil {
		writeNamedContextError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, context)
}

func (s *Server) createNamedContext(response http.ResponseWriter, request *http.Request) {
	input, err := decodeNamedContextInput(response, request)
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	context, err := s.intelligence.CreateNamedContext(request.Context(), input)
	if err != nil {
		writeNamedContextError(response, err)
		return
	}
	writeJSON(response, http.StatusCreated, context)
}

func (s *Server) updateNamedContext(response http.ResponseWriter, request *http.Request) {
	input, err := decodeNamedContextInput(response, request)
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	context, err := s.intelligence.UpdateNamedContext(
		request.Context(),
		request.PathValue("contextID"),
		input,
	)
	if err != nil {
		writeNamedContextError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, context)
}

func (s *Server) deleteNamedContext(response http.ResponseWriter, request *http.Request) {
	if err := s.intelligence.DeleteNamedContext(request.Context(), request.PathValue("contextID")); err != nil {
		writeNamedContextError(response, err)
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func decodeNamedContextInput(
	response http.ResponseWriter,
	request *http.Request,
) (contextscope.NamedContextInput, error) {
	request.Body = http.MaxBytesReader(response, request.Body, 128<<10)
	var input contextscope.NamedContextInput
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return input, errors.New("invalid named context request")
	}
	return input, nil
}

func writeSearchJSON(response http.ResponseWriter, result codeintel.SearchResponse) {
	status := http.StatusOK
	if result.ReferenceIndex != nil && result.ReferenceIndex.State == "building" {
		status = http.StatusAccepted
		response.Header().Set("Retry-After", "2")
	}
	writeJSON(response, status, result)
}

func (s *Server) apiSymbol(response http.ResponseWriter, request *http.Request) {
	limit, err := apiSearchLimit(request.URL.Query().Get("limit"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	compact, err := optionalBool(request.URL.Query().Get("compact"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("compact must be a boolean"))
		return
	}
	repositoryID, repositoryName := repositorySelector(request.URL.Query().Get("repo"))
	result, err := s.intelligence.FindSymbol(request.Context(), codeintel.SymbolRequest{
		Symbol:       request.URL.Query().Get("symbol"),
		RepositoryID: repositoryID,
		Repository:   repositoryName,
		Language:     request.URL.Query().Get("lang"),
		Limit:        limit,
		Compact:      compact,
	})
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func (s *Server) apiSymbolJSON(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, 1<<20)
	var input codeintel.SymbolRequest
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("invalid symbol request"))
		return
	}
	result, err := s.intelligence.FindSymbol(request.Context(), input)
	if err != nil {
		writeContextError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func (s *Server) apiASTSearch(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, 64<<10)
	var input codeintel.ASTSearchRequest
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("invalid AST search request"))
		return
	}
	result, err := s.intelligence.SearchAST(request.Context(), input)
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	status := http.StatusOK
	if result.Index.State == "building" {
		response.Header().Set("Retry-After", "2")
		status = http.StatusAccepted
	}
	writeJSON(response, status, result)
}

func (s *Server) apiRepositories(response http.ResponseWriter, request *http.Request) {
	repositories, err := s.intelligence.Repositories(request.Context())
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, repositories)
}

func (s *Server) apiSCIPJava(response http.ResponseWriter, _ *http.Request) {
	writeJSON(response, http.StatusOK, s.scipJava.ProviderStatus())
}

func (s *Server) retrySCIPJava(response http.ResponseWriter, request *http.Request) {
	repositoryID, err := strconv.ParseInt(strings.TrimSpace(request.PathValue("repositoryID")), 10, 64)
	if err != nil || repositoryID <= 0 {
		writeAPIError(response, http.StatusBadRequest, errors.New("positive repository ID is required"))
		return
	}
	if err := s.scipJava.Retry(request.Context(), repositoryID); err != nil {
		writeAPIError(response, http.StatusConflict, err)
		return
	}
	if strings.HasPrefix(request.URL.Path, "/api/") {
		writeJSON(response, http.StatusAccepted, map[string]any{
			"repository_id": repositoryID,
			"state":         "pending",
		})
		return
	}
	if strings.EqualFold(strings.TrimSpace(request.Header.Get("HX-Request")), "true") {
		s.repositoryList(response, request)
		return
	}
	http.Redirect(response, request, "/", http.StatusSeeOther)
}

func (s *Server) apiWhoAmI(response http.ResponseWriter, request *http.Request) {
	principal, ok := security.PrincipalFromContext(request.Context())
	if !ok {
		writeAPIError(response, http.StatusUnauthorized, errors.New("authenticated identity is unavailable"))
		return
	}
	viewer := s.conversationViewer(request.Context())
	groups := append([]string{}, principal.Groups...)
	writeJSON(response, http.StatusOK, map[string]any{
		"id":          viewer.Author.ID,
		"name":        viewer.Author.Name,
		"email":       viewer.Author.Email,
		"provider":    viewer.Author.Provider,
		"groups":      groups,
		"admin":       viewer.Admin,
		"role":        identity.NormalizeRole(principal.Role),
		"permissions": identity.Permissions(principal.Role),
	})
}

func (s *Server) apiFile(response http.ResponseWriter, request *http.Request) {
	start, end := parseLineRange(request.URL.Query().Get("lines"))
	repositoryID, repositoryName := repositorySelector(request.PathValue("repository"))
	file, err := s.intelligence.GetFile(request.Context(), codeintel.FileRequest{
		RepositoryID: repositoryID,
		Repository:   repositoryName,
		Revision:     request.URL.Query().Get("rev"),
		Path:         request.URL.Query().Get("path"),
		StartLine:    start,
		EndLine:      end,
	})
	if err != nil {
		writeCodeIntelligenceError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, file)
}

func (s *Server) apiTree(response http.ResponseWriter, request *http.Request) {
	offset, err := nonNegativeInteger(request.URL.Query().Get("offset"), "offset")
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	repositoryID, repositoryName := repositorySelector(request.PathValue("repository"))
	tree, err := s.intelligence.ListTree(request.Context(), codeintel.TreeRequest{
		RepositoryID: repositoryID,
		Repository:   repositoryName,
		Revision:     request.URL.Query().Get("rev"),
		Path:         request.URL.Query().Get("path"),
		Offset:       offset,
	})
	if err != nil {
		writeCodeIntelligenceError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, tree)
}

func nonNegativeInteger(value, name string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", name)
	}
	return parsed, nil
}

func (s *Server) apiGitLog(response http.ResponseWriter, request *http.Request) {
	limit, err := apiBoundedInteger(
		request.URL.Query().Get("limit"),
		"limit",
		codeintel.DefaultGitLogLimit,
		codeintel.MaximumGitLogLimit,
	)
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	repositoryID, repositoryName := repositorySelector(request.PathValue("repository"))
	result, err := s.intelligence.GitLog(request.Context(), codeintel.GitLogRequest{
		RepositoryID: repositoryID,
		Repository:   repositoryName,
		Revision:     request.URL.Query().Get("rev"),
		Path:         request.URL.Query().Get("path"),
		Limit:        limit,
	})
	if err != nil {
		writeCodeIntelligenceError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func (s *Server) apiGitDiff(response http.ResponseWriter, request *http.Request) {
	contextLines, err := apiBoundedInteger(
		request.URL.Query().Get("context"),
		"context",
		codeintel.DefaultDiffContext,
		codeintel.MaximumDiffContext,
	)
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	repositoryID, repositoryName := repositorySelector(request.PathValue("repository"))
	result, err := s.intelligence.GitDiff(request.Context(), codeintel.GitDiffRequest{
		RepositoryID: repositoryID,
		Repository:   repositoryName,
		FromRevision: request.URL.Query().Get("from"),
		ToRevision:   request.URL.Query().Get("to"),
		Path:         request.URL.Query().Get("path"),
		ContextLines: contextLines,
	})
	if err != nil {
		writeCodeIntelligenceError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, result)
}

// Run listens until the context is cancelled and then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	s.server.BaseContext = func(net.Listener) context.Context {
		return ctx
	}
	listener, err := net.Listen("tcp", s.server.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.server.Addr, err)
	}

	errChannel := make(chan error, 1)
	go func() {
		errChannel <- s.server.Serve(listener)
	}()
	if s.config.OpenBrowser {
		go func() {
			localURL := "http://" + listener.Addr().String()
			if err := openBrowser(localURL); err != nil {
				slog.Warn("open browser", "url", localURL, "error", err)
			}
		}()
	}

	select {
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.server.Shutdown(shutdownContext); err != nil {
			return fmt.Errorf("shut down HTTP server: %w", err)
		}
		return nil
	case err := <-errChannel:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (s *Server) home(response http.ResponseWriter, request *http.Request) {
	data, err := s.pageData(request.Context())
	if err != nil {
		http.Error(response, "Could not load repositories", http.StatusInternalServerError)
		return
	}
	s.render(response, "index", data)
}

func (s *Server) chatPage(response http.ResponseWriter, request *http.Request) {
	data, err := s.pageData(request.Context())
	if err != nil {
		http.Error(response, "Could not load repositories", http.StatusInternalServerError)
		return
	}
	data.ActivePage = "chat"
	s.render(response, "chat", data)
}

func (s *Server) contextPage(response http.ResponseWriter, request *http.Request) {
	repositoryID, err := optionalRepositoryID(request.URL.Query().Get("repository"))
	if err != nil || repositoryID <= 0 {
		http.Error(response, "A positive repository context is required", http.StatusBadRequest)
		return
	}
	line, err := strconv.Atoi(request.URL.Query().Get("line"))
	if err != nil && request.URL.Query().Get("line") != "" {
		http.Error(response, "Context line must be a positive integer", http.StatusBadRequest)
		return
	}
	useDefaults := false
	effective, err := s.intelligence.ResolveEffectiveContexts(
		request.Context(),
		contextscope.EffectiveRequest{
			Contexts: []contextscope.Selector{{
				Kind:         request.URL.Query().Get("kind"),
				RepositoryID: repositoryID,
				Revision:     request.URL.Query().Get("revision"),
				Path:         request.URL.Query().Get("path"),
				Symbol:       request.URL.Query().Get("symbol"),
				SymbolKind:   request.URL.Query().Get("symbol_kind"),
				Line:         line,
			}},
			UseDefaults: &useDefaults,
		},
	)
	if err != nil {
		writeContextPageError(response, err)
		return
	}
	if len(effective.Contexts) == 0 {
		http.Error(response, "Context resolved without a usable target", http.StatusUnprocessableEntity)
		return
	}
	base, err := s.pageData(request.Context())
	if err != nil {
		http.Error(response, "Could not load context", http.StatusInternalServerError)
		return
	}
	base.ActivePage = "context"
	title := "Structured context"
	if len(effective.Contexts) > 0 {
		title = effective.Contexts[0].Label
	}
	s.render(response, "context", contextPageData{
		pageData: base,
		Title:    title,
		Contexts: effective.Contexts,
		ShareURL: effective.Contexts[0].URL,
		UseURL:   "/chat?context_url=" + url.QueryEscape(effective.Contexts[0].URL),
	})
}

func (s *Server) namedContextPage(response http.ResponseWriter, request *http.Request) {
	named, err := s.intelligence.GetNamedContext(request.Context(), request.PathValue("contextID"))
	if err != nil {
		if errors.Is(err, contextscope.ErrNamedContextNotFound) {
			http.NotFound(response, request)
			return
		}
		http.Error(response, "Could not load named context", http.StatusInternalServerError)
		return
	}
	base, err := s.pageData(request.Context())
	if err != nil {
		http.Error(response, "Could not load context", http.StatusInternalServerError)
		return
	}
	base.ActivePage = "context"
	s.render(response, "context", contextPageData{
		pageData:     base,
		Title:        named.Title,
		Description:  named.Description,
		Category:     named.Category,
		DefaultScope: named.DefaultScope,
		Contexts:     named.Contexts,
		Issues:       named.Issues,
		ShareURL:     named.URL,
		UseURL:       "/chat?context=" + url.QueryEscape(named.ID),
	})
}

func writeContextPageError(response http.ResponseWriter, err error) {
	var resolution *contextscope.ResolutionError
	if errors.As(err, &resolution) {
		http.Error(response, resolution.Error(), http.StatusUnprocessableEntity)
		return
	}
	http.Error(response, err.Error(), http.StatusBadRequest)
}

func (s *Server) mapPage(response http.ResponseWriter, request *http.Request) {
	data, err := s.pageData(request.Context())
	if err != nil {
		http.Error(response, "Could not load repositories", http.StatusInternalServerError)
		return
	}
	data.ActivePage = "maps"
	s.render(response, "maps", data)
}

func (s *Server) dependencyPage(response http.ResponseWriter, request *http.Request) {
	data, err := s.pageData(request.Context())
	if err != nil {
		http.Error(response, "Could not load repositories", http.StatusInternalServerError)
		return
	}
	repositoryID, err := optionalRepositoryID(request.URL.Query().Get("repository"))
	if err != nil {
		http.Error(response, "Invalid repository", http.StatusBadRequest)
		return
	}
	view := strings.ToLower(strings.TrimSpace(request.URL.Query().Get("view")))
	if view == "" || view == "topology" {
		if s.dependencies == nil {
			http.Error(response, "Distributed topology service is unavailable", http.StatusServiceUnavailable)
			return
		}
		options, err := dependencyTopologyOptions(request)
		if err != nil {
			http.Error(response, err.Error(), http.StatusBadRequest)
			return
		}
		snapshot, progress, err := s.maps.ReadTopologySnapshot(request.Context(), repositoryID)
		if err != nil {
			slog.Error("compose distributed topology", "repository_id", repositoryID, "error", err)
			http.Error(response, "Distributed topology could not be built", http.StatusInternalServerError)
			return
		}
		topology, err := s.dependencies.Topology(request.Context(), snapshot, progress, options)
		if err != nil {
			slog.Error("join runtime topology observations", "repository_id", repositoryID, "error", err)
			http.Error(response, "Distributed topology could not be loaded", http.StatusInternalServerError)
			return
		}
		data.ActivePage = "dependencies"
		s.render(response, "dependencies", dependencyPageData{
			pageData: data, Topology: topology, TopologyView: true,
			SelectedRepositoryID: repositoryID,
			APIURL:               dependencyTopologyURL("/api/dependencies/topology", repositoryID, options),
		})
		return
	}
	options, err := dependencyOptions(request)
	if err != nil {
		http.Error(response, err.Error(), http.StatusBadRequest)
		return
	}
	snapshot, progress, err := s.maps.ReadDependencySnapshot(request.Context(), repositoryID)
	if err != nil {
		slog.Error("build dependency inventory", "repository_id", repositoryID, "error", err)
		http.Error(response, "Dependency inventory could not be built", http.StatusInternalServerError)
		return
	}
	data.ActivePage = "dependencies"
	inventory, err := s.dependencyInventory(request.Context(), snapshot, options)
	if err != nil {
		slog.Error("load dependency registry observations", "error", err)
		http.Error(response, "Dependency inventory could not be built", http.StatusInternalServerError)
		return
	}
	inventory.BuildProgress = progress
	findingsView := view == "findings"
	advisoryOptions, err := dependencyAdvisoryOptions(request)
	if err != nil {
		http.Error(response, err.Error(), http.StatusBadRequest)
		return
	}
	findings := dependencies.FindingResponse{}
	if findingsView {
		if s.dependencies == nil {
			http.Error(response, "Dependency advisory service is unavailable", http.StatusServiceUnavailable)
			return
		}
		findings, err = s.dependencies.Findings(request.Context(), snapshot, advisoryOptions)
		if err != nil {
			slog.Error("join dependency advisories", "repository_id", repositoryID, "error", err)
			http.Error(response, "Dependency findings could not be loaded", http.StatusInternalServerError)
			return
		}
	}
	previousURL := ""
	nextURL := ""
	firstRow, lastRow := 0, 0
	if findingsView {
		if findings.Offset > 0 {
			previousURL = dependencyAdvisoryURL(
				"/dependencies", repositoryID, advisoryOptions,
				max(0, findings.Offset-findings.Limit), true,
			)
		}
		if findings.HasMore {
			nextURL = dependencyAdvisoryURL(
				"/dependencies", repositoryID, advisoryOptions,
				findings.Offset+findings.ReturnedCount, true,
			)
		}
		if findings.ReturnedCount > 0 {
			firstRow = findings.Offset + 1
			lastRow = findings.Offset + findings.ReturnedCount
		}
	} else {
		if inventory.Offset > 0 {
			previousURL = dependencyURL("/dependencies", repositoryID, options, max(0, inventory.Offset-inventory.Limit))
		}
		if inventory.HasMore {
			nextURL = dependencyURL("/dependencies", repositoryID, options, inventory.Offset+inventory.ReturnedCount)
		}
		if inventory.ReturnedCount > 0 {
			firstRow = inventory.Offset + 1
			lastRow = inventory.Offset + inventory.ReturnedCount
		}
	}
	s.render(response, "dependencies", dependencyPageData{
		pageData:             data,
		Inventory:            inventory,
		Findings:             findings,
		AdvisoryOptions:      advisoryOptions,
		FindingsView:         findingsView,
		SelectedRepositoryID: repositoryID,
		PreviousURL:          previousURL,
		NextURL:              nextURL,
		APIURL: func() string {
			if findingsView {
				return dependencyAdvisoryURL(
					"/api/dependencies/findings", repositoryID, advisoryOptions, findings.Offset, false,
				)
			}
			return dependencyURL("/api/dependencies", repositoryID, options, inventory.Offset)
		}(),
		SARIFURL: dependencyAdvisoryURL(
			"/api/dependencies/findings.sarif", repositoryID, advisoryOptions, 0, false,
		),
		RefreshURL: dependencyURL("/api/dependencies/refresh", repositoryID, options, 0),
		AdvisoryRefreshURL: dependencyAdvisoryURL(
			"/api/dependencies/advisories/refresh", repositoryID, dependencies.AdvisoryOptions{}, 0, false,
		),
		FirstRow: firstRow,
		LastRow:  lastRow,
		RefreshProgress: func() dependencies.RefreshProgress {
			if s.dependencies == nil {
				return dependencies.RefreshProgress{State: "unavailable"}
			}
			return s.dependencies.Progress()
		}(),
		AdvisoryProgress: func() dependencies.AdvisoryRefreshProgress {
			if s.dependencies == nil {
				return dependencies.AdvisoryRefreshProgress{State: "unavailable"}
			}
			return s.dependencies.AdvisoryProgress()
		}(),
	})
}

func (s *Server) wikiPage(response http.ResponseWriter, request *http.Request) {
	data, err := s.pageData(request.Context())
	if err != nil {
		http.Error(response, "Could not load repositories", http.StatusInternalServerError)
		return
	}
	data.ActivePage = "wiki"
	s.render(response, "wiki", data)
}

func (s *Server) mcpPage(response http.ResponseWriter, request *http.Request) {
	data, err := s.pageData(request.Context())
	if err != nil {
		http.Error(response, "Could not load MCP configuration", http.StatusInternalServerError)
		return
	}
	data.ActivePage = "mcp"
	endpointBaseURL := strings.TrimRight(strings.TrimSpace(s.config.MCPBaseURL), "/")
	if s.security != nil {
		if publicURL := s.security.Settings().PublicURL; publicURL != "" {
			endpointBaseURL = publicURL
		}
	}
	if endpointBaseURL == "" {
		endpointBaseURL = "http://" + request.Host
	}
	data.MCP = buildMCPPageData(
		endpointBaseURL+"/mcp",
		s.config.MCPToken,
		s.config.MCPCommand,
		strings.TrimRight(strings.TrimSpace(s.config.MCPBaseURL), "/"),
	)
	data.MCP.Shared = data.AuthMode != "" && data.AuthMode != string(security.ModeLocal)
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("Referrer-Policy", "no-referrer")
	response.Header().Set("X-Robots-Tag", "noindex, nofollow")
	s.render(response, "mcp-setup", data)
}

func (s *Server) apiWiki(response http.ResponseWriter, request *http.Request) {
	repositoryID, err := requiredRepositoryID(request.URL.Query().Get("repository"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	site, err := s.docs.Plan(request.Context(), repositoryID)
	if err != nil {
		writeDocumentationError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, site)
}

func (s *Server) apiWikiPage(response http.ResponseWriter, request *http.Request) {
	repositoryID, err := requiredRepositoryID(request.PathValue("repositoryID"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	page, err := s.docs.Page(request.Context(), repositoryID, strings.TrimSpace(request.PathValue("page")))
	if err != nil {
		writeDocumentationError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, page)
}

func (s *Server) generateWiki(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, 16<<10)
	var input struct {
		RepositoryID int64  `json:"repository_id"`
		Page         string `json:"page"`
		Refresh      bool   `json:"refresh"`
		SurveyOnly   bool   `json:"survey_only"`
		PlanOnly     bool   `json:"plan_only"`
		Preset       string `json:"preset"`
		Provider     string `json:"provider"`
		Model        string `json:"model"`
		Effort       string `json:"effort"`
		Timeout      int    `json:"timeout_seconds"`
		TokenBudget  int64  `json:"token_budget"`
	}
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("invalid documentation generation request"))
		return
	}
	if input.RepositoryID <= 0 {
		writeAPIError(response, http.StatusBadRequest, errors.New("repository_id must be a positive integer"))
		return
	}
	site, err := s.docs.Generate(request.Context(), docs.GenerateRequest{
		RepositoryID: input.RepositoryID,
		Page:         strings.TrimSpace(input.Page),
		Refresh:      input.Refresh,
		SurveyOnly:   input.SurveyOnly,
		PlanOnly:     input.PlanOnly,
		Preset:       strings.TrimSpace(input.Preset),
		Provider:     strings.TrimSpace(input.Provider),
		Model:        strings.TrimSpace(input.Model),
		Effort:       strings.TrimSpace(input.Effort),
		Timeout:      input.Timeout,
		TokenBudget:  input.TokenBudget,
	})
	if err != nil {
		writeDocumentationError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, site)
}

func (s *Server) exportWiki(response http.ResponseWriter, request *http.Request) {
	repositoryID, err := requiredRepositoryID(request.URL.Query().Get("repository"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	content, fileName, err := s.docs.Export(request.Context(), repositoryID)
	if err != nil {
		writeDocumentationError(response, err)
		return
	}
	response.Header().Set("Content-Type", "application/zip")
	response.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, fileName))
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(content)
}

func (s *Server) apiMap(response http.ResponseWriter, request *http.Request) {
	repositoryID, err := optionalRepositoryID(request.URL.Query().Get("repository"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	refresh := request.URL.Query().Get("refresh") == "true"
	principal, allowed := s.requirePermission(response, request, func() identity.Permission {
		if refresh {
			return identity.PermissionManageArtifacts
		}
		return identity.PermissionReadRepositories
	}())
	if !allowed {
		return
	}
	snapshot, err := s.maps.Snapshot(request.Context(), repositoryID, refresh)
	if err != nil {
		slog.Error("build repository map", "repository_id", repositoryID, "error", err)
		writeAPIError(response, http.StatusInternalServerError, errors.New("repository map could not be built"))
		return
	}
	if refresh {
		s.recordApplicationEvent(request, principal, "generation.map", "map", strconv.FormatInt(repositoryID, 10), "success", nil)
	}
	writeJSON(response, http.StatusOK, snapshot)
}

func (s *Server) apiGraphQuery(response http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(response, request.Body, 64<<10)
	var input struct {
		RepositoryID int64 `json:"repository_id,omitempty"`
		graph.QueryRequest
	}
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("invalid graph query request"))
		return
	}
	if input.RepositoryID < 0 {
		writeAPIError(response, http.StatusBadRequest, errors.New("repository_id must be positive when provided"))
		return
	}
	snapshot, err := s.maps.Snapshot(request.Context(), input.RepositoryID, false)
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, errors.New("graph snapshot could not be loaded"))
		return
	}
	result, err := graph.QueryGraph(snapshot, input.QueryRequest)
	if err != nil {
		writeJSON(response, http.StatusUnprocessableEntity, map[string]any{
			"error":  err.Error(),
			"result": result,
		})
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func (s *Server) apiDependencies(response http.ResponseWriter, request *http.Request) {
	repositoryID, err := optionalRepositoryID(request.URL.Query().Get("repository"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	options, err := dependencyOptions(request)
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	snapshot, progress, err := s.maps.ReadDependencySnapshot(request.Context(), repositoryID)
	if err != nil {
		slog.Error("build dependency inventory", "repository_id", repositoryID, "error", err)
		writeAPIError(response, http.StatusInternalServerError, errors.New("dependency inventory could not be built"))
		return
	}
	inventory, err := s.dependencyInventory(request.Context(), snapshot, options)
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, errors.New("dependency observations could not be loaded"))
		return
	}
	inventory.BuildProgress = progress
	status := http.StatusOK
	if progress.State == "building" {
		status = http.StatusAccepted
		response.Header().Set("Retry-After", "2")
	}
	writeJSON(response, status, inventory)
}

func (s *Server) apiDependencyTopology(response http.ResponseWriter, request *http.Request) {
	if s.dependencies == nil {
		writeAPIError(response, http.StatusServiceUnavailable, errors.New("distributed topology service is unavailable"))
		return
	}
	repositoryID, err := optionalRepositoryID(request.URL.Query().Get("repository"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	options, err := dependencyTopologyOptions(request)
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	snapshot, progress, err := s.maps.ReadTopologySnapshot(request.Context(), repositoryID)
	if err != nil {
		slog.Error("compose distributed topology", "repository_id", repositoryID, "error", err)
		writeAPIError(response, http.StatusInternalServerError, errors.New("distributed topology could not be built"))
		return
	}
	topology, err := s.dependencies.Topology(request.Context(), snapshot, progress, options)
	if err != nil {
		slog.Error("join runtime topology observations", "repository_id", repositoryID, "error", err)
		writeAPIError(response, http.StatusInternalServerError, errors.New("runtime topology observations could not be loaded"))
		return
	}
	topology = dependencies.SanitizeTopology(topology)
	status := http.StatusOK
	if progress.State == "building" {
		status = http.StatusAccepted
		response.Header().Set("Retry-After", "2")
	}
	writeJSON(response, status, topology)
}

func (s *Server) importDependencyTopology(response http.ResponseWriter, request *http.Request) {
	if s.dependencies == nil {
		writeAPIError(response, http.StatusServiceUnavailable, errors.New("distributed topology service is unavailable"))
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 4<<20)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var input dependencies.TopologyImportRequest
	if err := decoder.Decode(&input); err != nil {
		writeAPIError(response, http.StatusBadRequest, fmt.Errorf("decode runtime topology observations: %w", err))
		return
	}
	result, err := s.dependencies.ImportTopologyObservations(request.Context(), input)
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	writeJSON(response, http.StatusCreated, result)
}

func (s *Server) dependencyInventory(
	ctx context.Context,
	snapshot graph.Snapshot,
	options dependencies.Options,
) (dependencies.Inventory, error) {
	if s.dependencies == nil {
		return dependencies.BuildPage(snapshot, options), nil
	}
	return s.dependencies.Inventory(ctx, snapshot, options)
}

func (s *Server) refreshDependencies(response http.ResponseWriter, request *http.Request) {
	repositoryID, err := optionalRepositoryID(request.URL.Query().Get("repository"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	options, err := dependencyOptions(request)
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	snapshot, _, err := s.maps.ReadDependencySnapshot(request.Context(), repositoryID)
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, errors.New("dependency inventory could not be built"))
		return
	}
	force, err := optionalBool(request.URL.Query().Get("force"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("force must be true or false"))
		return
	}
	progress, err := s.dependencies.StartRefresh(snapshot, options, force)
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, errors.New("dependency refresh could not be started"))
		return
	}
	writeJSON(response, http.StatusAccepted, progress)
}

func (s *Server) dependencyRefreshProgress(response http.ResponseWriter, _ *http.Request) {
	writeJSON(response, http.StatusOK, s.dependencies.Progress())
}

func (s *Server) apiDependencyFindings(response http.ResponseWriter, request *http.Request) {
	repositoryID, err := optionalRepositoryID(request.URL.Query().Get("repository"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	options, err := dependencyAdvisoryOptions(request)
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	snapshot, progress, err := s.maps.ReadDependencySnapshot(request.Context(), repositoryID)
	if err != nil {
		slog.Error("build dependency inventory for findings", "repository_id", repositoryID, "error", err)
		writeAPIError(response, http.StatusInternalServerError, errors.New("dependency inventory could not be built"))
		return
	}
	findings, err := s.dependencies.Findings(request.Context(), snapshot, options)
	if err != nil {
		slog.Error("join dependency advisories", "repository_id", repositoryID, "error", err)
		writeAPIError(response, http.StatusInternalServerError, errors.New("dependency findings could not be loaded"))
		return
	}
	status := http.StatusOK
	if progress.State == "building" {
		status = http.StatusAccepted
		response.Header().Set("Retry-After", "2")
	}
	writeJSON(response, status, findings)
}

func (s *Server) refreshDependencyAdvisories(response http.ResponseWriter, request *http.Request) {
	repositoryID, err := optionalRepositoryID(request.URL.Query().Get("repository"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	force, err := optionalBool(request.URL.Query().Get("force"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, errors.New("force must be true or false"))
		return
	}
	snapshot, _, err := s.maps.ReadDependencySnapshot(request.Context(), repositoryID)
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, errors.New("dependency inventory could not be built"))
		return
	}
	progress, err := s.dependencies.StartAdvisoryRefresh(snapshot, force)
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, fmt.Errorf("advisory refresh could not be started: %w", err))
		return
	}
	writeJSON(response, http.StatusAccepted, progress)
}

func (s *Server) dependencyAdvisoryProgress(response http.ResponseWriter, _ *http.Request) {
	writeJSON(response, http.StatusOK, s.dependencies.AdvisoryProgress())
}

func (s *Server) exportDependencyFindingsSARIF(response http.ResponseWriter, request *http.Request) {
	repositoryID, err := optionalRepositoryID(request.URL.Query().Get("repository"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	options, err := dependencyAdvisoryOptions(request)
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	options.Offset = 0
	options.Limit = dependencies.MaximumFindingLimit
	snapshot, progress, err := s.maps.ReadDependencySnapshot(request.Context(), repositoryID)
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, errors.New("dependency inventory could not be built"))
		return
	}
	if progress.State == "building" {
		response.Header().Set("Retry-After", "2")
		writeAPIError(response, http.StatusConflict, errors.New("dependency inventory is still building"))
		return
	}
	all, err := s.dependencies.Findings(request.Context(), snapshot, options)
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, errors.New("dependency findings could not be loaded"))
		return
	}
	for all.HasMore {
		options.Offset = all.Offset + all.ReturnedCount
		page, err := s.dependencies.Findings(request.Context(), snapshot, options)
		if err != nil {
			writeAPIError(response, http.StatusInternalServerError, errors.New("dependency findings could not be loaded"))
			return
		}
		all.Findings = append(all.Findings, page.Findings...)
		all.ReturnedCount = len(all.Findings)
		all.HasMore = page.HasMore
		all.Offset = 0
		if len(all.Findings) > 50_000 {
			writeAPIError(response, http.StatusUnprocessableEntity, errors.New("SARIF export exceeds 50000 findings; narrow the filters"))
			return
		}
	}
	content, err := dependencies.FindingsSARIF(all, s.config.Version)
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, errors.New("dependency findings SARIF could not be generated"))
		return
	}
	response.Header().Set("Content-Type", "application/sarif+json")
	response.Header().Set("Content-Disposition", `attachment; filename="repokarta-dependency-findings.sarif"`)
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(content)
}

func (s *Server) apiArtifactProgress(response http.ResponseWriter, request *http.Request) {
	progress, err := s.maps.StructureProgress(request.Context(), 0)
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, errors.New("artifact progress could not be loaded"))
		return
	}
	writeJSON(response, http.StatusOK, progress)
}

func dependencyOptions(request *http.Request) (dependencies.Options, error) {
	query := request.URL.Query()
	options := dependencies.Options{
		Query:        query.Get("query"),
		Package:      query.Get("package"),
		Ecosystem:    query.Get("ecosystem"),
		Usage:        query.Get("usage"),
		Relationship: query.Get("relationship"),
		Resolution:   query.Get("resolution"),
		CheckStatus:  query.Get("check_status"),
		Distance:     query.Get("distance"),
		Limit:        dependencies.DefaultPageLimit,
	}
	if len(options.Query) > 200 || len(options.Package) > 200 ||
		len(options.Ecosystem) > 50 || len(options.Usage) > 50 ||
		len(options.Relationship) > 50 || len(options.Resolution) > 50 ||
		len(options.CheckStatus) > 50 || len(options.Distance) > 50 {
		return dependencies.Options{}, errors.New("dependency filters are too long")
	}
	for _, check := range []struct {
		name    string
		value   string
		allowed []string
	}{
		{"check_status", options.CheckStatus, []string{
			"current", "behind", "ahead", "prerelease", "unavailable",
			"private_internal", "unresolved", "registry_error", "stale", "unchecked",
		}},
		{"distance", options.Distance, []string{"major", "minor", "patch", "none", "unknown"}},
	} {
		value := strings.ToLower(strings.TrimSpace(check.value))
		if value != "" && !slices.Contains(check.allowed, value) {
			return dependencies.Options{}, fmt.Errorf(
				"%s must be one of %s", check.name, strings.Join(check.allowed, ", "),
			)
		}
	}
	if value := strings.TrimSpace(query.Get("offset")); value != "" {
		offset, err := strconv.Atoi(value)
		if err != nil || offset < 0 {
			return dependencies.Options{}, errors.New("offset must be a non-negative integer")
		}
		options.Offset = offset
	}
	if value := strings.TrimSpace(query.Get("limit")); value != "" {
		limit, err := strconv.Atoi(value)
		if err != nil || limit < 1 || limit > dependencies.MaximumPageLimit {
			return dependencies.Options{}, fmt.Errorf("limit must be between 1 and %d", dependencies.MaximumPageLimit)
		}
		options.Limit = limit
	}
	options.Query = strings.TrimSpace(options.Query)
	options.Package = strings.TrimSpace(options.Package)
	options.Ecosystem = strings.ToLower(strings.TrimSpace(options.Ecosystem))
	options.Usage = strings.ToLower(strings.TrimSpace(options.Usage))
	options.CheckStatus = strings.ToLower(strings.TrimSpace(options.CheckStatus))
	options.Distance = strings.ToLower(strings.TrimSpace(options.Distance))
	return options, nil
}

func dependencyTopologyOptions(request *http.Request) (dependencies.TopologyOptions, error) {
	query := request.URL.Query()
	options := dependencies.TopologyOptions{
		Query:       strings.TrimSpace(query.Get("query")),
		Protocol:    strings.ToLower(strings.TrimSpace(query.Get("protocol"))),
		Origin:      strings.ToLower(strings.TrimSpace(query.Get("origin"))),
		Environment: strings.TrimSpace(query.Get("environment")),
		Provider:    strings.TrimSpace(query.Get("provider")),
		Direction:   strings.ToLower(strings.TrimSpace(query.Get("direction"))),
		Depth:       1,
	}
	if options.Direction == "" {
		options.Direction = "both"
	}
	if len(options.Query) > 200 || len(options.Protocol) > 30 ||
		len(options.Origin) > 30 || len(options.Environment) > 80 ||
		len(options.Provider) > 80 || len(options.Direction) > 10 {
		return dependencies.TopologyOptions{}, errors.New("topology filters are too long")
	}
	if options.Protocol != "" && !slices.Contains(
		[]string{"http", "grpc", "kafka", "database", "mcp", "amqp", "unknown"},
		options.Protocol,
	) {
		return dependencies.TopologyOptions{}, errors.New("protocol filter is unsupported")
	}
	if options.Origin != "" && !slices.Contains(
		[]string{"static", "runtime", "confirmed"}, options.Origin,
	) {
		return dependencies.TopologyOptions{}, errors.New("origin must be static, runtime, or confirmed")
	}
	if !slices.Contains([]string{"both", "inbound", "outbound"}, options.Direction) {
		return dependencies.TopologyOptions{}, errors.New("direction must be both, inbound, or outbound")
	}
	if value := strings.TrimSpace(query.Get("depth")); value != "" {
		depth, err := strconv.Atoi(value)
		if err != nil || depth < 1 || depth > 2 {
			return dependencies.TopologyOptions{}, errors.New("depth must be 1 or 2")
		}
		options.Depth = depth
	}
	for key, target := range map[string]*time.Time{
		"observed_from": &options.ObservedFrom,
		"observed_to":   &options.ObservedTo,
	} {
		value := strings.TrimSpace(query.Get(key))
		if value == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return dependencies.TopologyOptions{}, fmt.Errorf("%s must be RFC3339", key)
		}
		*target = parsed.UTC()
	}
	if !options.ObservedFrom.IsZero() && !options.ObservedTo.IsZero() &&
		options.ObservedFrom.After(options.ObservedTo) {
		return dependencies.TopologyOptions{}, errors.New("observed_from must not be after observed_to")
	}
	return options, nil
}

func dependencyAdvisoryOptions(request *http.Request) (dependencies.AdvisoryOptions, error) {
	query := request.URL.Query()
	options := dependencies.AdvisoryOptions{
		Query: query.Get("query"), Ecosystem: query.Get("ecosystem"),
		Severity: query.Get("severity"), Usage: query.Get("usage"),
		Package: query.Get("package"), Limit: dependencies.DefaultFindingLimit,
	}
	if len(options.Query) > 200 || len(options.Ecosystem) > 50 ||
		len(options.Severity) > 50 || len(options.Usage) > 50 || len(options.Package) > 200 {
		return dependencies.AdvisoryOptions{}, errors.New("dependency finding filters are too long")
	}
	for _, check := range []struct {
		name    string
		value   string
		allowed []string
	}{
		{"ecosystem", options.Ecosystem, []string{"maven", "npm", "pypi", "go", "cargo", "nuget"}},
		{"severity", options.Severity, []string{"critical", "high", "medium", "low", "unknown"}},
		{"usage", options.Usage, []string{"production", "implementation", "test", "development", "build", "unknown"}},
	} {
		value := strings.ToLower(strings.TrimSpace(check.value))
		if value != "" && !slices.Contains(check.allowed, value) {
			return dependencies.AdvisoryOptions{}, fmt.Errorf(
				"%s must be one of %s", check.name, strings.Join(check.allowed, ", "),
			)
		}
	}
	if value := strings.TrimSpace(query.Get("offset")); value != "" {
		offset, err := strconv.Atoi(value)
		if err != nil || offset < 0 {
			return dependencies.AdvisoryOptions{}, errors.New("offset must be a non-negative integer")
		}
		options.Offset = offset
	}
	if value := strings.TrimSpace(query.Get("limit")); value != "" {
		limit, err := strconv.Atoi(value)
		if err != nil || limit < 1 || limit > dependencies.MaximumFindingLimit {
			return dependencies.AdvisoryOptions{}, fmt.Errorf(
				"limit must be between 1 and %d", dependencies.MaximumFindingLimit,
			)
		}
		options.Limit = limit
	}
	options.Query = strings.TrimSpace(options.Query)
	options.Ecosystem = strings.ToLower(strings.TrimSpace(options.Ecosystem))
	options.Severity = strings.ToLower(strings.TrimSpace(options.Severity))
	options.Usage = strings.ToLower(strings.TrimSpace(options.Usage))
	options.Package = strings.TrimSpace(options.Package)
	return options, nil
}

func dependencyURL(base string, repositoryID int64, options dependencies.Options, offset int) string {
	query := url.Values{}
	if base == "/dependencies" {
		query.Set("view", "inventory")
	}
	if repositoryID > 0 {
		query.Set("repository", strconv.FormatInt(repositoryID, 10))
	}
	if value := strings.TrimSpace(options.Query); value != "" {
		query.Set("query", value)
	}
	if value := strings.TrimSpace(options.Package); value != "" {
		query.Set("package", value)
	}
	if value := strings.TrimSpace(options.Ecosystem); value != "" {
		query.Set("ecosystem", value)
	}
	if value := strings.TrimSpace(options.Usage); value != "" {
		query.Set("usage", value)
	}
	if value := strings.TrimSpace(options.Relationship); value != "" {
		query.Set("relationship", value)
	}
	if value := strings.TrimSpace(options.Resolution); value != "" {
		query.Set("resolution", value)
	}
	if value := strings.TrimSpace(options.CheckStatus); value != "" {
		query.Set("check_status", value)
	}
	if value := strings.TrimSpace(options.Distance); value != "" {
		query.Set("distance", value)
	}
	query.Set("limit", strconv.Itoa(options.Limit))
	if offset > 0 {
		query.Set("offset", strconv.Itoa(offset))
	}
	return base + "?" + query.Encode()
}

func dependencyTopologyURL(
	base string,
	repositoryID int64,
	options dependencies.TopologyOptions,
) string {
	query := url.Values{}
	if repositoryID > 0 {
		query.Set("repository", strconv.FormatInt(repositoryID, 10))
	}
	for key, value := range map[string]string{
		"query": options.Query, "protocol": options.Protocol, "origin": options.Origin,
		"environment": options.Environment, "provider": options.Provider,
		"direction": options.Direction,
	} {
		if value = strings.TrimSpace(value); value != "" {
			query.Set(key, value)
		}
	}
	if !options.ObservedFrom.IsZero() {
		query.Set("observed_from", options.ObservedFrom.UTC().Format(time.RFC3339))
	}
	if !options.ObservedTo.IsZero() {
		query.Set("observed_to", options.ObservedTo.UTC().Format(time.RFC3339))
	}
	if options.Depth > 0 {
		query.Set("depth", strconv.Itoa(options.Depth))
	}
	if encoded := query.Encode(); encoded != "" {
		return base + "?" + encoded
	}
	return base
}

func dependencyAdvisoryURL(
	base string,
	repositoryID int64,
	options dependencies.AdvisoryOptions,
	offset int,
	view bool,
) string {
	query := url.Values{}
	if repositoryID > 0 {
		query.Set("repository", strconv.FormatInt(repositoryID, 10))
	}
	if view {
		query.Set("view", "findings")
	}
	if value := strings.TrimSpace(options.Query); value != "" {
		query.Set("query", value)
	}
	if value := strings.TrimSpace(options.Ecosystem); value != "" {
		query.Set("ecosystem", value)
	}
	if value := strings.TrimSpace(options.Severity); value != "" {
		query.Set("severity", value)
	}
	if value := strings.TrimSpace(options.Usage); value != "" {
		query.Set("usage", value)
	}
	if value := strings.TrimSpace(options.Package); value != "" {
		query.Set("package", value)
	}
	if options.Limit > 0 && options.Limit != dependencies.DefaultFindingLimit {
		query.Set("limit", strconv.Itoa(options.Limit))
	}
	if offset > 0 {
		query.Set("offset", strconv.Itoa(offset))
	}
	if encoded := query.Encode(); encoded != "" {
		return base + "?" + encoded
	}
	return base
}

func (s *Server) exportMap(response http.ResponseWriter, request *http.Request) {
	repositoryID, err := optionalRepositoryID(request.URL.Query().Get("repository"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err)
		return
	}
	snapshot, err := s.maps.Snapshot(request.Context(), repositoryID, false)
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, errors.New("repository map could not be exported"))
		return
	}
	content, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, errors.New("repository map could not be encoded"))
		return
	}
	fileName := "repokarta-map-all.json"
	if repositoryID > 0 && len(snapshot.Repositories) == 1 {
		fileName = "repokarta-map-" + safeDownloadName(snapshot.Repositories[0].Name) + ".json"
	}
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, fileName))
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(content)
}

func (s *Server) repositoryList(response http.ResponseWriter, request *http.Request) {
	data, err := s.pageData(request.Context())
	if err != nil {
		http.Error(response, "Could not load repositories", http.StatusInternalServerError)
		return
	}
	// The fragment carries out-of-band copies of the header health chip and the
	// drawer metric tiles so a catalogue change updates all three together.
	s.render(response, "repository-list-fragment", data)
}

func (s *Server) refreshRepositories(response http.ResponseWriter, request *http.Request) {
	if err := s.refresher.Refresh(request.Context()); err != nil {
		slog.Error("refresh repository catalogue", "error", err)
		http.Error(response, "Could not refresh repositories", http.StatusInternalServerError)
		return
	}
	s.repositoryList(response, request)
}

func (s *Server) search(response http.ResponseWriter, request *http.Request) {
	data, err := s.pageData(request.Context())
	if err != nil {
		http.Error(response, "Could not load repositories", http.StatusInternalServerError)
		return
	}

	repositoryID, repositoryName := repositorySelector(request.URL.Query().Get("repo"))
	for _, repository := range data.Repositories {
		if repository.ID == repositoryID {
			repositoryName = repository.Name
			break
		}
	}
	mode := strings.TrimSpace(request.URL.Query().Get("mode"))
	if mode == "" {
		mode = "zoekt"
	}
	query := search.Query{
		Text:       strings.TrimSpace(request.URL.Query().Get("q")),
		Repository: repositoryName,
		Language:   strings.TrimSpace(request.URL.Query().Get("lang")),
		Path:       strings.TrimSpace(request.URL.Query().Get("path")),
		File:       strings.TrimSpace(request.URL.Query().Get("file")),
		Mode:       mode,
		Limit:      parseSearchLimit(request.URL.Query().Get("limit")),
	}
	data.Search.Query = query
	data.Search.SelectedRepositoryID = repositoryID
	data.Search.Performed = query.Text != ""
	if data.Search.Performed {
		input := codeintel.SearchRequest{
			Query:        query.Text,
			RepositoryID: repositoryID,
			Repository:   query.Repository,
			Language:     query.Language,
			Path:         query.Path,
			File:         query.File,
			Mode:         query.Mode,
			Limit:        query.Limit,
		}
		result, searchError := s.intelligence.Search(request.Context(), input)
		if searchError != nil {
			data.Search.Error = searchError.Error()
		} else {
			if err := s.intelligence.RecordRecentSearch(request.Context(), input, result); err != nil {
				slog.Warn("record recent HTML search", "error", err)
			}
			data.Search.Duration = formatMilliseconds(result.DurationMS)
			data.Search.MatchCount = result.MatchCount
			data.Search.FileCount = result.MatchingFiles
			data.Search.EstimatedFiles = result.EstimatedTotalFiles
			data.Search.ReturnedFiles = result.ReturnedFiles
			data.Search.ReturnedItems = result.ReturnedItems
			data.Search.Limit = result.Limit
			data.Search.TotalFilesExact = result.TotalFilesExact
			data.Search.FilesSkipped = result.FilesSkipped
			data.Search.ShardsSkipped = result.ShardsSkipped
			data.Search.Warnings = result.Warnings
			data.Search.Truncated = result.Truncated
			data.Search.ResultType = result.ResultType
			data.Search.Matches = resolveSearchViews(result.Matches, data.Repositories)
			data.Search.Items = result.Items
			data.Search.Facets = result.Facets
			data.Search.FacetCoverage = result.FacetCoverage
		}
	}

	if request.Header.Get("HX-Request") == "true" {
		s.render(response, "search-results", data)
		return
	}
	s.render(response, "index", data)
}

func (s *Server) project(response http.ResponseWriter, request *http.Request) {
	repositoryID, err := strconv.ParseInt(request.PathValue("repositoryID"), 10, 64)
	if err != nil || repositoryID <= 0 {
		http.Error(response, "Invalid repository", http.StatusBadRequest)
		return
	}
	offset, err := nonNegativeInteger(request.URL.Query().Get("offset"), "offset")
	if err != nil {
		http.Error(response, err.Error(), http.StatusBadRequest)
		return
	}
	repository, err := s.intelligence.RepositoryByID(request.Context(), repositoryID)
	if err != nil {
		http.NotFound(response, request)
		return
	}
	tree, err := s.intelligence.ListTree(request.Context(), codeintel.TreeRequest{
		RepositoryID: repositoryID,
		Revision:     request.URL.Query().Get("rev"),
		Path:         request.URL.Query().Get("path"),
		Offset:       offset,
	})
	if err != nil {
		switch {
		case errors.Is(err, source.ErrUnsafePath), errors.Is(err, source.ErrUnknownRevision):
			http.Error(response, "Invalid project path or revision", http.StatusBadRequest)
		default:
			http.Error(response, "Could not open project directory", http.StatusNotFound)
		}
		return
	}
	base, err := s.pageData(request.Context())
	if err != nil {
		http.Error(response, "Could not load project", http.StatusInternalServerError)
		return
	}
	base.ActivePage = "project"
	previousURL := ""
	if tree.Offset > 0 {
		previousURL = projectDirectoryURL(repositoryID, tree.Revision, tree.Path, max(0, tree.Offset-tree.Limit))
	}
	nextURL := ""
	if tree.NextOffset > 0 {
		nextURL = projectDirectoryURL(repositoryID, tree.Revision, tree.Path, tree.NextOffset)
	}
	firstEntry, lastEntry := 0, 0
	if len(tree.Entries) > 0 {
		firstEntry = tree.Offset + 1
		lastEntry = tree.Offset + len(tree.Entries)
	}
	s.render(response, "project", projectPageData{
		pageData:    base,
		Repository:  repository,
		Revision:    tree.Revision,
		Path:        tree.Path,
		Breadcrumbs: projectBreadcrumbs(repositoryID, repository.Name, tree.Revision, tree.Path, false),
		Entries:     projectEntryViews(repositoryID, tree.Revision, tree.Entries, ""),
		PreviousURL: previousURL,
		NextURL:     nextURL,
		FirstEntry:  firstEntry,
		LastEntry:   lastEntry,
	})
}

func (s *Server) source(response http.ResponseWriter, request *http.Request) {
	repositoryID, err := strconv.ParseInt(request.PathValue("repositoryID"), 10, 64)
	if err != nil || repositoryID <= 0 {
		http.NotFound(response, request)
		return
	}
	repository, err := s.intelligence.RepositoryByID(request.Context(), repositoryID)
	if err != nil {
		http.NotFound(response, request)
		return
	}

	startLine, endLine := parseLineRange(request.URL.Query().Get("lines"))
	focusStart, focusEnd := parseFocusRange(request.URL.Query().Get("focus"))
	if focusStart > 0 && (focusStart < startLine || focusEnd > endLine) {
		startLine, endLine = codeintel.SourceWindow(focusStart, focusEnd)
	}
	file, err := source.OpenFile(
		request.Context(),
		repository,
		request.URL.Query().Get("rev"),
		request.URL.Query().Get("path"),
		startLine,
		endLine,
	)
	if err != nil {
		switch {
		case errors.Is(err, source.ErrUnsafePath), errors.Is(err, source.ErrUnknownRevision):
			http.Error(response, "Invalid source citation", http.StatusBadRequest)
		case errors.Is(err, source.ErrUnsupportedFile):
			http.Error(response, "This file cannot be displayed safely", http.StatusUnsupportedMediaType)
		default:
			slog.Error("open source file", "repository", repository.Name, "error", err)
			http.Error(response, "Could not open source file", http.StatusNotFound)
		}
		return
	}

	previousStart, previousEnd := 0, 0
	if file.StartLine > 1 {
		previousEnd = file.StartLine - 1
		previousStart = max(1, previousEnd-(maximumSourceLines-1))
	}
	nextStart, nextEnd := 0, 0
	if file.EndLine < file.TotalLines {
		nextStart = file.EndLine + 1
		nextEnd = min(file.TotalLines, nextStart+(maximumSourceLines-1))
	}
	if focusStart < file.StartLine || focusStart > file.EndLine {
		focusStart, focusEnd = 0, 0
	} else {
		focusEnd = min(focusEnd, file.EndLine)
	}
	citationStart, citationEnd := file.StartLine, file.EndLine
	if focusStart > 0 {
		citationStart, citationEnd = focusStart, focusEnd
	}
	directory := path.Dir(file.Path)
	if directory == "." {
		directory = ""
	}
	tree, treeErr := s.intelligence.ListTree(request.Context(), codeintel.TreeRequest{
		RepositoryID: repositoryID,
		Revision:     file.Revision,
		Path:         directory,
	})
	var treeEntries []projectEntryView
	if treeErr == nil {
		treeEntries = projectEntryViews(repositoryID, file.Revision, tree.Entries, file.Path)
	}

	data := sourcePageData{
		Version:       s.config.Version,
		ChatEnabled:   s.agents != nil,
		File:          file,
		ProjectURL:    projectDirectoryURL(repositoryID, file.Revision, directory, 0),
		Breadcrumbs:   projectBreadcrumbs(repositoryID, repository.Name, file.Revision, file.Path, true),
		TreeEntries:   treeEntries,
		RemoteURL:     remoteFileURL(repository.OriginURL, file.Revision, file.Path, citationStart, citationEnd),
		Citation:      fmt.Sprintf("%s@%s:%s#L%d-L%d", repository.Name, shortCommit(file.Revision), file.Path, citationStart, citationEnd),
		PreviousStart: previousStart,
		PreviousEnd:   previousEnd,
		NextStart:     nextStart,
		NextEnd:       nextEnd,
		FocusStart:    focusStart,
		FocusEnd:      focusEnd,
		Intelligence: s.sourceIntelligence(
			request.Context(),
			repositoryID,
			file.Revision,
			file.Path,
			file.StartLine,
			file.EndLine,
		),
	}
	s.render(response, "source", data)
}

func (s *Server) sourceIntelligence(
	ctx context.Context,
	repositoryID int64,
	revision, filePath string,
	startLine, endLine int,
) sourceIntelligenceView {
	view := sourceIntelligenceView{
		Routes:  []sourceRouteView{},
		Callers: []sourceCallerView{},
		State:   "ready",
		TopologyURL: fmt.Sprintf(
			"/dependencies?repository=%d&protocol=http&direction=inbound&depth=1",
			repositoryID,
		),
	}
	if s.maps == nil {
		view.State = "unavailable"
		view.Message = "Route and caller artifacts are unavailable in this runtime."
		return view
	}

	routeSnapshot, routeProgress, err := s.maps.ReadRouteSnapshot(ctx, repositoryID)
	if err != nil {
		view.State = "unavailable"
		view.Message = "Route artifacts could not be read."
		return view
	}
	for _, node := range routeSnapshot.Nodes {
		if node.Kind != "route" {
			continue
		}
		evidence, ok := routeEvidenceForFile(node, repositoryID, filePath)
		if !ok {
			continue
		}
		if evidence.URL == "" {
			evidenceRevision := evidence.Revision
			if evidenceRevision == "" {
				evidenceRevision = revision
			}
			evidence.URL = sourceEvidenceURL(
				repositoryID,
				evidenceRevision,
				filePath,
				evidence.Line,
			)
		}
		view.Routes = append(view.Routes, sourceRouteView{
			Label:         node.Label,
			Line:          max(1, evidence.Line),
			URL:           evidence.URL,
			VisibleWindow: evidence.Line >= startLine && evidence.Line <= endLine,
		})
	}
	sort.Slice(view.Routes, func(left, right int) bool {
		if view.Routes[left].VisibleWindow != view.Routes[right].VisibleWindow {
			return view.Routes[left].VisibleWindow
		}
		if view.Routes[left].Line != view.Routes[right].Line {
			return view.Routes[left].Line < view.Routes[right].Line
		}
		return view.Routes[left].Label < view.Routes[right].Label
	})
	view.RouteCount = len(view.Routes)
	if len(view.Routes) > maximumSourceRoutes {
		view.OmittedRoutes = len(view.Routes) - maximumSourceRoutes
		view.Routes = view.Routes[:maximumSourceRoutes]
	}
	if routeProgress.State == "building" || !routeSnapshot.Scope.Complete {
		view.Partial = true
		view.State = "building"
	}
	if len(view.Routes) == 0 {
		if view.Partial {
			view.Message = "Route artifacts are still building; endpoints in this file may not be available yet."
		} else {
			view.Message = "No supported HTTP route declaration was detected in this file."
		}
		return view
	}
	if s.dependencies == nil {
		view.State = "unavailable"
		view.Message = "Routes were detected, but caller topology is unavailable in this runtime."
		return view
	}

	topologySnapshot, topologyProgress, err := s.maps.ReadTopologySnapshot(ctx, repositoryID)
	if err != nil {
		view.State = "unavailable"
		view.Message = "Routes were detected, but caller topology could not be read."
		return view
	}
	topology, err := s.dependencies.Topology(
		ctx,
		topologySnapshot,
		topologyProgress,
		dependencies.TopologyOptions{Protocol: "http", Direction: "both", Depth: 1},
	)
	if err != nil {
		view.State = "unavailable"
		view.Message = "Routes were detected, but inbound callers could not be resolved."
		return view
	}
	routeComponentIDs := sourceComponentIDs(topology.Components, repositoryID, filePath)
	seenCallers := make(map[string]bool)
	routeCallers := make([]map[string]bool, len(view.Routes))
	for routeIndex := range routeCallers {
		routeCallers[routeIndex] = make(map[string]bool)
	}
	for _, connection := range topology.Connections {
		if !strings.EqualFold(connection.Protocol, "http") ||
			!routeComponentIDs[connection.Target] ||
			connection.Source == connection.Target {
			continue
		}
		caller := sourceCallerView{
			Name:       connection.SourceName,
			State:      connection.State,
			Confidence: connection.Confidence,
		}
		if len(connection.Evidence) > 0 {
			caller.Evidence = connection.Evidence[0]
		}
		key := strings.ToLower(caller.Name) + "\x00" + caller.State + "\x00" + caller.Evidence.URL
		if !seenCallers[key] {
			seenCallers[key] = true
			view.Callers = append(view.Callers, caller)
		}
		for routeIndex := range view.Routes {
			if routeMatchesCallerEvidence(view.Routes[routeIndex].Label, connection.Evidence) &&
				!routeCallers[routeIndex][key] {
				routeCallers[routeIndex][key] = true
				view.Routes[routeIndex].Callers = append(
					view.Routes[routeIndex].Callers,
					caller,
				)
			}
		}
	}
	sort.Slice(view.Callers, func(left, right int) bool {
		return strings.ToLower(view.Callers[left].Name) < strings.ToLower(view.Callers[right].Name)
	})
	for routeIndex := range view.Routes {
		sort.Slice(view.Routes[routeIndex].Callers, func(left, right int) bool {
			return strings.ToLower(view.Routes[routeIndex].Callers[left].Name) <
				strings.ToLower(view.Routes[routeIndex].Callers[right].Name)
		})
	}
	if topology.Partial || topologyProgress.State == "building" {
		view.Partial = true
		view.State = "building"
	}
	if len(view.Callers) == 0 {
		view.Message = "No inbound HTTP caller evidence is currently indexed for this service."
	} else {
		view.Message = "Callers are attributed at service level; route badges require a matching commit-pinned URL path."
	}
	return view
}

func sourceComponentIDs(
	components []dependencies.TopologyComponent,
	repositoryID int64,
	filePath string,
) map[string]bool {
	filePath = strings.Trim(strings.ReplaceAll(filePath, "\\", "/"), "/")
	selected := make(map[string]bool)
	longestRoot := -1
	for _, component := range components {
		if component.RepositoryID != repositoryID {
			continue
		}
		root := strings.Trim(strings.ReplaceAll(component.Path, "\\", "/"), "/")
		if root == "" || root == "." {
			root = ""
		}
		if root != "" && filePath != root && !strings.HasPrefix(filePath, root+"/") {
			continue
		}
		if len(root) < longestRoot {
			continue
		}
		if len(root) > longestRoot {
			clear(selected)
			longestRoot = len(root)
		}
		selected[component.ID] = true
	}
	return selected
}

func routeEvidenceForFile(
	node graph.Node,
	repositoryID int64,
	filePath string,
) (graph.Evidence, bool) {
	for _, evidence := range node.Evidence {
		if evidence.RepositoryID == repositoryID && evidence.Path == filePath {
			return evidence, true
		}
	}
	if node.RepositoryID == repositoryID && node.Path == filePath {
		if len(node.Evidence) > 0 {
			return node.Evidence[0], true
		}
		return graph.Evidence{
			RepositoryID: repositoryID,
			Path:         filePath,
			Line:         1,
			Label:        node.Label,
		}, true
	}
	return graph.Evidence{}, false
}

func sourceEvidenceURL(repositoryID int64, revision, filePath string, line int) string {
	line = max(1, line)
	values := url.Values{
		"rev":   []string{revision},
		"path":  []string{filePath},
		"focus": []string{fmt.Sprintf("%d-%d", line, line)},
	}
	return fmt.Sprintf("/source/%d?%s#L%d", repositoryID, values.Encode(), line)
}

func routeMatchesCallerEvidence(routeLabel string, evidence []graph.Evidence) bool {
	routePath := endpointPath(routeLabel)
	if routePath == "" {
		return false
	}
	for _, item := range evidence {
		if routePathMatches(routePath, endpointPath(item.Label)) {
			return true
		}
	}
	return false
}

func endpointPath(value string) string {
	value = strings.TrimSpace(value)
	if fields := strings.Fields(value); len(fields) > 1 &&
		strings.HasPrefix(fields[len(fields)-1], "/") {
		value = fields[len(fields)-1]
	}
	if parsed, err := url.Parse(value); err == nil && parsed.Path != "" {
		value = parsed.Path
	}
	if !strings.HasPrefix(value, "/") {
		return ""
	}
	value = strings.TrimSuffix(path.Clean(value), "/")
	if value == "" {
		return "/"
	}
	return value
}

func routePathMatches(routePath, evidencePath string) bool {
	if routePath == "" || evidencePath == "" {
		return false
	}
	routeParts := strings.Split(strings.Trim(routePath, "/"), "/")
	evidenceParts := strings.Split(strings.Trim(evidencePath, "/"), "/")
	if len(routeParts) != len(evidenceParts) {
		return false
	}
	for index, routePart := range routeParts {
		if (strings.HasPrefix(routePart, "{") && strings.HasSuffix(routePart, "}")) ||
			strings.HasPrefix(routePart, ":") ||
			routePart == "*" {
			continue
		}
		if routePart != evidenceParts[index] {
			return false
		}
	}
	return true
}

func (s *Server) events(response http.ResponseWriter, request *http.Request) {
	flusher, ok := response.(http.Flusher)
	if !ok {
		http.Error(response, "Streaming is not supported", http.StatusInternalServerError)
		return
	}
	response.Header().Set("Content-Type", "text/event-stream")
	response.Header().Set("Cache-Control", "no-cache")
	response.Header().Set("X-Accel-Buffering", "no")

	ticker := time.NewTicker(eventPollInterval)
	defer ticker.Stop()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	signature := ""
	for {
		select {
		case <-request.Context().Done():
			return
		case <-ticker.C:
			repositories, err := s.intelligence.CatalogRepositories(request.Context())
			if err != nil {
				return
			}
			nextSignature := repositorySignature(repositories)
			if nextSignature != signature {
				signature = nextSignature
				fmt.Fprint(response, "event: repositories\ndata: updated\n\n")
				flusher.Flush()
			}
		case <-heartbeat.C:
			fmt.Fprint(response, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) health(response http.ResponseWriter, _ *http.Request) {
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(map[string]string{
		"status":  "ok",
		"version": s.config.Version,
	})
}

func (s *Server) pageData(ctx context.Context) (pageData, error) {
	repositories, err := s.intelligence.CatalogRepositories(ctx)
	if err != nil {
		return pageData{}, err
	}
	data := pageData{
		Version:             s.config.Version,
		RepositoryRoot:      s.config.RepositoryRoot,
		Repositories:        repositories,
		RepositoryLabels:    catalog.DisplayNames(repositories),
		ActivePage:          "search",
		ChatEnabled:         s.agents != nil,
		WikiEnabled:         s.docs != nil,
		DependenciesEnabled: s.maps != nil,
		MCPEnabled:          s.config.MCPHandler != nil,
		InsightsEnabled:     s.insights != nil,
		CanManageArtifacts:  s.security == nil,
		Search: searchData{
			Query: search.Query{Limit: codeintel.DefaultSearchLimit},
		},
	}
	if s.scipJava != nil {
		data.SCIPJava = s.scipJava.ProviderStatus()
		data.SCIPJavaEnabled = data.SCIPJava.Enabled
	}
	if s.security != nil {
		data.AuthMode = string(s.security.Mode())
		data.AdminEnabled = s.security.AdminEnabled()
		if principal, ok := security.PrincipalFromContext(ctx); ok {
			data.CanAdminister = principal.Admin
			data.CanManageArtifacts = identity.Allows(principal.Role, identity.PermissionManageArtifacts)
			data.UserLabel = principal.Name
			if data.UserLabel == "" {
				data.UserLabel = principal.Email
			}
			if data.UserLabel == "" {
				data.UserLabel = principal.ID
			}
		}
	}
	for _, repository := range repositories {
		switch repository.IndexState {
		case "ready":
			data.ReadyCount++
		case "error":
			data.ErrorCount++
		case "empty":
			data.EmptyCount++
		default:
			data.PendingCount++
		}
		if repository.IndexState != "empty" {
			data.IndexableCount++
		}
	}
	if s.maps != nil {
		progress, progressErr := s.maps.StructureProgress(ctx, 0)
		if progressErr == nil {
			data.ArtifactProgress = progress
		}
	}
	return data, nil
}

func buildMCPPageData(endpoint, token, command, stdioBaseURL string) mcpPageData {
	command = strings.TrimSpace(command)
	if command == "" {
		command = "repokarta"
	}
	if stdioBaseURL == "" {
		stdioBaseURL = strings.TrimSuffix(endpoint, "/mcp")
	}
	httpConfiguration, _ := json.MarshalIndent(map[string]any{
		"mcpServers": map[string]any{
			"repokarta": struct {
				Type    string            `json:"type"`
				URL     string            `json:"url"`
				Headers map[string]string `json:"headers"`
			}{
				Type: "http",
				URL:  endpoint,
				Headers: map[string]string{
					"Authorization": "Bearer " + token,
				},
			},
		},
	}, "", "  ")
	stdioConfiguration, _ := json.MarshalIndent(map[string]any{
		"mcpServers": map[string]any{
			"repokarta": struct {
				Command string   `json:"command"`
				Args    []string `json:"args"`
			}{
				Command: command,
				Args:    []string{"mcp", "-url", stdioBaseURL},
			},
		},
	}, "", "  ")
	tokenPreview := token
	if len(token) > 20 {
		tokenPreview = token[:8] + "••••••••" + token[len(token)-8:]
	}
	catalog := mcpserver.ToolCatalog()
	tools := make([]mcpToolView, len(catalog))
	for index, tool := range catalog {
		tools[index] = mcpToolView{Name: tool.Name, Description: tool.Description}
	}
	return mcpPageData{
		Endpoint:       endpoint,
		Token:          token,
		TokenPreview:   tokenPreview,
		HTTPConfig:     string(httpConfiguration),
		HTTPConfigView: strings.ReplaceAll(string(httpConfiguration), "Bearer "+token, "Bearer <current-token>"),
		StdioConfig:    string(stdioConfiguration),
		Tools:          tools,
	}
}

func (s *Server) render(response http.ResponseWriter, name string, data any) {
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(response, name, data); err != nil {
		slog.Error("render template", "template", name, "error", err)
	}
}

func optionalRepositoryID(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	repositoryID, err := strconv.ParseInt(value, 10, 64)
	if err != nil || repositoryID <= 0 {
		return 0, errors.New("repository must be a positive integer")
	}
	return repositoryID, nil
}

func optionalBool(value string) (bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return false, nil
	}
	return strconv.ParseBool(value)
}

// repositorySelector accepts either the stable numeric repository ID returned
// by /api/repositories or a repository name. Numeric selectors are preferred
// because repository names are not unique across roots.
func repositorySelector(value string) (int64, string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, ""
	}
	if repositoryID, err := strconv.ParseInt(value, 10, 64); err == nil && repositoryID > 0 {
		return repositoryID, ""
	}
	return 0, value
}

func requiredRepositoryID(value string) (int64, error) {
	repositoryID, err := optionalRepositoryID(value)
	if err != nil {
		return 0, err
	}
	if repositoryID == 0 {
		return 0, errors.New("repository is required")
	}
	return repositoryID, nil
}

func writeDocumentationError(response http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, docs.ErrPageNotFound), errors.Is(err, sql.ErrNoRows):
		writeAPIError(response, http.StatusNotFound, errors.New("documentation page was not found"))
	case errors.Is(err, docs.ErrInvalidKnowledgePreset):
		writeAPIError(response, http.StatusUnprocessableEntity, err)
	case errors.Is(err, docs.ErrNothingToExport):
		// An empty Wiki is a normal state, not a server failure, and the
		// caller needs the actual reason.
		writeAPIError(response, http.StatusConflict, err)
	case strings.Contains(err.Error(), ".repokarta.yml"):
		writeAPIError(response, http.StatusUnprocessableEntity, err)
	case errors.Is(err, docs.ErrGenerationRejected):
		// A quality gate rejected the provider result. The reason is the whole
		// value of the message and is safe to return: it names sections, counts,
		// and page slugs, never filesystem paths or credentials. It is also
		// logged so a completed run can be diagnosed after the fact.
		slog.Warn("documentation generation rejected", "error", err)
		writeAPIError(response, http.StatusUnprocessableEntity, err)
	default:
		slog.Error("documentation request", "error", err)
		writeAPIError(response, http.StatusInternalServerError, errors.New("documentation request could not be completed"))
	}
}

func safeDownloadName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var output strings.Builder
	lastDash := false
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') {
			output.WriteRune(character)
			lastDash = false
		} else if !lastDash && output.Len() > 0 {
			output.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(output.String(), "-")
}

func resolveSearchViews(matches []codeintel.SearchMatch, repositories []catalog.Repository) []searchMatchView {
	views := make([]searchMatchView, 0, len(matches))
	for _, match := range matches {
		view := searchMatchView{
			ResultType: match.ResultType,
			Repository: match.Repository,
			Revision:   match.Revision,
			Path:       match.Path,
			Language:   match.Language,
			Ranking:    match.Ranking,
			Actions:    match.Actions,
			Lines:      make([]search.LineMatch, 0, len(match.Lines)),
		}
		if len(match.Lines) > 0 {
			view.FocusLine = match.Lines[0].Number
		}
		for _, line := range match.Lines {
			view.Lines = append(view.Lines, search.LineMatch{
				Number:              line.Number,
				Text:                line.Text,
				Before:              line.Before,
				After:               line.After,
				Fragments:           line.Fragments,
				ReferenceKind:       line.ReferenceKind,
				ReferenceTarget:     line.ReferenceTarget,
				ReferenceReceiver:   line.ReferenceReceiver,
				ReferenceConfidence: line.ReferenceConfidence,
			})
		}
		for _, repository := range repositories {
			if repository.IndexedCommit != match.Revision && repository.HeadCommit != match.Revision {
				continue
			}
			normalizedSearchName := strings.ReplaceAll(match.Repository, "\\", "/")
			if normalizedSearchName == repository.Name ||
				strings.HasSuffix(strings.ToLower(normalizedSearchName), "/"+strings.ToLower(repository.Name)) {
				view.RepositoryID = repository.ID
				view.Repository = repository.Name
				break
			}
		}
		views = append(views, view)
	}
	return views
}

func parseSearchLimit(value string) int {
	limit, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || limit <= 0 {
		return codeintel.DefaultSearchLimit
	}
	return min(limit, codeintel.MaximumSearchLimit)
}

func apiSearchLimit(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return codeintel.DefaultSearchLimit, nil
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit <= 0 || limit > codeintel.MaximumSearchLimit {
		return 0, fmt.Errorf("limit must be an integer from 1 to %d", codeintel.MaximumSearchLimit)
	}
	return limit, nil
}

func apiBoundedInteger(value, name string, fallback, maximum int) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 || parsed > maximum {
		return 0, fmt.Errorf("%s must be an integer from 1 to %d", name, maximum)
	}
	return parsed, nil
}

func writeCodeIntelligenceError(response http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, source.ErrUnsafePath), errors.Is(err, source.ErrUnknownRevision):
		writeAPIError(response, http.StatusBadRequest, err)
	case errors.Is(err, source.ErrUnsupportedFile):
		writeAPIError(response, http.StatusUnsupportedMediaType, err)
	default:
		writeAPIError(response, http.StatusNotFound, err)
	}
}

func writeAPIError(response http.ResponseWriter, status int, err error) {
	writeJSON(response, status, map[string]any{
		"error": map[string]string{
			"message": err.Error(),
		},
	})
}

func writeContextOrAPIError(response http.ResponseWriter, err error) {
	var resolutionError *contextscope.ResolutionError
	if errors.As(err, &resolutionError) {
		writeContextError(response, resolutionError)
		return
	}
	if errors.Is(err, contextscope.ErrNamedContextNotFound) ||
		errors.Is(err, contextscope.ErrNamedContextForbidden) ||
		errors.Is(err, contextscope.ErrNamedContextConflict) {
		writeNamedContextError(response, err)
		return
	}
	writeAPIError(response, http.StatusBadRequest, err)
}

func writeNamedContextError(response http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, contextscope.ErrNamedContextNotFound):
		writeAPIError(response, http.StatusNotFound, err)
	case errors.Is(err, contextscope.ErrNamedContextForbidden):
		writeAPIError(response, http.StatusForbidden, err)
	case errors.Is(err, contextscope.ErrNamedContextConflict):
		writeAPIError(response, http.StatusConflict, err)
	default:
		writeContextOrAPIErrorWithoutNamedContext(response, err)
	}
}

func writeSearchWorkspaceError(response http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, searchworkspace.ErrNotFound):
		writeAPIError(response, http.StatusNotFound, err)
	case errors.Is(err, searchworkspace.ErrForbidden):
		writeAPIError(response, http.StatusForbidden, err)
	case errors.Is(err, searchworkspace.ErrConflict):
		writeAPIError(response, http.StatusConflict, err)
	default:
		writeContextOrAPIError(response, err)
	}
}

func writeContextOrAPIErrorWithoutNamedContext(response http.ResponseWriter, err error) {
	var resolutionError *contextscope.ResolutionError
	if errors.As(err, &resolutionError) {
		writeContextError(response, resolutionError)
		return
	}
	writeAPIError(response, http.StatusBadRequest, err)
}

func writeContextError(response http.ResponseWriter, err error) {
	var resolutionError *contextscope.ResolutionError
	if !errors.As(err, &resolutionError) {
		writeAPIError(response, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(response, http.StatusUnprocessableEntity, map[string]any{
		"error": map[string]any{
			"message": resolutionError.Error(),
			"code":    "context_resolution_failed",
			"issues":  resolutionError.Issues,
		},
	})
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(status)
	if err := json.NewEncoder(response).Encode(value); err != nil {
		slog.Warn("encode JSON response", "error", err)
	}
}

func parseLineRange(value string) (int, int) {
	parts := strings.SplitN(value, "-", 2)
	start, _ := strconv.Atoi(parts[0])
	end := start + 199
	if len(parts) == 2 {
		if parsed, err := strconv.Atoi(parts[1]); err == nil {
			end = parsed
		}
	}
	if start <= 0 {
		start = 1
	}
	if end < start {
		end = start
	}
	if end-start+1 > maximumSourceLines {
		end = start + maximumSourceLines - 1
	}
	return start, end
}

func parseFocusRange(value string) (int, int) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, 0
	}
	parts := strings.SplitN(value, "-", 2)
	start, err := strconv.Atoi(parts[0])
	if err != nil || start <= 0 {
		return 0, 0
	}
	end := start
	if len(parts) == 2 {
		parsed, err := strconv.Atoi(parts[1])
		if err != nil || parsed < start {
			return 0, 0
		}
		end = parsed
	}
	if end-start+1 > maximumSourceLines {
		end = start + maximumSourceLines - 1
	}
	return start, end
}

func highlightLine(line search.LineMatch) template.HTML {
	if len(line.Fragments) == 0 {
		return template.HTML(template.HTMLEscapeString(line.Text))
	}
	fragments := append([]search.Fragment(nil), line.Fragments...)
	sort.Slice(fragments, func(left, right int) bool {
		return fragments[left].Start < fragments[right].Start
	})

	var output strings.Builder
	position := 0
	for _, fragment := range fragments {
		if fragment.Start < position || fragment.Start < 0 || fragment.End > len(line.Text) || fragment.End < fragment.Start {
			continue
		}
		output.WriteString(template.HTMLEscapeString(line.Text[position:fragment.Start]))
		output.WriteString(`<mark class="search-highlight">`)
		output.WriteString(template.HTMLEscapeString(line.Text[fragment.Start:fragment.End]))
		output.WriteString("</mark>")
		position = fragment.End
	}
	output.WriteString(template.HTMLEscapeString(line.Text[position:]))
	return template.HTML(output.String())
}

func fragmentRanges(line search.LineMatch) string {
	values := make([]string, 0, len(line.Fragments))
	for _, fragment := range line.Fragments {
		if fragment.Start < 0 || fragment.End <= fragment.Start || fragment.End > len(line.Text) {
			continue
		}
		start := len(utf16.Encode([]rune(line.Text[:fragment.Start])))
		end := start + len(utf16.Encode([]rune(line.Text[fragment.Start:fragment.End])))
		values = append(values, strconv.Itoa(start)+":"+strconv.Itoa(end))
	}
	return strings.Join(values, ",")
}

func formatDuration(duration time.Duration) string {
	if duration < time.Millisecond {
		return "<1 ms"
	}
	return fmt.Sprintf("%d ms", duration.Milliseconds())
}

func formatMilliseconds(milliseconds float64) string {
	if milliseconds < 1 {
		return "<1 ms"
	}
	return fmt.Sprintf("%.0f ms", milliseconds)
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return "Not yet"
	}
	return value.Local().Format("Jan 2, 15:04")
}

func shortCommit(commit string) string {
	if len(commit) <= 8 {
		return commit
	}
	return commit[:8]
}

func statusLabel(state string) string {
	switch state {
	case "ready":
		return "Indexed"
	case "indexing":
		return "Indexing"
	case "error":
		return "Needs attention"
	case "empty":
		return "Empty"
	default:
		return "Queued"
	}
}

func scipStatusLabel(state string) string {
	switch state {
	case "ready":
		return "Precise"
	case "indexing":
		return "Compiling"
	case "pending":
		return "Queued"
	case "failed":
		return "Failed"
	case "unavailable":
		return "Unavailable"
	case "skipped":
		return "Not applicable"
	default:
		return "Not generated"
	}
}

func scipFailureLabel(category string) string {
	switch category {
	case scipjava.FailureEnvironment:
		return "Environment"
	case scipjava.FailureJDKIncompatibleWrapper:
		return "JDK / Gradle compatibility"
	case scipjava.FailureCompileError:
		return "Compilation"
	default:
		return "Build"
	}
}

// staticAssets serves the embedded frontend with a build-derived validator.
//
// Asset paths are unversioned (/assets/app.js, /assets/app.css) and embed.FS
// reports a zero modification time, so http.ServeContent emitted no ETag, no
// Last-Modified, and no Cache-Control. A browser was therefore free to keep
// serving a previous build's JavaScript and CSS against freshly rendered HTML
// after an upgrade, which presents as a badly broken page rather than as a
// caching problem. Revalidating on every load costs nothing over loopback.
func staticAssets(dist fs.FS) http.Handler {
	tag := buildETag(dist)
	files := http.FileServer(http.FS(dist))
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if tag != "" {
			response.Header().Set("ETag", tag)
		}
		response.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(response, request)
	})
}

// buildETag hashes every embedded asset once at startup so all assets from one
// build share a validator. An empty result disables conditional requests rather
// than failing to serve.
func buildETag(dist fs.FS) string {
	digest := sha256.New()
	err := fs.WalkDir(dist, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		content, readErr := fs.ReadFile(dist, path)
		if readErr != nil {
			return readErr
		}
		fmt.Fprintf(digest, "%s:%d:", path, len(content))
		digest.Write(content)
		return nil
	})
	if err != nil {
		slog.Warn("compute asset etag", "error", err)
		return ""
	}
	return fmt.Sprintf("%q", hex.EncodeToString(digest.Sum(nil))[:32])
}

// nextSearchLimit is the file limit a "Show more" control should request after
// a truncated result set. It roughly doubles the current limit and stops at the
// service ceiling so the control never offers a limit the API would reject.
func nextSearchLimit(limit int) int {
	if limit < 1 {
		limit = codeintel.DefaultSearchLimit
	}
	next := limit * 2
	if next > codeintel.MaximumSearchLimit {
		next = codeintel.MaximumSearchLimit
	}
	return next
}

// indexProgress reports how far first-run indexing has advanced, as a
// percentage suitable for a progress bar width. It is clamped so a catalogue
// that changes size mid-scan cannot produce an out-of-range bar.
func indexProgress(ready, total int) int {
	if total <= 0 {
		return 0
	}
	percent := ready * 100 / total
	return min(100, max(0, percent))
}

func repositorySignature(repositories []catalog.Repository) string {
	var builder strings.Builder
	for _, repository := range repositories {
		fmt.Fprintf(
			&builder,
			"%d:%s:%s:%s:%s:%s:%s;",
			repository.ID,
			repository.HeadCommit,
			repository.ScanState,
			repository.ScanError,
			repository.IndexState,
			repository.IndexError,
			func() string {
				if repository.SCIPJava == nil {
					return ""
				}
				return repository.SCIPJava.State + ":" +
					repository.SCIPJava.Revision + ":" +
					repository.SCIPJava.FailureCategory + ":" +
					repository.SCIPJava.FailureSummary + ":" +
					repository.SCIPJava.Error
			}(),
		)
	}
	return builder.String()
}

func projectDirectoryURL(repositoryID int64, revision, directory string, offset int) string {
	values := url.Values{}
	if revision != "" {
		values.Set("rev", revision)
	}
	if directory != "" {
		values.Set("path", directory)
	}
	if offset > 0 {
		values.Set("offset", strconv.Itoa(offset))
	}
	target := "/projects/" + strconv.FormatInt(repositoryID, 10)
	if encoded := values.Encode(); encoded != "" {
		target += "?" + encoded
	}
	return target
}

func projectSourceURL(repositoryID int64, revision, filePath string) string {
	values := url.Values{
		"rev":   {revision},
		"path":  {filePath},
		"lines": {"1-200"},
	}
	return "/source/" + strconv.FormatInt(repositoryID, 10) + "?" + values.Encode()
}

func projectBreadcrumbs(
	repositoryID int64,
	repositoryName, revision, currentPath string,
	includeFile bool,
) []projectBreadcrumbView {
	breadcrumbs := []projectBreadcrumbView{{
		Label: repositoryName,
		URL:   projectDirectoryURL(repositoryID, revision, "", 0),
	}}
	parts := strings.Split(strings.Trim(currentPath, "/"), "/")
	if len(parts) == 1 && parts[0] == "" {
		return breadcrumbs
	}
	directoryParts := parts
	if includeFile {
		directoryParts = parts[:len(parts)-1]
	}
	for index, part := range directoryParts {
		directory := strings.Join(parts[:index+1], "/")
		breadcrumbs = append(breadcrumbs, projectBreadcrumbView{
			Label: part,
			URL:   projectDirectoryURL(repositoryID, revision, directory, 0),
		})
	}
	if includeFile {
		breadcrumbs = append(breadcrumbs, projectBreadcrumbView{Label: parts[len(parts)-1]})
	}
	return breadcrumbs
}

func projectEntryViews(
	repositoryID int64,
	revision string,
	entries []codeintel.TreeEntry,
	activePath string,
) []projectEntryView {
	output := make([]projectEntryView, 0, len(entries))
	for _, entry := range entries {
		target := projectSourceURL(repositoryID, revision, entry.Path)
		if entry.Type == "directory" {
			target = projectDirectoryURL(repositoryID, revision, entry.Path, 0)
		}
		output = append(output, projectEntryView{
			Name:   path.Base(entry.Path),
			Path:   entry.Path,
			Type:   entry.Type,
			URL:    target,
			Active: entry.Path == activePath,
		})
	}
	return output
}

func remoteFileURL(origin, revision, filePath string, startLine, endLine int) string {
	origin = strings.TrimSpace(origin)
	if origin == "" {
		return ""
	}
	if strings.HasPrefix(origin, "git@") {
		parts := strings.SplitN(strings.TrimPrefix(origin, "git@"), ":", 2)
		if len(parts) == 2 {
			origin = "https://" + parts[0] + "/" + parts[1]
		}
	}
	origin = strings.TrimSuffix(origin, ".git")
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	switch strings.ToLower(parsed.Host) {
	case "github.com", "gitlab.com":
		parsed.Path = path.Join(parsed.Path, "blob", revision, filePath)
		parsed.Fragment = fmt.Sprintf("L%d-L%d", startLine, endLine)
	case "bitbucket.org":
		parsed.Path = path.Join(parsed.Path, "src", revision, filePath)
		parsed.Fragment = fmt.Sprintf("lines-%d:%d", startLine, endLine)
	default:
		return origin
	}
	return parsed.String()
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("X-Content-Type-Options", "nosniff")
		response.Header().Set("X-Frame-Options", "DENY")
		response.Header().Set(
			"Content-Security-Policy",
			"default-src 'self'; base-uri 'self'; frame-ancestors 'none'; object-src 'none'; "+
				"script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; "+
				"font-src 'self'; connect-src 'self'; form-action 'self'",
		)
		next.ServeHTTP(response, request)
	})
}

func openBrowser(localURL string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", localURL)
	case "darwin":
		command = exec.Command("open", localURL)
	default:
		command = exec.Command("xdg-open", localURL)
	}
	return command.Start()
}

func requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		started := time.Now()
		next.ServeHTTP(response, request)
		slog.Debug(
			"HTTP request",
			"method", request.Method,
			"path", request.URL.Path,
			"duration", time.Since(started),
		)
	})
}
