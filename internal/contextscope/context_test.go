package contextscope

import (
	"strings"
	"testing"
)

func TestResolutionErrorAndPrompt(t *testing.T) {
	var nilError *ResolutionError
	if nilError.Error() != "structured context could not be resolved" {
		t.Fatalf("nil error = %q", nilError.Error())
	}
	one := (&ResolutionError{Issues: []Issue{{Message: "file is stale"}}}).Error()
	if one != "file is stale" {
		t.Fatalf("single issue = %q", one)
	}
	many := (&ResolutionError{Issues: []Issue{{Message: "one"}, {Message: "two"}}}).Error()
	if many != "2 structured contexts could not be resolved" {
		t.Fatalf("many issues = %q", many)
	}
	if Prompt(nil) != "" {
		t.Fatal("empty contexts produced a prompt")
	}
	prompt := Prompt([]Context{
		{Kind: KindRepository, RepositoryID: 7, Revision: "abc"},
		{Kind: KindFile, RepositoryID: 7, Revision: "abc", Path: "internal/main.go"},
		{
			Kind: KindSymbol, RepositoryID: 7, Revision: "abc",
			Path: "internal/main.go", Symbol: "run", SymbolKind: "function",
			StartLine: 12, EndLine: 20,
		},
	})
	for _, expected := range []string{
		"repository_id=7 revision=abc",
		`file repository_id=7 revision=abc path="internal/main.go"`,
		`symbol repository_id=7 revision=abc path="internal/main.go" symbol="run" symbol_kind="function" lines=12-20`,
	} {
		if !strings.Contains(prompt, expected) {
			t.Fatalf("prompt missing %q: %s", expected, prompt)
		}
	}
}
