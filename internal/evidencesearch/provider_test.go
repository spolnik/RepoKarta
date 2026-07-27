package evidencesearch

import (
	"context"
	"testing"

	"github.com/spolnik/RepoKarta/internal/codeintel"
	"github.com/spolnik/RepoKarta/internal/dependencies"
	"github.com/spolnik/RepoKarta/internal/docs"
	"github.com/spolnik/RepoKarta/internal/graph"
	"github.com/spolnik/RepoKarta/internal/insights"
	"github.com/spolnik/RepoKarta/internal/querylang"
)

type providerGraph struct {
	dependency graph.Snapshot
	routes     graph.Snapshot
	progress   graph.ArtifactProgress
}

func (g providerGraph) ReadDependencySnapshot(
	context.Context,
	int64,
) (graph.Snapshot, graph.ArtifactProgress, error) {
	return g.dependency, g.progress, nil
}

func (g providerGraph) ReadRouteSnapshot(
	context.Context,
	int64,
) (graph.Snapshot, graph.ArtifactProgress, error) {
	return g.routes, g.progress, nil
}

type providerDependencies struct{}

func (providerDependencies) Inventory(
	_ context.Context,
	snapshot graph.Snapshot,
	options dependencies.Options,
) (dependencies.Inventory, error) {
	return dependencies.BuildPage(snapshot, options), nil
}

type providerWiki struct {
	page docs.Page
}

func (w providerWiki) Pages(context.Context, int64) ([]docs.Page, error) {
	page := w.page
	page.Markdown = ""
	return []docs.Page{page}, nil
}

func (w providerWiki) Page(context.Context, int64, string) (docs.Page, error) {
	return w.page, nil
}

type providerInsights struct {
	response insights.QueryResponse
}

func (i providerInsights) Query(context.Context, insights.Filter) (insights.QueryResponse, error) {
	return i.response, nil
}

func TestProviderSearchesEveryPreparedEvidenceFamily(t *testing.T) {
	revision := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	evidence := graph.Evidence{
		RepositoryID: 7,
		Repository:   "payments",
		Revision:     revision,
		Path:         "internal/service.go",
		Line:         12,
		Label:        "source evidence",
		URL:          "http://ui/source/7#L12",
	}
	value := 91.5
	provider := New(
		providerGraph{
			dependency: graph.Snapshot{
				Repositories: []graph.Repository{{ID: 7, Name: "payments", Revision: revision}},
				Manifests: []graph.Manifest{{
					RepositoryID: 7,
					Repository:   "payments",
					Kind:         "go.mod",
					Path:         "go.mod",
					Evidence:     evidence,
					Declarations: []graph.DependencyDeclaration{{
						Ecosystem:  "go",
						Package:    "github.com/gorilla/mux",
						Declared:   "v1.8.1",
						Resolution: "exact",
						Evidence: graph.Evidence{
							RepositoryID: 7, Repository: "payments", Revision: revision,
							Path: "go.mod", Line: 4, URL: "http://ui/source/7#L4",
						},
					}},
				}},
				Scope: graph.Scope{Complete: true, TotalRepositories: 1, AnalyzedRepositories: 1},
			},
			routes: graph.Snapshot{
				Repositories: []graph.Repository{{ID: 7, Name: "payments", Revision: revision}},
				Nodes: []graph.Node{{
					Kind: "route", Label: "GET /payments/{id}", Layer: "Routes",
					RepositoryID: 7, Evidence: []graph.Evidence{evidence},
				}},
				Scope: graph.Scope{Complete: true, TotalRepositories: 1, AnalyzedRepositories: 1},
			},
			progress: graph.ArtifactProgress{
				State: "ready", RequestedRepositories: 1, ReadyRepositories: 1,
			},
		},
		providerDependencies{},
		providerWiki{page: docs.Page{
			RepositoryID:    7,
			Slug:            "architecture",
			Title:           "Architecture",
			Summary:         "Payment routing and storage.",
			Status:          docs.StatusReady,
			Revision:        revision,
			Markdown:        "# Architecture\n\nThe payment router uses mux.",
			SupportingFiles: []string{"internal/service.go"},
		}},
		providerInsights{response: insights.QueryResponse{
			Current: []insights.Observation{{
				RepositoryID: 7,
				Repository:   "payments",
				Revision:     revision,
				Tool:         "repokarta",
				Kind:         insights.KindMetric,
				Key:          "coverage",
				Value:        &value,
				Unit:         "percent",
				Path:         "internal/service.go",
				StartLine:    12,
				EndLine:      12,
				SourceURL:    evidence.URL,
				State:        insights.StateDerived,
			}},
		}},
		"http://ui",
	)
	repository := codeintel.DerivedEvidenceRepository{
		ID: 7, Name: "payments", Revision: revision,
	}
	tests := []struct {
		resultType string
		query      string
		title      string
	}{
		{resultType: "dependency", query: "mux result_type:dependency", title: "github.com/gorilla/mux"},
		{resultType: "route", query: "payments result_type:route", title: "GET /payments/{id}"},
		{resultType: "wiki_page", query: "router result_type:wiki_page", title: "Architecture"},
		{resultType: "code_insight", query: "coverage result_type:code_insight", title: "coverage"},
	}
	for _, testCase := range tests {
		t.Run(testCase.resultType, func(t *testing.T) {
			parsed, err := querylang.Parse(testCase.query)
			if err != nil {
				t.Fatal(err)
			}
			result, err := provider.SearchDerivedEvidence(t.Context(), codeintel.DerivedEvidenceRequest{
				ResultType:   testCase.resultType,
				Query:        parsed,
				Repositories: []codeintel.DerivedEvidenceRepository{repository},
				Limit:        10,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Items) != 1 ||
				result.Items[0].ResultType != testCase.resultType ||
				result.Items[0].Title != testCase.title ||
				result.Items[0].RepositoryID != repository.ID ||
				result.Items[0].SourceURL == "" {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

func TestProviderReportsMissingPreparedArtifactsInsteadOfBuilding(t *testing.T) {
	provider := New(
		providerGraph{progress: graph.ArtifactProgress{
			State: "building", RequestedRepositories: 1, PendingRepositories: 1,
		}},
		providerDependencies{},
		providerWiki{},
		providerInsights{},
		"http://ui",
	)
	parsed, err := querylang.Parse("result_type:dependency")
	if err != nil {
		t.Fatal(err)
	}
	result, err := provider.SearchDerivedEvidence(t.Context(), codeintel.DerivedEvidenceRequest{
		ResultType: "dependency",
		Query:      parsed,
		Repositories: []codeintel.DerivedEvidenceRepository{{
			ID: 7, Name: "payments", Revision: "aaaaaaaa",
		}},
		Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Truncated || result.TotalExact || len(result.Warnings) != 1 ||
		result.Warnings[0].Code != "dependency_artifact_building" {
		t.Fatalf("partial result = %#v", result)
	}
}
