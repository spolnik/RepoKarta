# RepoKarta

<p align="center">
  <img src="https://raw.githubusercontent.com/spolnik/RepoKarta/main/web/public/assets/repokarta-mark-192.png" alt="RepoKarta" width="128">
</p>

<p align="center">
  <strong>Understand the code you already have.</strong><br>
  Search, explore, map, and document one Git repository—or a whole workspace—from a local web app.
</p>

RepoKarta turns a folder of Git repositories into a searchable, navigable code
workspace. Point it at your projects, let it index the committed source, and
use your browser to move from a search result to the exact file, revision, map,
dependency, or explanation behind it.

It is local-first and read-only by default. Search does not require an AI
provider, an API key, Docker, or a cloud account.

> **Project status:** RepoKarta is under active pre-release development. The
> core workflows are usable, but interfaces and stored data formats may still
> evolve. See [SCOPE.md](./SCOPE.md) for the current roadmap and implementation
> status.

## What can I do with it?

- **Search across repositories.** Find text, files, symbols, references,
  implementations, routes, dependencies, commits, diffs, Wiki pages, and code
  insights from one search box.
- **Browse and investigate the indexed revision.** Open a repository tree,
  select a class, enum, method, or function to find its usages, search the
  repository without leaving the editor, and inspect history or diffs without
  changing branches in your worktree.
- **See how a system fits together.** Follow component-to-component HTTP,
  gRPC, Kafka, database, and MCP communication with visible direction,
  commit-pinned source evidence, and separately timestamped runtime signals.
- **Ask questions with citations.** Use Codex, Claude Code, or the Anthropic
  API to answer questions against RepoKarta's read-only code tools.
- **Create living documentation.** Generate beautifully structured,
  human-readable, commit-pinned repository pages,
  review their citations, and export them as Markdown.
- **Review engineering signals.** Import coverage and static-analysis reports,
  compare revisions, and inspect dependency versions without running project
  code.
- **Upgrade Java navigation when requested.** Use an installed or explicitly
  configured `scip-java` to build compiler-precise reference indexes in a
  background, exact-commit worktree while normal indexing remains available.

### The important difference

RepoKarta keeps the evidence attached:

- results point to an exact repository, commit, path, and line;
- partial or still-building results say so instead of pretending to be
  complete;
- repositories remain the source of truth—RepoKarta stores indexes and
  generated artifacts separately;
- AI is an optional layer on top of deterministic search and source tools.

## Quick start

### 1. Choose a repository root

