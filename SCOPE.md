# RepoKarta scope

## Product statement

RepoKarta is a local-first workspace for understanding a collection of Git
repositories from existing local roots, including ghorg directories, and from
explicitly approved GitHub or GitLab acquisitions.

It combines four capabilities:

1. Fast cross-repository source search powered by Zoekt.
2. Grounded questions and answers about code, with file-and-line citations.
3. Interactive architecture and dependency maps.
4. Commit-aware, automatically generated repository documentation.

RepoKarta binds to `127.0.0.1` by default and can be configured for an
authenticated shared deployment. User-owned source repositories remain
read-only. RepoKarta may clone and synchronize only checkouts that it owns, and
stores indexes, metadata, generated documents, and settings outside source
repositories.

The intended users are individual developers and engineering teams working with
private repositories on managed Windows, Linux, and Apple Silicon macOS
systems.

## Product principles

- **Local first:** repository contents and indexes remain in the
  operator-controlled RepoKarta deployment; there is no hosted control plane.
- **One command:** pointing RepoKarta at a ghorg directory should be enough to
  discover and index its repositories.
- **Search is foundational:** deterministic code search should work without an
  AI provider or API key.
- **Evidence over confidence:** AI answers, diagrams, and documentation must
  link to the exact source files and revisions that support them.
- **Read-only by default:** RepoKarta explains code; it does not edit
  user-owned source repositories or execute repository code.
- **Incremental work:** unchanged repositories, files, and documentation pages
  should not be recomputed.
- **Native and portable:** Windows amd64, macOS arm64, and Linux amd64/arm64
  are first-class release targets.
- **Replaceable integrations:** Zoekt and AI providers are hidden behind
  RepoKarta-owned interfaces.

## Core user journey

1. The user downloads or builds the RepoKarta executable.
2. The user runs `repokarta serve <repository-root>`.
3. RepoKarta discovers all Git repositories below that root.
4. RepoKarta incrementally indexes the locally configured default revision of
   each repository, falling back to the current checkout when no remote default
   is available.
5. The browser opens a local dashboard showing indexing state.
6. The user searches all repositories or limits a query by repository,
   language, path, or file.
7. The user opens a result in a syntax-highlighted source viewer.
8. If a supported local provider harness is authenticated, the user can ask a
   question across the indexed repositories and receive an answer with
   commit-pinned source citations.
9. The user can generate and browse a repository wiki and architecture map.
10. When repositories change, RepoKarta refreshes affected indexes and marks
    derived documentation stale.

## In scope

### Repository catalogue

- Discover regular and bare Git repositories beneath one or more local roots.
- Work naturally with the directory layout produced by ghorg.
- Record repository path, display name, origin URL, default revision, HEAD
  commit, scan state, index state, and timestamps.
- Treat a repository with no commits as an explicit terminal empty state, show
  the reason, and exclude it from pending work and readiness denominators.
- Allow repositories and directories to be excluded.
- Skip hidden directories such as `.git` internals and `.jobguard-wt`
  worktrees by default.
- Respect `.gitignore` rules while discovering repositories so vendored
  copies, build output, and dependency caches are never registered as
  repositories.
- Disambiguate repositories that share a name in every picker using a stable
  path or identity suffix.
- Never fetch, pull, checkout, reset, clean, or otherwise modify user-owned
  repositories. Remote synchronization is limited to RepoKarta-owned
  acquisition checkouts.
- Detect committed updates through a bounded filesystem watcher over Git
  metadata, with manual refresh retained as an explicit recovery control.

### Code search

- Use Zoekt as the search engine and pin the exact upstream revision.
- Wrap Zoekt behind internal RepoKarta interfaces because Zoekt does not publish
  a stable v1 module.
- Maintain the small, reviewable RepoKarta Windows portability delta over the
  pinned upstream Zoekt revision. Keep the exact upstream commit, modification
  notices, Apache-2.0 license, and packaged attribution synchronized.
- Store Zoekt shards outside source repositories.
- Incrementally index the immutable commit resolved from a configured
  `origin/HEAD` even when it differs from the checkout, without switching or
  mutating the worktree; fall back to current `HEAD` when unavailable.
- Support substring, regular-expression, boolean, repository, language, path,
  file, and symbol queries exposed by Zoekt.
- Return bounded result sets with explicit completeness metadata and stream
  large server-rendered result prefixes incrementally to the browser.
- Accept an explicit bounded result limit and report returned files, matching
  files, skipped candidates/shards, truncation, and whether totals are exact.
- Never turn an unavailable search capability into a silent empty result.
- Show repository, revision, path, line range, context, and match highlights.
- Treat Universal Ctags as an optional enhancement for symbol ranking rather
  than a runtime requirement. Probe it at startup, rebuild indexes when its
  availability changes, and warn on `sym:` queries when it is unavailable.
- Fall back to a RepoKarta-owned Git CLI shadow repository when go-git cannot
  open a repository format supported by the installed Git runtime.

### Source browser

- Repository and directory tree.
- Syntax-highlighted file viewer.
- Embed repository-scoped code search and symbol-usage discovery directly in
  the file viewer. Selecting an identifier prepares syntax-backed or
  compiler-precise reference search without making the user reselect the
  repository scope.
- On files with detected HTTP routes, show commit-pinned endpoints and inbound
  service callers from the distributed topology. Distinguish an exact matching
  route-path witness from a service-level caller edge, and preserve partial or
  still-building artifact states.
- Stable URLs containing repository, revision, path, and line range.
- Copyable citations.
- Links to the configured remote origin when one exists.
- Safe handling of binary files, generated files, very large files, and
  unsupported encodings.

### Questions about code

- Initial provider harnesses:
  - Codex app-server using the user's existing Codex/ChatGPT login.
  - Claude Code stream-json using the user's existing Claude login.
- Treat Codex and Claude as harness adapters behind one RepoKarta-owned Go
  interface; do not pretend their agent protocols are interchangeable model
  APIs.
- Start provider harnesses locally and let them own authentication. Do not
  collect, proxy, store, or reproduce ChatGPT or Claude subscription
  credentials.
- Keep direct API-key and local-model providers possible behind the same
  conversation interface.
- Implement a read-only agent loop with narrowly scoped tools:
  - `list_repositories`
  - `search_code`
  - `get_file`
  - `list_tree`
  - `find_symbol`
  - `find_references`
  - `search_ast`
  - `git_log`
  - `git_diff`
  - `read_repository_map`
  - `read_code_reachability`
  - `read_dependency_inventory`
  - `read_system_topology`
  - `list_deep_wiki_pages`
  - `read_generated_document`
- Stream answer progress and citations to the browser.
- Make repository scope and estimated/actual usage visible.
- Expose only a hardcoded, provider-specific model catalog with human-readable
  labels, and reject any model outside it in the backend. Free-text model entry
  is not supported.
- Bound turn controls to a 5-minute minimum, 30-minute default, and 60-minute
  maximum, and show the selected timeout next to elapsed time.
- Show the output budget only for providers that map it to a real request
  limit, and explain in the interface that it is a ceiling rather than a target
  or a context-window limit.
- Select repositories by the stable numeric repository ID in JSON endpoints.
  Every repository-aware MCP tool also accepts an exact repository name for
  agent ergonomics; ambiguous names fail with the matching numeric IDs instead
  of guessing. Cross-repository reads keep both selectors optional.
- Run provider harnesses outside every indexed repository. Claude loads only
  user-scoped operational settings (including hooks, environment, and
  telemetry), never project/local settings; grounding instructions disable
  personal and project memory so answers rest only on RepoKarta tool evidence.
- Persist titled conversations locally by default, keep provider credentials
  out of that storage, bind each conversation to its authenticated author, and
  provide explicit per-conversation deletion.
- Keep an AI-provider interface so additional hosted or local models can be
  added later.
- Keep the default orchestration Go-native. The Claude Agent SDK may remain an
  optional future adapter, but normal runtime must not require Node.js or
  Python.
- Serve the same authenticated, loopback-only MCP tools to every provider.
- Keep a JSON API as the protocol-independent capability layer and MCP as a
  thin adapter over the same Go service or that API. Do not add MCP-only code
  capabilities.
- Run Codex read-only with approvals disabled. Run Claude in plan mode with
  shell, mutation, and web tools disabled.

### Architecture visualization

- Derive factual nodes and edges from repository structure before asking an LLM
  to describe them.
- Initial graph inputs:
  - Git repository boundaries
  - language and manifest detection
  - package/workspace manifests
  - imports and dependencies
  - executable entry points
  - HTTP routes where reliably detectable
  - databases and external services where reliably detectable
- Build a bounded, language-neutral structural index from pure-Go Tree-sitter
  grammars for Java, Kotlin, Gradle Groovy, TypeScript/TSX, JavaScript, Go,
  SQL, Bash, and Python. Record declarations, imports, inheritance, type usages,
  call sites, Gradle DSL calls, exact byte/line ranges, parser diagnostics, and
  confidence without executing repository code.
