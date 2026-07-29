package releasecheck

import (
	"strings"
	"testing"
)

// TestRoadmapKeepsM7ScopeAndCompletedMilestones makes roadmap contraction an
// explicit code-review decision. When a capability is delivered, update this
// test with SCOPE.md; a rebase must not silently delete or mark it complete.
func TestRoadmapKeepsM7ScopeAndCompletedMilestones(t *testing.T) {
	root := repositoryRoot(t)
	scope := normalizeScope(string(readFile(t, root, "SCOPE.md")))
	m7 := scopeSection(t, scope, "### M7:", "### M8:")
	m9 := scopeSection(t, scope, "### M9:", "### M10:")
	m17 := scopeSection(t, scope, "### M17:", "### M18:")
	m18 := scopeSection(t, scope, "### M18:", "### M19:")
	m19 := scopeSection(t, scope, "### M19:", "### M20:")
	m20 := scopeSection(t, scope, "### M20:", "### M21:")
	m21 := scopeSection(t, scope, "### M21:", "## Definition of quality")

	for _, completed := range []string{
		"- [x] Add permission-aware Chat autocomplete for `@repository` and `@file`",
		"- [x] Extend permission-aware autocomplete to `@directory` and `@symbol`",
		"- [x] Render repository and file mentions as removable context chips",
		"- [x] Resolve repository and file mentions against the commit-pinned catalogue",
		"- [x] Share the typed repository/file context contract across Chat",
		"- [x] Support pasting a RepoKarta source",
		"- [x] Extend the same structured chip contract to directory and symbol",
		"- [x] Extend resolution to directory and symbol contexts",
		"- [x] Add named search contexts",
		"- [x] Make every effective context visible",
		"- [x] Add one documented query grammar",
		"- [x] Provide explicit result types",
		"- [x] Add qualified programming-element search",
		"- [x] Add result facets",
		"- [x] Rank exact symbol and path matches",
		"- [x] Add one-click actions",
		"- [x] Import optional SCIP indexes",
		"- [x] Add graph queries",
		"- [x] Search commits and diffs",
		"- [x] Ingest CODEOWNERS as commit-pinned metadata",
		"- [x] Persist per-author recent and saved searches",
		"- [x] Turn a saved deterministic query into a monitor",
		"- [x] Add an optional Deep Search mode",
		"- [x] Stream a concise exploration trace",
		"- [x] Preserve structured mentions and named contexts",
		"- [x] Provide cancellation, retry from persisted deterministic evidence",
		"- [x] Make every Deep Search answer addressable",
		"- [x] Keep optional semantic retrieval or reranking clearly labeled and bounded",
	} {
		if !strings.Contains(m7, completed) {
			t.Errorf("M7 completed scope is missing %q", completed)
		}
	}
	if strings.Contains(m7, "- [ ]") {
		t.Error("M7 still contains unchecked scope")
	}
	if strings.Contains(m9, "- [ ]") {
		t.Error("M9 still contains unchecked scope")
	}
	if strings.Contains(m17, "- [ ]") {
		t.Error("M17 still contains unchecked scope")
	}
	if strings.Contains(m18, "- [ ]") {
		t.Error("M18 still contains unchecked scope")
	}
	if strings.Contains(m19, "- [ ]") {
		t.Error("M19 still contains unchecked scope")
	}
	if strings.Contains(m20, "- [ ]") {
		t.Error("M20 still contains unchecked scope")
	}
	if strings.Contains(m21, "- [ ]") {
		t.Error("M21 still contains unchecked scope")
	}
	for _, completed := range []string{
		"- [x] Derive revision-pinned framework and executable reachability roots",
		"- [x] Watch bounded Git metadata",
		"- [x] Stream large bounded search result prefixes",
		"- [x] Resolve a locally configured `origin/HEAD`",
		"- [x] Build and boot-smoke Linux amd64 and arm64",
	} {
		if !strings.Contains(m20, completed) {
			t.Errorf("M20 completed scope is missing %q", completed)
		}
	}

	status := scopeSection(
		t,
		scope,
		"## Current implementation version",
		"## Recommended next session",
	)
	for _, required := range []string{
		"M0 through M21 are complete",
		"M21 adds the separately authorized Code tab",
		"M7 now includes qualified symbol search",
		"M9 now includes explicit comparison and distance states",
		"Linked-worktree discovery deduplication",
		"M10 enterprise identity and administration are complete",
		"M17 makes frontend builds reproducible",
		"M18 consolidates Git execution and atomic publication",
		"OpenTelemetry metrics, structured logs, and traces are available",
		"M20 also debounces committed local-repository changes",
	} {
		if !strings.Contains(status, required) {
			t.Errorf("implementation status is missing %q", required)
		}
	}
}

func TestM18ConsolidationBoundaries(t *testing.T) {
	root := repositoryRoot(t)
	for relative, maximumBytes := range map[string]int{
		"internal/httpserver/server.go": 30_000,
		"internal/graph/graph.go":       60_000,
	} {
		if size := len(readFile(t, root, relative)); size > maximumBytes {
			t.Errorf("%s grew to %d bytes, want at most %d", relative, size, maximumBytes)
		}
	}

	for _, relative := range []string{
		"internal/atomicfile/atomicfile.go",
		"internal/gitexec/gitexec.go",
		"internal/sourceintelligence/sourceintelligence.go",
		"internal/httpserver/conversations.go",
		"internal/httpserver/dependencies.go",
		"internal/httpserver/middleware.go",
		"internal/httpserver/render.go",
		"internal/httpserver/routes.go",
		"internal/httpserver/search.go",
		"internal/httpserver/source.go",
		"internal/graph/artifact_io.go",
		"internal/graph/git_plumbing.go",
		"internal/graph/manifest_dotnet.go",
		"internal/graph/manifest_go.go",
		"internal/graph/manifest_jvm.go",
		"internal/graph/manifest_node.go",
		"internal/graph/manifest_python.go",
		"internal/graph/manifest_rust.go",
		"internal/graph/service_artifacts.go",
		"internal/graph/spring.go",
		"web/src/repository-picker.mjs",
		"web/src/repository-picker.test.mjs",
	} {
		if len(readFile(t, root, relative)) == 0 {
			t.Errorf("%s is empty", relative)
		}
	}
}

func normalizeScope(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func scopeSection(t *testing.T, scope, startMarker, endMarker string) string {
	t.Helper()
	start := strings.Index(scope, startMarker)
	if start < 0 {
		t.Fatalf("scope is missing section marker %q", startMarker)
	}
	end := strings.Index(scope[start+len(startMarker):], endMarker)
	if end < 0 {
		t.Fatalf("scope is missing section marker %q after %q", endMarker, startMarker)
	}
	return scope[start : start+len(startMarker)+end]
}
