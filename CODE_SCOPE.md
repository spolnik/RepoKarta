# RepoKarta Code workspace scope

Status: implemented for `0.101.0-dev`.

## Goal

Add a first-class **Code** tab where an authorized developer can describe a
change in natural language, let a configured coding provider inspect and
modify an isolated Git worktree, follow its visible actions, answer bounded
approval requests, and review the resulting diff.

This is the agent-coding part of Cursor, Claude Code, and Codex. It is not a
browser text editor, terminal, Git host, branch publisher, or pull-request
client.

## Product boundary

The existing Chat surface stays read-only. Code is a separate permission,
navigation destination, durable session type, provider profile, and worktree
lifecycle.

A Code session:

1. selects one repository already visible to the user;
2. pins an exact baseline commit;
3. creates a RepoKarta-owned shadow Git repository and linked worktree under
   the data directory;
4. starts Codex or Claude Code with that worktree as its only writable project
   directory;
5. streams assistant text, file/command actions, and approvals;
6. derives every displayed diff from Git;
7. may discard one changed file, discard the whole owned worktree, or finish
   as an isolated local commit.

The registered source checkout is never switched, staged, modified, linked to
the Code worktree, or used as the provider working directory.

## Authorization

RepoKarta has a `developer` role and a `repositories.write` permission.

- `reader` retains read-only product access.
- `knowledge-maintainer` adds Chat and Wiki generation.
- `developer` adds isolated Code sessions.
- `administrator` includes developer capabilities and administration.

Both conditions are required to create or use a Code session:

- the authenticated identity has `repositories.write`; and
- an administrator enabled Code worktrees for that repository.

Normal repository owner, visibility, user-grant, and group-grant checks still
apply. A Code session is owner-scoped; another user receives a not-found
response. Administrators do not gain access to another user's transcript or
worktree.

## Persistence

Schema version 25 adds:

- `repository_access.code_enabled`;
- `conversations.code_mode`;
- `code_sessions`;
- `code_actions`; and
- `code_approvals`.

The metadata backend stores session lifecycle, provider selection, exact
baseline, isolated branch name, transcript binding, visible action summaries,
single-use approval decisions, diff version checks, and the final isolated
commit.

Source state remains file-backed in the RepoKarta data directory. Conversation
records carry an explicit Code marker so a restarted Code transcript cannot be
resumed through the normal Chat endpoint.

## Worktree model

Each session owns:

```text
<data>/code-worktrees/<session-id>/
  ownership.json
  repository.git/
  checkout/
  hooks-disabled/
```

The manager fetches the selected exact commit from the registered repository
into a per-session bare shadow repository and creates
`repokarta/code/<session-id>` there. This avoids adding worktree metadata,
branches, refs, index locks, hooks, or configuration to the user's repository.

Ownership is proven by a marker containing the session ID, repository ID,
baseline, and branch. Cleanup rejects unknown IDs, missing markers, marker
mismatches, escaped paths, and symlink escapes. Git hooks are disabled for
agent and finish operations.

## Provider profiles

### Codex

RepoKarta uses the current Codex app-server protocol.

- process working directory: the owned checkout;
- approval policy: `on-request`;
- permission profile: `repokarta-code-workspace`;
- filesystem access: minimal runtime reads plus write access only to the
  session's explicit runtime workspace root;
- network access: disabled by the profile;
- command and file-change approval requests: surfaced in the Code tab;
- supported decisions in RepoKarta: approve once or decline.

### Claude Code

Claude Code uses a separate coding profile.

- process working directory: the owned checkout;
- permission mode: `acceptEdits`;
- allowed tools: Read, Edit, Write, Glob, Grep, and RepoKarta MCP;
- denied tools: Bash, NotebookEdit, Task, Agent, WebFetch, and WebSearch.

Claude may edit files but cannot execute repository commands in this profile.
Verification therefore comes from provider-visible safe capabilities and
RepoKarta's own Git diff/finish operations. Shell execution can be added later
only with a command policy that validates executable, arguments, working
directory, environment, timeout, output limits, and approval semantics.

Provider credentials remain ephemeral and are never stored in Code metadata,
argv, browser storage, transcripts, diffs, or audit details.

## Diff and review

The Code tab reuses RepoKarta's established source/diff presentation rather
than implementing a text editor.

The backend returns:

- exact baseline and current head;
- a stable diff version;
- changed paths and statuses;
- insertion/deletion totals;
- a bounded unified patch;
- truncation metadata; and
- bounded, path-contained file previews.

Tracked and untracked files are included. Binary files are identified without
being decoded. Context lines and total returned bytes are bounded.

Per-file discard and finish requests carry the displayed diff version. If the
workspace changed after review, the operation fails and requires a refresh.
This prevents a stale browser view from discarding or committing unseen
changes.

## Approvals and visible actions

The browser never receives hidden reasoning. It receives normalized,
persisted operational events:

- command started/completed;
- file change started/completed;
- bounded command output;
- approval requested/resolved;
- workspace created;
- diff snapshot; and
- lifecycle failure or completion.

Approvals are single-use and session-bound. Unknown, already-resolved, or
cross-session approval IDs fail closed. `acceptForSession` is not exposed by
the UI.

## HTTP surface

```text
GET    /code
GET    /api/code/sessions
POST   /api/code/sessions
GET    /api/code/sessions/{sessionID}
DELETE /api/code/sessions/{sessionID}
POST   /api/code/sessions/{sessionID}/turns
POST   /api/code/sessions/{sessionID}/interrupt
GET    /api/code/sessions/{sessionID}/diff
GET    /api/code/sessions/{sessionID}/file?path=...
POST   /api/code/sessions/{sessionID}/files/discard
POST   /api/code/sessions/{sessionID}/approvals/{approvalID}
POST   /api/code/sessions/{sessionID}/finish
```

Turn responses use bounded NDJSON streaming. Every route requires
`repositories.write`, then checks session ownership and repository policy.
Mutating operations retain the normal authenticated audit boundary.

## User experience

The Code tab provides:

- eligible repository, coding provider, model, and effort selection;
- exact-baseline override for advanced use;
- durable session history;
- a coding conversation without editor chrome;
- visible action and approval panels;
- baseline-to-workspace diff statistics and unified patch;
- bounded file preview;
- per-file discard;
- interrupt, finish, and discard controls; and
- explicit empty, unavailable, running, awaiting-approval, failed, finished,
  and discarded states.

An administrator enables Code per repository in **Repository access**. Role
selectors and SCIM/provider group mappings accept `developer`.

## Explicit non-goals

This scope does not:

- edit text directly in the browser;
- open an interactive shell or terminal;
- mutate the registered checkout;
- push branches or create pull requests;
- merge, rebase, or cherry-pick into a user repository;
- grant network access to coding providers;
- expose hidden chain-of-thought;
- allow arbitrary filesystem paths;
- let Chat inherit Code write permissions; or
- treat a provider-reported patch as authoritative over Git.

## Acceptance evidence

Completion requires:

- red-first identity and worktree tests;
- source repository status, worktree list, and commit unchanged by creation,
  editing, finish, and cleanup;
- traversal, symlink, marker, stale-diff, and unknown-cleanup rejection;
- durable SQLite and PostgreSQL-compatible metadata migrations;
- durable Code marker enforcement across provider restarts;
- coding-profile tests for Codex sandboxing and Claude tool restrictions;
- HTTP authorization and owner isolation tests;
- frontend typecheck, unit tests, and production build;
- full Go test suite and `go vet`;
- package/license validation; and
- an application version increment.