- Expose bounded Java and Go Tree-sitter queries through the shared JSON and
  MCP capability layer. Persist per-document node-kind inventories for
  conservative candidate pruning, preserve named captures and structural/text
  predicates, bind opaque cursors to the artifact and query, and report
  pagination separately from artifact and matcher completeness.
- Derive revision-pinned static reachability from persisted structure using
  executable and framework entry roots, call/type/injection/implementation
  edges, and evidence-backed witness paths. Report reachable,
  probably-unreachable, and unknown states only after exposing artifact,
  document, ambiguity, and runtime completeness; never claim a dead-code proof
  across reflection, profiles, qualifiers, generated code, callbacks, or
  external registration.
- Feed a curated subset of parsed types, functions, and build facts into the
  Deep Wiki survey starting point. Keep the full syntax inventory separate from
  visual graph nodes so generic call sites do not create a graph hairball.
- Read Gradle builds in both Groovy and Kotlin syntax: string coordinates,
  Groovy map and Kotlin named `group`/`name`/`version` arguments,
  `platform`/`enforcedPlatform` BOM imports, `project(...)` dependencies, and
  `libs.*` version-catalog accessors resolved through `libs.versions.toml`.
  Resolve interpolated versions from `gradle.properties` and common literal
  Groovy/Kotlin assignments; record unresolved versions without inventing a
  literal value.
- Read Spring routes from `@RequestMapping`, the HTTP-method mapping
  annotations, class-level path prefixes, `@HttpExchange` HTTP interfaces, and
  `RouterFunctions` predicates.
- Derive repository-to-repository `service_call` edges from `@FeignClient`
  targets and from `http://`, `https://`, and `lb://` hosts found in files that
  actually build a client (Feign, WebClient, RestClient, RestTemplate, or an
  HTTP interface proxy), plus outbound base URLs in main Spring application
  configuration. Prefer production configuration and `src/main` evidence;
  retain test-only edges only as explicit low-confidence facts. Only targets
  that resolve to a discovered repository, Gradle `rootProject.name`, or
  `spring.application.name` become legacy map edges; unresolved variables and
  infrastructure hosts are dropped from that repository-map layer.
- Maintain a separate distributed-system topology over deployable components,
  not package/source-file nodes. Preserve directed HTTP, gRPC, Kafka,
  database, MCP, and explicitly declared relationships with protocol,
  interaction, transport, confidence, source/runtime origin, peer-resolution
  state, and bounded evidence.
- Reconcile component aliases across repositories and monorepo application
  roots without resolving queues or databases to same-named services. Preserve
  unresolved service peers and inferred external resources visibly instead of
  inventing ownership.
- Provide repository, package, component, and dependency views.
- Use a small TypeScript visualization island within the otherwise
  server-rendered HTMX application.
- Support filtering, focusing, expanding neighbors, following a dependency
  chain, and opening supporting source.
- Prefer legible curated layers over an unfiltered graph containing every
  symbol.
- Default the map to a single repository. The cross-repository view is an
  explicit choice, is bounded to a fixed number of repositories, and returns
  explicit scope metadata with total, analyzed, omitted, limit, and completeness
  rather than appearing fleet-complete.

### Living documentation

- Generate Markdown documentation tied to a repository commit.
- Use an isolated, read-only provider session to inspect implementation and
  tests, then plan a bounded, primarily flat knowledge hierarchy.
- Require an explicit curated model. Preserve the Codex profile effort floor;
  expose all supported Claude effort levels per model, including provider
  default for models without configurable effort.
- Checkpoint a bounded repository survey to `survey.md`, then atomically save
  the hierarchy before generating pages one at a time.
- Reuse that survey and its exact source URLs as the primary evidence pack for
  every page instead of repeating repository-wide discovery.
- Keep provider plans to five through twelve focused pages, or three through
  six for compact repositories. Keep Architecture Overview short and
  navigational; use child pages only when a focused flow cannot fit its parent.
- Keep deterministic structural planning as the provider-free fallback.
- Generate pages independently so individual pages can be retried and refreshed.
- Require source citations for architectural and behavioral claims.
- Produce Mermaid diagrams when the evidence supports them, render them safely
  in the Wiki, and allow full-screen zoom and SVG download.
- Store generation status, source revision, supporting files, citations,
  provider, model, token usage, and timestamps in a filesystem manifest.
- Persist every generated Wiki page as a Markdown file outside SQLite and
  outside the source repository.
- Compare Git revisions to identify pages affected by a change.
- Mark affected pages stale before regenerating them.
- Allow repository-specific steering through a reviewed configuration file,
  tentatively `.repokarta.yml`.
- Export generated documentation without requiring the RepoKarta UI.

### Local operations

- Bind to `127.0.0.1` by default.
- Store metadata in SQLite by default using a pure-Go driver. Allow an
  operator-selected PostgreSQL 18+ backend for shared deployments without
  changing application persistence logic.
- Store large Zoekt shards and generated artifacts on the filesystem rather
  than in the relational metadata database.
- Use WAL mode and forward migrations for SQLite. Maintain PostgreSQL-specific
  migrations at the same schema version where dialects differ.
- Provide a one-way, atomic, row-count-verified SQLite-to-PostgreSQL migration
  that requires an empty destination and preserves filesystem artifacts.
- Expose indexing and generation progress through Server-Sent Events.
- Include health and diagnostic endpoints without exposing secrets.
- Provide clear storage usage and cleanup controls.
- Support graceful shutdown and recovery of interrupted jobs.

### Packaging

- Embed production Vite assets into the Go executable.
- Build at least:
  - `windows/amd64`
  - `darwin/arm64`
- Add `linux/amd64` and `linux/arm64` when inexpensive.
- End users should not require Node.js, Python, Docker, PostgreSQL, or Redis.
- Universal Ctags remains optional.
- Provide checksums for releases.
- Add macOS signing, notarization, and a Homebrew formula after the product
  stabilizes.

## Enduring non-goals and boundaries

- A hosted SaaS or cloud control plane.
- Mutation of user-owned local repositories. Fetching and synchronization are
  restricted to RepoKarta-owned acquisition checkouts; repository hooks and
  repository code are never executed.
- Compiler-precise Sourcegraph or SCIP parity for every language. RepoKarta
  combines deterministic structural analysis with optional Java SCIP indexes,
  reports incomplete precision explicitly, and does not claim framework
  reachability or dead-code proof.
- General hosted-forge coverage beyond the explicitly configured GitHub and
  GitLab acquisition adapters.
- Searching every historical commit or every branch.
- Editing code, applying patches, executing builds, or running arbitrary shell
  commands through the AI.
- A general-purpose IDE.
- Automatic publication of generated documentation.
- Mobile support.
- Vector embeddings as the primary retrieval mechanism.
- Telemetry enabled by default.

## Technical architecture

```text
Local Git repositories (read-only)
              |
              v
     Repository catalogue
       |              |
       v              v
 Zoekt indexer    Structural analyzers
       |              |
       v              v
 Zoekt shards    Architecture facts
       |              |
       +-------+------+
               |
               v
          Go services
      search / files / git
       AI / docs / graphs
               |
               v
       Go HTTP + HTMX UI
        + graph JS island
```

### Proposed repository layout

```text
cmd/repokarta/          CLI executable
internal/app/           startup and lifecycle
internal/catalog/       repository discovery and metadata
internal/store/         relational metadata backends and migrations
internal/search/        RepoKarta search interfaces
internal/search/zoekt/  Zoekt adapter
internal/source/        safe file and Git access
internal/ai/            provider and agent abstractions
internal/docs/          documentation pipeline
internal/graph/         structural facts and graph construction
internal/httpserver/    HTTP handlers, view models, SSE
web/templates/          Go HTML templates
web/src/                Vite, HTMX, Tailwind, graph TypeScript
scripts/                developer build scripts
```

The layout should grow only as working capabilities are implemented. Empty
architectural packages should not be created merely to mirror this diagram.

## Data ownership and storage

Default storage:

```text
Windows:
%LOCALAPPDATA%\RepoKarta\
  repokarta.db
  indexes\
  docs\
  logs\

macOS:
~/Library/Caches/RepoKarta/
  repokarta.db
  indexes/
  docs/
  logs/
```

The selected relational metadata database owns:

- repository catalogue;
- scan and job state;
- document metadata and citations;
- titled conversation metadata and transcripts;
- configuration that is not secret;
- schema version.

The filesystem owns:

- Zoekt shards;
- generated Markdown;
- graph snapshots and exports;
- temporary job artifacts.

