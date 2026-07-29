// Package apicontract owns the load-bearing JSON shapes shared by the Go HTTP
// server and the generated browser client types.
package apicontract

import (
	"github.com/spolnik/RepoKarta/internal/agent"
	"github.com/spolnik/RepoKarta/internal/dependencies"
	"github.com/spolnik/RepoKarta/internal/docs"
	"github.com/spolnik/RepoKarta/internal/graph"
)

//go:generate go run ./cmd/generate -output ../../web/src/generated/api-contract.ts

type HealthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

type ErrorDetail struct {
	Message string `json:"message"`
}

type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

type ProviderStatusesResponse struct {
	Providers []agent.Status `json:"providers"`
}

type ConversationEvent = agent.Event
type ArtifactProgress = graph.ArtifactProgress
type DependencyRefreshProgress = dependencies.RefreshProgress
type WikiSite = docs.Site
type WikiPage = docs.Page
