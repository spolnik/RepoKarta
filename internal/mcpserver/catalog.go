package mcpserver

// ToolSummary is the user-facing catalog owned beside the actual MCP server.
type ToolSummary struct {
	Name        string
	Description string
}

// ToolCatalog returns the complete read-only tool surface used by RepoKarta's
// configured HTTP server.
func ToolCatalog() []ToolSummary {
	return []ToolSummary{
		{Name: "list_repositories", Description: "Indexed repositories, stable IDs, and pinned commits."},
		{Name: "list_named_contexts", Description: "Permission-checked personal and administrator-published reusable scopes."},
		{Name: "resolve_effective_contexts", Description: "Exact explicit, named, and default contexts with provenance and canonical URLs."},
		{Name: "search_code", Description: "Literal, regex, Zoekt, or AST-reference search with explicit completeness metadata."},
		{Name: "find_symbol", Description: "Commit-pinned symbol definitions from the Zoekt/ctags index."},
		{Name: "find_references", Description: "Compiler-precise SCIP references when available, with labeled syntax-backed AST fallback."},
		{Name: "search_ast", Description: "Bounded Tree-sitter structural queries with explicit artifact readiness and pagination."},
		{Name: "get_file", Description: "Bounded source reads with exact revision and citation URLs."},
		{Name: "list_tree", Description: "Bounded repository trees at an exact indexed commit."},
		{Name: "git_log", Description: "Newest-first commit history with truncation metadata."},
		{Name: "git_diff", Description: "Resolved revisions and bounded unified patches."},
		{Name: "read_repository_map", Description: "Complete static snapshot: structure, routes, entry points, dependencies, and edges."},
		{Name: "read_code_reachability", Description: "Revision-pinned framework reachability with completeness and conservative classifications."},
		{Name: "read_dependency_inventory", Description: "Focused manifests, versioned coordinates, and outbound HTTP calls."},
		{Name: "read_system_topology", Description: "Directed component-level HTTP, gRPC, Kafka, database, MCP, and runtime-observed relationships."},
		{Name: "read_dependency_findings", Description: "Compact scope-aware OSV findings with manifest and advisory-snapshot evidence."},
		{Name: "list_deep_wiki_pages", Description: "Persisted Wiki plan, page slugs, hierarchy, status, and provenance."},
		{Name: "read_generated_document", Description: "Generated Deep Wiki pages and their grounded evidence."},
		{Name: "read_code_insights", Description: "Normalized metrics, findings, history, provenance, and advisory thresholds without executing scanners."},
		{Name: "compare_code_insights", Description: "Metric deltas and introduced or resolved findings between exact stored revisions."},
	}
}