SQLite is the zero-configuration default. PostgreSQL 18+ is optional for a
shared deployment and uses the same Store behavior through a backend-neutral
boundary. Search indexes, generated documents, maps, conversation attachments,
and other large artifacts remain in the data directory for both backends.

API keys must come from environment variables, an operating-system credential
store, or an explicit runtime prompt. They must never be written to SQLite,
PostgreSQL, logs, generated documents, repository configuration, or browser
local storage.

## Security boundary

- The HTTP listener defaults to loopback and rejects unexpected Host/Origin
  values.
- Frontend installs use a committed npm lockfile with integrity metadata.
  Fresh dependency resolution waits at least 14 days after publication, saves
  new direct dependencies exactly, rejects unbounded direct specifications,
  and requires explicit approval for guarded major-version lines.
- All repository paths are canonicalized and checked against configured roots.
- Symlinks and junctions must not escape configured roots silently.
- Ignore `.git` internals, secrets, credentials, dependency caches, build
  outputs, and user-configured patterns during AI context collection.
- Search indexing and AI context inclusion are separate policy decisions.
- Chat, Wiki, and retrieval AI tools are read-only and enforce file-size,
  result-count, and time limits. The separately authorized Code workspace may
  write only inside a RepoKarta-owned isolated worktree.
- Repository content is untrusted input and cannot redefine RepoKarta's agent
  instructions or tool permissions.
- Destructive cleanup actions must identify exact RepoKarta-owned paths.

## Milestones

### M0: portable skeleton

- [x] Go executable with `serve` command.
- [x] Pure-Go SQLite database and initial schema.
- [x] Read-only local Git worktree discovery.
- [x] Embedded Vite, Tailwind, and HTMX page.
- [x] Health endpoint and graceful shutdown.
- [x] Windows build script.
- [x] macOS/Linux build script.
- [x] Continuous integration on Windows, macOS, and Linux.

Exit condition: a fresh checkout can build a native executable, point it at a
directory, and display the discovered repositories.

### M1: useful local code search

- [x] Complete the Zoekt Windows portability spike and record the integration
  decision.
- [x] RepoKarta-owned search and indexing interfaces.
- [x] Pinned Zoekt adapter.
- [x] Incremental indexing jobs and visible status.
- [x] Search query endpoint with cancellation and limits.
- [x] Search page with repository, language, path, and file filtering.
- [x] Source viewer and stable citations.
- [x] Optional Universal Ctags detection.

Exit condition: cross-repository search is fast and useful without AI.

### M2: grounded code questions

- [x] Anthropic API configuration without secret persistence.
- [x] Read-only Go agent loop.
- [x] Search, file, tree, symbol, log, and diff tools.
- [x] Streamed UI with file-and-line citations.
- [x] Persistent titled conversations with native resume and transcript replay.
- [x] Usage, cancellation, timeout, and budget controls.
- [x] Adversarial tests for prompt injection from repository content.

Exit condition: answers about multiple repositories are useful, traceable to
source, and cannot modify or execute repository content.

### M3: repository maps

- [x] Language and manifest inventory.
- [x] Package and dependency graph extraction.
- [x] Layered interactive graph.
- [x] Evidence panel for every generated node and edge.
- [x] Graph snapshot and export.

Exit condition: the map explains the repository at a useful level rather than
rendering an unreadable file-level hairball.

### M4: living documentation

- [x] Repository-specific, provider-grounded hierarchical knowledge planning.
- [x] Survey, plan, and page checkpoints with resumable page-by-page progress,
  elapsed time, timeout visibility, and cancellation.
- [x] Curated top-tier Wiki model presets with high-or-stronger effort.
- [x] One standard-quality Wiki pipeline for every provider; reduced-depth
  Fast generation is disabled in both the interface and backend.
- [x] Bounded five-to-twelve-page plans (three to six for compact repositories)
  with short architecture and glossary pages.
- [x] Survey-grounded page turns with page-specific writing, tool-call, and
  output budgets.
- [x] Filesystem-only Markdown and manifest persistence outside SQLite.
- [x] Source citations and Mermaid validation.
- [x] Three-pane Deep Wiki reader with hierarchy, filtering, page outline, and
  relevant source files.
- [x] Editorial Wiki page typography, reading metadata, visible section rhythm,
  and generation quality gates that reject wall-of-text prose.
- [x] Full-screen Mermaid zoom and SVG download.
- [x] Commit-aware staleness and selective regeneration.
- [x] Administrator-selected batch generation with shared quality controls,
  durable per-repository checkpoints, continued processing after individual
  failures, and visible repository outcomes.
- [x] `.repokarta.yml` steering.
- [x] Markdown export.
- [x] Hardcoded per-harness model catalogs with human-readable labels and
  backend rejection of any other model.
- [x] Five-minute minimum, thirty-minute default, and sixty-minute maximum
  checkpoint timeouts taken from the interface, with the chosen timeout shown
  beside elapsed time.
- [x] Output budget limited to providers that map it to a request limit, with
  an in-product explanation.
- [x] Neutral harness grounding: provider processes run outside every indexed
  repository; Claude keeps user operational settings while project/local
  settings and personal/project memory remain excluded.
- [x] Gradle dependency, Spring route, and inter-service HTTP extraction with
  exact revision, path, and line evidence.

Exit condition: generated documentation explains real subsystems, flows,
types, state, failure paths, and tests with commit-pinned evidence, while
remaining trustworthy and economical to refresh after normal repository
changes.

### M5: distributable local product

- [x] Release matrix and checksums.
- [x] Storage and cleanup UI with bounded inventory, exact-target dry runs,
  confirmation, stale-plan rejection, and source-path protection.
- [x] Diagnostics export with an explicit manifest, secret redaction, and
  source/prompt/database/log omission.
- [x] Windows amd64 packaging with version and bundled-license smoke checks.
- [x] macOS arm64 packaging with optional Developer ID signing and notarization.
- [x] Linux amd64 and arm64 packaging with native-runner boot smoke tests.
- [x] Source-build Homebrew formula with CI installation and license checks.
- [x] Upgrade and database/artifact migration tests, including idempotent retry
  and unsupported-future-format rejection.

### M6: shared deployment

The executable now has a tested shared-deployment authentication boundary while
remaining loopback-only by default. Non-secret provider settings persist in
the selected metadata database; bootstrap administrator credentials and SAML
private keys do not.

- [x] Configurable bind address beyond `127.0.0.1`.
- [x] Four explicit access modes: loopback-only local, Cloudflare Access JWT,
  native SAML SP, and startup-gated unauthenticated shared access.
- [x] Startup-credential-protected administration for authentication settings.
- [x] Per-user ownership and access enforcement for durable conversations,
  with strict owner-only list, read, continue, rename, delete, and interrupt
  semantics; administrator status is not a transcript-access bypass.
- [x] Per-user separation of generated artifacts on a shared instance.
- [x] Shared or team deployment packaging and operations.

Repository authorization is deny-by-default on shared instances. Each
repository has a private owner plus explicit user and identity-provider group
grants, or an explicit instance-shared scope. Source, search, maps, Wiki
plans/pages, dependency facts, exports, conversation-scoped MCP reads, and
repository pickers enforce the same policy. Existing and newly discovered
repositories default to private `local:admin`, while loopback-local behavior
remains unchanged. Release archives carry service templates and a shared
deployment runbook covering TLS, identity bootstrap, backup, upgrades,
rollback, authorization smoke tests, and deprovisioning.

Exit condition: two users sharing an instance cannot discover or retrieve one
another's repositories or derived artifacts unless an explicit user, group, or
shared scope allows it, and the release bundle contains the operational
material needed to deploy and recover the service.

### M7: advanced search and deep exploration

Extend deterministic code search with structured context, reusable scopes,
graph-aware navigation, and an optional agentic Deep Search experience. Normal
search must remain fast and AI-free.

#### Structured mentions and scopes

- [x] Add permission-aware Chat autocomplete for `@repository` and `@file`,
  including keyboard navigation, bounded suggestions, and useful matching
  across names containing spaces.
- [x] Extend permission-aware autocomplete to `@directory` and `@symbol`, and
  provide repository, file, directory, and symbol parity in the Deep Search
  composer.
- [x] Render repository and file mentions as removable context chips and
  transmit stable repository IDs, indexed revisions, and exact paths; never
  parse display labels back out of prompt text.
- [x] Extend the same structured chip contract to directory and symbol
  identities.
- [x] Resolve repository and file mentions against the commit-pinned catalogue
  used by search. Show invalid, missing, unauthorized, unindexed, and stale
  contexts as actionable errors instead of silently widening the scope.
- [x] Extend resolution to directory and symbol contexts, including explicit
  ambiguous-identity errors.
- [x] Share the typed repository/file context contract across Chat,
  `POST /api/search`, the JSON client, and MCP; persist resolved contexts with
  user turns and replay them without reconstructing identities from message
  text.
