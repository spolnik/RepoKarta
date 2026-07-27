# Compiler-precise SCIP indexes

RepoKarta can consume a standard `index.scip` produced by a compiler-backed
language indexer. This is an optional precision layer above the built-in
Tree-sitter structural index; RepoKarta does not execute builds or language
toolchains while serving an interactive request.

## Produce an index

Run the maintained indexer for the repository language in the repository's
normal trusted build environment. Common producers include `scip-go`,
`scip-java`, `scip-typescript`, and `scip-python`. The output is normally named
`index.scip`.

The index must describe the same commit RepoKarta has indexed. Dependencies,
generated sources, compiler flags, and framework plugins should be prepared in
that build environment before invoking the indexer. Pin the indexer version in
CI for reproducible results.

## Import an index

First read the repository ID and `indexed_commit` from `GET /api/repositories`,
then stop RepoKarta before writing to the same local data directory:

```powershell
repokarta scip import `
  -data-dir C:\path\to\repokarta-data `
  -repository-id 7 `
  -revision 0123456789abcdef0123456789abcdef01234567 `
  -root backend `
  C:\artifacts\index.scip
```

`-revision` defaults to the repository's current indexed commit. Import fails
closed if an explicit revision differs from that commit. The original protobuf
is validated and projected into a bounded RepoKarta-owned artifact; source
paths must be canonical repository-relative SCIP paths. The importer accepts
at most 256 MiB, 200,000 documents, and 2,000,000 symbol occurrences.
`-root` defaults to the repository root (`.`); set it to the repository-relative
project directory when the indexer ran in a monorepo subdirectory.

Re-import after RepoKarta indexes a new commit. Artifacts for older revisions
are never silently applied to newer source.

## Reference resolution and fallback

Reference search uses SCIP when every repository in the requested scope has an
artifact for its exact indexed commit and either:

- the query is a full SCIP symbol identity; or
- the source-level name resolves to exactly one SCIP symbol across that scope.

Results report `reference_resolution` as `scip-exact` or
`scip-unique-name`, `reference_index.provider` as `scip`, and each occurrence
with `reference_confidence: "compiler"`. Definitions are excluded from
reference results.

If any artifact is missing or stale, or a bare name is ambiguous, RepoKarta
retains the existing syntax-backed Tree-sitter behavior. A corrupt artifact is
reported explicitly instead of being silently ignored. Fallback results report
`syntax-target-name`, the `tree-sitter` provider, and does not present the
fallback as compiler-precise.

This first slice imports symbol identities and reference occurrences. SCIP
definitions, implementations, signatures, hover documentation, dependency
indexes, and graph reachability remain future layers. Framework entry-point and
dependency-injection roots must also be modeled separately before RepoKarta can
label code unreachable; absence from the current reference graph is not a
dead-code claim.
