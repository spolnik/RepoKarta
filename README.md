# RepoKarta

RepoKarta is fast, local-first code search for the Git repositories already on
your laptop. Point it at a ghorg directory (or any repository root), and it
discovers, incrementally indexes, and searches committed source without
modifying a worktree.

The current implementation delivers M1 code search, M2 grounded code
questions, M3 evidence-backed repository maps, M4 living documentation, M5
native distribution, M6 shared deployment, M8 code insights, and the M9
dependency-management workspace:

- regular, linked-worktree, and bare Git repository discovery;
- origin, default revision, HEAD, scan, and index metadata in SQLite;
- explicit terminal `empty` state and reason for repositories with no commits,
  excluded from indexing and derived-artifact pending counts;
- canonical path reconciliation that removes stale and duplicate catalogue rows
  on every discovery;
- administrator-reviewed repository acquisition for local Git roots, GitHub
  organizations/teams/repositories, and GitLab groups/projects, with explicit
  archived, fork, visibility, topic, allow, deny, and already-managed states;
- RepoKarta-owned hosted checkouts with manual or scheduled synchronization,
  stable provider identity, bounded backoff, source-free audit events, and
  recoverable removal;
- native Zoekt indexing on Windows amd64, macOS arm64, and Linux;
- incremental reindexing when a repository HEAD changes;
- literal, regular-expression, and native Zoekt query modes;
- syntax-backed reference search over persisted AST call, import, extends, and
  implements relations, with relation metadata, explicit coverage limits, and
  background structural-index warming after repositories become search-ready;
- repository, language, path, and file filters;
- caller-controlled file limits up to 500, with explicit matched, returned,
  skipped, truncated, and exact/estimated completeness metadata;
- automatic Universal Ctags discovery, plus an explicit warning when `sym:`
  search is unavailable;
- a Git CLI indexing fallback for repositories that go-git cannot open,
  including `extensions.worktreeConfig=true` repositories;
- bounded, cancellable result sets with highlighted source matches;
- commit-pinned source pages and copyable file-and-line citations;
- bounded Git history and exact commit-to-commit diffs without indexing every
  historical tree;
- historical file and tree reads restricted to commits reachable from the
  catalogue's recorded indexed or HEAD commits;
- live indexing state through Server-Sent Events;
- loopback Host and Origin validation;
- streamed, multi-turn questions through a local Codex or Claude Code harness,
  or a Go-native Anthropic Messages API loop;
- animation-frame-batched response streaming with one final Markdown,
  sanitization, syntax-highlighting, and Mermaid pass per answer, including a
  full-window vector diagram viewer with fit, percentage zoom, and SVG
  download controls;
- reuse of the provider's existing ChatGPT/Codex or Claude login without
  collecting subscription credentials, plus `ANTHROPIC_API_KEY` configuration
  that is read only from the launch environment;
- one provider-neutral Go conversation interface;
- a JSON code-intelligence API used by the UI and protocol adapters;
- authenticated loopback HTTP MCP plus a stdio MCP adapter with read-only
  `list_repositories`, `search_code`, `find_symbol`, `find_references`,
  `get_file`, `list_tree`, `git_log`, `git_diff`, `read_repository_map`,
  `read_dependency_inventory`, `list_deep_wiki_pages`,
  `read_generated_document`, `read_code_insights`, and
  `compare_code_insights` tools;
- read-only Codex sandboxes, Claude plan mode, and disabled mutation/shell
  tools;
- authoritative citation chips recorded from the exact MCP tool results rather
  than trusting a model to reproduce source URLs;
- locally persisted, author-owned conversations with reopen, rename, delete,
  administrator-visible own/all history filters, native provider resume, and
  bounded transcript-replay fallback;
- deny-by-default shared repository ownership with explicit user, identity
  provider group, and instance-shared grants inherited by source, Search,
  Maps, Wiki, dependencies, exports, and conversation-scoped MCP tools;
- immediately evaluated reader, knowledge-maintainer, and administrator roles,
  including direct assignments, SCIM group membership, and exact
  identity-provider group mappings;
- SCIM 2.0 user and group provisioning with stable external IDs, idempotent
  replacement and patch operations, suspension, deprovisioning, and
  next-request session revocation;