- [x] Support pasting a RepoKarta source, map, Wiki, repository, or search URL
  into a composer and converting it into an equivalent structured context chip.
- [x] Add named search contexts representing repositories and revisions for a
  team, product, service fleet, release, or personal task, with personal and
  administrator-managed defaults.
- [x] Make every effective context visible, URL-addressable, copyable, and
  reusable through the JSON API and MCP surface.

#### Query and result experience

- [x] Add one documented query grammar with autocomplete for repository,
  revision, language, path, filename, symbol kind, result type, ownership, and
  positive or negative filters while retaining the existing simple form.
- [x] Provide explicit result types for content, file path, repository, symbol
  definition, reference, implementation, dependency, route, commit, diff,
  generated Wiki page, and captured code insight.
- [x] Add qualified programming-element search by package, type, method, member,
  and full name, with exact, prefix, case-sensitive, and case-insensitive modes.
- [x] Add result facets and “search within these results” refinement without
  discarding the original query, scope, revision, or completeness metadata.
- [x] Rank exact symbol and path matches ahead of fuzzy content matches and
  explain important ranking signals; never let personalization hide complete
  deterministic results.
- [x] Add one-click actions from a result to open source, focus the element in
  Maps, inspect dependencies, find references or implementations, start a
  scoped conversation, or add the result to the current context.
- [x] Import optional SCIP indexes for compiler-accurate cross-repository
  definitions, references, implementations, type signatures, and hover
  documentation while retaining syntax-backed results and confidence labels
  when precise data is unavailable.
  - [x] Import bounded standard `index.scip` protobufs into RepoKarta-owned
    artifacts bound to an exact repository and indexed commit.
  - [x] Prefer compiler-resolved reference occurrences when the requested
    repository scope has complete, current SCIP coverage and the symbol
    identity is exact or unambiguous; otherwise retain labeled Tree-sitter
    fallback.
  - [x] Discover or explicitly configure an external `scip-java`, generate
    exact-commit Gradle indexes in a bounded background queue, persist visible
    per-repository status, and support retry without blocking normal indexing.
  - [x] Classify Java SCIP failures as environment, JDK/wrapper compatibility,
    or compilation failures, and select a compatible per-repository Gradle JVM
    from exact-commit toolchain/wrapper metadata and configured local JDKs.
  - [x] Add precise definitions, implementations, signatures, hover
    documentation, dependency indexes, and cross-repository symbol stitching.
- [x] Add graph queries for upstream and downstream impact, shortest evidenced
  connection paths between two repositories, files, or symbols, and bounded
  traversal by relation type and depth.
- [x] Search commits and diffs by author, message, path, added or removed text,
  date range, branch, and revision range without pretending unindexed history is
  part of the default-branch content index.
- [x] Ingest CODEOWNERS as commit-pinned metadata for owner display and filters,
  including explicit owned, unowned, unresolved-owner, and unavailable states.

#### Saved searches, monitoring, and Deep Search

- [x] Persist per-author recent and saved searches with title, query, context,
  filters, result type, and revision policy; allow administrators to publish
  shared read-only search templates.
- [x] Turn a saved deterministic query into a monitor that reports newly added
  or removed matches between comparable indexed revisions, with bounded history
  and explicit notification-delivery status.
- [x] Add an optional Deep Search mode that answers natural-language questions
  through the existing read-only search, symbol, reference, file, tree, Git,
  map, dependency, and Wiki tools rather than introducing an ungrounded
  retrieval path.
- [x] Stream a concise exploration trace showing current stage, searches
  executed, files read, contexts used, elapsed time, coverage warnings, and
  sources; do not expose hidden model reasoning.
- [x] Preserve structured mentions and named contexts across follow-up
  questions, while allowing the user to add, remove, or replace scope without
  restarting the conversation.
- [x] Provide cancellation, retry from persisted deterministic evidence,
  time/token/tool-call budgets, and a visible “search more broadly” action when
  the first bounded exploration is incomplete.
- [x] Make every Deep Search answer addressable by URL and shareable only after
  rechecking repository permissions for every viewer; never expose cited source
  through a shared answer when that viewer cannot access it.
- [x] Keep optional semantic retrieval or reranking clearly labeled and bounded.
  Literal, regex, symbol, AST, SCIP, and graph evidence remain authoritative,
  and semantic similarity must not turn a partial negative answer into a
  complete one.

Initial delivery order:

1. Structured `@repository` and `@file` mentions with context chips.
2. Named search contexts, typed result categories, facets, and result actions.
3. Qualified element search plus graph connection and impact queries.
4. Optional SCIP precision and commit/diff/ownership search.
5. Saved searches and deterministic monitors.
6. Agentic Deep Search with transparent trace, budgets, follow-ups, and
   permission-safe sharing.

Exit condition: users can precisely scope, refine, save, navigate, and monitor
deterministic searches, then optionally ask a deeper natural-language question
whose complete exploration path, sources, limits, and permissions remain
visible and enforceable.

### M8: code insights and monitoring

Add a commit-aware Code Insights workspace for test coverage, static-analysis
findings, maintainability signals, and quality trends without silently running
repository build scripts.

- [x] Define a normalized observation model for metrics and findings containing
  repository, revision, branch, tool, rule or metric key, severity, file and
  line evidence where available, observed timestamp, source run, and ingestion
  confidence.
- [x] Import coverage from established report formats before adding
  language-specific execution: JaCoCo XML, LCOV, and Cobertura XML, with
  explicit line, branch, aggregate, skipped-file, and parse-error states.
- [x] Import static-analysis findings through SARIF 2.1.0 as the primary
  tool-neutral interchange, preserving rule metadata, severity, fingerprints,
  locations, suppressions, and code-flow evidence when supplied.
- [x] Add an optional read-only SonarQube Community Build adapter over its Web
  API for project measures, issues, quality-gate status, coverage, complexity,
  duplication, reliability, security, and maintainability metrics.
- [x] Keep SonarQube externally managed rather than embedding its server,
  database, or scanner in RepoKarta; store only connection configuration,
  project mappings, redacted credential references, and normalized observations.
- [x] Evaluate optional Semgrep Community Edition and MegaLinter ingestion via
  SARIF or JSON, recording exact engine, rule-pack, configuration, and license
  provenance instead of presenting every finding as RepoKarta-native analysis.
- [x] Derive only safe deterministic metrics directly from RepoKarta's committed
  syntax data, such as code size and bounded complexity indicators; label these
  separately from externally measured coverage and scanner findings.
- [x] Map every imported report to the exact Git revision it analyzed. Reject,
  quarantine, or visibly mark reports whose revision or paths cannot be
  reconciled with the indexed snapshot.
- [x] Show fleet, repository, branch, directory, file, language, tool, rule,
  severity, ownership, and time filters with drill-down to commit-pinned source.
- [x] Track current values and history separately, including new-code versus
  overall coverage, introduced versus resolved findings, quality-gate changes,
  and regressions between comparable revisions.
- [x] Support administrator-defined advisory thresholds for coverage,
  reliability, security, maintainability, duplication, and unresolved findings;
  never claim RepoKarta enforced a CI gate unless the originating system proves
  that outcome.
- [x] Ingest trusted CI artifacts, configured scanner APIs, or explicitly
  uploaded reports by default. Any local scanner or test execution must be a
  separately enabled, sandboxed, resource-bounded policy with cancellation and
  no repository mutation.
- [x] Poll external systems with bounded concurrency, backoff, credential
  rotation support, retention controls, and explicit stale, partial, unavailable,
  and rate-limited states.
- [x] Expose normalized, already-computed insights through the read-only API and
  MCP surface without invoking AI; AI may explain selected evidence but must not
  manufacture missing measurements.

Initial integration order:

1. Generic SARIF and coverage-report ingestion.
2. SonarQube Community Build Web API.
3. Optional Semgrep Community Edition and MegaLinter report adapters.
4. Explicitly sandboxed local execution only if imported CI evidence proves
   insufficient.

Exit condition: a user can compare code quality and coverage across repositories
and revisions, distinguish measured facts from derived indicators, open exact
source evidence, and see incomplete or stale monitoring without RepoKarta
executing untrusted project code by default.

### M9: dependency management

Add a read-only Dependency Management workspace over the dependencies already
captured from committed manifests and build files.

- [x] Maintain a normalized dependency inventory with repository, manifest,
  ecosystem, package coordinate, declared version or constraint, resolution
  confidence, revision, path, and line evidence.
- [x] Resolve versions from supported build indirection where source proves the
  value; show unresolved variables and constraints honestly instead of treating
  them as concrete versions.
- [x] Query the appropriate public package registry for the latest available
  stable version, including at least Maven Central/Gradle, npm, Go modules,
  Cargo, and PyPI as their manifest extractors mature.
