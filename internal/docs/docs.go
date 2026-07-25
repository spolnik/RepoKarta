// Package docs plans and generates commit-pinned repository documentation.
//
// Planning and generation use only deterministic structural facts and
// read-only Git commands. Generated artifacts live in RepoKarta-owned storage;
// source repositories are never modified.
package docs

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/graph"
	"go.yaml.in/yaml/v4"
)

const (
	documentVersion       = 1
	maximumSteeringBytes  = 64 << 10
	maximumSteeringNote   = 2_000
	maximumCitations      = 40
	gitCommandTimeout     = 20 * time.Second
	StatusPlanned         = "planned"
	StatusGenerating      = "generating"
	StatusReady           = "ready"
	StatusStale           = "stale"
	StatusError           = "error"
	deterministicProvider = "repokarta"
	deterministicModel    = "structural-v1"
)

var (
	errPageNotFound = errors.New("documentation page not found")
	mermaidBlock    = regexp.MustCompile("(?s)```mermaid[ \t]*\n(.*?)```")
	mermaidFence    = regexp.MustCompile("(?m)^```mermaid[ \t]*$")
)

// ErrPageNotFound is returned for an unknown or excluded page.
var ErrPageNotFound = errPageNotFound

// Storage owns document metadata and repository catalogue access.
type Storage interface {
	RepositoryByID(context.Context, int64) (catalog.Repository, error)
	ListDocumentPages(context.Context, int64) ([]Page, error)
	SaveDocumentPage(context.Context, Page) error
}

// MapService supplies deterministic, commit-pinned structural facts.
type MapService interface {
	Snapshot(context.Context, int64, bool) (graph.Snapshot, error)
}

// Page records one independently generated document and its provenance.
type Page struct {
	RepositoryID    int64            `json:"repository_id"`
	Slug            string           `json:"slug"`
	Title           string           `json:"title"`
	Summary         string           `json:"summary"`
	Order           int              `json:"order"`
	Status          string           `json:"status"`
	Revision        string           `json:"revision,omitempty"`
	Provider        string           `json:"provider,omitempty"`
	Model           string           `json:"model,omitempty"`
	InputTokens     int64            `json:"input_tokens"`
	OutputTokens    int64            `json:"output_tokens"`
	StartedAt       time.Time        `json:"started_at,omitempty"`
	GeneratedAt     time.Time        `json:"generated_at,omitempty"`
	UpdatedAt       time.Time        `json:"updated_at,omitempty"`
	Error           string           `json:"error,omitempty"`
	SupportingFiles []string         `json:"supporting_files"`
	Citations       []graph.Evidence `json:"citations"`
	Markdown        string           `json:"markdown,omitempty"`
}

// Steering is the validated documentation section of .repokarta.yml.
type Steering struct {
	Title   string            `json:"title,omitempty" yaml:"title"`
	Include []string          `json:"include,omitempty" yaml:"include"`
	Exclude []string          `json:"exclude,omitempty" yaml:"exclude"`
	Notes   map[string]string `json:"notes,omitempty" yaml:"notes"`
}

// Site is the current documentation plan for one repository.
type Site struct {
	Version      int       `json:"version"`
	RepositoryID int64     `json:"repository_id"`
	Repository   string    `json:"repository"`
	Revision     string    `json:"revision"`
	UpdatedAt    time.Time `json:"updated_at"`
	Steering     Steering  `json:"steering"`
	Pages        []Page    `json:"pages"`
	Ready        int       `json:"ready"`
	Stale        int       `json:"stale"`
	Pending      int       `json:"pending"`
	Failed       int       `json:"failed"`
}

// GenerateRequest controls independent or whole-site generation.
type GenerateRequest struct {
	RepositoryID int64
	Page         string
	Refresh      bool
}

type steeringFile struct {
	Docs Steering `yaml:"docs"`
}

type pageSpec struct {
	Slug    string
	Title   string
	Summary string
	Order   int
}

var defaultPageSpecs = []pageSpec{
	{Slug: "overview", Title: "Overview", Summary: "Repository purpose, languages, manifests, and structural footprint.", Order: 1},
	{Slug: "architecture", Title: "Architecture", Summary: "Packages, entry points, routes, and their evidence-backed relationships.", Order: 2},
	{Slug: "dependencies", Title: "Dependencies", Summary: "Declared external dependencies and the manifests that introduce them.", Order: 3},
}

// Service persists resumable documentation jobs outside source repositories.
type Service struct {
	store     Storage
	maps      MapService
	directory string
	mu        sync.Mutex
}

