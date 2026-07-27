# RepoKarta scope

## Product statement

RepoKarta is a local-first workspace for understanding a collection of Git
repositories already present on a developer's laptop, especially repositories
downloaded with ghorg.

It combines four capabilities:

1. Fast cross-repository source search powered by Zoekt.
2. Grounded questions and answers about code, with file-and-line citations.
3. Interactive architecture and dependency maps.
4. Commit-aware, automatically generated repository documentation.

RepoKarta runs on `127.0.0.1`, treats source repositories as read-only, and
stores all indexes, metadata, generated documents, and settings outside those
repositories.

The intended first users are individual developers working with private
repositories on managed Windows and Apple Silicon macOS laptops.

## Product principles

- **Local first:** repository contents and indexes remain on the user's laptop.
- **One command:** pointing RepoKarta at a ghorg directory should be enough to
  discover and index its repositories.
- **Search is foundational:** deterministic code search should work without an
  AI provider or API key.
- **Evidence over confidence:** AI answers, diagrams, and documentation must
  link to the exact source files and revisions that support them.
- **Read-only by default:** RepoKarta explains code; it does not edit source
  repositories or execute repository code.
- **Incremental work:** unchanged repositories, files, and documentation pages
  should not be recomputed.
- **Native and portable:** Windows amd64 and macOS arm64 are first-class release
  targets.
- **Replaceable integrations:** Zoekt and AI providers are hidden behind
  RepoKarta-owned interfaces.

## MVP user journey

1. The user downloads or builds the RepoKarta executable.
2. The user runs `repokarta serve <repository-root>`.
3. RepoKarta discovers all Git repositories below that root.
4. RepoKarta incrementally indexes the default revision of each repository.
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
- Never fetch, pull, checkout, reset, clean, or otherwise modify repositories.
- Detect updates manually and, later, with a filesystem watcher.

### Code search

- Use Zoekt as the search engine and pin the exact upstream revision.
- Wrap Zoekt behind internal RepoKarta interfaces because Zoekt does not publish
  a stable v1 module.
- Resolve native Windows support before selecting the final integration shape.
  The upstream revision tested on 2026-07-24 does not compile its indexing
  package for Windows: its memory-mapped index file is restricted to Unix build
  tags and its builder calls `unix.Umask`. Preferred resolution order:
  1. contribute or maintain a small, reviewable Windows portability patch;
  2. use a RepoKarta-managed Zoekt helper process;
  3. use WSL or Docker only as an opt-in fallback, not the default product.
- Store Zoekt shards outside source repositories.
- Incrementally index default revisions.
- Support substring, regular-expression, boolean, repository, language, path,
  file, and symbol queries exposed by Zoekt.
- Stream large result sets to the browser.
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
  - `git_log`
  - `git_diff`
  - `read_repository_map`
  - `read_dependency_inventory`
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
- Select repositories by the stable numeric repository ID in every
  repository-specific MCP tool and JSON endpoint. Cross-repository search keeps
  it optional. Repository names are not unique and are never the advertised
  selector.
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
  `spring.application.name` become edges; unresolved variables and
  infrastructure hosts are dropped.
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
- Store metadata in SQLite using a pure-Go driver.
- Store large Zoekt shards and generated artifacts on the filesystem rather
  than in SQLite.
- Use WAL mode and migrations for SQLite.
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

## Explicit non-goals for the MVP

- A hosted SaaS or cloud control plane.
- Multiple users, organizations, SSO, or repository permission synchronization.
- Shared or team deployment. RepoKarta binds to loopback with no authentication
  because it is a single-user local product. A configurable bind address,
  authentication, and shared deployment are deliberately deferred to M6 and are
  not implemented.
- Scheduled ghorg synchronization. RepoKarta never fetches or updates
  repositories.
- Compiler-precise Sourcegraph or SCIP parity. Structural extraction is
  regex-and-manifest based: it is evidence-backed and commit-pinned, but it is
  not a compiler front end and does not resolve every reference.
- GitHub, GitLab, or Bitbucket repository cloning and synchronization. ghorg or
  the user remains responsible for local repository acquisition and updates.
- Searching every historical commit or every branch.
- Editing code, applying patches, executing builds, or running arbitrary shell
  commands through the AI.
- Compiler-precise cross-repository navigation for every language.
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
internal/store/         SQLite and migrations
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

SQLite owns small relational state:

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

API keys must come from environment variables, an operating-system credential
store, or an explicit runtime prompt. They must never be written to SQLite,
logs, generated documents, repository configuration, or browser local storage.

## Security boundary

- The HTTP listener defaults to loopback and rejects unexpected Host/Origin
  values.
