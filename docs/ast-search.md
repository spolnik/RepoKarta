# Structural AST search

RepoKarta exposes bounded Tree-sitter queries over commit-pinned Java and Go
source. This is a language-aware search layer for declarations, annotations,
calls, and syntactic relationships. It does not execute repository code and it
does not claim compiler-level symbol resolution or dead-code proof.

## Query source

`POST /api/ast/search` accepts the standard Tree-sitter S-expression query
format and requires at least one named capture. Supported predicates include
text equality and
regular expressions (`#eq?`, `#not-eq?`, `#match?`, `#not-match?`,
`#any-of?`) and structural constraints (`#has-parent?`,
`#not-has-parent?`, `#has-ancestor?`, `#not-has-ancestor?`).

This Java query finds methods annotated with `@GetMapping`:

```json
{
  "repository_id": 7,
  "language": "java",
  "query": "((method_declaration (modifiers (marker_annotation name: (identifier) @annotation)) name: (identifier) @method) @declaration (#eq? @annotation \"GetMapping\") (#has-ancestor? @method class_declaration))",
  "path_prefix": "src/main/java",
  "limit": 50
}
```

This Go query finds `Handle` calls made inside a function:

```scheme
((call_expression
  function: (selector_expression
    field: (field_identifier) @method)) @call
  (#eq? @method "Handle")
  (#has-ancestor? @call function_declaration))
```

The equivalent MCP tool is `search_ast`. Its `cursor` parameter accepts the
opaque `next_cursor` returned by the preceding call.

## Index planning and completeness

The background structural artifact stores the sorted set of named syntax-node
kinds present in every parsed document. When all top-level query roots can be
identified conservatively, RepoKarta uses those inventories to exclude files
that cannot match. Queries with wildcard, anonymous, or alternation roots fall
back to scanning every language/path candidate so the planner does not reduce
recall.

Responses distinguish three different conditions:

- `next_cursor` means another exact result page may exist;
- `truncated` means a per-file query execution exhausted its safety budget;
- `complete` means every requested repository artifact was ready, the artifact
  was not truncated, and no scanned source file or matcher result was omitted.

`index.state` is `building` when background structural artifacts are still
missing; JSON returns `202 Accepted` with `Retry-After: 2` in that state.
Interactive AST search never creates artifacts. Cursors bind to the artifact
ID and normalized query; changing either makes the cursor stale instead of
silently mixing revisions or queries.

## Bounds

- query text: 16 KiB;
- response page: 50 matches by default, 200 maximum;
- candidate source files examined per page: 32;
- per-file matches: 1,000;
- captured text: 500 bytes after whitespace compaction;
- source blob: 2 MiB, UTF-8 text only;
- matcher work: bounded per pattern/node attempt.

Every match contains all named captures, exact normalized byte and one-based
line/column ranges, the indexed commit, a citation, and a source URL.

## Precision boundary

Tree-sitter queries describe syntax, including annotations and parent/ancestor
relationships. SCIP remains the compiler-produced layer for exact symbol
identity. Framework entry roots, dependency-injection edges, reflection,
configuration, generated code, and runtime registration require separate
language packs before reachability can support a credible dead-code finding.