- append-only redacted audit evidence for authentication, authorization,
  administration, role changes, cross-author access, repository catalogue
  changes, exports, generation, and destructive RepoKarta-owned operations,
  with bounded filters, retention, and JSON or CSV export;
- visible token usage plus per-turn cancellation, timeout, and output-token
  budget controls;
- adversarial coverage that keeps instructions found in repository content
  from expanding the agent's read-only tool permissions;
- embedded frontend assets in one native Go executable;
- deterministic language and manifest inventories extracted from committed
  source without executing repository code;
- a bounded pure-Go syntax-tree index for Java, Kotlin, Gradle Groovy,
  TypeScript/TSX, JavaScript, Go, SQL, Bash, and Python, with exact declaration,
  relation, build-fact, and parser-diagnostic ranges;
- commit-keyed package, dependency, entry-point, and HTTP-route graphs with
  source evidence on every node and relationship, resolved Gradle versions,
  and production-first Spring HTTP-call edges;
- an interactive layered map with scope and view filters, search, neighbor
  focus, evidence inspection, downloadable JSON snapshots, and explicit
  analyzed/omitted repository counts for bounded fleet views;
- Deep Wiki surveys seeded with a curated subset of parsed types, functions,
  and Gradle build facts;
- one standard-quality Wiki pipeline for every provider; no reduced-depth Fast
  mode is exposed or accepted by the generation API;
- bounded provider plans with five to twelve focused pages (three to six for
  compact repositories), a short architecture orientation, and a concise
  glossary;
- page turns that reuse the saved survey evidence instead of repeating broad
  discovery, with page-specific writing, tool-call, and output budgets;
- deterministic three-page documentation plans generated independently from
  commit-pinned structural facts;
- durable page status, provider/model metadata, source sets, citations, and
  filesystem-backed Markdown with interrupted-job recovery;
- commit-aware selective staleness and regeneration based on exact changed
  supporting files;
- validated Mermaid diagrams, reviewed `.repokarta.yml` steering, a dedicated
  Wiki workspace, and portable Markdown ZIP export;
- administrator storage inventory with signed dry-run cleanup plans that can
  remove only stale map snapshots, orphaned attachments, logs, and interrupted
  temporary files while protecting source, live indexes, current Wiki content,
  SQLite, and SAML identity;
- source-free diagnostic ZIP export with an explicit inclusion/omission
  manifest, redacted failures, format versions, readiness, and storage totals.
- a commit-aware Code Insights workspace that imports LCOV, JaCoCo XML,
  Cobertura XML, SARIF 2.1.0, Semgrep JSON, and MegaLinter SARIF without
  executing repository code;
- normalized coverage metrics and scanner findings with exact producer,
  revision, branch, rule, severity, fingerprint, suppression, code-flow,
  location, timestamp, confidence, and provenance metadata;
- strict revision and committed-path reconciliation with visible current,
  partial, quarantined, unresolved-path, skipped, parse-error, stale,
  unavailable, and rate-limited states;
- current-versus-history queries, fleet/repository filters, revision
  comparisons, introduced and resolved findings, metric deltas, and
  administrator-defined advisory thresholds that are never represented as
  enforced CI gates;
- bounded deterministic code-size and lexical complexity indicators over
  committed source, clearly separated from externally measured coverage;
- an optional read-only SonarQube Community Build Web API adapter with bounded
  polling, backoff, retention, externally rotated environment-variable
  credentials, and no embedded server, database, or scanner.

The full product definition and non-goals live in [SCOPE.md](./SCOPE.md).

## Run

Requirements for a development build:

- Go 1.26 or newer
- Node.js 24 or newer
- Git

Build the embedded frontend:

```sh
npm --prefix web ci
npm --prefix web run build
```

Start RepoKarta:

```sh
go run ./cmd/repokarta serve /path/to/ghorg
```

Windows example:

```powershell
go run ./cmd/repokarta serve C:\Work\ghorg
```

RepoKarta opens `http://127.0.0.1:7331` in the default browser. Use
`-open=false` to keep it in the terminal. Exclude directories with a repeatable
flag:

