package audit

import (
	"strings"
	"testing"
)

func TestNormalizeRedactsSecretsAndBoundsEvidence(t *testing.T) {
	event := Normalize(Event{
		Action: "security.test", TargetType: "request",
		Metadata: map[string]string{
			"token":               "secret",
			"password_hint":       "secret",
			"repository_source":   "package main",
			"safe":                strings.Repeat("x", 600),
			"authentication_mode": "saml",
		},
	})
	for _, key := range []string{"token", "password_hint", "repository_source"} {
		if _, ok := event.Metadata[key]; ok {
			t.Fatalf("secret-bearing metadata %q was retained", key)
		}
	}
	if len(event.Metadata["safe"]) != 512 {
		t.Fatalf("safe metadata length = %d, want 512", len(event.Metadata["safe"]))
	}
	if event.ActorID != "unknown" || event.CorrelationID == "" || event.CreatedAt.IsZero() {
		t.Fatalf("normalized event is incomplete: %#v", event)
	}
}
