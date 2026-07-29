# Frontend contracts and reproducible assets

RepoKarta keeps the embedded browser application and the Go HTTP server in one
mechanically checked contract.

## Generated API types

Load-bearing JSON responses are declared in
`internal/apicontract/contract.go`. Generate the browser types with:

```sh
go generate ./internal/apicontract
```

The generated file is `web/src/generated/api-contract.ts`. Do not edit it by
hand. `npm --prefix web run typecheck` regenerates it before checking the
browser, and CI fails if generation changes the committed file.

`web/src/api-client.mjs` is the only native `fetch` boundary. It applies common
headers, maps structured API errors, and performs minimal runtime shape checks
for health, provider, structural-artifact progress, dependency-refresh
progress, and Wiki responses. A response that does not match its contract is a
visible request failure rather than partially trusted page state.

## Streaming and background completion

Chat NDJSON is reduced one line at a time by a tested reducer. A malformed line
is logged and skipped; later events continue to render. Artifact and dependency
pollers retry transient failures with bounded exponential backoff. Completion
fetches freshly rendered HTML and replaces only the named page region, so
background work does not discard composer, filter, or navigation state.

Server-provided browser links pass through one HTTP(S)-only guard. Invalid,
`javascript:`, and `data:` navigation targets lose their `href` and are marked
disabled.

## Reproducible embedded assets

Every `npm --prefix web run build` removes `web/dist/assets` before invoking
Vite. The tracked `web/dist/.keep` remains available for fresh-clone Go
embedding, while the packaged binary receives exactly one asset generation.
The native build scripts, release packagers, CI, and Homebrew all invoke this
same build contract.

Package-smoke CI extracts the produced Windows or macOS archive, starts that
exact binary against disposable empty storage, and probes `/`, `/healthz`, and
`/assets/app.js`.