// New creates a living-documentation service.
func New(store Storage, maps MapService, directory string) (*Service, error) {
	if store == nil || maps == nil {
		return nil, errors.New("documentation storage and map service are required")
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, fmt.Errorf("create documentation directory: %w", err)
	}
	return &Service{store: store, maps: maps, directory: directory}, nil
}

// Plan returns and persists the deterministic page plan and current statuses.
func (s *Service) Plan(ctx context.Context, repositoryID int64) (Site, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.plan(ctx, repositoryID)
}

func (s *Service) plan(ctx context.Context, repositoryID int64) (Site, error) {
	repository, err := s.store.RepositoryByID(ctx, repositoryID)
	if err != nil {
		return Site{}, err
	}
	snapshot, err := s.maps.Snapshot(ctx, repositoryID, false)
	if err != nil {
		return Site{}, fmt.Errorf("load structural snapshot: %w", err)
	}
	if len(snapshot.Repositories) != 1 {
		return Site{}, errors.New("repository documentation requires one structural snapshot")
	}
	revision := snapshot.Repositories[0].Revision
	steering, _, err := loadSteering(ctx, repository, revision)
	if err != nil {
		return Site{}, err
	}
	specs, err := selectedPageSpecs(steering)
	if err != nil {
		return Site{}, err
	}
	stored, err := s.store.ListDocumentPages(ctx, repositoryID)
	if err != nil {
		return Site{}, fmt.Errorf("load documentation metadata: %w", err)
	}
	storedBySlug := make(map[string]Page, len(stored))
	for _, page := range stored {
		storedBySlug[page.Slug] = page
	}

	changedByRevision := make(map[string]map[string]struct{})
	now := time.Now().UTC()
	pages := make([]Page, 0, len(specs))
	for _, spec := range specs {
		page, exists := storedBySlug[spec.Slug]
		if !exists {
			page = Page{
				RepositoryID: repositoryID,
				Slug:         spec.Slug,
				Status:       StatusPlanned,
				UpdatedAt:    now,
			}
		}
		page.Title = spec.Title
		page.Summary = spec.Summary
		page.Order = spec.Order
		if page.Status == StatusGenerating {
			page.Status = StatusError
			page.Error = "Generation was interrupted. Retry this page to resume."
			page.UpdatedAt = now
		}
		if page.Status == "" {
			page.Status = StatusPlanned
		}
		if page.SupportingFiles == nil {
			page.SupportingFiles = []string{}
		}
		if page.Citations == nil {
			page.Citations = []graph.Evidence{}
		}
		if page.Revision != "" && page.Revision != revision && (page.Status == StatusReady || page.Status == StatusStale) {
			changed, ok := changedByRevision[page.Revision]
			if !ok {
				changed, err = changedFiles(ctx, repository, page.Revision, revision)
				if err != nil {
					changed = nil
				}
				changedByRevision[page.Revision] = changed
			}
			if changed == nil || affected(page.SupportingFiles, changed) {
				page.Status = StatusStale
				page.UpdatedAt = now
			} else {
				page.Status = StatusReady
			}
		}
		if err := s.store.SaveDocumentPage(ctx, page); err != nil {
			return Site{}, fmt.Errorf("persist documentation plan: %w", err)
		}
		pages = append(pages, page)
	}
	return summarizeSite(repository, revision, steering, pages), nil
}

// Generate builds planned, stale, failed, or explicitly refreshed pages.
// Status is stored before each page so interrupted runs are resumable.
func (s *Service) Generate(ctx context.Context, request GenerateRequest) (Site, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	site, err := s.plan(ctx, request.RepositoryID)
	if err != nil {
		return Site{}, err
	}
	snapshot, err := s.maps.Snapshot(ctx, request.RepositoryID, false)
	if err != nil {
		return Site{}, err
	}
	targetFound := request.Page == ""
	for index := range site.Pages {
		page := &site.Pages[index]
		if request.Page != "" && page.Slug != request.Page {
			continue
		}
		targetFound = true
		if request.Page == "" && !request.Refresh &&
			page.Status != StatusPlanned && page.Status != StatusStale && page.Status != StatusError {
			continue
		}
		if request.Page != "" && !request.Refresh && page.Status == StatusReady && page.Revision == site.Revision {
			continue
		}
		started := time.Now().UTC()
		page.Status = StatusGenerating
		page.Error = ""
		page.StartedAt = started
		page.UpdatedAt = started
		if err := s.store.SaveDocumentPage(ctx, *page); err != nil {
			return Site{}, err
		}

		generated, generateErr := generatePage(*page, site, snapshot)
		if generateErr == nil {
			generateErr = validateGeneratedPage(generated, site.Revision)
		}
		if generateErr == nil {
			generateErr = s.writeMarkdown(generated)
		}
		if generateErr != nil {
			page.Status = StatusError
			page.Error = generateErr.Error()
			page.UpdatedAt = time.Now().UTC()
			if saveErr := s.store.SaveDocumentPage(ctx, *page); saveErr != nil {
				return Site{}, errors.Join(generateErr, saveErr)
			}
			continue
		}
		*page = generated
		if err := s.store.SaveDocumentPage(ctx, generated); err != nil {
			return Site{}, err
		}
	}
	if !targetFound {
		return Site{}, ErrPageNotFound
	}
	return summarizeSite(
		catalog.Repository{ID: site.RepositoryID, Name: site.Repository},
		site.Revision,
		site.Steering,
		site.Pages,
	), nil
}

