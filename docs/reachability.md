# Static code reachability

RepoKarta derives a conservative reachability report from the persisted
structural artifact for an exact indexed commit. Reading the report does not
execute repository code, change a checkout, or start an indexing job.

## Results

The report uses three states:

- `reachable`: a declaration is a recognized executable or framework entry
  point, or has a syntax-backed path from one.
- `probably_unreachable`: a private declaration has no path from a recognized
  root and every requested structural artifact and parsed document is complete.
- `unknown`: the available evidence cannot support either of the other states.

RepoKarta does not label declarations `dead`. Reflection, conditional Spring
profiles, qualifiers, generated code, callbacks, configuration-driven entry
points, and external runtime registration remain explicit dynamic boundaries.
`runtime_complete` therefore remains false even when `static_analysis_complete`
is true.

Recognized roots include Go and JVM `main` functions, Go `init` functions,
Spring application/configuration/component stereotypes, bean factories,
request handlers, scheduled methods, and common event or message listeners.
Calls, type references, dependency-injection candidates, and
implementation/extension relationships are resolved conservatively by name.
Ambiguous and unresolved relations are counted in the completeness payload.

## Interfaces

Use the repository Maps page for a summary. The evidence-bearing JSON contract
is available at:

```text
GET /api/reachability?repository=<repository-id>
```

The read-only MCP equivalent is `read_code_reachability`. Both surfaces return
the same commit-pinned symbol evidence, witness paths, summary, and
completeness fields.

## Repository update and indexing behavior

For local user-owned repositories, RepoKarta watches Git metadata such as
`HEAD`, packed refs, and ref directories. A ref change schedules the normal
debounced catalogue refresh; source trees are not watched because uncommitted
files are outside the indexed contract. The watcher never fetches, checks out,
resets, or otherwise mutates a repository.

When `refs/remotes/origin/HEAD` is configured and resolves locally, RepoKarta
indexes that immutable default-branch commit even if another branch is checked
out. The current checkout and its commit remain recorded separately. If a
configured remote default is unavailable, indexing falls back to the current
`HEAD`. This selection never performs network access or changes the worktree.

Large browser searches use a cancellable NDJSON response. The server sends an
immediate start event and then bounded server-rendered result prefixes while
preserving the final exact limits, truncation, and completeness summary.

## Linux packages

Release automation produces `linux-amd64` and `linux-arm64` tarballs alongside
Windows amd64 and macOS arm64 packages. Each archive includes required
third-party licenses, including the full Zoekt Apache-2.0 text, and is booted
on its native GitHub Actions runner before release publication.

To create a Linux package from a Unix-like build host:

```sh
REPOKARTA_PACKAGE_PLATFORM=linux-amd64 \
  ./scripts/package-release.sh "$(go run ./cmd/repokarta version)"
```

Use `linux-arm64` for the arm64 archive.
