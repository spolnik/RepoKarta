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
	"strings"

	"github.com/spolnik/RepoKarta/internal/app"
	"github.com/spolnik/RepoKarta/internal/codeintel"
	"github.com/spolnik/RepoKarta/internal/mcpserver"
	"github.com/spolnik/RepoKarta/internal/security"
)

var version = "0.40.0-dev"

type stringList []string

func (values *stringList) String() string {
	return fmt.Sprint([]string(*values))
}

func (values *stringList) Set(value string) error {
	*values = append(*values, value)
	return nil
}

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
	case "mcp":
		return serveMCP(os.Args[2:])
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

func serveMCP(args []string) error {
	flags := flag.NewFlagSet("mcp", flag.ContinueOnError)
	baseURL := flags.String("url", "http://127.0.0.1:7331", "running RepoKarta server URL")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("mcp does not accept positional arguments")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return mcpserver.RunStdio(
		ctx,
		mcpserver.Config{Version: version, BaseURL: *baseURL},
		codeintel.NewClient(*baseURL),
	)
}

func serve(args []string) error {
	defaults, err := app.DefaultConfig()
	if err != nil {
		return err
	}

	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	listenAddress := flags.String("listen", defaults.ListenAddress, "HTTP listen address")
	dataDirectory := flags.String("data-dir", defaults.DataDirectory, "RepoKarta data directory")
	openBrowser := flags.Bool("open", defaults.OpenBrowser, "open the local dashboard in the default browser")
	codexCommand := flags.String("codex-command", defaults.CodexCommand, "Codex CLI command or absolute path")
	claudeCommand := flags.String("claude-command", defaults.ClaudeCommand, "Claude Code CLI command or absolute path")
	authMode := flags.String("auth-mode", string(defaults.Security.Mode), "authentication mode: local, cloudflare-access, saml, or open")
	publicURL := flags.String("public-url", defaults.Security.PublicURL, "public HTTPS URL used by shared authentication modes")
	allowOpen := flags.Bool("allow-open", defaults.AllowOpen, "allow the administrator to enable unauthenticated shared access")
	adminUser := flags.String("admin-user", defaults.AdminUser, "bootstrap administrator username")
	adminPasswordFile := flags.String("admin-password-file", strings.TrimSpace(os.Getenv("REPOKARTA_ADMIN_PASSWORD_FILE")), "file containing the bootstrap administrator password")
	cloudflareTeamDomain := flags.String("cloudflare-team-domain", defaults.Security.CloudflareTeamDomain, "Cloudflare Access team domain")
	cloudflareAudience := flags.String("cloudflare-audience", defaults.Security.CloudflareAudience, "Cloudflare Access application audience tag")
	samlMetadataURL := flags.String("saml-metadata-url", defaults.Security.SAMLMetadataURL, "SAML identity-provider metadata URL")
	samlEntityID := flags.String("saml-entity-id", defaults.Security.SAMLEntityID, "optional SAML service-provider entity ID")
	var excludes stringList
	flags.Var(&excludes, "exclude", "directory to exclude; repeat for multiple directories")
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
	adminPassword := ""
	if strings.TrimSpace(*adminPasswordFile) != "" {
		passwordBytes, err := os.ReadFile(*adminPasswordFile)
		if err != nil {
			return fmt.Errorf("read administrator password file: %w", err)
		}
		adminPassword = strings.TrimRight(string(passwordBytes), "\r\n")
		if adminPassword == "" {
			return errors.New("administrator password file is empty")
		}
	}

	cfg := app.Config{
		ListenAddress:  *listenAddress,
		DataDirectory:  *dataDirectory,
		RepositoryRoot: repositoryRoot,
		Excludes:       excludes,
		Version:        version,
		OpenBrowser:    *openBrowser,
		CodexCommand:   *codexCommand,
		ClaudeCommand:  *claudeCommand,
		AllowOpen:      *allowOpen,
		AdminUser:      *adminUser,
		AdminPassword:  adminPassword,
		Security: security.Settings{
			Mode:                 security.Mode(*authMode),
			PublicURL:            *publicURL,
			CloudflareTeamDomain: *cloudflareTeamDomain,
			CloudflareAudience:   *cloudflareAudience,
			SAMLMetadataURL:      *samlMetadataURL,
			SAMLEntityID:         *samlEntityID,
		},
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	return app.Run(ctx, cfg)
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `RepoKarta - local code search, maps, and living documentation

Usage:
  repokarta serve [options] [repository-root]
  repokarta mcp [options]
  repokarta version

Serve options:
  -listen string     HTTP listen address (default 127.0.0.1:7331)
  -data-dir string   RepoKarta-owned data directory
  -exclude string    directory to exclude (repeatable)
  -open bool         open the local dashboard (default true)
  -codex-command     Codex CLI command or absolute path
  -claude-command    Claude Code CLI command or absolute path
  -auth-mode         local, cloudflare-access, saml, or open (default local)
  -public-url        public HTTPS URL for shared modes
  -allow-open        permit administrator-controlled unauthenticated shared mode
  -admin-user        bootstrap administrator username
  -admin-password-file file containing the bootstrap administrator password
  -cloudflare-team-domain Cloudflare Access team domain
  -cloudflare-audience Cloudflare Access application audience tag
  -saml-metadata-url SAML identity-provider metadata URL
  -saml-entity-id    optional SAML service-provider entity ID

MCP options:
  -url string        running RepoKarta URL (default http://127.0.0.1:7331)`)
}