// Page returns current metadata and generated Markdown for one page.
func (s *Service) Page(ctx context.Context, repositoryID int64, slug string) (Page, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	site, err := s.plan(ctx, repositoryID)
	if err != nil {
		return Page{}, err
	}
	for _, page := range site.Pages {
		if page.Slug != slug {
			continue
		}
		if page.Status != StatusReady && page.Status != StatusStale {
			return page, nil
		}
		content, err := os.ReadFile(s.markdownPath(repositoryID, slug))
		if err != nil {
			return Page{}, fmt.Errorf("read generated page: %w", err)
		}
		page.Markdown = string(content)
		return page, nil
	}
	return Page{}, ErrPageNotFound
}

// Export creates a portable Markdown ZIP without touching the repository.
func (s *Service) Export(ctx context.Context, repositoryID int64) ([]byte, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	site, err := s.plan(ctx, repositoryID)
	if err != nil {
		return nil, "", err
	}
	var output bytes.Buffer
	archive := zip.NewWriter(&output)
	index := strings.Builder{}
	fmt.Fprintf(&index, "# %s documentation\n\n", site.Repository)
	fmt.Fprintf(&index, "Generated from commit `%s` by RepoKarta.\n\n", site.Revision)
	index.WriteString("## Pages\n\n")
	exported := 0
	for _, page := range site.Pages {
		if page.Status != StatusReady && page.Status != StatusStale {
			continue
		}
		content, readErr := os.ReadFile(s.markdownPath(repositoryID, page.Slug))
		if readErr != nil {
			continue
		}
		writer, createErr := archive.Create(page.Slug + ".md")
		if createErr != nil {
			archive.Close()
			return nil, "", createErr
		}
		if _, writeErr := writer.Write(content); writeErr != nil {
			archive.Close()
			return nil, "", writeErr
		}
		stale := ""
		if page.Status == StatusStale {
			stale = " (stale)"
		}
		fmt.Fprintf(&index, "- [%s](%s.md)%s\n", page.Title, page.Slug, stale)
		exported++
	}
	if exported == 0 {
		archive.Close()
		return nil, "", errors.New("generate at least one page before exporting")
	}
	indexWriter, err := archive.Create("README.md")
	if err != nil {
		archive.Close()
		return nil, "", err
	}
	if _, err := indexWriter.Write([]byte(index.String())); err != nil {
		archive.Close()
		return nil, "", err
	}
	manifestWriter, err := archive.Create("repokarta-manifest.json")
	if err != nil {
		archive.Close()
		return nil, "", err
	}
	manifest, _ := json.MarshalIndent(site, "", "  ")
	if _, err := manifestWriter.Write(manifest); err != nil {
		archive.Close()
		return nil, "", err
	}
	if err := archive.Close(); err != nil {
		return nil, "", err
	}
	return output.Bytes(), "repokarta-wiki-" + safeName(site.Repository) + ".zip", nil
}

func summarizeSite(repository catalog.Repository, revision string, steering Steering, pages []Page) Site {
	site := Site{
		Version:      documentVersion,
		RepositoryID: repository.ID,
		Repository:   repository.Name,
		Revision:     revision,
		UpdatedAt:    time.Now().UTC(),
		Steering:     steering,
		Pages:        pages,
	}
	slices.SortFunc(site.Pages, func(left, right Page) int {
		if left.Order != right.Order {
			return left.Order - right.Order
		}
		return strings.Compare(left.Slug, right.Slug)
	})
	for _, page := range site.Pages {
		switch page.Status {
		case StatusReady:
			site.Ready++
		case StatusStale:
			site.Stale++
		case StatusError:
			site.Failed++
		default:
			site.Pending++
		}
	}
	return site
}

