package graph

import "testing"

func TestCODEOWNERSUsesPrecedenceAndLastMatchingRule(t *testing.T) {
	files := []string{"CODEOWNERS", ".github/CODEOWNERS", "docs/CODEOWNERS"}
	if got := codeownersPath(files); got != ".github/CODEOWNERS" {
		t.Fatalf("CODEOWNERS path = %q", got)
	}
	repository := Repository{ID: 7, Name: "payments", Revision: "abc123"}
	index := parseCODEOWNERS(
		repository,
		".github/CODEOWNERS",
		[]byte("* @platform\n/docs/ @writers\n/docs/private/* invalid-owner\n"),
		func(path string, line int, label string) Evidence {
			return Evidence{
				RepositoryID: repository.ID,
				Repository:   repository.Name,
				Revision:     repository.Revision,
				Path:         path,
				Line:         line,
				Label:        label,
			}
		},
	)
	owned := ResolveOwners(index, "internal/server.go")
	if owned.State != "owned" || len(owned.Owners) != 1 || owned.Owners[0] != "@platform" {
		t.Fatalf("owned match = %#v", owned)
	}
	writers := ResolveOwners(index, "docs/guide.md")
	if writers.State != "owned" || writers.Owners[0] != "@writers" || writers.Evidence.Line != 2 {
		t.Fatalf("directory match = %#v", writers)
	}
	unresolved := ResolveOwners(index, "docs/private/plan.md")
	if unresolved.State != "unresolved_owner" ||
		len(unresolved.UnresolvedOwners) != 1 ||
		unresolved.Evidence.Line != 3 {
		t.Fatalf("unresolved match = %#v", unresolved)
	}
}

func TestOwnershipDistinguishesUnavailableAndUnowned(t *testing.T) {
	if got := ResolveOwners(OwnershipIndex{}, "main.go"); got.State != "unavailable" {
		t.Fatalf("unavailable match = %#v", got)
	}
	if got := ResolveOwners(OwnershipIndex{Available: true}, "main.go"); got.State != "unowned" {
		t.Fatalf("unowned match = %#v", got)
	}
}
