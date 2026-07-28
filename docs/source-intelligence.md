# Source editor intelligence

RepoKarta's commit-pinned source viewer includes the same deterministic search
and reference capabilities as the main Search page. The editor does not build a
second index or silently widen scope.

## Repository-scoped search

Every source page locks embedded searches to the repository being viewed:

- **Find usages** calls the shared reference search with
  `mode=references`. A Java SCIP artifact is preferred when an exact compatible
  index is ready; otherwise RepoKarta uses persisted syntax-backed AST
  relations.
- **Code search** calls the shared Zoekt search with the repository selector
  already set.
- Selecting a single identifier in the source viewer prepares **Find usages**.
  Selecting a longer code fragment prepares **Code search**.
- `Ctrl+Shift+F` (or `Cmd+Shift+F`) focuses embedded code search.

Results retain their indexed revision, source link, relation kind, receiver,
confidence, bounds, warnings, and completeness state. A `202 Accepted`
reference response remains usable but says that structural artifacts are still
building. The editor never turns an unavailable index into a confident empty
result.

The browser uses the same JSON boundary available to integrations:

```text
GET /api/search?q={symbol}&repo={repository-id}&mode=references&limit=50
GET /api/search?q={query}&repo={repository-id}&mode=zoekt&limit=50
```

MCP clients use the existing `find_references`, `find_symbol`, `search_code`,
and `get_file` tools over the same service and permission checks.

## HTTP routes and inbound callers

When a viewed file contains a supported detected HTTP route, the editor reads
the already-prepared route artifact and displays each commit-pinned declaration.
It does not analyze source synchronously during the page request.

The caller panel then projects inbound HTTP edges from the fleet-backed
distributed topology. In a monorepo, the controller file is assigned to the
longest matching deployable-component root so sibling services remain distinct:

- **route-path evidence** means a caller's commit-pinned URL witness has the
  same path as the declared route. Spring-style path parameters such as
  `/orders/{id}` match one concrete segment.
- **service-level caller** means the topology proves that the source service
  calls the service owning the controller, but available evidence does not
  prove which endpoint it invokes.
- `static_only`, `confirmed`, and `runtime_only` remain distinct. Runtime
  observations are timestamped aggregates and are never invented from source.
- building, partial, unresolved, and unavailable topology states stay visible.

This deliberately avoids claiming that every service-level edge invokes every
controller method. Use **Open caller topology** for the full inbound
neighborhood, its evidence, runtime window, and fleet freshness state.
