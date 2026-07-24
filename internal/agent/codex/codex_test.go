package codex

import (
	"testing"

	"github.com/spolnik/RepoKarta/internal/agent"
)

func TestTurnStartParamsIncludeEffort(t *testing.T) {
	params := turnStartParams("thread", agent.Turn{Message: "prompt"}, nil, "xhigh")
	if params["effort"] != "xhigh" {
		t.Fatalf("effort = %#v", params["effort"])
	}
}

func TestTurnStartParamsLeaveProviderEffortDefault(t *testing.T) {
	params := turnStartParams("thread", agent.Turn{Message: "prompt"}, nil, "")
	if _, exists := params["effort"]; exists {
		t.Fatalf("default params unexpectedly contain effort: %#v", params)
	}
}

func TestTurnStartParamsIncludeLocalImages(t *testing.T) {
	params := turnStartParams(
		"thread",
		agent.Turn{Message: "inspect this"},
		[]string{`C:\temp\one.png`, `C:\temp\two.webp`},
		"high",
	)
	input, ok := params["input"].([]map[string]any)
	if !ok || len(input) != 3 {
		t.Fatalf("input = %#v", params["input"])
	}
	if input[0]["type"] != "text" || input[1]["type"] != "localImage" || input[2]["path"] != `C:\temp\two.webp` {
		t.Fatalf("unexpected input items: %#v", input)
	}
}
