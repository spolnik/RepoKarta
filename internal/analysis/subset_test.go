//go:build grammar_subset

package analysis

import (
	"slices"
	"testing"

	"github.com/odvcencio/gotreesitter/grammars"
)

func TestProductionGrammarSubsetMatchesSupportedLanguages(t *testing.T) {
	got := make([]string, 0)
	for _, entry := range grammars.AllLanguages() {
		got = append(got, entry.Name)
	}
	slices.Sort(got)
	want := []string{
		"bash",
		"go",
		"groovy",
		"java",
		"javascript",
		"kotlin",
		"python",
		"sql",
		"tsx",
		"typescript",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("embedded grammars = %v, want %v", got, want)
	}
}
