package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spolnik/RepoKarta/internal/access"
	"github.com/spolnik/RepoKarta/internal/acquisition"
	"github.com/spolnik/RepoKarta/internal/agent"
	anthropicprovider "github.com/spolnik/RepoKarta/internal/agent/anthropic"
	"github.com/spolnik/RepoKarta/internal/agent/claude"
	"github.com/spolnik/RepoKarta/internal/agent/codex"
	"github.com/spolnik/RepoKarta/internal/codeintel"
	"github.com/spolnik/RepoKarta/internal/dependencies"
	"github.com/spolnik/RepoKarta/internal/docs"
	"github.com/spolnik/RepoKarta/internal/evidencesearch"
	"github.com/spolnik/RepoKarta/internal/graph"
	"github.com/spolnik/RepoKarta/internal/httpserver"
	"github.com/spolnik/RepoKarta/internal/insights"
	"github.com/spolnik/RepoKarta/internal/maintenance"
	"github.com/spolnik/RepoKarta/internal/mcpserver"
	"github.com/spolnik/RepoKarta/internal/scim"
	"github.com/spolnik/RepoKarta/internal/search"
	zoektadapter "github.com/spolnik/RepoKarta/internal/search/zoekt"
	"github.com/spolnik/RepoKarta/internal/security"
	"github.com/spolnik/RepoKarta/internal/store"
)

type Config struct {
	ListenAddress          string
	DataDirectory          string
	RepositoryRoot         string
	Excludes               []string
	Version                string
	OpenBrowser            bool
	CodexCommand           string
	ClaudeCommand          string
	AllowOpen              bool
	AdminUser              string
	AdminPassword          string
	SCIMToken              string
	Security               security.Settings
	RepositorySyncInterval time.Duration
	AcquisitionGitHubAPI   string
	AcquisitionGitLabAPI   string
	AcquisitionGitHubHost  string
	AcquisitionGitLabHost  string
	DependencyRegistries   []dependencies.RegistryConfig
}

func DefaultConfig() (Config, error) {
	cacheDirectory, err := os.UserCacheDir()
	if err != nil {
		return Config{}, fmt.Errorf("resolve user cache directory: %w", err)
	}

	workingDirectory, err := os.Getwd()
	if err != nil {
		return Config{}, fmt.Errorf("resolve working directory: %w", err)
	}
	dependencyRegistries, err := dependencies.ParseRegistryConfigs(
		os.Getenv("REPOKARTA_DEPENDENCY_REGISTRIES"),
	)
	if err != nil {
		return Config{}, err
	}

	return Config{
		ListenAddress:  "127.0.0.1:7331",
		DataDirectory:  filepath.Join(cacheDirectory, "RepoKarta"),
		RepositoryRoot: workingDirectory,
		OpenBrowser:    true,
		CodexCommand:   "codex",
		ClaudeCommand:  "claude",
		AdminUser:      strings.TrimSpace(os.Getenv("REPOKARTA_ADMIN_USER")),
		Security: security.Settings{
			Mode:                 security.Mode(envOrDefault("REPOKARTA_AUTH_MODE", string(security.ModeLocal))),
			PublicURL:            strings.TrimSpace(os.Getenv("REPOKARTA_PUBLIC_URL")),
			CloudflareTeamDomain: strings.TrimSpace(os.Getenv("REPOKARTA_CF_TEAM_DOMAIN")),
			CloudflareAudience:   strings.TrimSpace(os.Getenv("REPOKARTA_CF_AUDIENCE")),
			SAMLMetadataURL:      strings.TrimSpace(os.Getenv("REPOKARTA_SAML_METADATA_URL")),
			SAMLEntityID:         strings.TrimSpace(os.Getenv("REPOKARTA_SAML_ENTITY_ID")),
		},
		AllowOpen:             envBool("REPOKARTA_ALLOW_OPEN"),
		AcquisitionGitHubAPI:  strings.TrimSpace(os.Getenv("REPOKARTA_GITHUB_API")),
		AcquisitionGitLabAPI:  strings.TrimSpace(os.Getenv("REPOKARTA_GITLAB_API")),
		AcquisitionGitHubHost: strings.TrimSpace(os.Getenv("REPOKARTA_GITHUB_HOST")),
		AcquisitionGitLabHost: strings.TrimSpace(os.Getenv("REPOKARTA_GITLAB_HOST")),
		DependencyRegistries:  dependencyRegistries,
	}, nil
}

