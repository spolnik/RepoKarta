package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/spolnik/RepoKarta/internal/agent"
	anthropicprovider "github.com/spolnik/RepoKarta/internal/agent/anthropic"
	"github.com/spolnik/RepoKarta/internal/agent/claude"
	"github.com/spolnik/RepoKarta/internal/agent/codex"
	"github.com/spolnik/RepoKarta/internal/codeintel"
	"github.com/spolnik/RepoKarta/internal/httpserver"
	"github.com/spolnik/RepoKarta/internal/mcpserver"
	"github.com/spolnik/RepoKarta/internal/search"
	zoektadapter "github.com/spolnik/RepoKarta/internal/search/zoekt"
	"github.com/spolnik/RepoKarta/internal/store"
)

type Config struct {
	ListenAddress  string
	DataDirectory  string
	RepositoryRoot string
	Excludes       []string
	Version        string
	OpenBrowser    bool
	CodexCommand   string
	ClaudeCommand  string
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

	return Config{
		ListenAddress:  "127.0.0.1:7331",
		DataDirectory:  filepath.Join(cacheDirectory, "RepoKarta"),
		RepositoryRoot: workingDirectory,
		OpenBrowser:    true,
		CodexCommand:   "codex",
		ClaudeCommand:  "claude",
	}, nil
}

func Run(ctx context.Context, cfg Config) error {
	if err := os.MkdirAll(cfg.DataDirectory, 0o755); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	for _, directory := range []string{"indexes", "docs", "logs"} {
		if err := os.MkdirAll(filepath.Join(cfg.DataDirectory, directory), 0o755); err != nil {
			return fmt.Errorf("create %s directory: %w", directory, err)
		}
	}

	database, err := store.Open(filepath.Join(cfg.DataDirectory, "repokarta.db"))
	if err != nil {
		return err
	}
	defer database.Close()

	engine, err := zoektadapter.New(filepath.Join(cfg.DataDirectory, "indexes"))
	if err != nil {
		return err
	}
	defer engine.Close()
	if changed, err := database.EnsureIndexConfiguration(ctx, engine.IndexConfiguration()); err != nil {
		return fmt.Errorf("validate index configuration: %w", err)
	} else if changed {
		slog.Info("index capabilities changed; queued repositories for rebuild", "configuration", engine.IndexConfiguration())
	}

	coordinator := search.NewCoordinator(cfg.RepositoryRoot, cfg.Excludes, database, engine)
	if err := coordinator.Start(ctx); err != nil {
		return err
	}
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
	baseURL := "http://" + cfg.ListenAddress
	intelligence := codeintel.New(database, engine, baseURL)
	citations := mcpserver.NewCitationTracker()
	mcpHandler := mcpserver.NewHandler(mcpserver.Config{
		Version: cfg.Version,
		BaseURL: baseURL,
		Token:   mcpToken,
	}, intelligence, citations)
	conversations := agent.NewManager(
		cfg.RepositoryRoot,
		baseURL+"/mcp",
		mcpToken,
		&codex.Adapter{Command: cfg.CodexCommand},
		&claude.Adapter{Command: cfg.ClaudeCommand},
		&anthropicprovider.Adapter{Intelligence: intelligence, Citations: citations},
	).UseCitations(citations).UsePersistence(database)
	conversations.StartIdleReaper(ctx, 30*time.Minute)
	defer conversations.Close()

	server, err := httpserver.New(httpserver.Config{
		Address:        cfg.ListenAddress,
		RepositoryRoot: cfg.RepositoryRoot,
		Version:        cfg.Version,
		OpenBrowser:    cfg.OpenBrowser,
		MCPHandler:     mcpHandler,
		Conversations:  conversations,
	}, intelligence, coordinator)
	if err != nil {
		return err
	}

	return server.Run(ctx)
}
