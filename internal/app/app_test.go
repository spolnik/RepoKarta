package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/spolnik/RepoKarta/internal/security"
)

func TestDefaultConfigReadsBoundedEnvironment(t *testing.T) {
	t.Setenv("REPOKARTA_AUTH_MODE", "open")
	t.Setenv("REPOKARTA_ALLOW_OPEN", "true")
	t.Setenv("REPOKARTA_ADMIN_USER", " admin ")
	t.Setenv("REPOKARTA_PUBLIC_URL", " https://repo.example.com ")
	t.Setenv("REPOKARTA_GITHUB_API", " https://api.github.test ")
	t.Setenv("REPOKARTA_SCIP_JAVA_MODE", " auto ")
	t.Setenv("REPOKARTA_SCIP_JAVA_COMMAND", " C:\\tools\\scip-java.exe ")
	t.Setenv("REPOKARTA_DEPENDENCY_REGISTRIES", `[{
		"ecosystem":"npm",
		"base_url":"https://npm.example.com",
		"metadata_url_template":"https://npm.example.com/{package}",
		"package_prefixes":["@acme/"],
		"token_env":"ACME_NPM_TOKEN"
	}]`)
	config, err := DefaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	if config.ListenAddress != "127.0.0.1:7331" ||
		config.Security.Mode != security.ModeOpen ||
		config.Security.PublicURL != "https://repo.example.com" ||
		!config.AllowOpen || config.AdminUser != "admin" ||
		config.AcquisitionGitHubAPI != "https://api.github.test" ||
		config.SCIPJavaMode != "auto" ||
		config.SCIPJavaCommand != `C:\tools\scip-java.exe` ||
		config.SCIPJavaTimeout <= 0 ||
		config.SCIPJavaConcurrency != 1 ||
		len(config.DependencyRegistries) != 1 ||
		config.DependencyRegistries[0].TokenEnv != "ACME_NPM_TOKEN" ||
		filepath.Base(config.DataDirectory) != "RepoKarta" {
		t.Fatalf("default config = %#v", config)
	}
}

func TestDefaultConfigRequiresExplicitSCIPJavaCommand(t *testing.T) {
	t.Setenv("REPOKARTA_SCIP_JAVA_MODE", "")
	t.Setenv("REPOKARTA_SCIP_JAVA_COMMAND", `C:\tools\scip-java.exe`)

	config, err := DefaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	if config.SCIPJavaMode != "required" {
		t.Fatalf("SCIP Java mode = %q, want required", config.SCIPJavaMode)
	}
}

func TestRunBuildsAndShutsDownLocalApplication(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve application address: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release application address: %v", err)
	}
	dataDirectory := filepath.Join(t.TempDir(), "data")
	repositoryRoot := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runError := make(chan error, 1)
	go func() {
		runError <- Run(ctx, Config{
			ListenAddress:  address,
			DataDirectory:  dataDirectory,
			RepositoryRoot: repositoryRoot,
			Version:        "coverage-test",
			OpenBrowser:    false,
			CodexCommand:   "missing-codex-for-test",
			ClaudeCommand:  "missing-claude-for-test",
			Security:       security.Settings{Mode: security.ModeLocal},
		})
	}()

	startupContext, stopWaiting := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopWaiting()
	if err := waitForTCPServer(startupContext, address, runError); err != nil {
		t.Fatal(err)
	}

	cancel()
	select {
	case err := <-runError:
		if err != nil {
			t.Fatalf("run with canceled context: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("application did not shut down after cancellation")
	}
}

func waitForTCPServer(ctx context.Context, address string, runError <-chan error) error {
	retry := time.NewTicker(25 * time.Millisecond)
	defer retry.Stop()

	for {
		connection, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			return nil
		}

		select {
		case err := <-runError:
			if err == nil {
				return errors.New("application stopped before listening")
			}
			return fmt.Errorf("application stopped before listening: %w", err)
		case <-ctx.Done():
			return fmt.Errorf("wait for application listener: %w", ctx.Err())
		case <-retry.C:
		}
	}
}
