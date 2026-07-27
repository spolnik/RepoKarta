// Package contextscope defines RepoKarta's protocol-independent structured
// repository, file, directory, and symbol context contract.
package contextscope

import (
	"fmt"
	"strings"
)

const (
	// MaximumContexts bounds every request before repository or Git access.
	MaximumContexts = 32

	KindRepository = "repository"
	KindFile       = "file"
	KindDirectory  = "directory"
	KindSymbol     = "symbol"
)

// Selector is the client-supplied identity of a repository, committed path, or
// source declaration. Display labels are deliberately absent: callers must
// send stable repository IDs, pinned revisions, paths, and symbol coordinates.
type Selector struct {
	Kind         string `json:"kind"`
	RepositoryID int64  `json:"repository_id"`
	Revision     string `json:"revision,omitempty"`
	Path         string `json:"path,omitempty"`
	Symbol       string `json:"symbol,omitempty"`
	SymbolKind   string `json:"symbol_kind,omitempty"`
	Line         int    `json:"line,omitempty"`
}

// Context is a selector resolved against the viewer's current, commit-pinned
// catalogue. Repository and Label are presentation-only output fields.
type Context struct {
	Kind         string `json:"kind"`
	RepositoryID int64  `json:"repository_id"`
	Repository   string `json:"repository"`
	Revision     string `json:"revision"`
	Path         string `json:"path,omitempty"`
	Symbol       string `json:"symbol,omitempty"`
	SymbolKind   string `json:"symbol_kind,omitempty"`
	Line         int    `json:"line,omitempty"`
	StartLine    int    `json:"start_line,omitempty"`
	EndLine      int    `json:"end_line,omitempty"`
	Label        string `json:"label"`
}

// Issue reports one context that could not be resolved without broadening the
// request. Code is stable for clients; Message is actionable presentation text.
type Issue struct {
	Index    int      `json:"index"`
	Code     string   `json:"code"`
	Message  string   `json:"message"`
	Selector Selector `json:"context"`
}

// ResolutionError prevents callers from silently dropping invalid contexts.
type ResolutionError struct {
	Issues []Issue `json:"issues"`
}

func (e *ResolutionError) Error() string {
	if e == nil || len(e.Issues) == 0 {
		return "structured context could not be resolved"
	}
	if len(e.Issues) == 1 {
		return e.Issues[0].Message
	}
	return fmt.Sprintf("%d structured contexts could not be resolved", len(e.Issues))
}

// Prompt renders already-resolved contexts for a provider. It never parses a
// user-visible mention back into an identity.
func Prompt(contexts []Context) string {
	if len(contexts) == 0 {
		return ""
	}
	var output strings.Builder
	output.WriteString("RepoKarta resolved the following structured context at exact indexed revisions. Start within this context and cite fresh RepoKarta tool evidence:\n")
	for _, context := range contexts {
		fmt.Fprintf(&output, "- %s repository_id=%d revision=%s", context.Kind, context.RepositoryID, context.Revision)
		if context.Path != "" {
			fmt.Fprintf(&output, " path=%q", context.Path)
		}
		if context.Symbol != "" {
			fmt.Fprintf(
				&output,
				" symbol=%q symbol_kind=%q lines=%d-%d",
				context.Symbol,
				context.SymbolKind,
				context.StartLine,
				context.EndLine,
			)
		}
		output.WriteByte('\n')
	}
	return strings.TrimSpace(output.String())
}