- All repository paths are canonicalized and checked against configured roots.
- Symlinks and junctions must not escape configured roots silently.
- Ignore `.git` internals, secrets, credentials, dependency caches, build
  outputs, and user-configured patterns during AI context collection.
- Search indexing and AI context inclusion are separate policy decisions.
- AI tools are read-only and enforce file-size, result-count, and time limits.
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
- [x] Continuous integration on Windows and macOS.

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
- [x] Full-screen Mermaid zoom and SVG download.
- [x] Commit-aware staleness and selective regeneration.
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
- [x] Source-build Homebrew formula with CI installation and license checks.
- [x] Upgrade and database/artifact migration tests, including idempotent retry
  and unsupported-future-format rejection.

### M6: shared deployment

The executable now has a tested shared-deployment authentication boundary while
remaining loopback-only by default. Non-secret provider settings persist in
SQLite; bootstrap administrator credentials and SAML private keys do not.

- [x] Configurable bind address beyond `127.0.0.1`.
- [x] Four explicit access modes: loopback-only local, Cloudflare Access JWT,
  native SAML SP, and startup-gated unauthenticated shared access.
- [x] Startup-credential-protected administration for authentication settings.
- [x] Per-user ownership and access enforcement for durable conversations,
  with administrator own/all and author filters.
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
- [ ] Extend permission-aware autocomplete to `@directory` and `@symbol`, and
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
- [ ] Add named search contexts representing repositories and revisions for a
  team, product, service fleet, release, or personal task, with personal and
  administrator-managed defaults.
- [ ] Make every effective context visible, URL-addressable, copyable, and
  reusable through the JSON API and MCP surface.

#### Query and result experience

- [ ] Add one documented query grammar with autocomplete for repository,
  revision, language, path, filename, symbol kind, result type, ownership, and
  positive or negative filters while retaining the existing simple form.
- [ ] Provide explicit result types for content, file path, repository, symbol
  definition, reference, implementation, dependency, route, commit, diff,
  generated Wiki page, and captured code insight.
- [ ] Add qualified programming-element search by package, type, method, member,
  and full name, with exact, prefix, case-sensitive, and case-insensitive modes.
- [ ] Add result facets and “search within these results” refinement without
  discarding the original query, scope, revision, or completeness metadata.
- [ ] Rank exact symbol and path matches ahead of fuzzy content matches and
  explain important ranking signals; never let personalization hide complete
  deterministic results.
- [ ] Add one-click actions from a result to open source, focus the element in
  Maps, inspect dependencies, find references or implementations, start a
  scoped conversation, or add the result to the current context.
- [ ] Import optional SCIP indexes for compiler-accurate cross-repository
  definitions, references, implementations, type signatures, and hover
  documentation while retaining syntax-backed results and confidence labels
  when precise data is unavailable.
- [ ] Add graph queries for upstream and downstream impact, shortest evidenced
  connection paths between two repositories, files, or symbols, and bounded
  traversal by relation type and depth.
- [ ] Search commits and diffs by author, message, path, added or removed text,
  date range, branch, and revision range without pretending unindexed history is
  part of the default-branch content index.
- [ ] Ingest CODEOWNERS as commit-pinned metadata for owner display and filters,
  including explicit owned, unowned, unresolved-owner, and unavailable states.

#### Saved searches, monitoring, and Deep Search

- [ ] Persist per-author recent and saved searches with title, query, context,
  filters, result type, and revision policy; allow administrators to publish
  shared read-only search templates.
- [ ] Turn a saved deterministic query into a monitor that reports newly added
  or removed matches between comparable indexed revisions, with bounded history
  and explicit notification-delivery status.
- [ ] Add an optional Deep Search mode that answers natural-language questions
  through the existing read-only search, symbol, reference, file, tree, Git,
  map, dependency, and Wiki tools rather than introducing an ungrounded
  retrieval path.
- [ ] Stream a concise exploration trace showing current stage, searches
  executed, files read, contexts used, elapsed time, coverage warnings, and
  sources; do not expose hidden model reasoning.
- [ ] Preserve structured mentions and named contexts across follow-up
  questions, while allowing the user to add, remove, or replace scope without
  restarting the conversation.
- [ ] Provide cancellation, retry from persisted deterministic evidence,
  time/token/tool-call budgets, and a visible “search more broadly” action when
  the first bounded exploration is incomplete.
- [ ] Make every Deep Search answer addressable by URL and shareable only after
  rechecking repository permissions for every viewer; never expose cited source
  through a shared answer when that viewer cannot access it.
- [ ] Keep optional semantic retrieval or reranking clearly labeled and bounded.
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
- [ ] Compare declared and latest stable versions and expose clear states such
  as current, behind, ahead/prerelease, unavailable, private/internal,
  unresolved, and registry error.
