package codex

import "testing"

func TestTurnStartParamsIncludeEffort(t *testing.T) {
	params := turnStartParams("thread", "prompt", "xhigh")
	if params["effort"] != "xhigh" {
		t.Fatalf("effort = %#v", params["effort"])
	}
}

func TestTurnStartParamsLeaveProviderEffortDefault(t *testing.T) {
	params := turnStartParams("thread", "prompt", "")
	if _, exists := params["effort"]; exists {
		t.Fatalf("default params unexpectedly contain effort: %#v", params)
	}
}