func selectedPageSpecs(steering Steering) ([]pageSpec, error) {
	known := make(map[string]pageSpec, len(defaultPageSpecs))
	for _, spec := range defaultPageSpecs {
		known[spec.Slug] = spec
	}
	selected := make(map[string]bool, len(defaultPageSpecs))
	if len(steering.Include) == 0 {
		for slug := range known {
			selected[slug] = true
		}
	} else {
		for _, slug := range steering.Include {
			if _, ok := known[slug]; !ok {
				return nil, fmt.Errorf(".repokarta.yml docs.include contains unknown page %q", slug)
			}
			selected[slug] = true
		}
	}
	for _, slug := range steering.Exclude {
		if _, ok := known[slug]; !ok {
			return nil, fmt.Errorf(".repokarta.yml docs.exclude contains unknown page %q", slug)
		}
		delete(selected, slug)
	}
	if len(selected) == 0 {
		return nil, errors.New(".repokarta.yml excludes every documentation page")
	}
	specs := make([]pageSpec, 0, len(selected))
	for _, spec := range defaultPageSpecs {
		if selected[spec.Slug] {
			specs = append(specs, spec)
		}
	}
	return specs, nil
}

func loadSteering(ctx context.Context, repository catalog.Repository, revision string) (Steering, bool, error) {
	content, found, err := readGitFile(ctx, repository.Path, revision, ".repokarta.yml", maximumSteeringBytes)
	if err != nil || !found {
		return Steering{}, found, err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true)
	var file steeringFile
	if err := decoder.Decode(&file); err != nil {
		return Steering{}, true, fmt.Errorf("parse .repokarta.yml: %w", err)
	}
	steering := file.Docs
	steering.Title = strings.TrimSpace(steering.Title)
	if len([]rune(steering.Title)) > 120 {
		return Steering{}, true, errors.New(".repokarta.yml docs.title exceeds 120 characters")
	}
	steering.Include = normalizedSlugs(steering.Include)
	steering.Exclude = normalizedSlugs(steering.Exclude)
	for slug, note := range steering.Notes {
		if !knownSlug(slug) {
			return Steering{}, true, fmt.Errorf(".repokarta.yml docs.notes contains unknown page %q", slug)
		}
		note = strings.TrimSpace(note)
		if len([]rune(note)) > maximumSteeringNote {
			return Steering{}, true, fmt.Errorf(".repokarta.yml note for %q exceeds %d characters", slug, maximumSteeringNote)
		}
		steering.Notes[slug] = note
	}
	return steering, true, nil
}