- [ ] Display version discrepancies fleet-wide and per repository, with filters
  by ecosystem, package, repository, severity of version distance, and check
  status.
- [x] Record the registry source and observation timestamp separately from the
  commit-pinned declaration evidence so a freshness result never appears to be
  a historical source fact.
- [x] Cache registry responses, honor rate limits and offline operation, bound
  refresh concurrency, and surface partial or stale results explicitly.
- [ ] Do not send private/internal package names to public registries unless an
  administrator explicitly configures that ecosystem and registry as safe.
- [x] Keep the feature advisory and read-only: RepoKarta may explain an upgrade
  discrepancy, but it must not rewrite manifests, lockfiles, or repositories.

Exit condition: a user can see which captured dependencies are behind the
latest public stable release, understand unresolved and private-package gaps,
and trace every declared version back to exact committed source without
RepoKarta modifying a repository.

### M10: enterprise identity and administration

Add an auditable organization-control layer without weakening RepoKarta's
read-only source boundary.

- [x] Record append-only audit events for authentication, authorization
  failures, administration changes, role assignments, cross-author access,
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
  cross-author conversations, repository acquisition, security settings,
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

## Definition of quality

A capability is not complete merely because its happy path renders. Relevant
completion criteria include:

- repeatable tests;
- cancellation and timeouts;
- clear empty, loading, error, stale, and partial states;
- Windows and macOS path handling;
- no repository mutation;
- no secret leakage;
- bounded memory, disk, result size, and AI cost;
- source citations that still resolve against the recorded revision;
- recovery after interrupted indexing or generation;
- accessible keyboard navigation and readable light/dark themes.

## Decisions already made

- Name: RepoKarta.
- Backend and application host: Go.
- Search engine: Zoekt behind a RepoKarta adapter. A direct pinned dependency is
  preferred, contingent on resolving native Windows compilation.
- Metadata database: SQLite with a pure-Go driver.
- Primary interface: server-rendered Go templates and HTMX.
- Frontend build and styling: Vite and Tailwind CSS.
- Visualization: a focused TypeScript island, not a full SPA.
- Default deployment: a local native executable.
- Primary repository source: existing local Git repositories, including ghorg
  directories.
- AI providers: local Codex and Claude Code harnesses using the user's existing
  login, plus an optional Anthropic API adapter using the user's API key.
- AI retrieval: iterative deterministic search and file tools before embeddings.
- Default source access: read-only.

## Open questions

- Whether to index only `HEAD` initially or resolve and index each repository's
  configured default branch when it differs from the current checkout.
- Whether the repository catalogue should support multiple roots in M1 or after
  search is complete.
- Which graph library offers the best balance of large-graph performance,
  accessibility, layout quality, and bundle size.
- Which repositories and cross-file questions justify adding optional SCIP
  indexes after measuring the syntax-tree layer's coverage and cost.
- Whether generated Markdown should live only in RepoKarta storage or optionally
  export to a user-selected directory automatically.
- Whether an optional Claude Agent SDK helper materially improves Q&A enough to
  justify a Node-based companion.

## Current implementation version

`0.53.0-dev`. M0 through M6 are complete; M7 is in progress; M8 is complete.
The M9 dependency inventory, public registry refresh, lockfile resolution, and
explicit private-registry routing are implemented; discrepancy filtering and
fail-closed classification of unconfigured internal package prefixes remain.
Linked-worktree discovery deduplication and M10 enterprise identity and
administration are complete; M11 is complete. The implemented M7-M11 slices
now resolve directory Git trees and exact syntax-backed symbol declarations
alongside repository/file identities, scope deterministic matches to symbol
line ranges, turn pasted same-origin RepoKarta URLs into permission-checked
chips, enforce insight mutation permissions and revision staleness, paginate
dependency declarations, cache immutable file context trees, open the
administrator console directly in loopback-local mode, and support
credential-backed acquisition from explicitly configured GitHub and GitLab
HTTPS hosts with canonical checkout validation. Empty Git repositories remain
visible as terminal `empty` catalogue entries without inflating pending index
or derived-artifact work.

## Recommended next session

Continue M7 with named search contexts and complete repository/file/directory/
symbol parity when the Deep Search composer is introduced. Preserve the current
fail-closed permission, revision, ambiguity, and completeness behavior before
adding facets or agentic Deep Search.

Continue M9 afterward with discrepancy/status filters and a fail-closed way to
classify internal package prefixes before public refresh. Preserve the current
bounded cache, separate observation timestamps and errors, and explicit
private-registry routing.