```powershell
go run ./cmd/repokarta serve -exclude archived -exclude vendor C:\Work\ghorg
```

Search works without an AI provider or API key. Literal mode is the default;
regular-expression mode treats the whole query as a regex; Zoekt mode exposes
boolean, `repo:`, `lang:`, `file:`, `sym:`, and other native Zoekt syntax.
RepoKarta enables symbol indexing automatically when `universal-ctags` is on
`PATH`, or when `ctags`/`CTAGS_COMMAND` identifies itself as Universal Ctags via
`--version`. This supports Homebrew's `ctags` name without accidentally enabling
the BSD ctags shipped by macOS. If Universal Ctags is unavailable, `sym:`
searches return a machine-readable and visible warning instead of a misleading
silent zero.

## Code insights

Open `/insights` to import trusted CI reports, derive bounded committed-source
indicators, compare stored revisions, and inspect monitoring state. Every
upload must name the exact Git revision it analyzed. A report that does not
match the indexed snapshot is retained as quarantined evidence and is excluded
from default current-value queries. When the indexed revision later advances,
older runs become explicitly stale and their observations move to history.

The read-only query endpoints are:

- `GET /api/insights` for bounded history, current values, runs, facets, and
  explicit completeness warnings;
- `GET /api/insights/compare` for metric deltas and introduced or resolved
  findings between two exact stored revisions;
- `GET /api/insights/thresholds` for advisory threshold configuration and
  status.

Knowledge maintainers and administrators can upload a report with
`POST /api/insights/import` or request bounded committed-source indicators with
`POST /api/insights/derive`. Administrators configure advisory thresholds and
an externally managed SonarQube Community Build through the
`/api/insights/sonar` endpoints. Sonar tokens are read from the configured
environment-variable name at poll time; their values are never stored.
RepoKarta does not include a local test or scanner execution endpoint.

Open `/dependencies` for the commit-pinned declaration inventory. Package,
manifest, repository, ecosystem, production/test/development/build usage,
relationship, and resolution filters are evaluated before returning a bounded
page. npm manifest sections, Gradle configurations, Maven scopes, Cargo
dependency tables, Python dependency groups, and NuGet project references
remain visible instead of being flattened away. Go module usage is classified
from committed production and test imports. Both the HTML workspace and
`/api/dependencies` default to 100 declarations per page and reject limits
above 500. Cold reads compose only already-prepared per-repository artifacts:
the API returns
`202 Accepted` with ready and pending repository counts while the eight-worker
background pool completes the fleet, and the HTML workspace shows the same
progress instead of blocking on source analysis.

When a supported lockfile is committed, the inventory keeps the manifest's
declared constraint and the installed resolution as separate facts. It reads
`package-lock.json`, `npm-shrinkwrap.json`, `pnpm-lock.yaml`,
`gradle.lockfile`, `Cargo.lock`, `uv.lock`, `poetry.lock`,
`packages.lock.json`, and exact `go.mod` requirements. Ambiguous transitive
matches remain unresolved instead of guessing. The table therefore distinguishes
**Declared**, **Resolved**, and registry-observed **Latest stable** versions,
including current, update available, ahead, prerelease, unresolved-constraint,
stale-cache, and registry-error states.

Knowledge maintainers and administrators can start a token-free public refresh
with `POST /api/dependencies/refresh`. Supported public services are the npm
registry, Maven Central, PyPI, crates.io, the public Go module proxy, and NuGet.
RepoKarta deduplicates package coordinates, checks them through an eight-worker
pool, and caches observations in SQLite for 24 hours. `/dependencies` never
waits for a registry: it joins cached version and observation time onto
commit-pinned declarations. `GET /api/dependencies/progress` reports the
current refresh. Conditional request validators, registry throttling responses,
and short-lived error caching avoid unnecessary repeat traffic. Registry checks
never modify manifests or lockfiles.

Private packages can be routed explicitly by ecosystem and longest package
prefix. Configuration is read from `REPOKARTA_DEPENDENCY_REGISTRIES`; it stores
only an environment-variable name and reads the bearer token at request time.
The metadata endpoint must return that ecosystem's normal registry response
shape. HTTPS is required except for a loopback development registry:

```powershell
$env:ACME_NPM_TOKEN = Read-Host -MaskInput "Private registry token"
$env:REPOKARTA_DEPENDENCY_REGISTRIES = @'
[
  {
    "ecosystem": "npm",
    "base_url": "https://npm.example.com",
    "metadata_url_template": "https://npm.example.com/{package}",
    "package_prefixes": ["@acme/"],
    "token_env": "ACME_NPM_TOKEN"
  }
]
'@
```

Templates may use `{package}` for npm, PyPI, or NuGet; `{group_path}` and
`{artifact}` for Maven; `{module}` for Go; or `{cargo_path}` for a Cargo sparse
index. A package that does not match an explicit private route uses its public
ecosystem service, so configure every internal prefix before starting a
refresh.

## Deployment authentication

RepoKarta stays loopback-only by default. A shared deployment can use one of
four explicit modes:

- `local`: no user login, but Host and Origin checks restrict the interface to
  loopback.
- `cloudflare-access`: validates Cloudflare Access application JWTs from
  `Cf-Access-Jwt-Assertion`, including signature, issuer, audience, and time
  claims.
- `saml`: runs a native SAML 2.0 service provider. This works with
  Cloudflare's generic SAML SaaS application and other SAML identity providers.
- `open`: shared access without user authentication. This mode is unavailable
  unless the service starts with `-allow-open=true`.

In loopback-local mode, `/admin` opens directly for the single local
administrator and creates an ephemeral CSRF-protected console session. Shared
modes retain the separate bootstrap login. Supply its username and password
file at service startup; the credentials are held in memory and are not written
to SQLite:

```powershell
go run ./cmd/repokarta serve `
  -listen 0.0.0.0:7331 `
  -open=false `
  -admin-user repokarta-admin `
  -admin-password-file C:\secure\repokarta-admin-password.txt `
  -scim-token-file C:\secure\repokarta-scim-token.txt `
  C:\Work\ghorg
```

Sign in at `/admin`, set the exact public HTTPS URL, and choose the access mode.
Only non-secret provider settings persist. Native SAML generates its private
key under the RepoKarta data directory; the admin page shows the service
provider metadata and ACS URLs to enter in the identity provider.

For Cloudflare Access JWT mode, enter the team domain such as
`https://team.cloudflareaccess.com` and the Access application audience tag.
For native SAML with Cloudflare, create a generic SAML SaaS application using:

```text
Entity ID: https://repokarta.example.com/saml/metadata
ACS URL:   https://repokarta.example.com/saml/acs
```

Then enter Cloudflare's IdP metadata URL in RepoKarta. The same settings can be
supplied on first startup with `-auth-mode`, `-public-url`,
`-cloudflare-team-domain`, `-cloudflare-audience`, `-saml-metadata-url`, and
`-saml-entity-id`. Corresponding environment variables are
`REPOKARTA_AUTH_MODE`, `REPOKARTA_PUBLIC_URL`,
`REPOKARTA_CF_TEAM_DOMAIN`, `REPOKARTA_CF_AUDIENCE`,
`REPOKARTA_SAML_METADATA_URL`, and `REPOKARTA_SAML_ENTITY_ID`.
`REPOKARTA_ADMIN_USER`, `REPOKARTA_ADMIN_PASSWORD_FILE`,
`REPOKARTA_SCIM_TOKEN_FILE`, and `REPOKARTA_ALLOW_OPEN` cover the startup-only
controls. The SCIM bearer token itself is read only from its
permission-restricted file and is never persisted.

Authentication establishes a stable conversation author. Shared users can
list, open, continue, rename, interrupt, and delete only their own
conversations; administrators can inspect all authors. Loopback-local mode
always acts as the local administrator. Repositories and every derived
artifact default to private `local:admin` ownership after an upgrade or new
discovery. Use the administrator panel to grant a stable user ID, an IdP group,
or explicit instance-wide shared visibility.