- [x] Compare declared and latest stable versions and expose clear states such
  as current, behind, ahead/prerelease, unavailable, private/internal,
  unresolved, and registry error.
- [x] Display version discrepancies fleet-wide and per repository, with filters
  by ecosystem, package, repository, severity of version distance, and check
  status.
- [x] Record the registry source and observation timestamp separately from the
  commit-pinned declaration evidence so a freshness result never appears to be
  a historical source fact.
- [x] Cache registry responses, honor rate limits and offline operation, bound
  refresh concurrency, and surface partial or stale results explicitly.
- [x] Do not send private/internal package names to public registries unless an
  administrator explicitly configures that ecosystem and registry as safe.
- [x] Keep the feature advisory and read-only: RepoKarta may explain an upgrade
  discrepancy, but it must not rewrite manifests, lockfiles, or repositories.
- [x] Join exact resolved versions, or lower-confidence exact declared versions,
  to a locally persisted, timestamped and content-versioned OSV.dev snapshot.
  Evaluate affected ranges with ecosystem-correct ordering, including Maven
  qualifiers, and preserve usage, scope, resolution, revision, and both source
  and advisory-snapshot evidence on every finding.
- [x] Refresh OSV data manually and on a paced daily schedule without blocking
  reads; expose progress, snapshot age, uncovered ecosystems, declarations with
  no usable version, invalid versions, and packages not covered by the current
  snapshot.
- [x] Expose scope-aware findings in the Dependencies UI, bounded JSON, compact
  read-only MCP, and SARIF 2.1.0 for side-by-side Insights ingestion. Findings
  are advisory-only and are never represented as an enforced CI gate.

Exit condition: a user can see which captured dependencies are behind the
latest public stable release, understand unresolved and private-package gaps,
and trace every declared version back to exact committed source without
RepoKarta modifying a repository.

### M10: enterprise identity and administration

Add an auditable organization-control layer without weakening RepoKarta's
read-only source boundary.

- [x] Record append-only audit events for authentication, authorization
  failures, administration changes, role assignments, denied cross-author access,
  repository acquisition and removal, exports, generation, and destructive
  RepoKarta-owned data operations.
- [x] Include actor, action, target, outcome, authentication provider, request
  correlation ID, and timestamp while redacting credentials, tokens, prompts,
  and repository source content by default.
- [x] Provide bounded audit-log search, filters, retention controls, and
  administrator export with explicit completeness and retention metadata.
- [x] Support SCIM 2.0 user and group provisioning, updates, suspension, and
  deprovisioning with stable external IDs and idempotent operations.
- [x] Map configured identity-provider or SCIM groups to RepoKarta roles;
  unknown or removed identities receive no implicit elevated access.
- [x] Define an explicit permission matrix for reader, knowledge maintainer,
  and administrator roles, including AI generation, shared artifacts,
  owner-only conversations, repository acquisition, security settings,
  role management, and audit-log access.
- [x] Make role changes auditable and immediately effective for new requests;
  deprovisioning must revoke active application sessions without erasing
  historical authorship.
- [x] Keep loopback-local mode simple by treating its single local identity as
  administrator without requiring SCIM or external role configuration.

Exit condition: an administrator can provision and deprovision identities,
assign least-privilege roles, and reconstruct security-relevant activity from
redacted, queryable audit evidence.

### M11: repository acquisition

Add a separate administrator-managed intake pipeline for bringing repositories
into RepoKarta-owned storage and keeping them current.

- [x] Support existing local roots, explicit Git remote URLs, ghorg-managed
  directories, and organization discovery adapters for configured Git hosts.
- [x] Maintain a repository-source registry containing canonical remote
  identity, provider, RepoKarta-owned checkout path, default branch, inclusion
  policy, credential reference, sync state, timestamps, and actionable errors.
- [x] Preview discoveries before acquisition, deduplicate aliases and renamed
  remotes, and expose archived, forked, private, excluded, and already-managed
  states explicitly.
- [x] Clone and fetch without pushing or modifying upstream repositories;
  disable executable hooks and avoid submodule recursion unless an
  administrator deliberately enables a bounded policy.
- [x] Use configured credential helpers or secret references without storing
  raw Git credentials in repository metadata, logs, diagnostics, or audit
  payloads.
- [x] Provide manual and scheduled synchronization with cancellation, bounded
  concurrency, backoff, rate-limit handling, and honest partial-fleet status.
- [x] Allow organization, team, topic, visibility, archive, fork, and repository
  allow/deny policies before data is cloned.
- [x] Trigger catalogue refresh and commit-pinned indexing only after a checkout
  reaches a verified Git revision; preserve the last usable index when a later
  synchronization fails.
- [x] Audit discovery, acquisition, synchronization, policy skips, credential
  failures, and removal using repository identity and revision metadata rather
  than source content.
- [x] Remove only checkouts proven to be RepoKarta-owned, require an explicit
  confirmation for deletion, and never delete or rewrite user-owned local
  repositories.

Exit condition: an administrator can discover, approve, acquire, synchronize,
and safely remove a bounded repository fleet while every indexed revision has
clear acquisition provenance and failures never silently shrink coverage.

### M12: distributed dependency topology

Redesign Dependencies around deployable components and communication flows
while preserving the package inventory and advisory workspace as focused
secondary views.

- [x] Make the component-level system topology the default Dependencies view;
  keep package declarations and security findings on explicit tabs.
- [x] Detect directed HTTP, gRPC, Kafka publish/consume, database, and MCP
  relationships from committed source and configuration without executing
  repository code. MCP retains protocol identity and transport (`stdio` or
  Streamable HTTP) rather than collapsing into generic HTTP.
- [x] Model services, databases, queues, brokers, MCP servers, and external
  services with stable aliases, technology, repository/path ownership,
  capabilities, and exact revision-pinned evidence.
- [x] Recognize multiple deployable components through Spring application
  roots, Docker Compose services, Backstage catalog entities, and an explicit
  `.repokarta.yml` topology correction layer.
- [x] Reconcile fleet peers with kind-aware alias matching; represent Kafka
  flow as publisher to topic and topic to consumer; never resolve a same-named
  database/topic to an application service.
- [x] Persist bounded runtime observations independently from static artifacts
  with provider, environment, observation window, request/error counts, p95
  latency, import time, and 90-day retention.
- [x] Merge equivalent static and runtime edges into `confirmed` relationships
  while keeping `static_only`, `runtime_only`, and unresolved peers visible as
  architecture drift.
- [x] Expose filtering, focus-neighbor interaction, accessible connection
  inventory, evidence/telemetry inspection, bounded JSON read/import APIs, and
  the read-only `read_system_topology` MCP tool.
- [x] Scope repository topology as a bounded fleet-backed neighborhood with
  explicit inbound/outbound direction, depth-one defaults, optional depth-two
  context, grouped hub overflow, unresolved possible callers, and honest fleet
  snapshot freshness.
- [x] Require artifact-management permission for runtime imports; keep reads
  cache-only and return explicit `202 Accepted` progress for cold topology
  artifacts.
- [x] Resolve placeholder targets through map-key registry candidates, in-file
  defaults, ranked cross-repository assignments, and name-shape fallback;
  retain secret/test-only/missing assignments in a citation-backed unresolved
  collection and never create `${VAR}` components.

Exit condition: a user can answer which component communicates with which
service or resource, in which direction and over which protocol, distinguish
declared/static architecture from observed runtime traffic, and open the exact
evidence without treating package imports as distributed-system calls.

### M13: security boundary hardening

Close the gaps found in the 2026-07 full-code review between marketed
guarantees and actual enforcement, before promoting shared deployment.

- [x] Enforce filesystem-read isolation for provider harnesses: extend the
  Claude disallowed-tool set with `Read`, `Glob`, `Grep`, and subagent tools
  (or path-scope reads to the attachment directory when the CLI supports it),
  and disable Codex shell read access so the authenticated MCP surface is the
  only capability. Prompt text alone must not carry the exfiltration boundary.
- [x] Stop passing the Claude MCP bearer token through argv; write the
  MCP configuration to a 0600 file inside the attachment directory, mirroring
  the Codex environment-variable approach.
- [x] Gate `GET /mcp/setup` behind an explicit permission whenever the access
  mode is not loopback-local, so reader-role and anonymous principals cannot
  harvest the shared token.
- [x] Replace the single shared MCP token with per-principal or
  per-conversation tokens that support revocation and audit attribution.
- [x] Throttle `/admin/login` with per-source exponential backoff after
  repeated failures; keep the existing audit events as the alerting signal.
- [x] Add a baseline security-header middleware (`X-Content-Type-Options:
  nosniff`, `frame-ancestors 'none'`, and a CSP compatible with the embedded
  frontend) applied once at the top of the handler chain.
- [x] Fix the audit filter that drops `since` when `since` equals `until`
  (query-value-keyed map), and make the audit export loop assert pagination
  progress instead of trusting `NextBefore`.
