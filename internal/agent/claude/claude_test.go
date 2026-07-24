package claude

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/spolnik/RepoKarta/internal/agent"
)

func TestCommandArgumentsIncludeProviderModelAndEffort(t *testing.T) {
	arguments := commandArguments(agent.SessionConfig{
		Model:  "opus",
		Effort: "high",
	}, `{"mcpServers":{}}`, `/tmp/attachments`)

	if !containsPair(arguments, "--model", "opus") {
		t.Fatalf("arguments do not include model: %#v", arguments)
	}
	if !containsPair(arguments, "--effort", "high") {
		t.Fatalf("arguments do not include effort: %#v", arguments)
	}
	if !containsPair(arguments, "--add-dir", `/tmp/attachments`) {
		t.Fatalf("arguments do not allow the attachment directory: %#v", arguments)
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

func TestCommandArgumentsLeaveProviderDefaultsUnchanged(t *testing.T) {
	arguments := commandArguments(agent.SessionConfig{}, `{"mcpServers":{}}`, "")
	if slices.Contains(arguments, "--model") || slices.Contains(arguments, "--effort") {
		t.Fatalf("default arguments unexpectedly override provider settings: %#v", arguments)
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
