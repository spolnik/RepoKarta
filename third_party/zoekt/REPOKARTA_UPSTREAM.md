# RepoKarta Zoekt pin

This directory is based on
[`sourcegraph/zoekt`](https://github.com/sourcegraph/zoekt) commit
`2b2ce2e398e6bee68d67143f567b6c6199340c7f` from 2026-07-24.

RepoKarta keeps this local module because upstream does not publish a stable v1
API and that revision does not compile its indexing package on Windows.

The RepoKarta portability delta is intentionally small:

- `index/indexfile_windows.go` maps shards with the Windows file-mapping API.
- `index/umask_unix.go` and `index/umask_windows.go` make builder permissions
  platform-specific.
- `index/builder.go` no longer imports `golang.org/x/sys/unix` directly.
- `search/shards.go` closes mapped Windows shards synchronously when an explicit
  directory searcher shutdown occurs.

All other files are an unmodified copy of the pinned upstream revision. The
upstream Apache 2.0 license is retained in `LICENSE`.
