// Package contextscope defines RepoKarta's protocol-independent structured
// repository and file context contract.
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
)

// Selector is the client-supplied identity of a repository or committed file.
// Display labels are deliberately absent: callers must send stable repository
// IDs, pinned revisions, and repository-relative paths.
type Selector struct {
	Kind         string `json:"kind"`
	RepositoryID int64  `json:"repository_id"`
	Revision     string `json:"revision,omitempty"`
	Path         string `json:"path,omitempty"`
}

// Context is a selector resolved against the viewer's current, commit-pinned
// catalogue. Repository and Label are presentation-only output fields.
type Context struct {
	Kind         string `json:"kind"`
	RepositoryID int64  `json:"repository_id"`
	Repository   string `json:"repository"`
	Revision     string `json:"revision"`
	Path         string `json:"path,omitempty"`
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
		output.WriteByte('\n')
	}
	return strings.TrimSpace(output.String())
}