When `-scim-token-file` is configured, the SCIM base URL is
`https://repokarta.example.com/scim/v2`. Provisioning clients authenticate with
that bearer token. The administrator page assigns direct and SCIM-group roles,
maps exact SAML or Cloudflare Access group claims, controls audit retention,
and shows recent redacted evidence. Application administrators can use the
permission-checked `/api/admin/*` JSON endpoints. `GET /api/whoami` returns the
effective role and permissions.

The role matrix is:

| Capability | Reader | Knowledge maintainer | Administrator |
| --- | --- | --- | --- |
| Read authorized repositories and artifacts, own conversations, artifact export | Yes | Yes | Yes |
| AI chat and Wiki generation | No | Yes | Yes |
| Cross-author conversations, repository refresh/acquisition, security settings, roles, audit evidence | No | No | Yes |

Unknown authenticated identities start as readers. Unknown or removed groups
never grant elevation. A suspended or deprovisioned managed identity is denied
on its next request even if its upstream SAML or Cloudflare session remains
valid. See [enterprise identity and audit operations](./docs/enterprise-administration.md).

Release archives include service templates and the complete
[shared-deployment runbook](./docs/shared-deployment.md), including reverse
proxy, backup, restore, upgrade, rollback, authorization verification, and
deprovisioning guidance.

## Repository acquisition

The protected `/admin` page can preview and approve three repository sources:

- a local directory, including a ghorg-managed root, whose repositories remain
  user-owned and are only inspected with read-only Git commands;
- a GitHub organization, team, user, or exact HTTPS repository URL on the
  configured GitHub host;
- a GitLab group, subgroup, or exact HTTPS project URL on the configured GitLab
  host.

Provider previews are bounded and show forks, archives, visibility, topic and
allow/deny policy exclusions, and repositories already managed under either
their canonical URL or stable provider ID. For private provider discovery,
enter only an environment-variable name such as `GITHUB_TOKEN` or
`GITLAB_TOKEN`. RepoKarta reads the value from its launch environment for the
API request and persists only the reference name. The same value is supplied
ephemerally to HTTPS Git clone and fetch through process-local Git
configuration; it is never embedded in the stored remote URL or command
arguments.

GitHub Enterprise and self-hosted GitLab origins can be enabled with
`-github-host`, `-github-api`, `-gitlab-host`, and `-gitlab-api`, or the
corresponding `REPOKARTA_GITHUB_HOST`, `REPOKARTA_GITHUB_API`,
`REPOKARTA_GITLAB_HOST`, and `REPOKARTA_GITLAB_API` environment variables.
Only the explicitly configured HTTPS origin is accepted for each provider.

Approved GitHub and GitLab repositories are cloned under RepoKarta's own data
directory with executable hooks and recursive submodules disabled. A successful
clone or fetch is verified at an exact Git revision before the normal catalogue
refresh queues commit-pinned indexing. Failed synchronization records an
actionable error and backoff without replacing the last usable index.

Manual **Sync** and explicit-confirmation removal are available per repository.
User-owned local repositories are only unregistered. RepoKarta-owned hosted
checkouts move to recoverable `repository-trash` storage. Enable the single
bounded background synchronization worker with, for example:

```powershell
go run ./cmd/repokarta serve `
  -repository-sync-interval 30m `
  C:\Work\ghorg
```

## Documentation steering

RepoKarta uses a small default plan: `overview`, `architecture`, and
`dependencies`. A reviewed `.repokarta.yml` at the repository root can title
the wiki, include or exclude pages, and add bounded guidance:

```yaml
docs:
  title: Engineering handbook
  include:
    - overview
    - architecture
    - dependencies
  exclude: []
  notes:
    architecture: Emphasize service boundaries and public entry points.
```

Unknown keys and page names are rejected. The configuration is read from the
same recorded commit as the generated pages; RepoKarta never writes it or any
other source file.

## JSON API and MCP

The JSON API is the capability boundary used by non-browser clients:

```text
GET /api/search?q=OpenFile&mode=literal&repo=RepoKarta&lang=Go&limit=100
GET /api/contexts/suggest?kind=repository&q=RepoKarta&limit=12
GET /api/contexts/suggest?kind=symbol&repository_id=1&q=OpenFile&limit=12
POST /api/contexts/resolve
GET /api/symbol?symbol=OpenFile&repo=RepoKarta&lang=Go&limit=100
POST /api/symbol
GET /api/repositories
GET /api/whoami
GET /api/file/{repository}?rev={commit}&path={path}&lines=1-200
GET /api/tree/{repository}?rev={commit}&path={directory}
GET /api/git/log/{repository}?rev={commit}&path={path}&limit=50
GET /api/git/diff/{repository}?from={commit}&to={commit}&path={path}&context=3
GET /api/maps?repository={repository-id}
GET /api/maps/export?repository={repository-id}
GET /api/dependencies?repository={repository-id}&usage=production
POST /api/dependencies/refresh?repository={repository-id}
GET /api/dependencies/progress
GET /api/wiki?repository={repository-id}
POST /api/wiki/generate
GET /api/wiki/{repository-id}/{page}
GET /api/wiki/export?repository={repository-id}
GET /api/conversations?scope=own|all
GET /api/conversations/{conversation-id}
PATCH /api/conversations/{conversation-id}
DELETE /api/conversations/{conversation-id}
```

Conversation list responses include the current viewer, effective scope, and
`can_view_all`. Asking for `scope=all` without administrator access safely
falls back to `own`.

`GET /api/whoami` returns the caller's exact stable RepoKarta identity and
current IdP groups. Use those values verbatim when configuring private
repository grants in the administrator panel.

Search responses always include `returned_files`, `matching_files`,
`estimated_total_files`, `total_files_exact`, `truncated`, `files_skipped`,
`shards_skipped`, and the effective `limit`. A consumer can therefore tell
whether a fleet-wide negative answer is complete.

Git history walks are newest-first and report `truncated`, `output_truncated`,
the effective `limit`, and their byte budget. Diffs return exact resolved
revisions, change counts, a unified patch, and `truncated`, `returned_bytes`,
and `maximum_bytes`. If `to` is omitted the indexed commit is used; if `from`
is omitted its first parent is used. Commits returned by `git_log` can be
passed to `get_file` and `list_tree` for commit-pinned historical evidence.

Open the **MCP** tab or `/mcp/setup` for the current Streamable HTTP endpoint,
masked bearer token, copyable client configuration, and the exact read-only
tool catalog. The page is never cached. Its process-scoped token rotates when
RepoKarta restarts, so refresh the configuration after a restart. Streamable
HTTP exposes all sixteen tools. `list_named_contexts` discovers reusable
personal and administrator-published scopes, while
`resolve_effective_contexts` expands explicit, named, and default contexts with
their provenance and canonical URLs. `find_references` searches persisted,
commit-pinned AST calls, type usages, imports, and heritage relations without
invoking AI. Structural artifacts are prepared in the background after code
indexing; an incomplete API request returns `202 Accepted`, `Retry-After`, and
per-repository progress instead of building inside the request. MCP returns the
same progress and partial-coverage warning in its normal tool result. The
artifact tools return
deterministic repository maps, a focused dependency/version and HTTP-call
inventory, the persisted Deep Wiki page index, and generated page content
without starting an AI run.

For MCP clients that launch a stdio process, keep RepoKarta running and add:

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

The stdio adapter is deliberately thin: it calls the JSON API, exposes the
core source/search/tree/Git tools, and adds no MCP-only code capability. Use
Streamable HTTP when a client also needs repository maps or generated Deep Wiki
pages. RepoKarta's own Codex and Claude harnesses use the complete tool
definitions over its authenticated loopback HTTP MCP endpoint.

## Ask with Codex, Claude, or the Anthropic API

RepoKarta detects local provider CLIs when it starts:

- Codex uses `codex app-server` and your existing `codex login` session.
- Claude uses the Claude Code `stream-json` protocol and your existing
  `claude auth login` session.
- Anthropic API uses the official Go SDK and reads `ANTHROPIC_API_KEY` from the
  environment that launches RepoKarta.

RepoKarta never receives the provider password or subscription credential. It
starts the installed harness as a child process and gives it a temporary bearer
token for RepoKarta's local MCP endpoint. The browser shows whether each
provider is installed and authenticated.

