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

	for _, completed := range []string{
		"- [x] Add permission-aware Chat autocomplete for `@repository` and `@file`",
		"- [x] Render repository and file mentions as removable context chips",
		"- [x] Resolve repository and file mentions against the commit-pinned catalogue",
		"- [x] Share the typed repository/file context contract across Chat",
		"- [x] Support pasting a RepoKarta source",
		"- [x] Extend the same structured chip contract to directory and symbol",
		"- [x] Extend resolution to directory and symbol contexts",
		"- [x] Add named search contexts",
		"- [x] Make every effective context visible",
		"- [x] Provide explicit result types",
		"- [x] Add result facets",
		"- [x] Rank exact symbol and path matches",
		"- [x] Add one-click actions",
		"- [x] Search commits and diffs",
	} {
		if !strings.Contains(m7, completed) {
			t.Errorf("M7 completed scope is missing %q", completed)
		}
	}

	for _, remaining := range []string{
		"- [ ] Extend permission-aware autocomplete to `@directory` and `@symbol`",
		"- [ ] Add one documented query grammar",
		"- [ ] Import optional SCIP indexes",
		"- [ ] Add graph queries",
		"- [ ] Persist per-author recent and saved searches",
		"- [ ] Add an optional Deep Search mode",
		"- [ ] Make every Deep Search answer addressable",
	} {
		if !strings.Contains(m7, remaining) {
			t.Errorf("M7 remaining scope is missing or no longer open: %q", remaining)
		}
	}

	status := scopeSection(
		t,
		scope,
		"## Current implementation version",
		"## Recommended next session",
	)
	for _, required := range []string{
		"M0 through M6 are complete",
		"M7 is in progress",
		"M9 dependency inventory, public registry refresh, lockfile resolution",
		"Linked-worktree discovery deduplication",
		"M10 enterprise identity and administration are complete",
	} {
		if !strings.Contains(status, required) {
			t.Errorf("implementation status is missing %q", required)
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
