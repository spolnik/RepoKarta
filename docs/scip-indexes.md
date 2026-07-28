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

## Automatic Java generation

RepoKarta can run an existing `scip-java` installation after ordinary source
indexing completes. Automatic execution is disabled by default because Gradle
build scripts execute repository code and may download dependencies.

Enable PATH discovery:

```powershell
repokarta serve `
  -scip-java-mode auto `
  C:\work\services
```

Or require a particular executable:

```powershell
repokarta serve `
  -scip-java-command C:\tools\scip-java.exe `
  -scip-java-timeout 30m `
  -scip-java-concurrency 1 `
  C:\work\services
```

The equivalent environment variables are
`REPOKARTA_SCIP_JAVA_MODE`, `REPOKARTA_SCIP_JAVA_COMMAND`,
`REPOKARTA_SCIP_JAVA_TIMEOUT`, and `REPOKARTA_SCIP_JAVA_CONCURRENCY`.
Modes are `off`, `auto`, and `required`. An explicit command implies
`required` unless the mode flag or environment variable was also set.
Concurrency is limited to 1 through 4 and defaults to 1.

### Select a compatible JDK per repository

RepoKarta reads the committed Gradle wrapper version, Java toolchain
declarations such as `JavaLanguageVersion.of(17)` or `jvmToolchain(17)`, and
Gradle daemon JVM criteria before starting `scip-java`. It prefers a configured
JDK matching the requested toolchain when that JDK can also run the wrapper.
For a legacy wrapper that cannot run on the requested toolchain JDK, RepoKarta
uses another compatible configured JDK to launch Gradle and exposes every
configured JDK to Gradle's normal toolchain discovery. Launcher selection
follows Gradle's published
[Java runtime compatibility matrix](https://docs.gradle.org/current/userguide/compatibility.html).

Configure versioned JDK homes with a comma-separated `major=home` mapping:

```powershell
$env:REPOKARTA_SCIP_JAVA_JDK_HOMES = '11=C:\Java\jdk-11,17=C:\Java\jdk-17,25=C:\Java\jdk-25'
repokarta serve -scip-java-mode auto C:\work\services
```

Use `REPOKARTA_SCIP_JAVA_JDK_HOME` or `-scip-java-jdk-home` only to force one
JDK for every repository. The forced override takes precedence and fails with
the `jdk_incompatible_wrapper` category when it cannot run a repository's
wrapper. Versioned homes are preferable for a mixed fleet. JDK paths remain
local configuration and are not returned by the API.

For each exact indexed commit, RepoKarta checks the committed Git tree for Java
sources and a Gradle build. Root and nested single-root Gradle builds, including
multi-project builds, are supported. Repositories with multiple independent
Gradle roots should be indexed as separate repositories.

Eligible builds run in a RepoKarta-owned detached Git worktree, never in the
user's checkout. RepoKarta invokes `scip-java index` from the detected build
root, imports the resulting bounded `index.scip`, records its indexer version
and configuration fingerprint, and removes the temporary worktree. A matching
ready artifact is not rebuilt until the indexed revision, indexer version, or
configuration changes.

The work is handled by a dedicated, deduplicated background queue. A failed,
unavailable, or inapplicable Java index never fails normal source indexing.
Provider status is available from `GET /api/scip/java`; per-repository status
is included in `GET /api/repositories`; and an authorized retry can be queued
with `POST /api/scip/java/retry/{repositoryID}` or the repository-list control.
Failure output is bounded and obvious credential-bearing lines are redacted.
Failed repository status also includes a stable `failure_category` and a short
`failure_summary`, while the bounded raw diagnostic remains available in
`error`. Categories are:

- `environment` for missing tools, Docker/service availability, permissions,
  networking, and timeouts;
- `jdk_incompatible_wrapper` when the selected or available JDK cannot launch
  the committed Gradle wrapper; and
- `compile_error` when Gradle reaches the build but compilation fails.

Successful and failed repository status records the detected Gradle version,
selected JDK major, and non-sensitive selection source. This makes the status
API and repository UI actionable without exposing local JDK paths.

RepoKarta neither downloads nor bundles `scip-java`; operators install and
trust the external Apache-2.0 tool and the repository build being executed.

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

Reference search uses SCIP when every applicable Java repository in the
requested scope has an artifact for its exact indexed commit and either:

- the query is a full SCIP symbol identity; or
- the source-level name resolves to exactly one SCIP symbol across that scope.

Results report `reference_resolution` as `scip-exact` or
`scip-unique-name`, `reference_index.provider` as `scip`, and each occurrence
with `reference_confidence: "compiler"`. Definitions are excluded from
reference results.

Repositories explicitly classified as non-Java do not prevent compiler-precise
Java resolution; manually imported artifacts for other languages remain part
of coverage when present. If any applicable artifact is missing or stale, or a
bare name is ambiguous, RepoKarta retains the existing syntax-backed
Tree-sitter behavior. A corrupt artifact is reported explicitly instead of
being silently ignored. Fallback results report `syntax-target-name`, the
`tree-sitter` provider, and do not present the fallback as compiler-precise.

This first slice imports symbol identities and reference occurrences. SCIP
definitions, implementations, signatures, hover documentation, dependency
indexes, and graph reachability remain future layers. Framework entry-point and
dependency-injection roots must also be modeled separately before RepoKarta can
label code unreachable; absence from the current reference graph is not a
dead-code claim.
