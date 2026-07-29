package sourceintelligence

import (
	"testing"

	"github.com/spolnik/RepoKarta/internal/graph"
)

func TestRouteMatchesCallerEvidence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		route    string
		evidence string
		want     bool
	}{
		{name: "exact", route: "GET /pets", evidence: "http://pets/pets", want: true},
		{name: "path parameter", route: "GET /pets/{id}", evidence: "http://pets/pets/42", want: true},
		{name: "different shape", route: "GET /pets/{id}", evidence: "http://pets/pets", want: false},
		{name: "non route label", route: "pet controller", evidence: "http://pets/pets", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := RouteMatchesCallerEvidence(test.route, []graph.Evidence{{Label: test.evidence}})
			if got != test.want {
				t.Fatalf("RouteMatchesCallerEvidence(%q, %q) = %v, want %v", test.route, test.evidence, got, test.want)
			}
		})
	}
}

func TestBuildReportsUnavailableRouteArtifacts(t *testing.T) {
	t.Parallel()

	view := Build(t.Context(), nil, nil, Request{RepositoryID: 42})
	if view.State != "unavailable" {
		t.Fatalf("State = %q, want unavailable", view.State)
	}
	if view.Message == "" {
		t.Fatal("Message is empty")
	}
	if view.Routes == nil || view.Callers == nil {
		t.Fatal("Build must return initialized route and caller collections")
	}
}
