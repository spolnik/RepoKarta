package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWritePublishesAndReplacesCompleteContent(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "snapshot.json")
	if err := Write(target, []byte("first"), Options{Pattern: "snapshot.*.tmp", Sync: true}); err != nil {
		t.Fatal(err)
	}
	if err := Write(target, []byte("second"), Options{Pattern: "snapshot.*.tmp", Sync: true}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "second" {
		t.Fatalf("content = %q, want second", content)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "snapshot.json" {
		t.Fatalf("published entries = %#v, want only snapshot.json", entries)
	}
}

func TestWriteAppliesRequestedMode(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Windows file modes are governed by ACLs")
	}
	target := filepath.Join(t.TempDir(), "private.json")
	if err := Write(target, []byte("{}"), Options{Mode: 0o640}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("mode = %o, want 640", got)
	}
}
