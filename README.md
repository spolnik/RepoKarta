# RepoKarta

Local code search, architecture maps, and living documentation for a directory
of Git repositories.

RepoKarta is designed to run as a native executable on a developer laptop. It
uses Go for the local server, Zoekt for code indexing and search, SQLite for
metadata, and a server-rendered HTMX interface with Vite and Tailwind CSS.

The product definition, architecture decisions, milestones, and non-goals live
in [SCOPE.md](./SCOPE.md).

## Current skeleton

The initial executable:

- discovers Git worktrees beneath a selected root without modifying them;
- records the discovered repositories in a local SQLite database;
- serves a small embedded HTMX interface;
- exposes `GET /healthz`;
- shuts down gracefully.

Code indexing, search, AI-assisted questions, architecture visualization, and
generated documentation are intentionally planned as subsequent milestones.
The current upstream Zoekt indexing package is not natively buildable on
Windows; the required adapter/portability spike is documented in `SCOPE.md`.

## Development

Requirements:

- Go 1.26 or newer
- Node.js 20 or newer

Install frontend dependencies and build the embedded assets:

```sh
npm --prefix web install
npm --prefix web run build
```

Run the Go tests:

```sh
go test ./...
```

Start RepoKarta against a directory containing local repositories:

```sh
go run ./cmd/repokarta serve /path/to/ghorg
```

On Windows:

```powershell
go run ./cmd/repokarta serve C:\Work\ghorg
```

By default the application listens on `http://127.0.0.1:7331`.

## Build native executables

Windows:

```powershell
.\scripts\build.ps1
```

macOS or Linux:

```sh
./scripts/build.sh
```

The resulting executable is written to `dist/`.
