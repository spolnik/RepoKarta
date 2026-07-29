package claude

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spolnik/RepoKarta/internal/agent"
)

func TestCommandArgumentsIncludeProviderModelAndEffort(t *testing.T) {
	configPath := `/tmp/attachments/mcp.json`
	arguments := commandArguments(agent.SessionConfig{
		Model:  "claude-opus-5",
		Effort: "high",
	}, configPath, `/tmp/attachments`)

	if !containsPair(arguments, "--model", "claude-opus-5") {
		t.Fatalf("arguments do not include model: %#v", arguments)
	}
	if !containsPair(arguments, "--effort", "high") {
		t.Fatalf("arguments do not include effort: %#v", arguments)
	}
	if !containsPair(arguments, "--add-dir", `/tmp/attachments`) {
		t.Fatalf("arguments do not allow the attachment directory: %#v", arguments)
	}
	if !containsPair(arguments, "--setting-sources", "user") {
		t.Fatalf("arguments do not load user settings only: %#v", arguments)
	}
	if !containsPair(arguments, "--mcp-config", configPath) {
		t.Fatalf("arguments do not reference the protected MCP file: %#v", arguments)
	}
	joined := strings.Join(arguments, " ")
	for _, blocked := range []string{"Glob", "Grep", "Task", "Agent"} {
		if !strings.Contains(joined, blocked) {
			t.Fatalf("disallowed tools omit %q: %#v", blocked, arguments)
		}
	}
	if !strings.Contains(joined, "Read(/tmp/attachments/**)") {
		t.Fatalf("attachment reads are not path-scoped: %#v", arguments)
	}
	if strings.Contains(joined, "test-token") {
		t.Fatalf("provider token leaked into argv: %#v", arguments)
	}
	if !containsPair(statusCommandArguments(), "--setting-sources", "user") {
		t.Fatalf("auth probe does not use the runtime setting source: %#v", statusCommandArguments())
	}
}

func TestCommandArgumentsUseOpusMediumByDefault(t *testing.T) {
	arguments := commandArguments(agent.SessionConfig{}, `{"mcpServers":{}}`, "")
	if !containsPair(arguments, "--model", "claude-opus-5") {
		t.Fatalf("default arguments do not select Opus 5: %#v", arguments)
	}
	if !containsPair(arguments, "--effort", "medium") {
		t.Fatalf("default arguments do not select medium effort: %#v", arguments)
	}
}

func TestCodingArgumentsAllowEditsButDenyShellAndWeb(t *testing.T) {
	arguments := commandArguments(agent.SessionConfig{
		Coding: true, Model: "claude-opus-5", Effort: "high",
	}, `/tmp/mcp.json`, "")
	joined := strings.Join(arguments, " ")
	if !containsPair(arguments, "--permission-mode", "acceptEdits") {
		t.Fatalf("coding mode does not accept file edits: %#v", arguments)
	}
	for _, expected := range []string{"Read", "Edit", "Write", "Glob", "Grep", "mcp__repokarta__*"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("coding allowed tools omit %q: %#v", expected, arguments)
		}
	}
	for _, blocked := range []string{"Bash", "NotebookEdit", "WebFetch", "WebSearch"} {
		if !strings.Contains(joined, blocked) {
			t.Fatalf("coding disallowed tools omit %q: %#v", blocked, arguments)
		}
	}
}

func TestCommandInheritsParentEnvironmentFromNeutralDirectory(t *testing.T) {
	directory := t.TempDir()
	command := newCommand(t.Context(), "claude", []string{"--version"}, directory)
	if command.Dir != directory {
		t.Fatalf("command directory = %q, want neutral directory %q", command.Dir, directory)
	}
	if command.Env != nil {
		t.Fatalf("command Env = %#v, want nil so os/exec inherits the parent environment", command.Env)
	}
}