func Run(ctx context.Context, cfg Config) error {
	if err := os.MkdirAll(cfg.DataDirectory, 0o755); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	for _, directory := range []string{"indexes", "maps", "docs", "logs"} {
		if err := os.MkdirAll(filepath.Join(cfg.DataDirectory, directory), 0o755); err != nil {
			return fmt.Errorf("create %s directory: %w", directory, err)
		}
	}

	database, err := store.Open(filepath.Join(cfg.DataDirectory, "repokarta.db"))
	if err != nil {
		return err
	}
	defer database.Close()

	securityManager, err := security.New(ctx, database, security.Config{
		Address:       cfg.ListenAddress,
		DataDirectory: cfg.DataDirectory,
		AllowOpen:     cfg.AllowOpen,
		AdminUser:     cfg.AdminUser,
		AdminPassword: cfg.AdminPassword,
		Initial:       cfg.Security,
		Identities:    database,
		Audit:         database,
	})
	if err != nil {
		return fmt.Errorf("initialize authentication: %w", err)
	}
	scimService, err := scim.New(database, database, cfg.SCIMToken)
	if err != nil {
		return fmt.Errorf("initialize SCIM provisioning: %w", err)
	}

	engine, err := zoektadapter.New(filepath.Join(cfg.DataDirectory, "indexes"))
	if err != nil {
		return err
	}
	defer engine.Close()
	if changed, err := database.EnsureIndexConfiguration(ctx, engine.IndexConfiguration()); err != nil {
		return fmt.Errorf("validate index configuration: %w", err)
	} else if changed {
		slog.Info("index capabilities changed; queued indexable repositories for rebuild", "configuration", engine.IndexConfiguration())
	}

	internalBaseURL := "http://" + cfg.ListenAddress
	baseURL := internalBaseURL
	if configured := securityManager.Settings().PublicURL; configured != "" {
		baseURL = configured
	}
	intelligence := codeintel.New(database, engine, baseURL).UseNamedContexts(database)
	maps, err := graph.New(database, filepath.Join(cfg.DataDirectory, "maps"), baseURL)
	if err != nil {
		return err
	}
	intelligence.UseStructure(maps)

	acquisitions, err := acquisition.New(acquisition.Config{
		DataDirectory: cfg.DataDirectory,
		Version:       cfg.Version,
		GitHubAPI:     cfg.AcquisitionGitHubAPI,
		GitLabAPI:     cfg.AcquisitionGitLabAPI,
		GitHubHost:    cfg.AcquisitionGitHubHost,
		GitLabHost:    cfg.AcquisitionGitLabHost,
	}, database)
	if err != nil {
		return fmt.Errorf("initialize repository acquisition: %w", err)
	}
	coordinator := search.NewCoordinator(cfg.RepositoryRoot, cfg.Excludes, database, engine).
		UseRepositoryProvider(acquisitions.CatalogueRepositories).
		UseIndexedObserver(func(observerContext context.Context, repositoryID int64) error {
			return maps.PrepareStructure(observerContext, repositoryID)
		})
	acquisitions.UseRefresher(coordinator.Refresh)
	if err := coordinator.Start(ctx); err != nil {
		return err
	}
	acquisitions.StartScheduledSync(ctx, cfg.RepositorySyncInterval)
	repositories, err := database.ListRepositories(ctx)
	if err != nil {
		return fmt.Errorf("load repositories: %w", err)
	}

	slog.Info(
		"starting RepoKarta",
		"address", cfg.ListenAddress,
		"repository_root", cfg.RepositoryRoot,
		"repositories", len(repositories),
		"data_directory", cfg.DataDirectory,
	)

	mcpToken, err := mcpserver.NewToken()
	if err != nil {
		return fmt.Errorf("create MCP bearer token: %w", err)
	}
	citations := mcpserver.NewCitationTracker()
	conversations := agent.NewManager(
		cfg.RepositoryRoot,
		internalBaseURL+"/mcp",
		mcpToken,
		&codex.Adapter{Command: cfg.CodexCommand},
		&claude.Adapter{Command: cfg.ClaudeCommand},
		&anthropicprovider.Adapter{Intelligence: intelligence, Citations: citations},
	).UseCitations(citations).UsePersistence(database)
	documents, err := docs.New(database, maps, filepath.Join(cfg.DataDirectory, "docs"))
	if err != nil {
		return err
	}
	documents.UseGenerator(conversations)
	codeInsights := insights.New(database, baseURL)
	codeInsights.StartPolling(ctx)
	dependencyRegistry := dependencies.NewService(ctx, database, nil)
	dependencyRegistry.UseRegistries(cfg.DependencyRegistries)
	derivedEvidence := evidencesearch.New(
		maps,
		dependencyRegistry,
		documents,
		codeInsights,
		baseURL,
	)
	intelligence.UseDerivedEvidence(derivedEvidence)
	operations, err := maintenance.New(maintenance.Config{
		DataDirectory:   cfg.DataDirectory,
		RepositoryRoot:  cfg.RepositoryRoot,
		Version:         cfg.Version,
		Address:         cfg.ListenAddress,
		DatabaseVersion: store.SchemaVersion,
		MapVersion:      graph.ArtifactVersion,
		WikiVersion:     docs.ArtifactVersion,
	}, database)
	if err != nil {
		return fmt.Errorf("initialize maintenance: %w", err)
	}
	securityManager.SetChangeHandler(func(settings security.Settings) {
		updatedBaseURL := internalBaseURL
		if settings.PublicURL != "" {
			updatedBaseURL = settings.PublicURL
		}
		intelligence.SetBaseURL(updatedBaseURL)
		maps.SetBaseURL(updatedBaseURL)
		codeInsights.SetBaseURL(updatedBaseURL)
		derivedEvidence.SetBaseURL(updatedBaseURL)
	})
	mcpHandler := mcpserver.NewHandler(mcpserver.Config{
		Version:   cfg.Version,
		BaseURL:   baseURL,
		Token:     mcpToken,
		Artifacts: mcpserver.Artifacts{Maps: maps, Documents: documents},
		Insights:  codeInsights,
		ResolveViewer: func(ctx context.Context, conversationID string) (access.Viewer, error) {
			if author, ok := conversations.AuthorForMCP(conversationID); ok {
				return access.Viewer{
					ID:     author.ID,
					Groups: append([]string(nil), author.Groups...),
				}, nil
			}
			conversation, err := database.GetConversation(ctx, conversationID)
			if err != nil {
				return access.Viewer{}, err
			}
			return access.Viewer{
				ID:     conversation.Author.ID,
				Groups: append([]string(nil), conversation.Author.Groups...),
			}, nil
		},
		AllowUnscoped: func() bool {
			return securityManager.Mode() == security.ModeLocal
		},
	}, intelligence, citations)
	mcpCommand, executableError := os.Executable()
	if executableError != nil {
		mcpCommand = "repokarta"
	}
	conversations.StartIdleReaper(ctx, 30*time.Minute)
	defer conversations.Close()

	server, err := httpserver.New(httpserver.Config{
		Address:               cfg.ListenAddress,
		RepositoryRoot:        cfg.RepositoryRoot,
		Version:               cfg.Version,
		OpenBrowser:           cfg.OpenBrowser,
		MCPHandler:            mcpHandler,
		MCPToken:              mcpToken,
		MCPBaseURL:            internalBaseURL,
		MCPCommand:            mcpCommand,
		Conversations:         conversations,
		Maps:                  maps,
		Docs:                  documents,
		Security:              securityManager,
		Maintenance:           operations,
		RepositoryAccess:      database,
		RepositoryAcquisition: acquisitions,
		Enterprise:            database,
		SCIMHandler: func() http.Handler {
			if scimService == nil {
				return nil
			}
			return scimService.Handler()
		}(),
		Insights:     codeInsights,
		Dependencies: dependencyRegistry,
	}, intelligence, coordinator)
	if err != nil {
		return err
	}

	return server.Run(ctx)
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envBool(name string) bool {
	value, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(name)))
	return err == nil && value
}