- [x] Fail closed on chat interrupt authorization when the conversation
  history service is unavailable instead of skipping the ownership check.
- [x] Replace string-matched error classification (`"required"`, `"unique"`)
  with sentinel errors across conversation, store, and SCIM handlers, and
  scrub raw store error text from SCIM responses.
- [x] Guard the contexts page against successful-but-empty context resolution
  instead of indexing the first element unconditionally.
- [x] Render admin/insights notice and error banners from a server-side
  allowlist instead of reflecting query-parameter text.
- [x] Remove the dead duplicate loopback Host/Origin validation from the HTTP
  server so the security manager is the single owner of the boundary logic.

Exit condition: prompt-injected repository content cannot cause a provider to
read files outside RepoKarta-provided evidence; MCP credentials are not
visible to lower-privileged principals or the local process list; bootstrap
login resists online brute force; audit exports are provably bounded and
correctly filtered.

### M14: index lifecycle and search availability

Fix the lifecycle gaps in the search and indexing pipeline: search must stay
available while indexing runs, and derived artifacts must not outlive their
repositories.

- [x] Stop holding the Zoekt adapter lock across the git-shadow fetch and
  shard build; take the write lock only for the brief searcher close/reopen,
  and make searcher acquisition honor the request context so queued searches
  respect their deadline.
- [x] Add derived-artifact garbage collection: after catalogue sync, diff live
  repository IDs against Zoekt shards, git-shadow clones, SCIP artifacts, and
  graph snapshots; delete or reclassify orphans as cleanable in maintenance
  instead of protected.
- [x] Collect superseded exact-revision SCIP artifacts after catalogue sync and
  successful replacement imports, abandoned `scip-java` worktrees at startup,
  and Wiki Markdown pages removed by a replacement plan. A stale or unreadable
  ready SCIP artifact now degrades reference search to Tree-sitter with an
  explicit warning and is rebuilt by automatic Java indexing.
- [x] Stop emitting search matches whose repository cannot be resolved; they
  currently leak the absolute local path as the repository name for
  unrestricted viewers.
- [x] Make catalogue removal survive transient failures: mark repositories
  missing from one discovery pass as unreachable and delete rows only after
  repeated confirmed misses, preserving IDs and index state across antivirus
  or removable-drive glitches.
- [x] Run `scip-java` builds from the RepoKarta-owned git-shadow clone instead
  of `git worktree add` against the user's repository, restoring the
  never-modify-repositories guarantee across crashes.
- [x] Cache decoded SCIP artifacts per repository and revision with the
  existing single-flight LRU pattern instead of re-reading and re-decoding
  every artifact on each reference query.
- [x] Bound and parallelize commit/diff entity search with a worker pool and a
  per-request git-invocation cap; parallelize the mixed-search child queries.
- [x] Storage hygiene: allow a small pool of read connections under WAL while
  keeping writes on one connection; replace the conversation-read N+1 with
  joined queries and lazy image loading; preserve `indexed_at` while a
  previous index still serves; use the case-folded path key in the catalogue
  delete comparison; synchronize `baseCtx` publication; correct the
  `GIT_CONFIG_NOSYSTEM` value to match its isolation intent; move the queue
  status write under the queue critical section.

Exit condition: search remains available and deadline-bound during fleet
indexing; removed repositories leave no servable or protected debris; a
reindexing repository is never reported as never indexed.

### M15: provider harness robustness and citation fidelity

Make subprocess lifecycles airtight on Windows and macOS and make the Sources
list a faithful record of the evidence a turn actually used.

- [x] Register conversations insert-if-absent under the manager lock; on a
  concurrent-resume conflict, close the losing session instead of leaking a
  live provider process.
- [x] Replace the one-shot reader error channel with a done channel plus
  sticky error so process death is observable to every waiter, and guard the
  stderr buffer against concurrent reads while the process runs.
- [x] Kill the full harness process tree: Windows Job Objects and POSIX
  process groups, so `.cmd` shims cannot orphan the real interpreter.
- [x] Add a timeout to the CLI `--version`/auth probes and cache their results
  briefly so a wedged CLI cannot freeze provider status or conversation start.
- [x] Distinguish user interrupt from client disconnect in the Anthropic
  adapter; only a deliberate interrupt may persist as `interrupted`.
- [x] Record `git_log`, `git_diff`, and `list_tree` tool output into the
  citation tracker; replace the alphabetical 12-source cap with per-tool or
  recency-weighted selection so one map read cannot evict the file citations
  an answer relies on; validate model-inline `source_url` links against the
  tracker and mark unverified links.
- [x] Converge the three AI tool surfaces: add AST search and named-context
  selectors to the native Anthropic loop, and wire insights, dependencies, and
  topology tools into the stdio MCP configuration.
- [x] Drop stale Codex deltas with empty turn IDs and let reader goroutines
  exit after session close instead of blocking on full channels.
- [x] Move the hardcoded MCP tool catalog out of the HTTP layer into the MCP
  server so the setup page cannot drift from the real tool list.

Exit condition: no orphaned provider process survives an interrupted, resumed,
or killed conversation on Windows or macOS; every listed source corresponds to
evidence a tool actually returned during the turn; answer capability does not
silently depend on which provider transport is in use.

### M16: derived-artifact correctness

Fix the heuristic and pipeline edge cases in topology, documentation, and
advisories found by the review, favoring the recently hardened areas.

- [x] Resolve bare `host:port` environment values in topology placeholder
  resolution: detect the `url.Parse` scheme misread and fall through to
  host/port splitting, with a regression test for `NAME_HOST: service:8080`.
- [x] Regenerate (or revision-rewrite for unchanged files) the survey when a
  single stale Wiki page is refreshed, so targeted regeneration cannot burn a
  provider turn and then fail the citation gate against the old revision.
- [x] Parse CVSS 4.0 severity vectors (or map provider severity labels) so
  modern criticals never sort last as `unknown`.
- [x] Separate stdout from stderr in the docs git runner so git warnings can
  never contaminate steering-file content or staleness diffs.
- [x] Harden the Kafka classifier: word-boundary the send/publish verbs,
  exclude STOMP/WebSocket template markers, and record which marker qualified
  the file as a Kafka source.
- [x] Carry protocol through placeholder resolution (broker and database
  indicators must not render as HTTP call edges) and support Spring-style
  `${lower.dotted}` placeholders in configuration files.
- [x] Stop collapsing private corporate FQDNs to their registrable domain when
  distinct hosts share a private DNS zone.
- [x] Make advisory refresh resilient: persist a partial snapshot with an
  explicit partial state on per-advisory failure, stop dispatching after a
  fatal error, and sort OSV range events with the ecosystem version ordering
  before evaluation.
- [x] Bring graph snapshot publishing up to the docs/advisories standard:
  Windows rename fallback, identity verification on cached reads, lock-safe
  reads, and garbage collection of superseded snapshot signatures and their
  per-signature locks.
- [x] Heuristic polish: hoist hot-path regex compilation; accept relative
  `@GetMapping` paths and multiple class-level prefixes; extend Go route
  detection beyond `Handle`/`HandleFunc`; cap topology neighborhoods by
  usefulness rather than ID order; tighten prerelease substring matching.

Exit condition: literal deployment targets resolve to edges with the correct
protocol; refreshing one stale page succeeds without wasted provider turns; a
CVSS-4-only critical advisory ranks by its real severity; topology output on
Windows survives concurrent refresh and read.

### M17: frontend contract and build integrity

Make the frontend/backend contract mechanical and the packaged asset tree
exactly reproducible.

- [x] Clean `web/dist/assets` before every build in all build and package
  scripts so locally packaged binaries never embed superseded hashed chunks.
- [x] Centralize fetches in one typed API helper that owns headers, error
  mapping, and minimal runtime shape checks for load-bearing endpoints;
  generate the TypeScript response types from the Go structs so the hand-
  mirrored types cannot drift.
- [x] Verify extracted-module declarations against their implementations:
  enable checked JavaScript over `src/**/*.mjs` or migrate new extractions to
  TypeScript.
- [x] Extract and unit-test the NDJSON chat-event reducer, the Wiki
  generation orchestration state machine, and a declarative initialization
  table that derives both element lookups and checks from one source.
- [x] Harden streaming: skip malformed NDJSON lines instead of aborting the
  turn, and give the artifact-progress poller bounded retry with backoff
  instead of halting on one transient error.
- [x] Add an HTTP(S)-scheme guard for every server-provided link sink (chat
  sources, evidence links, source-URL fallbacks).
- [x] Replace full-page reload completion signals with targeted region
  refreshes so background completion cannot discard in-progress user state.
- [x] Extend the dependency-policy check to validate `overrides` entries and
  pass the project npm configuration in the Homebrew formula.
