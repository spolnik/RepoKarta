package claude

import (
	"slices"
	"testing"

	"github.com/spolnik/RepoKarta/internal/agent"
)

func TestCommandArgumentsIncludeProviderModelAndEffort(t *testing.T) {
	arguments := commandArguments(agent.SessionConfig{
		Model:  "opus",
		Effort: "high",
	}, `{"mcpServers":{}}`)

	if !containsPair(arguments, "--model", "opus") {
		t.Fatalf("arguments do not include model: %#v", arguments)
	}
	if !containsPair(arguments, "--effort", "high") {
		t.Fatalf("arguments do not include effort: %#v", arguments)
	}
}

func TestCommandArgumentsLeaveProviderDefaultsUnchanged(t *testing.T) {
	arguments := commandArguments(agent.SessionConfig{}, `{"mcpServers":{}}`)
	if slices.Contains(arguments, "--model") || slices.Contains(arguments, "--effort") {
		t.Fatalf("default arguments unexpectedly override provider settings: %#v", arguments)
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
