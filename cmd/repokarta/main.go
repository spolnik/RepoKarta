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
	"github.com/spolnik/RepoKarta/internal/scipindex"
	"github.com/spolnik/RepoKarta/internal/scipjava"
	"github.com/spolnik/RepoKarta/internal/security"
	"github.com/spolnik/RepoKarta/internal/store"
)

var version = "0.83.0-dev"

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
	case "scip":
		return runSCIP(os.Args[2:])
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

func runSCIP(args []string) error {
	if len(args) == 0 || args[0] != "import" {
		return errors.New("usage: repokarta scip import [options] index.scip")
	}
	defaults, err := app.DefaultConfig()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("scip import", flag.ContinueOnError)
	dataDirectory := flags.String("data-dir", defaults.DataDirectory, "RepoKarta data directory")
	repositoryID := flags.Int64("repository-id", 0, "repository ID returned by RepoKarta")
	revision := flags.String("revision", "", "exact indexed commit; defaults to the repository's indexed commit")
	sourceRoot := flags.String("root", ".", "repository-relative project root used to produce the SCIP index")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("scip import requires exactly one index.scip path")
	}
	if *repositoryID <= 0 {
		return errors.New("scip import requires -repository-id")
	}
	database, err := store.Open(filepath.Join(*dataDirectory, "repokarta.db"))
	if err != nil {
		return err
	}
	defer database.Close()
	repository, err := database.RepositoryByID(context.Background(), *repositoryID)
	if err != nil {
		return fmt.Errorf("load repository %d: %w", *repositoryID, err)
	}
	indexedCommit := strings.TrimSpace(repository.IndexedCommit)
	if indexedCommit == "" {
		return fmt.Errorf("repository %d has no indexed commit", *repositoryID)
	}
	requestedRevision := strings.TrimSpace(*revision)
	if requestedRevision == "" {
		requestedRevision = indexedCommit
	}
	if requestedRevision != indexedCommit {
		return fmt.Errorf(
			"SCIP revision %q does not match repository %d indexed commit %q",
			requestedRevision,
			*repositoryID,
			indexedCommit,
		)
	}
	input, err := os.Open(flags.Arg(0))
	if err != nil {
		return fmt.Errorf("open SCIP index: %w", err)
	}
	defer input.Close()
	semanticIndexes, err := scipindex.New(filepath.Join(*dataDirectory, "scip"))
	if err != nil {
		return err
	}
	summary, err := semanticIndexes.Import(
		context.Background(),
		*repositoryID,
		requestedRevision,
		*sourceRoot,
		input,
	)
	if err != nil {
		return err
	}
	fmt.Printf(
		"Imported SCIP for repository %d at %s below %s: %d documents, %d symbols, %d occurrences (%s %s)\n",
		summary.RepositoryID,
		summary.Revision,
		summary.SourceRoot,
		summary.Documents,
		summary.Symbols,
		summary.Occurrences,
		summary.Indexer.Name,
		summary.Indexer.Version,
	)
	return nil
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
	scimTokenFile := flags.String("scim-token-file", strings.TrimSpace(os.Getenv("REPOKARTA_SCIM_TOKEN_FILE")), "file containing the SCIM 2.0 bearer token")
	cloudflareTeamDomain := flags.String("cloudflare-team-domain", defaults.Security.CloudflareTeamDomain, "Cloudflare Access team domain")
	cloudflareAudience := flags.String("cloudflare-audience", defaults.Security.CloudflareAudience, "Cloudflare Access application audience tag")
	samlMetadataURL := flags.String("saml-metadata-url", defaults.Security.SAMLMetadataURL, "SAML identity-provider metadata URL")
	samlEntityID := flags.String("saml-entity-id", defaults.Security.SAMLEntityID, "optional SAML service-provider entity ID")
	repositorySyncInterval := flags.Duration("repository-sync-interval", defaults.RepositorySyncInterval, "automatic managed-repository sync interval; zero disables scheduling")
	scipJavaMode := flags.String("scip-java-mode", defaults.SCIPJavaMode, "Java SCIP generation: off, auto, or required")
	scipJavaCommand := flags.String("scip-java-command", defaults.SCIPJavaCommand, "scip-java executable name or absolute path")
	scipJavaTimeout := flags.Duration("scip-java-timeout", defaults.SCIPJavaTimeout, "maximum time for one Java SCIP build")
	scipJavaConcurrency := flags.Int("scip-java-concurrency", defaults.SCIPJavaConcurrency, "maximum concurrent Java SCIP builds (1-4)")
	scipJavaJDKHome := flags.String("scip-java-jdk-home", defaults.SCIPJavaJDKHome, "JDK home forced for every Java SCIP build")
	acquisitionGitHubAPI := flags.String("github-api", defaults.AcquisitionGitHubAPI, "GitHub REST API base URL used for repository discovery")
	acquisitionGitLabAPI := flags.String("gitlab-api", defaults.AcquisitionGitLabAPI, "GitLab REST API base URL used for repository discovery")
	acquisitionGitHubHost := flags.String("github-host", defaults.AcquisitionGitHubHost, "allowed GitHub HTTPS Git host; defaults to github.com")
	acquisitionGitLabHost := flags.String("gitlab-host", defaults.AcquisitionGitLabHost, "allowed GitLab HTTPS Git host; defaults to gitlab.com")
	var excludes stringList
	flags.Var(&excludes, "exclude", "directory to exclude; repeat for multiple directories")
	if err := flags.Parse(args); err != nil {
		return err
	}
	scipJavaModeExplicit := false
	flags.Visit(func(current *flag.Flag) {
		if current.Name == "scip-java-mode" {
			scipJavaModeExplicit = true
		}
	})
	if !scipJavaModeExplicit && strings.TrimSpace(*scipJavaCommand) != "" {
		*scipJavaMode = scipjava.ModeRequired
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
	scimToken := ""
	if strings.TrimSpace(*scimTokenFile) != "" {
		tokenBytes, err := os.ReadFile(*scimTokenFile)
		if err != nil {
			return fmt.Errorf("read SCIM token file: %w", err)
		}
		scimToken = strings.TrimRight(string(tokenBytes), "\r\n")
		if scimToken == "" {
			return errors.New("SCIM token file is empty")
		}
	}

	cfg := app.Config{
		ListenAddress:          *listenAddress,
		DataDirectory:          *dataDirectory,
		RepositoryRoot:         repositoryRoot,
		Excludes:               excludes,
		Version:                version,
		OpenBrowser:            *openBrowser,
		CodexCommand:           *codexCommand,
		ClaudeCommand:          *claudeCommand,
		AllowOpen:              *allowOpen,
		AdminUser:              *adminUser,
		AdminPassword:          adminPassword,
		SCIMToken:              scimToken,
		RepositorySyncInterval: *repositorySyncInterval,
		AcquisitionGitHubAPI:   *acquisitionGitHubAPI,
		AcquisitionGitLabAPI:   *acquisitionGitLabAPI,
		AcquisitionGitHubHost:  *acquisitionGitHubHost,
		AcquisitionGitLabHost:  *acquisitionGitLabHost,
		SCIPJavaMode:           *scipJavaMode,
		SCIPJavaCommand:        *scipJavaCommand,
		SCIPJavaTimeout:        *scipJavaTimeout,
		SCIPJavaConcurrency:    *scipJavaConcurrency,
		SCIPJavaJDKHome:        *scipJavaJDKHome,
		SCIPJavaJDKHomes:       defaults.SCIPJavaJDKHomes,
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
  repokarta scip import [options] index.scip
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
  -scim-token-file  file containing the SCIM 2.0 bearer token
  -cloudflare-team-domain Cloudflare Access team domain
  -cloudflare-audience Cloudflare Access application audience tag
  -saml-metadata-url SAML identity-provider metadata URL
  -saml-entity-id    optional SAML service-provider entity ID
  -repository-sync-interval automatic managed-repository sync interval (default 0)
  -scip-java-mode  Java SCIP generation: off, auto, or required (default off)
  -scip-java-command scip-java executable name or absolute path
  -scip-java-timeout maximum time for one Java SCIP build (default 20m)
  -scip-java-concurrency concurrent Java SCIP builds, 1-4 (default 1)
  -scip-java-jdk-home JDK home forced for every Java SCIP build

MCP options:
  -url string        running RepoKarta URL (default http://127.0.0.1:7331)

SCIP import options:
  -data-dir string   RepoKarta data directory
  -repository-id int repository ID returned by RepoKarta
  -revision string   exact indexed commit (defaults to repository's indexed commit)
  -root string       repository-relative project root used by the indexer (default .)`)
}