- [x] Promote a minimal boot smoke (start binary, `GET /`, health, embedded
  asset) into the package-smoke CI job, which currently packages but never
  runs the server.

Exit condition: a locally packaged binary contains exactly one generation of
built assets; a backend response-shape change fails the type check or surfaces
a visible error instead of silently corrupting the page; one malformed stream
event costs one event, not the rest of the answer.

### M18: codebase consolidation

Reduce duplication and file size along seams the codebase has already proven,
with zero behavior change verified by the existing suites.

- [x] Extract a shared `internal/gitexec` runner (context timeout,
  `GIT_OPTIONAL_LOCKS=0`, environment hygiene, separated stderr) and adopt it
  in graph, docs, and insights.
- [x] Extract a shared `internal/atomicfile` publish helper with the Windows
  replace fallback and adopt it in graph, docs, and advisories.
- [x] Decompose the HTTP server along the established `admin.go`/
  `enterprise.go` seams: routes, conversations, search, dependencies, source,
  and render/middleware files; move source-intelligence domain joining out of
  the HTTP layer.
- [x] Decompose the graph package into per-ecosystem manifest parsers, Spring
  heuristics, git plumbing, and artifact IO files; collapse the four snapshot
  fan-out readers into one generic helper.
- [x] Continue the frontend extraction cadence established by the existing
  tested modules, prioritizing the largest untested closures.
- [x] Unify duplicated helpers: service-name normalization, kind-resolution,
  admin form parsing, and topology connection merging each get one owner.

Exit condition: each cross-cutting concern has exactly one implementation; the
largest backend and frontend files are reduced to focused units without any
observable behavior change.

### M19: OpenTelemetry observability

Make RepoKarta operable through vendor-neutral OpenTelemetry signals so an
operator can route application metrics, structured logs, and traces through an
OpenTelemetry Collector or an OTLP-capable agent such as the Datadog Agent
without adding a vendor SDK to RepoKarta.

- [x] Add one telemetry lifecycle at the application composition root for
  resource detection, meters, log records, tracers, batching, bounded shutdown
  flush, and error reporting. Telemetry is disabled by default, creates no
  exporter network traffic when disabled, and never makes request handling or
  background work depend on collector availability.
- [x] Support OTLP over gRPC and HTTP/protobuf using the standard
  `OTEL_*` environment-variable contract for endpoints, per-signal exporters,
  protocols, headers, TLS, timeouts, batching, sampling, and resource
  attributes. Reject invalid configured values at startup, but treat delivery
  failures after startup as bounded, observable degradation.
- [x] Identify every signal with stable OpenTelemetry resource attributes,
  including `service.name=repokarta`, the built `service.version`, a
  non-secret service instance ID, and an operator-supplied deployment
  environment. Permit resource enrichment without allowing it to replace
  RepoKarta-owned identity attributes with empty or misleading values.
- [x] Replace ad hoc process output with one context-aware structured `slog`
  pipeline. Preserve useful local console logs, optionally bridge the same
  records to OTLP, normalize severity and attribute types, and attach request
  correlation ID plus trace/span IDs when present. Pin and isolate the
  OpenTelemetry Go log bridge while the Go log signal is not stable.
- [x] Instrument HTTP server requests with the current OpenTelemetry semantic
  conventions: route-template, method, status class, duration, active requests,
  and response size. Never use raw URLs, query strings, source paths, user
  identities, repository names, or conversation IDs as metric attributes.
- [x] Publish Go runtime and process metrics plus bounded `repokarta.*`
  operational metrics for catalogue state, indexing queues and outcomes,
  search latency/errors/truncation, repository synchronization, AI and Wiki
  generation outcomes and token usage, dependency/advisory refresh, topology
  preparation, database-pool pressure, and maintenance jobs.
- [x] Define a cardinality and privacy contract for every custom instrument.
  Metric dimensions are limited to small enumerations such as operation, state,
  outcome, provider kind, and access mode; repository-specific diagnosis uses
  redacted logs or spans instead of unbounded metric labels. Prompts, source
  content, search queries, credentials, tokens, headers, local paths, and
  database URLs are never telemetry payloads.
- [x] Add spans around HTTP requests, outbound HTTP calls, Git/provider
  subprocesses, database operations where supported safely, and the indexing,
  acquisition, synchronization, generation, advisory, topology, and
  maintenance job lifecycles. Propagate context across queues and goroutines so
  correlated logs and metric exemplars can lead back to the initiating request
  or scheduled root span.
- [x] Bound exporter queues, payloads, retries, memory, and shutdown time;
  record dropped signals and the last export success/error without recursively
  exporting exporter failures. `/healthz` remains independent of an optional
  collector, while operator diagnostics expose enabled signals, protocol,
  sanitized endpoint identity, queue/drop state, and last delivery status.
- [x] Document a local OpenTelemetry Collector debug setup and production
  examples for both the Collector Datadog exporter and Datadog Agent OTLP
  ingestion. Credentials remain collector/agent-managed, examples bind
  receivers to loopback unless explicitly deployed otherwise, and no
  Datadog-specific behavior is required for other OTLP backends.
- [x] Ship starter metric definitions, dashboard queries, and alert guidance
  for availability, HTTP error rate and latency, indexing backlog/failures,
  generation failures and duration, sync/advisory freshness, database
  saturation, and telemetry drops. Document temporality and histogram choices
  so OTLP-to-Datadog translation preserves useful counts and latency
  distributions.
- [x] Add deterministic tests with an in-process OTLP receiver that assert
  resource identity, representative metrics, structured log correlation,
  trace parenting, redaction, and bounded attributes. Cover disabled mode,
  unreachable collectors, queue overflow, cancellation, shutdown flush, and
  Windows/macOS/Linux packaging without requiring network access or Datadog
  credentials.

Exit condition: an operator can point an unmodified RepoKarta binary at a
standard OTLP receiver and observe correlated, redacted metrics, logs, and
traces for HTTP traffic and core background jobs; the supplied Collector and
Datadog Agent examples make the same signals usable in Datadog, while a missing
or failing telemetry backend cannot make RepoKarta unavailable.

### M20: remaining local-product scope

- [x] Derive revision-pinned framework and executable reachability roots,
  dependency-injection and implementation edges, witness paths, and explicit
  static/runtime completeness. Classify declarations as reachable,
  probably-unreachable, or unknown without asserting dead code.
- [x] Watch bounded Git metadata for committed local-repository changes and
  debounce the normal read-only catalogue/index refresh.
- [x] Stream large bounded search result prefixes to the browser over a
  cancellable NDJSON contract while preserving the final exact completeness
  summary.
- [x] Resolve a locally configured `origin/HEAD` and index its immutable commit
  without changing the current checkout, with an explicit current-HEAD
  fallback.
- [x] Build and boot-smoke Linux amd64 and arm64 release tarballs alongside the
  existing Windows amd64 and macOS arm64 packages, preserving every required
  license.

Exit condition: the five previously planned local-product gaps have
evidence-backed API/UI/MCP or operational surfaces, repeatable tests, and
release validation without weakening read-only or completeness boundaries.

### M21: isolated agent coding workspace

- [x] Add the developer role, repository-write permission, and explicit
  per-repository Code opt-in.
- [x] Create exact-commit, shadow-backed Git worktrees without registering or
  mutating the source repository.
- [x] Add durable owner-scoped Code sessions, transcript bindings, actions,
  approvals, lifecycle state, and isolated finish commits.
- [x] Add separate Codex workspace-write and Claude edit-only provider
  profiles while preserving the existing read-only Chat profiles.
- [x] Add the Code tab, session history, streamed turns, visible actions,
  single-use approvals, bounded Git diffs and previews, per-file discard,
  interrupt, finish, and full discard.
- [x] Document the complete boundary and acceptance evidence in
  `CODE_SCOPE.md`.

Exit condition: a developer can ask an agent to implement a change, review the
Git-derived result, and finish or discard it without the registered repository
changing and without granting Chat or another user write access.

## Definition of quality

A capability is not complete merely because its happy path renders. Relevant
completion criteria include:

- repeatable tests;
- cancellation and timeouts;
- clear empty, loading, error, stale, and partial states;
- Windows, macOS, and Linux path handling;
- no repository mutation;
- no secret leakage;
- bounded memory, disk, result size, and AI cost;
- source citations that still resolve against the recorded revision;
- recovery after interrupted indexing or generation;
- accessible keyboard navigation and readable light/dark themes.

## Decisions already made

- Name: RepoKarta.
- Backend and application host: Go.
- Search engine: a pinned Zoekt revision behind a RepoKarta adapter, with a
  maintained native-Windows portability delta and complete upstream
  attribution.
- Metadata database: SQLite with a pure-Go driver by default; PostgreSQL 18+
  is an operator-selected shared-deployment backend with schema-compatible,
  backend-specific migrations and an explicit SQLite migration command.
