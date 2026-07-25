# RepoKarta

RepoKarta is fast, local-first code search for the Git repositories already on
your laptop. Point it at a ghorg directory (or any repository root), and it
discovers, incrementally indexes, and searches committed source without
modifying a worktree.

The current implementation delivers M1 code search and M2 grounded code
questions:

- regular, linked-worktree, and bare Git repository discovery;
- origin, default revision, HEAD, scan, and index metadata in SQLite;
- canonical path reconciliation that removes stale and duplicate catalogue rows
  on every discovery;
- native Zoekt indexing on Windows amd64, macOS arm64, and Linux;
- incremental reindexing when a repository HEAD changes;
- literal, regular-expression, and native Zoekt query modes;
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
  full-window vector diagram viewer with fit and percentage zoom controls;
- reuse of the provider's existing ChatGPT/Codex or Claude login without
  collecting subscription credentials, plus `ANTHROPIC_API_KEY` configuration
  that is read only from the launch environment;
- one provider-neutral Go conversation interface;
- a JSON code-intelligence API used by the UI and protocol adapters;
- authenticated loopback HTTP MCP plus a stdio MCP adapter with read-only
  `list_repositories`, `search_code`, `find_symbol`, `get_file`, `list_tree`,
  `git_log`, and `git_diff` tools;
- read-only Codex sandboxes, Claude plan mode, and disabled mutation/shell
  tools;
- authoritative citation chips recorded from the exact MCP tool results rather
  than trusting a model to reproduce source URLs;
- locally persisted, automatically titled conversations with reopen, rename,
  delete, native provider resume, and bounded transcript-replay fallback;
- visible token usage plus per-turn cancellation, timeout, and output-token
  budget controls;
- adversarial coverage that keeps instructions found in repository content
  from expanding the agent's read-only tool permissions;
- embedded frontend assets in one native Go executable.

Architecture maps and generated DeepWiki-style documentation remain later
milestones. The full product definition and non-goals live in
[SCOPE.md](./SCOPE.md).

## Run

Requirements for a development build:

- Go 1.26 or newer
- Node.js 20 or newer
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

## JSON API and MCP

The JSON API is the capability boundary used by non-browser clients:

```text
GET /api/search?q=OpenFile&mode=literal&repo=RepoKarta&lang=Go&limit=100
GET /api/symbol?symbol=OpenFile&repo=RepoKarta&lang=Go&limit=100
GET /api/repositories
GET /api/file/{repository}?rev={commit}&path={path}&lines=1-200
GET /api/tree/{repository}?rev={commit}&path={directory}
GET /api/git/log/{repository}?rev={commit}&path={path}&limit=50
GET /api/git/diff/{repository}?from={commit}&to={commit}&path={path}&context=3
GET /api/conversations
GET /api/conversations/{conversation-id}
PATCH /api/conversations/{conversation-id}
DELETE /api/conversations/{conversation-id}
```

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

The stdio adapter is deliberately thin: it calls the JSON API and exposes no
MCP-only code capability. RepoKarta's own Codex and Claude harnesses use the
same tool definitions over its authenticated loopback HTTP MCP endpoint.

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

Model and effort are configured per provider before a conversation starts.
Leaving either field on its default lets the selected harness choose. RepoKarta
passes Codex effort through app-server turn configuration and Claude effort
through Claude Code's `--effort` option. Each turn also has a bounded timeout
and, for the direct API loop, an output-token budget.

Conversation titles, messages, citations, status, and usage are stored locally.
Uploaded conversation images are stored as exact RepoKarta-owned files outside
SQLite. Provider processes are intentionally disposable: after an idle period
RepoKarta closes them while retaining the transcript and an opaque provider
resume cursor. Reopening a chat first attempts provider-native resume and
falls back to a bounded replay of the durable transcript when that cursor is
stale.

## Storage and safety

RepoKarta binds to loopback by default and treats repositories as read-only.
It does not fetch, pull, checkout, reset, clean, build, or execute repository
code.

Provider harnesses receive a narrow local MCP surface. Codex is started with a
read-only sandbox and approvals disabled. Claude is started in plan mode with
shell, edit, write, notebook, and web tools disabled. The MCP endpoint requires
a per-process random bearer token.

RepoKarta-owned state is kept outside source repositories:

- Windows: `%LOCALAPPDATA%\RepoKarta\`
- macOS: `~/Library/Caches/RepoKarta/`
- Linux: the operating system user cache directory

SQLite stores catalogue, job, and conversation metadata and transcripts.
Conversation images, Zoekt shards, and future generated documentation are
filesystem-backed under RepoKarta's own data directory. Deleting a chat removes
its transcript and its exact owned image files.

## Validate and package

```sh
npm --prefix web run typecheck
npm --prefix web run build
go test ./...
```

Native build scripts write the executable to `dist/` and include required
third-party license texts under `dist/licenses/`:

```powershell
.\scripts\build.ps1
```

```sh
./scripts/build.sh
```

The pinned Zoekt revision and Windows portability decision are documented in
[docs/zoekt-windows-portability.md](./docs/zoekt-windows-portability.md).
