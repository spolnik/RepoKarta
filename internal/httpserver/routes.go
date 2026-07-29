package httpserver

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/spolnik/RepoKarta/internal/codeintel"
	"github.com/spolnik/RepoKarta/internal/identity"
	"github.com/spolnik/RepoKarta/web"
)

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
		"joinStrings": strings.Join,
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
		telemetry:             config.Telemetry,
		code:                  config.Code,
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
	if server.code != nil && server.agents != nil {
		mux.HandleFunc("GET /code", server.controlled(
			identity.PermissionWriteRepositories, "code.page.read", "code-session", server.codePage,
		))
		mux.HandleFunc("GET /api/code/sessions", server.controlled(
			identity.PermissionWriteRepositories, "code.session.list", "code-session", server.codeSessions,
		))
		mux.HandleFunc("POST /api/code/sessions", server.controlled(
			identity.PermissionWriteRepositories, "code.session.create", "code-session", server.createCodeSession,
		))
		mux.HandleFunc("GET /api/code/sessions/{sessionID}", server.controlled(
			identity.PermissionWriteRepositories, "code.session.read", "code-session", server.codeSession,
		))
		mux.HandleFunc("DELETE /api/code/sessions/{sessionID}", server.controlled(
			identity.PermissionWriteRepositories, "code.session.discard", "code-session", server.discardCodeSession,
		))
		mux.HandleFunc("POST /api/code/sessions/{sessionID}/turns", server.controlled(
			identity.PermissionWriteRepositories, "code.turn", "code-session", server.codeTurn,
		))
		mux.HandleFunc("POST /api/code/sessions/{sessionID}/interrupt", server.controlled(
			identity.PermissionWriteRepositories, "code.interrupt", "code-session", server.interruptCodeTurn,
		))
		mux.HandleFunc("GET /api/code/sessions/{sessionID}/diff", server.controlled(
			identity.PermissionWriteRepositories, "code.diff.read", "code-session", server.codeDiff,
		))
		mux.HandleFunc("GET /api/code/sessions/{sessionID}/file", server.controlled(
			identity.PermissionWriteRepositories, "code.file.read", "code-session", server.codeFile,
		))
		mux.HandleFunc("POST /api/code/sessions/{sessionID}/files/discard", server.controlled(
			identity.PermissionWriteRepositories, "code.file.discard", "code-session", server.discardCodeFile,
		))
		mux.HandleFunc("POST /api/code/sessions/{sessionID}/approvals/{approvalID}", server.controlled(
			identity.PermissionWriteRepositories, "code.approval.resolve", "code-session", server.resolveCodeApproval,
		))
		mux.HandleFunc("POST /api/code/sessions/{sessionID}/finish", server.controlled(
			identity.PermissionWriteRepositories, "code.finish", "code-session", server.finishCodeSession,
		))
	}
	mux.HandleFunc("GET /contexts", server.contextPage)
	mux.HandleFunc("GET /contexts/{contextID}", server.namedContextPage)
	mux.HandleFunc("GET /projects/{repositoryID}", server.project)
	mux.HandleFunc("GET /source/{repositoryID}", server.source)
	mux.HandleFunc("GET /api/search", server.apiSearch)
	mux.HandleFunc("GET /api/search/stream", server.apiSearchStream)
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
		if server.docs != nil {
			mux.HandleFunc("POST /admin/wiki/generate", server.adminGenerateWiki)
		}
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
			if server.telemetry != nil {
				mux.HandleFunc("GET /api/admin/telemetry", server.controlled(
					identity.PermissionManageSecurity, "telemetry.status.read", "telemetry-configuration", server.apiTelemetryStatus,
				))
			}
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
		mux.HandleFunc("GET /api/reachability", server.apiReachability)
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
	if server.telemetry != nil {
		handler = server.telemetry.RouteHandler(handler)
	}
	if server.security != nil {
		handler = server.security.Middleware(handler)
	}
	handler = securityHeaders(handler)
	handler = requestLog(handler)
	if server.telemetry != nil {
		handler = server.telemetry.HTTPHandler(handler)
	}
	handler = correlationMiddleware(handler)
	server.server = &http.Server{
		Addr:              config.Address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	return server, nil
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
