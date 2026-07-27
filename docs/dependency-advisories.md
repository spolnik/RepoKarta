# Dependency advisory findings

RepoKarta joins its commit-pinned dependency inventory to a local snapshot of
OSV.dev advisories. The join is deterministic and contains no AI, repository
execution, reachability claim, manifest rewrite, or CI enforcement.

## Data flow

1. Dependency artifacts capture ecosystem, package, declared and resolved
   versions, usage, scope, resolution source, repository revision, and exact
   manifest or lockfile evidence.
2. A scheduled daily refresh, or an explicit refresh from the Dependencies
   workspace, sends the bounded set of unique ecosystem/package identities to
   the OSV batch API. Returned advisory IDs are hydrated at a paced rate.
3. RepoKarta publishes a complete fleet-relevant snapshot under the application
   data directory at `advisories/osv-snapshot.json`. The file records retrieval
   time, a content-hash version, the package-query digest, and the full OSV
   records used for matching. A failed refresh leaves the prior complete
   snapshot readable.
4. UI, JSON, MCP, and SARIF reads use only that local snapshot. They never wait
   for or initiate network access.

The refresh is bounded to 20,000 unique package identities and 50,000 hydrated
advisories. Batch requests are paginated, advisory retrieval is paced, and
progress is exposed at `/api/dependencies/advisories/progress`.

## Matching and evidence

RepoKarta selects a lockfile-resolved version when present. Otherwise, it uses
an exact declared version and marks the match as lower confidence. Constraints,
variables, absent versions, invalid ecosystem versions, packages added after
the snapshot, and ecosystems not covered by OSV are reported as explicit gaps.

OSV `ECOSYSTEM` ranges are evaluated with the packaging system's ordering rules
for Maven, npm, PyPI, Go modules, crates.io, and NuGet. `SEMVER` ranges follow
the OSV schema's SemVer ordering. Fixed boundaries are exclusive;
`last_affected` boundaries are inclusive. Maven qualifiers are not compared as
generic semver.

Every finding contains two independent evidence references:

- the exact repository revision and manifest or lockfile URL captured by the
  dependency inventory;
- the OSV advisory URL plus the local snapshot timestamp and content-hash
  version.

The finding record reserves an optional reachability field, but this iteration
does not populate it or imply that a vulnerable package is imported or called.

## Surfaces

- `/dependencies?view=findings` provides fleet and per-repository filters,
  severity-by-usage headlines, remediation versions, snapshot age, and coverage
  gaps.
- `/api/dependencies/findings` returns bounded JSON pages. `limit` must be from
  1 through 500 and invalid parameters return the standard structured error
  message.
- `read_dependency_findings` is a compact read-only MCP tool. It omits advisory
  prose bodies while retaining IDs, aliases, package versions, severity, usage,
  match basis, remediation versions, and both evidence citations.
- `/api/dependencies/findings.sarif` exports SARIF 2.1.0 that Insights can
  import beside external scanner evidence at the same revisions.

A response with `check_state: ready`, a positive checked-declaration count, and
zero findings means the snapshot was checked and no matches were found. States
such as `unavailable`, `partial`, or `stale` must not be interpreted as a clean
result. All findings and exported thresholds are advisory-only and are never
represented as an enforced CI gate.
