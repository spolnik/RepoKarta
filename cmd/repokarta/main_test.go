package main

import (
	"regexp"
	"testing"
)

// TestVersionIsTheCurrentImplementationVersion keeps the reported version in
// step with the completed milestone recorded in SCOPE.md.
func TestVersionIsTheCurrentImplementationVersion(t *testing.T) {
	if version != "0.40.0-dev" {
		t.Fatalf("version = %q, want %q", version, "0.40.0-dev")
	}
	if !regexp.MustCompile(`^\d+\.\d+\.\d+(-[a-z]+)?$`).MatchString(version) {
		t.Fatalf("version %q is not a semantic version", version)
	}
}
