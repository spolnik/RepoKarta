package claude

import (
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