func normalizedSlugs(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func knownSlug(slug string) bool {
	for _, spec := range defaultPageSpecs {
		if spec.Slug == slug {
			return true
		}
	}
	return false
}

func generatePage(page Page, site Site, snapshot graph.Snapshot) (Page, error) {
	nodes := repositoryNodes(snapshot.Nodes, site.RepositoryID)
	edges := repositoryEdges(snapshot.Edges, site.RepositoryID)
	manifests := repositoryManifests(snapshot.Manifests, site.RepositoryID)
	var markdown string
	var citations []graph.Evidence
	switch page.Slug {
	case "overview":
		markdown, citations = generateOverview(site, snapshot, nodes, manifests)
	case "architecture":
		markdown, citations = generateArchitecture(site, firstPartyNodes(nodes), firstPartyEdges(edges))
	case "dependencies":
		markdown, citations = generateDependencies(site, snapshot.Nodes, edges, manifests)
	default:
		return Page{}, ErrPageNotFound
	}
	if note := strings.TrimSpace(site.Steering.Notes[page.Slug]); note != "" {
		markdown += "\n## Repository guidance\n\n" + escapeMarkdown(note) + "\n"
	}
	citations = uniqueEvidence(citations, maximumCitations)
	files := make([]string, 0, len(citations)+1)
	seenFiles := make(map[string]struct{}, len(citations)+1)
	for _, citation := range citations {
		if _, exists := seenFiles[citation.Path]; exists {
			continue
		}
		seenFiles[citation.Path] = struct{}{}
		files = append(files, citation.Path)
	}
	sort.Strings(files)
	now := time.Now().UTC()
	page.Status = StatusReady
	page.Revision = site.Revision
	page.Provider = deterministicProvider
	page.Model = deterministicModel
	page.InputTokens = 0
	page.OutputTokens = 0
	page.GeneratedAt = now
	page.UpdatedAt = now
	page.Error = ""
	page.SupportingFiles = files
	page.Citations = citations
	page.Markdown = markdown
	return page, nil
}

func generateOverview(site Site, snapshot graph.Snapshot, nodes []graph.Node, manifests []graph.Manifest) (string, []graph.Evidence) {
	title := site.Repository
	if site.Steering.Title != "" {
		title = site.Steering.Title
	}
	repositoryEvidence := firstEvidence(nodes, "repository")
	citations := append([]graph.Evidence{}, repositoryEvidence...)
	var output strings.Builder
	fmt.Fprintf(&output, "# %s\n\n", escapeMarkdown(title))
	fmt.Fprintf(&output, "> Commit-pinned structural overview for `%s`. %s\n\n", shortCommit(site.Revision), citationLinks(repositoryEvidence))
	output.WriteString("## At a glance\n\n")
	fmt.Fprintf(&output, "- **Files analyzed:** %d committed files.\n", snapshot.FileCount)
	fmt.Fprintf(&output, "- **Structural facts:** %d nodes and %d relationships.\n", len(nodes), len(repositoryEdges(snapshot.Edges, site.RepositoryID)))
	fmt.Fprintf(&output, "- **Package manifests:** %d detected.\n", len(manifests))
	output.WriteString("\n## Language profile\n\n| Language | Files | Share |\n| --- | ---: | ---: |\n")
	for _, language := range snapshot.Repositories[0].Languages {
		fmt.Fprintf(&output, "| %s | %d | %.1f%% |\n", escapeMarkdown(language.Name), language.Files, language.Percentage)
	}
	output.WriteString("\n## Package boundaries\n\n")
	if len(manifests) == 0 {
		output.WriteString("No supported package manifest was detected in this snapshot.\n")
	} else {
		for _, manifest := range manifests {
			fmt.Fprintf(&output, "- **%s:** [%s](%s) declares `%s`.\n",
				escapeMarkdown(manifest.Kind),
				escapeMarkdown(manifest.Path),
				manifest.Evidence.URL,
				escapeMarkdown(manifest.Name),
			)
			citations = append(citations, manifest.Evidence)
		}
	}
	output.WriteString("\n## Trust boundary\n\nThis page is generated from committed files only. RepoKarta does not execute repository code while planning or generating documentation.\n")
	return output.String(), citations
}

func generateArchitecture(site Site, nodes []graph.Node, edges []graph.Edge) (string, []graph.Evidence) {
	packages := nodesOfKind(nodes, "package")
	entrypoints := nodesOfKind(nodes, "entrypoint")
	routes := nodesOfKind(nodes, "route")
	repositoryEvidence := firstEvidence(nodes, "repository")
	citations := append([]graph.Evidence{}, repositoryEvidence...)
	var output strings.Builder
	fmt.Fprintf(&output, "# Architecture\n\nStructural view of **%s** at commit `%s`. %s\n\n",
		escapeMarkdown(site.Repository),
		shortCommit(site.Revision),
		citationLinks(repositoryEvidence),
	)
	output.WriteString("## Component map\n\n```mermaid\nflowchart LR\n")
	output.WriteString("  repo[\"" + mermaidLabel(site.Repository) + "\"]\n")
	diagramPackages := prioritizedNodes(packages, []string{
		"app", "httpserver", "codeintel", "graph", "docs", "mcpserver", "search", "store", "agent", "catalog",
	})
	diagramNodes := append(append([]graph.Node{}, entrypoints...), diagramPackages...)
	if len(diagramNodes) > 9 {
		diagramNodes = diagramNodes[:9]
	}
	for index, node := range diagramNodes {
		fmt.Fprintf(&output, "  n%d[\"%s\"]\n", index, mermaidLabel(node.Label))
		fmt.Fprintf(&output, "  repo --> n%d\n", index)
		citations = append(citations, uniqueEvidence(node.Evidence, 2)...)
	}
	if len(routes) > 0 {
		routeLimit := min(4, len(routes))
		for index := range routeLimit {
			nodeIndex := len(diagramNodes) + index
			fmt.Fprintf(&output, "  n%d{{\"%s\"}}\n", nodeIndex, mermaidLabel(routes[index].Label))
			fmt.Fprintf(&output, "  repo --> n%d\n", nodeIndex)
			citations = append(citations, uniqueEvidence(routes[index].Evidence, 2)...)
		}
	}
	output.WriteString("```\n\n")
	fmt.Fprintf(&output, "The diagram is derived from %d structural relationships. %s\n\n", len(edges), citationLinks(uniqueEvidence(citations, 8)))
	writeNodeSection(&output, "Entry points", entrypoints, &citations)
	writeNodeSection(&output, "Packages", packages, &citations)
	writeNodeSection(&output, "HTTP routes", routes, &citations)
	return output.String(), citations
}

func generateDependencies(
	site Site,
	allNodes []graph.Node,
	edges []graph.Edge,
	manifests []graph.Manifest,
) (string, []graph.Evidence) {
	nodeByID := make(map[string]graph.Node, len(allNodes))
	for _, node := range allNodes {
		nodeByID[node.ID] = node
	}
	type declaredDependency struct {
		label    string
		evidence []graph.Evidence
	}
	byLabel := make(map[string]declaredDependency)
	for _, edge := range edges {
		if edge.Kind != "dependency" {
			continue
		}
		target, exists := nodeByID[edge.Target]
		if !exists || target.Kind != "dependency" {
			continue
		}
		key := strings.ToLower(target.Label)
		current := byLabel[key]
		current.label = target.Label
		current.evidence = append(current.evidence, edge.Evidence...)
		byLabel[key] = current
	}
	dependencies := make([]declaredDependency, 0, len(byLabel))
	for _, dependency := range byLabel {
		dependency.evidence = uniqueEvidence(dependency.evidence, 2)
		dependencies = append(dependencies, dependency)
	}
	slices.SortFunc(dependencies, func(left, right declaredDependency) int {
		return strings.Compare(strings.ToLower(left.label), strings.ToLower(right.label))
	})
	citations := make([]graph.Evidence, 0, len(dependencies)+len(manifests))
	var output strings.Builder
	fmt.Fprintf(&output, "# Dependencies\n\nDeclared dependency inventory for **%s** at `%s`.\n\n", escapeMarkdown(site.Repository), shortCommit(site.Revision))
	output.WriteString("## Manifest summary\n\n")
	if len(manifests) == 0 {
		output.WriteString("No supported dependency manifest was detected.\n")
	} else {
		for _, manifest := range manifests {
			fmt.Fprintf(&output, "- [%s](%s) — %s, %d declared dependencies.\n",
				escapeMarkdown(manifest.Path),
				manifest.Evidence.URL,
				escapeMarkdown(manifest.Kind),
				len(manifest.Dependencies),
			)
			citations = append(citations, manifest.Evidence)
		}
	}
	output.WriteString("\n## Declared packages\n\n")
	if len(dependencies) == 0 {
		output.WriteString("No external dependency nodes were produced for the supported manifests.\n")
	} else {
		limit := min(40, len(dependencies))
		for _, dependency := range dependencies[:limit] {
			fmt.Fprintf(&output, "- `%s` %s\n", escapeMarkdown(dependency.label), citationLinks(dependency.evidence))
			citations = append(citations, dependency.evidence...)
		}
		if len(dependencies) > limit {
			fmt.Fprintf(&output, "\n_%d additional declared packages are available in the repository map._\n", len(dependencies)-limit)
		}
	}
	return output.String(), citations
}

func writeNodeSection(output *strings.Builder, heading string, nodes []graph.Node, citations *[]graph.Evidence) {
	fmt.Fprintf(output, "## %s\n\n", heading)
	if len(nodes) == 0 {
		output.WriteString("No reliable facts were detected for this section.\n\n")
		return
	}
	limit := min(30, len(nodes))
	for _, node := range nodes[:limit] {
		evidence := uniqueEvidence(node.Evidence, 2)
		fmt.Fprintf(output, "- **%s** — %s %s\n",
			escapeMarkdown(node.Label),
			escapeMarkdown(node.Subtitle),
			citationLinks(evidence),
		)
		*citations = append(*citations, evidence...)
	}
	if len(nodes) > limit {
		fmt.Fprintf(output, "\n_%d additional facts are available in the repository map._\n", len(nodes)-limit)
	}
	output.WriteString("\n")
}

func validateGeneratedPage(page Page, revision string) error {
	if strings.TrimSpace(page.Markdown) == "" {
		return errors.New("generated page is empty")
	}
	if page.Revision != revision || revision == "" {
		return errors.New("generated page is not pinned to the current structural revision")
	}
	if len(page.Citations) == 0 {
		return errors.New("generated page has no source citations")
	}
	for _, citation := range page.Citations {
		if citation.Revision != revision {
			return fmt.Errorf("citation %s is pinned to a different revision", citation.Path)
		}
		if citation.Path == "" || citation.Line <= 0 || citation.URL == "" {
			return errors.New("generated page contains an incomplete citation")
		}
		parsed, err := url.Parse(citation.URL)
		if err != nil || parsed.Query().Get("rev") != revision {
			return fmt.Errorf("citation URL for %s does not preserve the exact revision", citation.Path)
		}
		if !strings.Contains(page.Markdown, citation.URL) {
			return fmt.Errorf("citation for %s is not present in generated Markdown", citation.Path)
		}
	}
	return validateMermaid(page.Markdown)
}

func validateMermaid(markdown string) error {
	fences := mermaidFence.FindAllStringIndex(markdown, -1)
	blocks := mermaidBlock.FindAllStringSubmatch(markdown, -1)
	if len(fences) != len(blocks) {
		return errors.New("Mermaid fence is not closed")
	}
	for _, match := range blocks {
		source := strings.TrimSpace(match[1])
		firstLine := source
		if newline := strings.IndexByte(firstLine, '\n'); newline >= 0 {
			firstLine = firstLine[:newline]
		}
		kind := strings.Fields(firstLine)
		if len(kind) == 0 ||
			(kind[0] != "flowchart" && kind[0] != "graph" && kind[0] != "sequenceDiagram" &&
				kind[0] != "classDiagram" && kind[0] != "stateDiagram" && kind[0] != "erDiagram") {
			return errors.New("Mermaid block uses an unsupported diagram grammar")
		}
		lower := strings.ToLower(source)
		if strings.Contains(lower, "%%{") || strings.Contains(lower, "javascript:") ||
			regexp.MustCompile(`(?m)^\s*click\s+`).MatchString(lower) {
			return errors.New("Mermaid block contains an unsafe directive")
		}
	}
	return nil
}

func (s *Service) writeMarkdown(page Page) error {
	directory := filepath.Join(s.directory, fmt.Sprintf("repository-%d", page.RepositoryID))
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create page directory: %w", err)
	}
	target := s.markdownPath(page.RepositoryID, page.Slug)
	temporary, err := os.CreateTemp(directory, page.Slug+".*.tmp")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if _, err := temporary.WriteString(page.Markdown); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, target); err != nil {
		return fmt.Errorf("publish generated page: %w", err)
	}
	return nil
}

