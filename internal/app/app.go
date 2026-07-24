package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/httpserver"
	"github.com/spolnik/RepoKarta/internal/store"
)

type Config struct {
	ListenAddress  string
	DataDirectory  string
	RepositoryRoot string
	Version        string
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
	}, nil
}

func Run(ctx context.Context, cfg Config) error {
	if err := os.MkdirAll(cfg.DataDirectory, 0o755); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}

	database, err := store.Open(filepath.Join(cfg.DataDirectory, "repokarta.db"))
	if err != nil {
		return err
	}
	defer database.Close()

	repositories, err := catalog.Discover(cfg.RepositoryRoot)
	if err != nil {
		return fmt.Errorf("discover repositories: %w", err)
	}
	if err := database.ReplaceRepositories(ctx, repositories); err != nil {
		return fmt.Errorf("store repositories: %w", err)
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
	}, database)
	if err != nil {
		return err
	}

	return server.Run(ctx)
}
