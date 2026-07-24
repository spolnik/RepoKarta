# Zoekt Windows portability decision

RepoKarta pins Zoekt at
`2b2ce2e398e6bee68d67143f567b6c6199340c7f` (2026-07-24).

## Reproduced upstream gaps

At that revision, importing `github.com/sourcegraph/zoekt/index` on
`windows/amd64` fails for two independent reasons:

1. `index/indexfile.go` is limited to Unix build tags, leaving
   `NewIndexFile` undefined on Windows.
2. `index/builder.go` calls `unix.Umask`, which is unavailable on Windows.

## Integration choice

RepoKarta uses a direct, pinned module with a small local portability patch.
This is preferred over a helper process because it preserves the one-binary
product and avoids requiring WSL or Docker. Zoekt remains behind
`internal/search.Engine`, so a future upstream patch, helper, or alternative
build can replace it without changing the application and HTTP layers.

The patch:

- implements read-only Windows shard mapping with `CreateFileMapping` and
  `MapViewOfFile`;
- makes process umask handling platform-specific, using a zero mask on Windows
  where file ACLs govern access;
- synchronously closes mapped shards when a Windows directory searcher is
  explicitly closed, because Windows will not remove an open mapped file.

The pinned source and exact delta are documented in
`third_party/zoekt/REPOKARTA_UPSTREAM.md`.

## Validation

`internal/search/zoekt/adapter_test.go` creates a temporary Git repository,
builds a native Zoekt shard, searches it with repository/language/path/file
filters, verifies the commit-pinned result, closes the mapping, and lets Windows
delete the temporary index. The same adapter builds on macOS and Linux using
Zoekt's upstream mmap implementation.