- Primary interface: server-rendered Go templates and HTMX.
- Frontend build and styling: Vite and Tailwind CSS.
- Visualization: Cytoscape in a focused TypeScript island, not a full SPA.
- Default deployment: a local native executable.
- Repository sources: existing local Git repositories, including ghorg
  directories, plus explicitly approved GitHub and GitLab acquisitions.
- AI providers: local Codex and Claude Code harnesses using the user's existing
  login, plus an optional Anthropic API adapter using the user's API key.
- AI retrieval: iterative deterministic search and file tools before embeddings.
- Default source access: read-only.

## Remaining planned scope

No committed implementation items remain in this scope. Further work should
start from measured product use or one of the open questions below rather than
silently expanding the roadmap.

## Open questions

- Whether generated Markdown should live only in RepoKarta storage or optionally
  export to a user-selected directory automatically.
- Whether an optional Claude Agent SDK helper materially improves Q&A enough to
  justify a Node-based companion.

## Current implementation version

`0.100.0-dev`. M0 through M21 are complete. M21 adds the separately authorized
Code tab, exact-commit shadow worktrees, durable coding transcripts/actions/
approvals, provider-specific write profiles, bounded Git review, and isolated
finish/discard lifecycles without mutating registered repositories. M4 now includes administrator-
selected batch Deep Wiki generation with per-repository outcomes and resumable
checkpoints. M7 now includes qualified symbol
search, precise optional SCIP data, commit-pinned CODEOWNERS, bounded evidence
graph queries, saved searches and deterministic monitors, and permission-safe
Deep Search with visible trace, budgets, retry, and revocable sharing. M9 now
includes explicit comparison and distance states, fleet and repository
filters, and fail-closed classification of unconfigured internal package
coordinates before registry refresh.
M12 distributed dependency topology is complete: the Dependencies landing view
now models deployable components and directed HTTP, gRPC, Kafka, database, and
MCP relationships; type-aware fleet reconciliation, explicit declarations,
runtime observation import/retention, static-vs-runtime drift, JSON, MCP, and
the interactive evidence view are implemented. Referential-integrity
enforcement and resilient partial rendering prevent dangling connections from
blanking the graph. Deployment-only repositories retain diagnostic counts for
connections dropped from suppressed source components. Placeholder resolution now
includes service-map keys, ranked fleet assignments, candidate and unresolved
payloads, registrable-domain naming, and resolved/candidate/unresolved honesty
counts. Repository scope is a fleet-backed, depth-one inbound/outbound
neighborhood with bounded overflow groups, optional depth-two context, possible
unresolved inbound callers, and explicit fleet freshness/partiality.
The commit-pinned source editor now embeds repository-scoped Zoekt search and
SCIP/AST usage discovery. Detected HTTP controller routes are shown beside
inbound topology callers; only caller evidence with a matching URL path is
labeled route-specific, while other edges remain explicitly service-level.
Linked-worktree discovery deduplication and M10 enterprise identity and
administration are complete; conversations are strictly owner-only, Wiki/admin
actions retain principal-scoped audit evidence, advisory results are grouped
without losing manifest occurrences, and every repository-aware MCP tool
accepts an exact name with ambiguity errors. Chat initialization keeps the
owner-only scope usable without a view-all toggle and exposes selector-contract
failures in both the page and debug log. M11 is complete. The implemented M7-M11 slices
now resolve directory Git trees and exact syntax-backed symbol declarations
alongside repository/file identities, scope deterministic matches to symbol
line ranges, and turn pasted same-origin RepoKarta URLs into permission-checked
chips. Revision-pinned named contexts cover team, product, service-fleet,
release, and personal-task scopes with private personal and administrator-
published defaults; Chat, JSON, and MCP expose every effective context,
provenance source, and canonical copyable URL. The implemented slices also
offer a contextual Chat launcher from Search, Maps, dependencies, Insights,
Wiki, MCP, named-context, and source-file views; its shareable URL carries the
originating mode, a permission-checked current-view context, and an explicitly
submitted question into a new conversation. A commit-pinned project browser
now traverses every directory through deterministic 500-entry pages, preserves
breadcrumbs and sibling navigation while reading files, and deep-links exact
lines without reading or mutating the worktree. The JSON and MCP tree contracts
expose offsets so large directories remain completely traversable. The
implemented slices also
provide all twelve explicit search result types, grammar-compatible facets,
mixed exact-path and exact-symbol ranking ahead of fuzzy content with
deterministic explanations, normalized source-index ranks, identifier-aware
filename and parent-directory boosts, multi-hit file coherence, and
query-aware test/example/legacy penalties. Permission-preserving result actions
for source, Maps, dependencies, references, implementations, scoped Chat, and
the last active conversation context. Repository-scoped Git history search now
filters commits and diffs by author, message, path, actual added or removed
lines, inclusive date range, exact reachable branch, and bounded revision
range. The implemented slices also
capture Java enum members, qualified field accesses, method references, and
normalized import targets for stronger AST reference recall; the search
workspace defaults to Zoekt syntax and uses explicit multi-language highlighting.
Dependency management documents the hosted-sync-to-registry workflow and shows
the registry used for every observation. Administration and Insights are split
into focused, directly navigable domain workspaces. The implemented slices also
enforce insight mutation permissions and revision staleness, paginate
dependency declarations, cache immutable file context trees, open the
administrator console directly in loopback-local mode, and support
credential-backed acquisition from explicitly configured GitHub and GitLab
HTTPS hosts with canonical checkout validation. Empty Git repositories remain
visible as terminal `empty` catalogue entries without inflating pending index
or derived-artifact work. Native JSON, MCP, and the direct Anthropic tool loop
now share compact source/symbol/reference discovery. Compact reference reads
stay on persisted structural artifacts and omit snippet bodies until selected
files are opened explicitly. Java and Go now also expose bounded Tree-sitter
query search with named captures, text and ancestor/parent predicates,
persisted node-kind candidate pruning, artifact-bound pagination, and explicit
coverage/truncation metadata through JSON and MCP. M20 now derives conservative
static reachability from those persisted artifacts, with
framework/executable roots, syntax-backed witness paths, and explicit
static/runtime completeness; it deliberately reports probably-unreachable or
unknown instead of claiming dead code. Language filters now canonicalize
case-insensitive names and aliases at the Zoekt boundary, while unknown values
produce an explicit machine-readable warning instead of a silent empty result.
Optional Java SCIP generation now discovers an installed or configured
`scip-java`, detects Gradle Java builds at the exact indexed revision, runs
them in isolated background worktrees, imports bounded artifacts, exposes
provider and repository status plus retry controls, and retains Tree-sitter
fallback whenever precise Java coverage is missing, stale, or failed. Its
status distinguishes environment or Docker failures, JDK-incompatible
wrappers, and compilation errors. Exact-commit Gradle wrapper and Java
toolchain metadata drive a compatible configured launcher JDK instead of
blindly inheriting the server JVM. Unusable ready artifacts now downgrade with
a machine-readable warning while the scheduler rebuilds them; superseded SCIP
revisions, abandoned build worktrees, and replaced Wiki pages are collected.
OpenTelemetry metrics, structured logs, and traces are available through
standard OTLP gRPC or HTTP/protobuf exporters with a disabled-by-default,
failure-isolated lifecycle. Stable service identity, correlated redacted logs,
bounded route and operation dimensions, runtime/catalogue/database metrics,
core job and outbound-call spans, administrator delivery diagnostics, local
Collector and Datadog examples, and packaged operational guidance are included.
M17 makes frontend builds reproducible, generates TypeScript API contracts from
Go response structs, checks extracted JavaScript, isolates Chat and Wiki state
reducers, recovers from malformed stream events and transient polling failures,
guards server-provided links, refreshes completed regions without discarding
page state, validates npm overrides, and boots packaged binaries in CI.
M18 consolidates Git execution and atomic publication behind shared packages,
splits HTTP and graph responsibilities into focused files, moves source
intelligence joining out of HTTP, replaces four graph fan-out readers with one
generic implementation, extracts the duplicated repository picker into a tested
frontend module, and gives duplicated normalization, resolution, form parsing,
and topology merging helpers one owner each.
Dependency workspace arrows and evidence separators are encoded as native UTF-8
characters, so the rendered topology and inventory views no longer expose
mojibake.
M20 also debounces committed local-repository changes from Git metadata,
indexes a locally configured remote default commit without switching the
checkout, streams bounded browser search results over NDJSON, and builds
license-complete Linux amd64/arm64 packages with native-runner boot smoke tests.

## Recommended next session

Use measured product feedback to choose the next milestone, or resolve one of
the two open questions above. Preserve the completed permission, revision,
ambiguity, registry-routing, evidence, privacy, cardinality, read-only, and
completeness boundaries.
