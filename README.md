# RepoKarta

RepoKarta is fast, local-first code search for the Git repositories already on
your laptop. Point it at a ghorg directory (or any repository root), and it
discovers, incrementally indexes, and searches committed source without
modifying a worktree.

The current implementation delivers the first useful M1 slice:

- regular, linked-worktree, and bare Git repository discovery;
- origin, default revision, HEAD, scan, and index metadata in SQLite;
- native Zoekt indexing on Windows amd64, macOS arm64, and Linux;
- incremental reindexing when a repository HEAD changes;
- literal, regular-expression, and native Zoekt query modes;
- repository, language, path, and file filters;
- bounded, cancellable result sets with highlighted source matches;
- commit-pinned source pages and copyable file-and-line citations;
- live indexing state through Server-Sent Events;
- loopback Host and Origin validation;
- embedded frontend assets in one native Go executable.

AI questions, architecture maps, and generated documentation remain later
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

## Storage and safety

RepoKarta binds to loopback by default and treats repositories as read-only.
It does not fetch, pull, checkout, reset, clean, build, or execute repository
code.

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

Native build scripts write the executable to `dist/`:

```powershell
.\scripts\build.ps1
```

```sh
./scripts/build.sh
```

The pinned Zoekt revision and Windows portability decision are documented in
[docs/zoekt-windows-portability.md](./docs/zoekt-windows-portability.md).
