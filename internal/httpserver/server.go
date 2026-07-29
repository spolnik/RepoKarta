package httpserver

import (
	"context"
	"html/template"
	"net/http"
	"time"

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
	"github.com/spolnik/RepoKarta/internal/scipjava"
	"github.com/spolnik/RepoKarta/internal/search"
	"github.com/spolnik/RepoKarta/internal/security"
	"github.com/spolnik/RepoKarta/internal/source"
	"github.com/spolnik/RepoKarta/internal/sourceintelligence"
	"github.com/spolnik/RepoKarta/internal/store"
	"github.com/spolnik/RepoKarta/internal/telemetry"
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
	Reachability(context.Context, int64) (graph.ReachabilityReport, error)
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
	Telemetry             *telemetry.System
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
	telemetry             *telemetry.System
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

type sourceIntelligenceView = sourceintelligence.View
type sourceRouteView = sourceintelligence.Route
type sourceCallerView = sourceintelligence.Caller

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

type conversationViewer struct {
	Author agent.ConversationAuthor
	Admin  bool
}
