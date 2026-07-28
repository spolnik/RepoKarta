# Advanced search and Deep Search

RepoKarta keeps ordinary search deterministic and AI-free. The same
permission-filtered, commit-pinned evidence is available in the browser,
`POST /api/search`, the JSON client, Chat, and MCP.

## Query grammar

Free text remains valid. Structured fields can be combined with quoted values,
and any filter can be negated with a leading `-`.

| Family | Fields and values |
| --- | --- |
| Scope | `repository`, `revision`, `language`, `path`, `file`, `owner` |
| Result | `result_type`, `symbol_kind` |
| Qualified symbol | `package`, `type_name`, `method`, `member`, `full_name` |
| Matching | `match:exact|prefix|contains`, `case:sensitive|insensitive` |
| History | `author`, `message`, `added`, `removed`, `after`, `before`, `branch`, `from`, `to` |

Autocomplete uses this grammar. `owner` is evaluated from the CODEOWNERS file
at the indexed commit and has explicit `owned`, `unowned`,
`unresolved_owner`, and `unavailable` states. Owner filters fail closed if
commit-pinned ownership metadata is unavailable.

Qualified symbol results prefer exact-revision SCIP data. Syntax-derived names
remain available with `confidence: "syntax"` when compiler data is absent.
SCIP results expose definitions, references, implementations, signatures,
documentation, relationships, and cross-repository stitching.

## Graph queries

`POST /api/graph/query` and MCP `query_evidence_graph` support:

- `mode: "impact"` with `direction: "upstream"|"downstream"|"both"`;
- `mode: "shortest_path"` between two repository, file, or symbol selectors;
- `relation_kinds`, a maximum depth of 6, and a maximum result limit of 1,000.

Responses retain edge evidence and explicitly report partial snapshots,
ambiguous selectors, depth/size truncation, and candidates.

## Saved searches and monitors

`GET /api/searches` returns only the current author's recent and personal
searches plus administrator-published shared templates. `POST /api/searches`
stores the complete structured request and its `pinned` or `latest_indexed`
revision policy. Shared templates are administrator-managed and read-only to
other users.

A saved search becomes a monitor through
`PUT /api/searches/{id}/monitor`. Running
`POST /api/search-monitors/{id}/run` compares stable result identities with
the previous exact revision snapshot and reports added/removed matches. History
is bounded to 1-100 runs. `notification_status: "not_configured"` is explicit
until a delivery integration exists; RepoKarta never implies a notification
was sent.

## Deep Search

Choose **Deep Search** in Chat to ask a natural-language question through the
existing read-only search, symbol, reference, AST, file, tree, Git, map,
dependency, Wiki, and graph tools. The trace shows scope resolution, searches,
file/tree reads, graph queries, elapsed time, source count, and coverage
warnings. It never exposes hidden model reasoning.

Deep Search preserves durable structured contexts across follow-ups. Turns can
be interrupted, retried from the persisted transcript, or retried with **Search
more broadly** under the same authorization boundary and bounded time/token
controls.

Share tokens are random and revocable. Every structured context and cited
`/source/{repository_id}` URL is re-authorized for each viewer. If any source
is inaccessible, RepoKarta denies the complete shared answer rather than
leaking a partial transcript.

Optional semantic similarity is not an authoritative evidence source.
RepoKarta does not currently enable semantic reranking. If added later it must
remain labeled and bounded and cannot convert a partial deterministic negative
into a complete answer.