Provider authentication is inherited from the account, environment, and process
tree that launched RepoKarta. Starting it from another agent, `launchd`, a
service, or CI can therefore show Claude or Codex as logged out even when an
interactive terminal is logged in. RepoKarta rechecks authentication when each
new conversation starts and again after a harness startup failure; launch it
from the same session where `codex login` or `claude auth login` succeeds.

The Claude harness runs with `--setting-sources user`. Your personal Claude
settings, including `env`, hooks, permission configuration, and organization
telemetry exporters, therefore apply as they do in a normal terminal session.
RepoKarta does not enable the `project` or `local` setting sources, and starts
Claude from a neutral temporary attachment directory rather than an indexed
repository, so repository `.claude/settings*.json`, project memory, and
auto-memory are not loaded. The grounding system prompt still instructs Claude
to ignore personal memory and use only RepoKarta tool evidence; RepoKarta's
command-line plan mode and disabled mutation, shell, and web tools remain the
authoritative safety boundary.

To enable the direct Anthropic provider without persisting its secret:

```powershell
$env:ANTHROPIC_API_KEY = "..."
go run ./cmd/repokarta serve C:\Work\ghorg
```

RepoKarta never writes that environment value to SQLite, browser storage,
logs, generated content, or repository configuration.

Override CLI discovery when needed:

```powershell
go run ./cmd/repokarta serve `
  -codex-command C:\path\to\codex.exe `
  -claude-command C:\path\to\claude.exe `
  C:\Work\ghorg
