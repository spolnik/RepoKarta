# Codebase consolidation boundaries

M18 reduces duplication without changing RepoKarta's external behavior. The
new boundaries make cross-cutting guarantees easier to review and keep the
largest composition files focused.

## Shared infrastructure

- `internal/gitexec` is the only production Git subprocess runner used by
  graph, documentation, and insight derivation. It applies a bounded context,
  disables optional locks and terminal prompts, sanitizes Git environment
  overrides, and returns stdout separately from stderr.
- `internal/atomicfile` owns same-directory temporary writes and publication,
  including the Windows replace fallback. Graph artifacts, Wiki output, and
  advisory snapshots use the same complete-file publication path.

## Domain files

`internal/httpserver` keeps route composition, conversations, search,
dependencies, source browsing, rendering, and middleware in separate files.
`internal/sourceintelligence` owns the join between commit-pinned route evidence
and caller topology; HTTP only adapts its result for templates.

`internal/graph` separates service artifact IO, Git plumbing, Spring heuristics,
and Go, Node, JVM, Rust, Python, and .NET manifest parsers. Repository fan-out
for dependencies, topology, routes, and structure uses one bounded generic
reader.

The web application uses one tested repository-picker controller for both Maps
and Wiki instead of maintaining two large event-handler closures. Its tests
cover keyboard and pointer selection, filtering, accessibility state, and
disabled native selectors.

## Verification contract

The release checks keep the milestone marked complete, require the focused
files to remain present, and prevent `server.go` and `graph.go` from silently
absorbing the extracted responsibilities again. Package tests exercise Git
environment hygiene, timeout and stderr behavior, atomic replacement, graph
artifact fan-out, source intelligence, and the shared repository picker.