Use a directory that contains one repository or many repositories. A
[ghorg](https://github.com/gabrie30/ghorg) workspace is a natural fit, but it
is not required. RepoKarta discovers regular repositories, linked worktrees,
and bare repositories recursively.

### 2. Build and start RepoKarta

You need Go 1.26 or newer, Node.js 24 or newer, and Git. From this repository:

```sh
npm --userconfig .npmrc --prefix web ci
npm --prefix web run build
go run ./cmd/repokarta serve /path/to/repositories
```

Windows example:

```powershell
npm --userconfig .npmrc --prefix web ci
npm --prefix web run build
go run ./cmd/repokarta serve C:\Work\GitHub
```

RepoKarta opens [http://127.0.0.1:7331](http://127.0.0.1:7331) and begins
cataloguing and indexing the committed source. The interface shows progress;
large workspaces can become useful repository by repository.

Use `-open=false` if you do not want the browser to open automatically:

```powershell
go run ./cmd/repokarta serve -open=false C:\Work\GitHub
```

### 3. Take a short tour

Once at least one repository is ready:

1. Search for a class, function, route, error message, or filename.
2. Open a result, select an identifier to find its usages, or use the embedded
   repository-scoped search without leaving the source editor.
3. Open **Maps** to explore packages, entry points, and relationships.
4. Open **Dependencies** to explore the distributed component topology, then
   switch to package inventory or security findings when needed.
5. If an AI provider is available, open **Chat**, add an `@repository`,
   `@file`, `@directory`, or `@symbol` context, and ask a focused question.
6. Open **Wiki** when you want a reusable, cited explanation of a repository.

You can stop RepoKarta with `Ctrl+C`. Your catalogue, indexes, conversations,
and generated artifacts remain in RepoKarta's data directory for the next
run.

## Install or build a native executable

### Homebrew from HEAD

```sh
brew tap spolnik/repokarta https://github.com/spolnik/RepoKarta.git
brew install --HEAD --build-from-source spolnik/repokarta/repokarta
```

### Native build from source

The platform build script creates a native executable with the complete parser
set and bundled third-party licenses:

```powershell
.\scripts\build.ps1
.\dist\repokarta.exe serve C:\Work\GitHub
```

```sh
./scripts/build.sh
./dist/repokarta serve ~/Code
```

## Everyday use

### Search in plain language first

An ordinary query searches source content. Add filters when the result set is
too broad:

```text
timeout
PaymentService repository:payments
"connection refused" language:Go -path:vendor
Service result_type:symbol_definition
BaseService result_type:implementation
"/api/search" result_type:route repository:RepoKarta
author:"Alex Smith" result_type:commit after:2026-01-01
```

Useful fields include `repository`, `revision`, `language`, `path`, `file`,
`author`, `message`, and `result_type`. Prefix a term or field with `-` to
exclude it. The search box autocompletes the supported grammar, so you do not
need to memorize it.

See [Advanced search and Deep Search](docs/advanced-search.md) for qualified
symbols, CODEOWNERS filters, evidence-graph queries, saved-search monitors, and
permission-safe Deep Search sharing. Dependency version states, discrepancy
filters, and fail-closed private registry routing are documented in
[Dependency management](docs/dependency-management.md).

RepoKarta also exposes explicit Zoekt, AST reference, regular-expression, and
literal modes. Filters, ranking explanations, result counts, and completeness
warnings stay visible in the results.

### Ask questions only when it helps

Search, source browsing, maps, dependencies, and imported insights work
without AI. To enable cited Chat and generated Wiki pages, use one of:

- **Codex:** install the Codex CLI and sign in with `codex login`;
- **Claude Code:** install Claude Code and sign in with
  `claude auth login`; or
- **Anthropic API:** set `ANTHROPIC_API_KEY` in the environment that starts
  RepoKarta.

RepoKarta detects the local CLIs at startup and uses their existing login
sessions. It does not ask for or store your ChatGPT or Claude subscription
credentials. The direct Anthropic API key is read from the launch environment
and is not written to RepoKarta's database.

Provider subprocesses are capability-confined. Codex receives a filesystem
permission profile that denies reads outside its attachment directory and
minimal runtime files. Claude receives attachment-scoped reads and disallows
shell, discovery, mutation, web, and subagent tools. Each provider session
uses a revocable conversation-bound MCP credential; Claude's credential lives
in a mode-0600 temporary configuration file rather than process arguments.

If a CLI is installed in a non-standard location:

```powershell
go run ./cmd/repokarta serve `
  -codex-command C:\Tools\codex.exe `
  -claude-command C:\Tools\claude.exe `
  C:\Work\GitHub
```

For good answers, give Chat the smallest useful context. A file or symbol is
usually better than an entire repository; a repository is usually better than
the whole catalogue. Context chips are permission-checked and pinned to the
indexed revision.

### Keep noisy folders out

Repeat `-exclude` for directory names that RepoKarta should skip during
discovery:

```powershell
.\repokarta.exe serve `
  -exclude archived `
  -exclude vendor `
  C:\Work\GitHub
```

Run `repokarta help` to see all command-line options.

## Where does my data go?

RepoKarta keeps its state outside your repositories:

| Platform | Default data directory |
| --- | --- |
| Windows | `%LOCALAPPDATA%\RepoKarta\` |
| macOS | `~/Library/Caches/RepoKarta/` |
| Linux | the current user's operating-system cache directory |

That directory contains the default SQLite metadata database, search indexes,
maps, generated documentation, conversation data, and other RepoKarta-owned
artifacts. Override it with `-data-dir` when needed. PostgreSQL 18 or newer is
an optional metadata backend for shared deployments; SQLite remains the
zero-configuration default. See the
[PostgreSQL backend and migration guide](./docs/postgresql.md).

Your local repositories are treated as read-only inputs. RepoKarta does not
fetch, checkout, reset, clean, build, execute, or delete them. The optional
administrator acquisition workflow can clone and update hosted repositories,
but only inside RepoKarta-owned storage; it never pushes upstream or runs
repository hooks.

By default, the server listens only on `127.0.0.1:7331` and validates the
browser origin. Provider processes receive a narrow, authenticated, read-only
tool surface.

## Use RepoKarta with other tools

### MCP

Keep RepoKarta running, then open the **MCP** page in the app for a copyable,
current client configuration. The token shown there is process-scoped and
changes after a restart.

For clients that launch a stdio adapter:

```json
{
  "mcpServers": {
    "repokarta": {
      "command": "repokarta",
      "args": ["mcp", "-url", "http://127.0.0.1:7331"]
    }
  }
}
```

The tools cover repository discovery, text and structural AST search, symbols,
references, files, trees, Git history, diffs, maps, dependencies, Wiki pages,
insights, and named contexts. They use the same permission and completeness
rules as the browser. Every repository-aware MCP tool accepts either
`repository_id` or an exact `repository` name; ambiguous names fail with the
matching IDs.

### JSON API

The browser, MCP adapters, and AI providers share the same JSON capability
boundary. A few useful read-only endpoints are:

```text
GET /api/repositories
GET /api/search?q=OpenFile&repo=RepoKarta
GET /api/search?q=OpenFile&repo=RepoKarta&mode=references
POST /api/ast/search
GET /api/symbol?symbol=OpenFile&repo=RepoKarta
GET /api/file/{repository}?rev={commit}&path={path}&lines=1-200
GET /api/git/log/{repository}?rev={commit}&limit=50
GET /api/maps?repository={repository-id}
GET /api/dependencies/topology?repository={repository-id}
GET /api/dependencies?repository={repository-id}
GET /api/wiki?repository={repository-id}
GET /api/whoami
```

Search and history responses report bounds and truncation explicitly. Prefer
the in-app MCP page when integrating an agent because it contains the current
endpoint, token, and complete tool catalogue.

## Shared and enterprise deployments

Local loopback mode is the simplest and safest way to use RepoKarta. For a
team deployment, RepoKarta also supports:

- Cloudflare Access JWT or native SAML authentication;
- deny-by-default repository access for users and identity-provider groups;
- reader, knowledge-maintainer, and administrator roles;
- SCIM 2.0 provisioning and deprovisioning;
- redacted audit evidence and bounded exports; and
- administrator-approved GitHub, GitLab, and local repository acquisition.

Conversation transcripts are owner-only on shared instances, including for
administrators. Wiki generation and administrative mutations retain the
authenticated principal in redacted audit evidence.

A shared deployment needs a trusted HTTPS reverse proxy, protected secret
files, explicit repository grants, backups, and post-deploy authorization
checks. Start with the [shared deployment runbook](./docs/shared-deployment.md),
then use the
[enterprise identity and audit guide](./docs/enterprise-administration.md).
Example service files live in [`deploy/`](./deploy/).

## Develop and verify

Run the frontend checks:

```sh
npm --prefix web test
npm --prefix web run typecheck
npm --prefix web run build
```

Frontend dependency changes are deliberately conservative. The committed
`web/package-lock.json` supplies exact artifacts and integrity hashes to
`npm ci`; the repository `.npmrc` saves new direct dependencies exactly and
applies a 14-day release-age window when npm performs a fresh resolution. Pass
that policy explicitly whenever npm can change the dependency tree:

```sh
npm --userconfig .npmrc --prefix web install <package>
```

The `check:dependencies` script rejects unbounded direct specifications and
unapproved major-line changes, including entries under `overrides`. In
particular, the browser test runtime stays
on `jsdom ^29.1.1` until its policy entry is deliberately reviewed. Run the
policy check directly with:

```sh
npm --prefix web run check:dependencies
```

The release-age window creates review time; it does not prove that an older
package is benign. Dependency upgrades still require lockfile review and the
normal test suite.

Run the standard Go test suite:

```sh
go test ./...
go vet ./...
```

The platform build scripts run the full parser-tag test suite, build the
executable into `dist/`, and copy all required third-party licenses into
`dist/licenses/`:

```powershell
.\scripts\build.ps1
```

```sh
./scripts/build.sh
```

Create a release-shaped package locally:

```powershell
$version = go run ./cmd/repokarta version
.\scripts\package-release.ps1 -Version $version
```

```sh
./scripts/package-release.sh "$(go run ./cmd/repokarta version)"
```

## Further reading

- [Product scope, decisions, and roadmap](./SCOPE.md)
- [Shared deployment operations](./docs/shared-deployment.md)
- [PostgreSQL backend and SQLite migration](./docs/postgresql.md)
- [Enterprise identity and audit operations](./docs/enterprise-administration.md)
- [Dependency advisory data and refresh behavior](./docs/dependency-advisories.md)
- [Distributed dependency topology and runtime observations](./docs/distributed-topology.md)
- [Source editor search, usages, routes, and caller evidence](./docs/source-intelligence.md)
- [Compiler-precise SCIP index imports](./docs/scip-indexes.md)
- [Structural AST query syntax and completeness](./docs/ast-search.md)
- [OpenTelemetry metrics, logs, traces, and Datadog routing](./docs/opentelemetry.md)
- [Frontend API contracts, streaming recovery, and reproducible assets](./docs/frontend-contracts.md)
- [Pinned Zoekt revision and Windows portability](./docs/zoekt-windows-portability.md)

RepoKarta vendors a pinned Zoekt revision under `third_party/zoekt`. Native
builds and release packages include the full Zoekt Apache License 2.0 text and
all other required third-party notices in their `licenses/` directory.

## License

RepoKarta is licensed under the [Apache License 2.0](./LICENSE). Release
packages include the project license at their root and dependency notices in
their `licenses/` directory.
