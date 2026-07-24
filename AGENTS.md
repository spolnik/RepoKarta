# RepoKarta agent instructions

## Zoekt license compliance

These requirements apply whenever changing `third_party/zoekt`, updating the
pinned Zoekt revision, or changing build and release packaging:

- Zoekt is distributed under Apache License 2.0. Preserve
  `third_party/zoekt/LICENSE` verbatim.
- Preserve all relevant upstream copyright, patent, trademark, and attribution
  notices in vendored source files.
- Every upstream Zoekt file changed by RepoKarta must carry a prominent comment
  near its existing header stating that RepoKarta contributors changed it. Do
  not rely only on a central changelog to mark modified files.
- New RepoKarta-authored files under `third_party/zoekt` must carry an
  appropriate copyright and Apache-2.0 license header.
- Keep `third_party/zoekt/REPOKARTA_UPSTREAM.md` synchronized with the exact
  upstream commit and the complete RepoKarta delta.
- Compare the vendored tree with that exact upstream commit before claiming
  that files are unmodified.
- When updating Zoekt, check whether upstream added a `NOTICE` file. If it did,
  preserve its applicable notices in source and binary distributions.
- Never distribute a RepoKarta binary without the full Zoekt Apache-2.0 license
  text. Build and release tooling must continue placing it at
  `dist/licenses/zoekt-Apache-2.0.txt` or an equivalent path inside every
  release package.

After relevant changes, verify the modification notices, the upstream delta,
and the contents of the produced distribution directory.
