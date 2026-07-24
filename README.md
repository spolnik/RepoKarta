# RepoKarta

RepoKarta is fast, local-first code search for the Git repositories already on
your laptop. Point it at a ghorg directory (or any repository root), and it
discovers, incrementally indexes, and searches committed source without
modifying a worktree.

The current implementation delivers the M1 search slice and the first complete
M2 conversation slice:

- regular, linked-worktree, and bare Git repository discovery;
- origin, default revision, HEAD, scan, and index metadata in SQLite;
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
- live indexing state through Server-Sent Events;
- loopback Host and Origin validation;
- streamed, multi-turn questions through either a local Codex or Claude Code
  harness;
- reuse of the provider's existing ChatGPT/Codex or Claude login without
  collecting subscription credentials;
- one provider-neutral Go conversation interface;
- a JSON code-intelligence API used by the UI and protocol adapters;
- authenticated loopback HTTP MCP plus a stdio MCP adapter with read-only
  `list_repositories`, `search_code`, `get_file`, and `list_tree` tools;
- read-only Codex sandboxes, Claude plan mode, and disabled mutation/shell
  tools;
- authoritative citation chips recorded from the exact MCP tool results rather
  than trusting a model to reproduce source URLs;
- ephemeral conversations that are not written to RepoKarta's database;
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
`PATH` (or `CTAGS_COMMAND` is set). If it is not available, `sym:` searches
return a machine-readable and visible warning instead of a misleading silent
zero.

## JSON API and MCP

The JSON API is the capability boundary used by non-browser clients:

```text
GET /api/search?q=OpenFile&mode=literal&repo=RepoKarta&lang=Go&limit=100
GET /api/repositories
GET /api/file/{repository}?rev={commit}&path={path}&lines=1-200
GET /api/tree/{repository}?rev={commit}&path={directory}
```

Search responses always include `returned_files`, `matching_files`,
`estimated_total_files`, `total_files_exact`, `truncated`, `files_skipped`,
`shards_skipped`, and the effective `limit`. A consumer can therefore tell
whether a fleet-wide negative answer is complete.

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

## Ask with Codex or Claude

RepoKarta detects local provider CLIs when it starts:

- Codex uses `codex app-server` and your existing `codex login` session.
- Claude uses the Claude Code `stream-json` protocol and your existing
  `claude auth login` session.

RepoKarta never receives the provider password or subscription credential. It
starts the installed harness as a child process and gives it a temporary bearer
token for RepoKarta's local MCP endpoint. The browser shows whether each
provider is installed and authenticated.

Override CLI discovery when needed:

```powershell
go run ./cmd/repokarta serve `
  -codex-command C:\path\to\codex.exe `
  -claude-command C:\path\to\claude.exe `
  C:\Work\ghorg
```

The model field is optional; leaving it empty uses the provider's default.
Conversation state lives only in memory for this first slice and disappears
when RepoKarta stops.

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

SQLite stores catalogue and job metadata. Zoekt shards live under `indexes/`;
future generated documentation will live under `docs/`.

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
