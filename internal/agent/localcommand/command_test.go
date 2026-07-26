package localcommand

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveConfiguredCommandsAndExecutableName(t *testing.T) {
	command := filepath.Join(t.TempDir(), "provider-command")
	if err := os.WriteFile(command, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve(command, "provider")
	if err != nil || resolved != command {
		t.Fatalf("resolved = %q, err = %v", resolved, err)
	}
	if _, err := Resolve(filepath.Join(t.TempDir(), "missing"), "provider"); err == nil {
		t.Fatal("missing absolute command was accepted")
	}
	if name := executable("codex"); !strings.HasPrefix(name, "codex") {
		t.Fatalf("executable name = %q", name)
	}
	if canExecute(command) {
		t.Fatal("non-executable fixture reported a successful version command")
	}
}
