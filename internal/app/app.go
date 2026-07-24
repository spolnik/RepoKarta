package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/spolnik/RepoKarta/internal/httpserver"
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

	server, err := httpserver.New(httpserver.Config{
		Address:        cfg.ListenAddress,
		RepositoryRoot: cfg.RepositoryRoot,
		Version:        cfg.Version,
		OpenBrowser:    cfg.OpenBrowser,
	}, database, engine, coordinator)
	if err != nil {
		return err
	}

	return server.Run(ctx)
}