```

Model and effort are configured per provider before a conversation starts and
their explicit human-readable names remain visible in the conversation header.
The Claude catalog includes Fable 5, Opus 5, Opus 4.8, Sonnet 5, and Haiku 4.5;
supported effort levels are selected per model. Leaving effort on its default
lets the selected harness choose. RepoKarta passes Codex effort through
app-server turn configuration, Claude Code effort through `--effort`, and
Anthropic API effort through `output_config.effort`. Claude Code and Anthropic
API start on Opus 5 with Medium effort unless you choose another model or
effort. Each turn also has a bounded timeout and, for the direct API loop, an
output-token budget.

Chat accepts permission-aware `@repository`, `@file`, `@directory`, and
`@symbol` chips. File, directory, and symbol autocomplete starts from one
selected repository identity. Directory chips are exact committed Git trees;
symbol chips carry an exact file, declaration kind, and start line and scope
deterministic matches to the resolved declaration range. Ambiguous, incomplete,
invalid, unavailable, unindexed, missing, and stale targets remain visible
errors rather than silently widening the question. Pasting a same-origin
RepoKarta source, map, Wiki, repository, or search URL resolves through
`POST /api/contexts/resolve`.

The folder button beside `@` opens named contexts. A personal context is
private to its author and can be made that author's default. Administrators
can publish shared read-only contexts and administrator defaults for a team,
product, service fleet, or release. Definitions store repository IDs and exact
indexed revisions; a stale, unavailable, or unauthorized member fails closed
instead of reverting to a fleet-wide search. Effective chips show whether they
came from an explicit selection, a named context, a personal default, or an
administrator default. Every chip and named definition has a copyable
permission-checked `/contexts` URL.

The same behavior is available without the UI:

```http
GET /api/contexts/named
POST /api/contexts/resolve
POST /api/contexts/named
PUT /api/contexts/named/{id}
DELETE /api/contexts/named/{id}
```

`POST /api/search`, `POST /api/symbol`, Chat, and the corresponding MCP tools
accept `named_context_ids` and `use_default_contexts`. Omitted default handling
means defaults are applied; set `use_default_contexts` to `false` for an
explicitly unscoped request. Existing simple searches with an explicit legacy
repository selector continue treating that selector as an override.

Conversation titles, messages, citations, status, and usage are stored locally.
Uploaded conversation images are stored as exact RepoKarta-owned files outside
SQLite. Provider processes are intentionally disposable: after an idle period
RepoKarta closes them while retaining the transcript and an opaque provider
resume cursor. Reopening a chat first attempts provider-native resume and
falls back to a bounded replay of the durable transcript when that cursor is
stale.

## Storage and safety

RepoKarta binds to loopback by default and treats repositories as read-only.
It never fetches, checks out, resets, cleans, builds, executes, or removes a
user-owned local repository. The administrator acquisition workflow may clone,
fetch, and fast-forward only a checkout whose exact canonical path is proven to
be under RepoKarta-owned storage. It never pushes upstream, runs repository
hooks, or recursively initializes submodules.

Provider harnesses receive a narrow local MCP surface. Codex is started with a
read-only sandbox and approvals disabled. Claude is started in plan mode with
shell, edit, write, notebook, and web tools disabled. The MCP endpoint requires
a per-process random bearer token.

RepoKarta-owned state is kept outside source repositories:

- Windows: `%LOCALAPPDATA%\RepoKarta\`
- macOS: `~/Library/Caches/RepoKarta/`
- Linux: the operating system user cache directory

SQLite stores catalogue, acquisition provenance and source-free events, job,
and conversation metadata and transcripts.
Conversation images, Zoekt shards, and future generated documentation are
filesystem-backed under RepoKarta's own data directory. Deleting a chat removes
its transcript and its exact owned image files.

The protected administrator page includes a storage inventory and diagnostics
workspace. Cleanup is intentionally narrower than file browsing: select safe
candidates, preview the exact paths and byte count, then confirm the signed
plan. RepoKarta re-inventories the targets immediately before deletion and
fails closed if a path, size, or modification time changed. It never accepts a
path supplied by the browser and never recursively deletes a source directory.

The diagnostics download contains only `manifest.json` and `diagnostics.json`.
It excludes database pages, prompts, source, absolute repository paths, logs,
Wiki text, images, environment variables, cookies, tokens, credentials, and
private keys.

## Validate and package

```sh
npm --prefix web test
npm --prefix web run typecheck
npm --prefix web run build
go test -tags "grammar_subset,grammar_subset_bash,grammar_subset_go,grammar_subset_groovy,grammar_subset_java,grammar_subset_javascript,grammar_subset_kotlin,grammar_subset_python,grammar_subset_sql,grammar_subset_tsx,grammar_subset_typescript" ./...
```

Native build scripts write the executable to `dist/` and include required
third-party license texts under `dist/licenses/`:

```powershell
.\scripts\build.ps1
```

```sh
./scripts/build.sh
```

## Releases and package verification

Pushing a semantic-version tag such as `v0.39.0` runs the release matrix for
Windows amd64 and Apple Silicon macOS. The workflow:

1. validates the frontend and Go application;
2. injects the tag version into the packaged binary;
3. includes the README and every required third-party license;
4. smoke-tests the packaged executable and Zoekt license;
5. creates a ZIP for Windows and a `tar.gz` for macOS;
6. verifies per-package SHA-256 files and publishes a combined `SHA256SUMS`;
7. creates the GitHub release only after both native packages pass.

Run the same packagers locally:

```powershell
.\scripts\package-release.ps1 -Version 0.54.0-dev
```

```sh
./scripts/package-release.sh 0.54.0-dev
```

macOS packages are signed with the hardened runtime when
`REPOKARTA_CODESIGN_IDENTITY` is configured. A release is also submitted to
Apple notary service when `APPLE_ID`, `APPLE_TEAM_ID`, and
`APPLE_APP_PASSWORD` are supplied together. The GitHub workflow imports the
base64 PKCS#12 certificate from `MACOS_CERTIFICATE_P12` using
`MACOS_CERTIFICATE_PASSWORD`; the signing identity name comes from
`MACOS_CODESIGN_IDENTITY`. Without those secrets CI still proves the unsigned
package layout, version, checksum, and licenses.

The repository publishes a HEAD formula at
[`Formula/repokarta.rb`](./Formula/repokarta.rb). Install and test it with:

```sh
brew tap spolnik/repokarta https://github.com/spolnik/RepoKarta.git
brew install --HEAD --build-from-source spolnik/repokarta/repokarta
brew test spolnik/repokarta/repokarta
```

Main-branch CI performs the same Homebrew installation on macOS. The formula
builds from source, installs the native executable, and preserves the packaged
third-party license directory.

The pinned Zoekt revision and Windows portability decision are documented in
[docs/zoekt-windows-portability.md](./docs/zoekt-windows-portability.md).