func (s *Service) markdownPath(repositoryID int64, slug string) string {
	return filepath.Join(s.directory, fmt.Sprintf("repository-%d", repositoryID), slug+".md")
}

func affected(supportingFiles []string, changed map[string]struct{}) bool {
	if len(supportingFiles) == 0 || changed == nil {
		return true
	}
	if _, ok := changed[".repokarta.yml"]; ok {
		return true
	}
	for _, file := range supportingFiles {
		if _, ok := changed[file]; ok {
			return true
		}
	}
	return false
}

func changedFiles(ctx context.Context, repository catalog.Repository, fromRevision, toRevision string) (map[string]struct{}, error) {
	if fromRevision == "" || toRevision == "" {
		return nil, errors.New("both revisions are required")
	}
	output, err := runGit(ctx, repository.Path, "diff", "--name-only", "--no-renames", fromRevision+".."+toRevision, "--")
	if err != nil {
		return nil, err
	}
	changed := make(map[string]struct{})
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		line = filepath.ToSlash(strings.TrimSpace(line))
		if line != "" {
			changed[line] = struct{}{}
		}
	}
	return changed, nil
}

func readGitFile(ctx context.Context, repositoryPath, revision, filePath string, maximum int) ([]byte, bool, error) {
	if repositoryPath == "" || revision == "" || filePath == "" || maximum <= 0 {
		return nil, false, nil
	}
	content, err := runGit(ctx, repositoryPath, "show", revision+":"+filePath)
	if err != nil {
		var commandError *exec.ExitError
		if errors.As(err, &commandError) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if len(content) > maximum {
		return nil, true, fmt.Errorf("%s exceeds %d bytes", filePath, maximum)
	}
	return content, true, nil
}

func runGit(ctx context.Context, repositoryPath string, arguments ...string) ([]byte, error) {
	bounded, cancel := context.WithTimeout(ctx, gitCommandTimeout)
	defer cancel()
	commandArguments := append([]string{"-C", repositoryPath}, arguments...)
	command := exec.CommandContext(bounded, "git", commandArguments...)
	output, err := command.CombinedOutput()
	if bounded.Err() != nil {
		return nil, fmt.Errorf("git command timed out")
	}
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(arguments, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func repositoryNodes(nodes []graph.Node, repositoryID int64) []graph.Node {
	output := make([]graph.Node, 0, len(nodes))
	for _, node := range nodes {
		if node.RepositoryID == repositoryID {
			output = append(output, node)
		}
	}
	return output
}

func repositoryEdges(edges []graph.Edge, repositoryID int64) []graph.Edge {
	output := make([]graph.Edge, 0, len(edges))
	for _, edge := range edges {
		for _, evidence := range edge.Evidence {
			if evidence.RepositoryID == repositoryID {
				output = append(output, edge)
				break
			}
		}
	}
	return output
}

func firstPartyNodes(nodes []graph.Node) []graph.Node {
	output := make([]graph.Node, 0, len(nodes))
	for _, node := range nodes {
		if firstPartyPath(node.Path) {
			output = append(output, node)
		}
	}
	return output
}

func firstPartyEdges(edges []graph.Edge) []graph.Edge {
	output := make([]graph.Edge, 0, len(edges))
	for _, edge := range edges {
		owned := false
		for _, evidence := range edge.Evidence {
			if firstPartyPath(evidence.Path) {
				owned = true
				break
			}
		}
		if owned {
			output = append(output, edge)
		}
	}
	return output
}

func firstPartyPath(value string) bool {
	value = strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(value)), "./")
	for _, prefix := range []string{"third_party/", "vendor/", "node_modules/"} {
		if strings.HasPrefix(value, prefix) {
			return false
		}
	}
	return true
}

func repositoryManifests(manifests []graph.Manifest, repositoryID int64) []graph.Manifest {
	output := make([]graph.Manifest, 0, len(manifests))
	for _, manifest := range manifests {
		if manifest.RepositoryID == repositoryID {
			output = append(output, manifest)
		}
	}
	return output
}

func nodesOfKind(nodes []graph.Node, kind string) []graph.Node {
	output := make([]graph.Node, 0)
	for _, node := range nodes {
		if node.Kind == kind {
			output = append(output, node)
		}
	}
	slices.SortFunc(output, func(left, right graph.Node) int {
		return strings.Compare(strings.ToLower(left.Label), strings.ToLower(right.Label))
	})
	return output
}

func prioritizedNodes(nodes []graph.Node, labels []string) []graph.Node {
	byLabel := make(map[string]graph.Node, len(nodes))
	for _, node := range nodes {
		byLabel[strings.ToLower(node.Label)] = node
	}
	output := make([]graph.Node, 0, len(nodes))
	used := make(map[string]struct{}, len(nodes))
	for _, label := range labels {
		if node, exists := byLabel[strings.ToLower(label)]; exists {
			output = append(output, node)
			used[node.ID] = struct{}{}
		}
	}
	for _, node := range nodes {
		if _, exists := used[node.ID]; !exists {
			output = append(output, node)
		}
	}
	return output
}

func firstEvidence(nodes []graph.Node, kind string) []graph.Evidence {
	for _, node := range nodes {
		if node.Kind == kind && len(node.Evidence) > 0 {
			return uniqueEvidence(node.Evidence, 2)
		}
	}
	for _, node := range nodes {
		if len(node.Evidence) > 0 {
			return uniqueEvidence(node.Evidence, 2)
		}
	}
	return nil
}

func uniqueEvidence(values []graph.Evidence, limit int) []graph.Evidence {
	output := make([]graph.Evidence, 0, min(len(values), limit))
	seen := make(map[string]struct{}, len(values))
	for _, evidence := range values {
		key := fmt.Sprintf("%d:%s:%s:%d", evidence.RepositoryID, evidence.Revision, evidence.Path, evidence.Line)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		output = append(output, evidence)
		if len(output) == limit {
			break
		}
	}
	return output
}

func citationLinks(evidence []graph.Evidence) string {
	if len(evidence) == 0 {
		return ""
	}
	links := make([]string, 0, len(evidence))
	for _, item := range evidence {
		links = append(links, fmt.Sprintf("[%s:%d](%s)", escapeMarkdown(item.Path), item.Line, item.URL))
	}
	return strings.Join(links, ", ")
}

func mermaidLabel(value string) string {
	value = strings.ReplaceAll(value, `"`, `'`)
	value = strings.ReplaceAll(value, "\n", " ")
	return strings.TrimSpace(value)
}

func escapeMarkdown(value string) string {
	replacer := strings.NewReplacer(
		"\\", "\\\\",
		"`", "\\`",
		"*", "\\*",
		"_", "\\_",
		"[", "\\[",
		"]", "\\]",
	)
	return replacer.Replace(strings.TrimSpace(value))
}

func safeName(value string) string {
	var output strings.Builder
	lastDash := false
	for _, character := range strings.ToLower(strings.TrimSpace(value)) {
		if unicode.IsLetter(character) || unicode.IsDigit(character) {
			output.WriteRune(character)
			lastDash = false
		} else if output.Len() > 0 && !lastDash {
			output.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(output.String(), "-")
}

func shortCommit(value string) string {
	if len(value) > 8 {
		return value[:8]
	}
	return value
}
