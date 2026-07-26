package app

import (
	"context"
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
	config, err := DefaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	if config.ListenAddress != "127.0.0.1:7331" ||
		config.Security.Mode != security.ModeOpen ||
		config.Security.PublicURL != "https://repo.example.com" ||
		!config.AllowOpen || config.AdminUser != "admin" ||
		config.AcquisitionGitHubAPI != "https://api.github.test" ||
		filepath.Base(config.DataDirectory) != "RepoKarta" {
		t.Fatalf("default config = %#v", config)
	}
}

func TestRunBuildsAndShutsDownLocalApplication(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(250*time.Millisecond, cancel)
	err := Run(ctx, Config{
		ListenAddress:  "127.0.0.1:0",
		DataDirectory:  filepath.Join(t.TempDir(), "data"),
		RepositoryRoot: t.TempDir(),
		Version:        "coverage-test",
		OpenBrowser:    false,
		CodexCommand:   "missing-codex-for-test",
		ClaudeCommand:  "missing-claude-for-test",
		Security:       security.Settings{Mode: security.ModeLocal},
	})
	if err != nil {
		t.Fatalf("run with canceled context: %v", err)
	}
}
