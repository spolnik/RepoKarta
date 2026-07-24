package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/spolnik/RepoKarta/internal/app"
)

const version = "0.1.0-dev"

func main() {
	if err := run(); err != nil {
		slog.Error("RepoKarta stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		printUsage()
		return errors.New("missing command")
	}

	switch os.Args[1] {
	case "serve":
		return serve(os.Args[2:])
	case "version", "--version", "-version":
		fmt.Println(version)
		return nil
	case "help", "--help", "-h":
		printUsage()
		return nil
	default:
		printUsage()
		return fmt.Errorf("unknown command %q", os.Args[1])
	}
}

func serve(args []string) error {
	defaults, err := app.DefaultConfig()
	if err != nil {
		return err
	}

	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	listenAddress := flags.String("listen", defaults.ListenAddress, "HTTP listen address")
	dataDirectory := flags.String("data-dir", defaults.DataDirectory, "RepoKarta data directory")
	if err := flags.Parse(args); err != nil {
		return err
	}

	repositoryRoot := defaults.RepositoryRoot
	if flags.NArg() > 1 {
		return errors.New("serve accepts at most one repository root")
	}
	if flags.NArg() == 1 {
		repositoryRoot = flags.Arg(0)
	}

	repositoryRoot, err = filepath.Abs(repositoryRoot)
	if err != nil {
		return fmt.Errorf("resolve repository root: %w", err)
	}

	cfg := app.Config{
		ListenAddress:  *listenAddress,
		DataDirectory:  *dataDirectory,
		RepositoryRoot: repositoryRoot,
		Version:        version,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	return app.Run(ctx, cfg)
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `RepoKarta - local code search, maps, and living documentation

Usage:
  repokarta serve [options] [repository-root]
  repokarta version

Serve options:
  -listen string     HTTP listen address (default 127.0.0.1:7331)
  -data-dir string   RepoKarta-owned data directory`)
}