func TestUserSettingsEnvAndStopHookApplyWithoutRepositorySettings(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a deterministic fake Claude CLI")
	}
	root := t.TempDir()
	userSettings := filepath.Join(root, "user-settings.json")
	repository := filepath.Join(root, "indexed-repository")
	projectSettings := filepath.Join(repository, ".claude", "settings.json")
	stopMarker := filepath.Join(root, "user-stop-hook.txt")
	projectStopMarker := filepath.Join(root, "project-stop-hook.txt")
	reportPath := filepath.Join(root, "child-report.json")
	if err := os.MkdirAll(filepath.Dir(projectSettings), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONFixture(t, userSettings, map[string]any{
		"env": map[string]string{"REPOKARTA_SETTINGS_MARKER": "user-loaded"},
		"hooks": map[string]any{
			"Stop": []any{map[string]any{"hooks": []any{
				map[string]string{"type": "command", "command": "record-user-stop"},
			}}},
		},
	})
	writeJSONFixture(t, projectSettings, map[string]any{
		"env": map[string]string{"REPOKARTA_SETTINGS_MARKER": "project-loaded"},
		"hooks": map[string]any{
			"Stop": []any{map[string]any{"hooks": []any{
				map[string]string{"type": "command", "command": "record-project-stop"},
			}}},
		},
	})
	helper := buildClaudeSettingsHelper(t, root)
	t.Setenv("REPOKARTA_TEST_USER_SETTINGS", userSettings)
	t.Setenv("REPOKARTA_TEST_REPOSITORY", repository)
	t.Setenv("REPOKARTA_TEST_STOP_MARKER", stopMarker)
	t.Setenv("REPOKARTA_TEST_PROJECT_STOP_MARKER", projectStopMarker)
	t.Setenv("REPOKARTA_TEST_REPORT", reportPath)
	t.Setenv("REPOKARTA_PARENT_MARKER", "parent-inherited")

	adapter := &Adapter{Command: helper}
	session, err := adapter.Start(t.Context(), agent.SessionConfig{
		MCPURL:   "http://127.0.0.1:7331/mcp",
		MCPToken: "test-token",
		Model:    "claude-haiku-4-5",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := session.Send(ctx, agent.Turn{Message: "one grounded turn"}, func(agent.Event) error {
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	var report struct {
		SettingSources string `json:"setting_sources"`
		SettingsMarker string `json:"settings_marker"`
		ParentMarker   string `json:"parent_marker"`
		WorkingDir     string `json:"working_dir"`
	}
	content, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(content, &report); err != nil {
		t.Fatal(err)
	}
	if report.SettingSources != "user" || report.SettingsMarker != "user-loaded" {
		t.Fatalf("child settings report = %+v", report)
	}
	if report.ParentMarker != "parent-inherited" {
		t.Fatalf("parent environment was not inherited: %+v", report)
	}
	if filepath.Clean(report.WorkingDir) == filepath.Clean(repository) {
		t.Fatalf("Claude ran in the indexed repository: %+v", report)
	}
	if _, err := os.Stat(stopMarker); err != nil {
		t.Fatalf("user Stop hook did not fire: %v", err)
	}
	if _, err := os.Stat(projectStopMarker); !os.IsNotExist(err) {
		t.Fatalf("repository-scoped Stop hook unexpectedly fired: %v", err)
	}
}

func TestStatusUsesExplicitModelNamesAndPerModelEfforts(t *testing.T) {
	status := (&Adapter{Command: "definitely-not-a-claude-command"}).Status(t.Context())
	if len(status.Models) == 0 || status.Models[0].ID != defaultModel {
		t.Fatalf("default model is not first in the catalog: %#v", status.Models)
	}
	for _, expected := range []struct {
		id          string
		label       string
		effortCount int
	}{
		{id: "claude-fable-5", label: "Fable 5", effortCount: 5},
		{id: "claude-opus-5", label: "Opus 5", effortCount: 5},
		{id: "claude-opus-4-8", label: "Opus 4.8", effortCount: 5},
		{id: "claude-sonnet-5", label: "Sonnet 5", effortCount: 5},
		{id: "claude-haiku-4-5", label: "Haiku 4.5", effortCount: 0},
	} {
		index := slices.IndexFunc(status.Models, func(model agent.ModelOption) bool {
			return model.ID == expected.id
		})
		if index < 0 || status.Models[index].Label != expected.label ||
			len(status.Models[index].Efforts) != expected.effortCount {
			t.Fatalf("model catalog does not contain %#v: %#v", expected, status.Models)
		}
	}
}

func TestContextUsageFromResponse(t *testing.T) {
	usage, ok := contextUsageFromResponse(json.RawMessage(`{
		"totalTokens":48000,
		"maxTokens":200000,
		"rawMaxTokens":200000,
		"percentage":24,
		"model":"claude-sonnet"
	}`))
	if !ok {
		t.Fatal("context usage was not parsed")
	}
	if usage.UsedTokens != 48000 || usage.MaxTokens != 200000 || usage.Percentage != 24 || usage.Model != "claude-sonnet" {
		t.Fatalf("unexpected context usage: %#v", usage)
	}
}

func TestCommandArgumentsLeaveHaikuEffortOnProviderDefault(t *testing.T) {
	arguments := commandArguments(agent.SessionConfig{Model: "claude-haiku-4-5"}, `{"mcpServers":{}}`, "")
	if !containsPair(arguments, "--model", "claude-haiku-4-5") {
		t.Fatalf("arguments do not select Haiku: %#v", arguments)
	}
	if slices.Contains(arguments, "--effort") {
		t.Fatalf("Haiku arguments unexpectedly override provider effort: %#v", arguments)
	}
}

func TestPromptWithAttachmentsListsOnlyExactPaths(t *testing.T) {
	prompt := promptWithAttachments(agent.Turn{
		Message: "What is shown?",
		Images: []agent.Image{
			{Name: "screen.png"},
			{Name: "diagram.webp"},
		},
	}, []string{`C:\safe\one.png`, `C:\safe\two.webp`})
	for _, expected := range []string{
		"What is shown?",
		"These exact attachment paths are the only local files you may read:",
		`"screen.png": C:\safe\one.png`,
		`"diagram.webp": C:\safe\two.webp`,
	} {
		if !strings.Contains(prompt, expected) {
			t.Fatalf("prompt does not contain %q: %s", expected, prompt)
		}
	}
}

func containsPair(arguments []string, flag, value string) bool {
	for index := 0; index+1 < len(arguments); index++ {
		if arguments[index] == flag && arguments[index+1] == value {
			return true
		}
	}
	return false
}

func writeJSONFixture(t *testing.T, path string, value any) {
	t.Helper()
	content, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
}

func buildClaudeSettingsHelper(t *testing.T, directory string) string {
	t.Helper()
	source := filepath.Join(directory, "main.go")
	executable := filepath.Join(directory, "claude-settings-helper.exe")
	if err := os.WriteFile(source, []byte(claudeSettingsHelperSource), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("go", "build", "-o", executable, source)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build fake Claude CLI: %v\n%s", err, output)
	}
	return executable
}

const claudeSettingsHelperSource = `package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

type settings struct {
	Env map[string]string ` + "`json:\"env\"`" + `
	Hooks map[string][]struct {
		Hooks []struct {
			Command string ` + "`json:\"command\"`" + `
		} ` + "`json:\"hooks\"`" + `
	} ` + "`json:\"hooks\"`" + `
}

func valueAfter(arguments []string, name string) string {
	for index := 0; index+1 < len(arguments); index++ {
		if arguments[index] == name {
			return arguments[index+1]
		}
	}
	return ""
}

func readSettings(path string) settings {
	var result settings
	content, _ := os.ReadFile(path)
	_ = json.Unmarshal(content, &result)
	return result
}

func main() {
	sources := valueAfter(os.Args, "--setting-sources")
	user := readSettings(os.Getenv("REPOKARTA_TEST_USER_SETTINGS"))
	marker := user.Env["REPOKARTA_SETTINGS_MARKER"]
	workingDirectory, _ := os.Getwd()
	repository := os.Getenv("REPOKARTA_TEST_REPOSITORY")
	projectLoaded := false
	if (strings.Contains(sources, "project") || strings.Contains(sources, "local")) &&
		strings.HasPrefix(filepath.Clean(workingDirectory), filepath.Clean(repository)) {
		project := readSettings(filepath.Join(repository, ".claude", "settings.json"))
		if value := project.Env["REPOKARTA_SETTINGS_MARKER"]; value != "" {
			marker = value
			projectLoaded = true
		}
	}
	report, _ := json.Marshal(map[string]any{
		"setting_sources": sources,
		"settings_marker": marker,
		"parent_marker": os.Getenv("REPOKARTA_PARENT_MARKER"),
		"working_dir": workingDirectory,
	})
	_ = os.WriteFile(os.Getenv("REPOKARTA_TEST_REPORT"), report, 0600)

	scanner := bufio.NewScanner(os.Stdin)
	if scanner.Scan() {
		if len(user.Hooks["Stop"]) > 0 {
			_ = os.WriteFile(os.Getenv("REPOKARTA_TEST_STOP_MARKER"), []byte("fired"), 0600)
		}
		if projectLoaded {
			_ = os.WriteFile(os.Getenv("REPOKARTA_TEST_PROJECT_STOP_MARKER"), []byte("fired"), 0600)
		}
		_, _ = os.Stdout.WriteString("{\"type\":\"result\",\"result\":\"ok\",\"session_id\":\"fixture\"}\n")
	}
}`
