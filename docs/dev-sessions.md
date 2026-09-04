# Development Tmux Sessions

`bin/dev-session` manages one tmux session per development initiative. The
session name is the resolved slug, and the tool follows the workspace layout
from `AGENTS.md`.

## Starting a session

Pass a short name by default:

```sh
bin/dev-session start api-token-rotation
```

If a unique existing slug already matches that name, `start` resumes it. If no
existing slug matches, it creates today's slug. On June 6, 2026, a new
`api-token-rotation` session resolves to `2026-06-06-api-token-rotation`.

Use `--new` to force today's slug even when older matching sessions exist:

```sh
bin/dev-session start api-token-rotation --new
```

Use `--as-is` when the argument is already the slug:

```sh
bin/dev-session start 2026-06-06-api-token-rotation --as-is
```

The first window is named `dev` and contains three panes:

- left: login shell with cwd set to the workspace repository root; the helper
  launches `codex` in this shell, so exiting Codex returns to the shell prompt;
- right top: shell, with cwd set to `work/<slug>`;
- right bottom: shell, with cwd set to `worktrees/<slug>`.

Managed tmux panes and worktree windows receive these environment variables:

- `VPSFREE_DEV_SESSION_SLUG`;
- `VPSFREE_DEV_SESSION_WORKSPACE`;
- `VPSFREE_DEV_SESSION_WORK_DIR`;
- `VPSFREE_DEV_SESSION_WORKTREES_DIR`.

`start` creates `work/<slug>/plan.md`, `work/<slug>/state.md`, and
`worktrees/<slug>/` when missing. Existing plan and state files are never
overwritten. New state files begin with exact YAML front matter containing
`lifecycle: active`; that anchored field is the only lifecycle authority. An
exact slug that is present under `archive/`, tracked there, or found there in
repository history cannot be reused, even if its archive is absent from the
current working tree. Archive index or history lookup failures are refused
rather than treated as proof that the slug is unused.

```yaml
---
lifecycle: active
---
```

Fill in a substantive plan and initial state and commit them in the workspace
repository before the first project-code commit or external mutation. Keep
active tracking current in the working tree, but do not commit each plan,
review, test/CI, deployment, or status update separately.

Short initiatives normally have only the initial active tracking commit and the
final archive commit. For an initiative that remains unfinished at the end of
an active working day, at most one consolidated tracking-only checkpoint may be
committed for that day when material progress is worth preserving. This is a
ceiling, not a requirement, and inactive calendar days do not count. An
additional same-day checkpoint is reserved for a genuine ownership handoff or
an explicit user request. A pause until a future working day can justify that
day's consolidated checkpoint, but not a second one. Ordinary functional
commits are not tracking-only checkpoints.

## Attaching and syncing

```sh
bin/dev-session attach api-token-rotation
bin/dev-session sync api-token-rotation
bin/dev-session stop api-token-rotation
bin/dev-session remove api-token-rotation
bin/dev-session finalize api-token-rotation
bin/dev-session list
bin/dev-session current
```

Lookup commands accept a short name when it resolves to exactly one known slug
from `work/`, `worktrees/`, or managed tmux sessions. If multiple slugs match,
the command fails and prints the candidates. Use the full slug with `--as-is`
to avoid ambiguity.

`sync` creates one managed tmux window for every git worktree under
`worktrees/<slug>/*`. It removes only managed windows whose worktree path no
longer exists, and it leaves user-created windows untouched.

`stop` normally only kills the exact managed tmux session and leaves all files
and worktrees in place. After `finalize` has moved tracking into `archive/`, it
refuses to stop until that exact archive move and its terminal state are
committed.

`current` prints the active slug and nothing else. It resolves the slug from the
managed tmux session environment, the caller's exact managed tmux pane, or a
cwd under `work/<slug>` / `worktrees/<slug>`. It never falls back to tmux's
server-current session when called outside a tmux pane. It exits with an error
when no active session can be found or when those sources disagree. Codex
instances should run it before creating a new initiative slug. The helper
accepts session identity only when both `VPSFREE_DEV_SESSION_SLUG` and the
canonical `VPSFREE_DEV_SESSION_WORKSPACE` match. It filters managed sessions
owned by other workspaces from listing and short-name lookup.

Managed sessions created before workspace identity metadata was introduced are
not adopted automatically. Inspect and stop such a session manually after its
writers are quiet, then restart the same active slug with `start --as-is` so it
receives canonical metadata. Existing symlinked workspace paths are recognized
and normalized automatically.

`remove` cleans up a development session:

```sh
bin/dev-session remove api-token-rotation
```

It removes clean git worktrees whose `HEAD` is attached to a shared
`refs/heads/*` branch under `worktrees/<slug>/`, removes the empty worktree
group directory, and then kills the managed tmux session when one exists. This
order makes it safe to run from inside its own managed session: the session is
killed only after cleanup has completed. Branches are kept.
`work/<slug>/plan.md` and `state.md` are kept by default.

Worktrees with changes reported by ordinary `git status --porcelain` are
refused unless `--force` is passed. Detached worktrees and paths outside the
exact initiative group are always refused. Cleanup then delegates removal to
`git worktree remove`; if Git refuses a worktree, resolve the reason and retry.
Branches are retained:

```sh
bin/dev-session remove api-token-rotation --force
```

`remove` is for stopping or cleaning up an active session. It never deletes the
initiative tracking directory. The former `--all` option is intentionally not
supported because completed and abandoned initiatives must retain a durable
record.

## Finalizing an initiative

Use `finalize` only after the initiative is fully complete or explicitly
abandoned:

```sh
bin/dev-session finalize api-token-rotation
```

Before running it:

- set the lifecycle field in the exact YAML front matter at the start of
  `state.md` to `complete` or `abandoned`; lifecycle-looking text anywhere in
  the Markdown body has no effect;
- make sure plan and state have an earlier commit under `work/<slug>/`;
- make sure that history includes a commit whose front matter has
  `lifecycle: active` before the terminal transition;
- resolve all review, CI, merge, approval, deployment, and cleanup work owned by
  the session;
- stop shells, editors, builds, and background processes that can still write
  into an initiative worktree;
- remove credentials, caches, reproducible bulk captures, and transient
  outputs, keeping plan/state and intentionally durable evidence.

`finalize` performs all safety checks before cleanup. It refuses missing
tracking files, an active or ambiguous lifecycle, tracking without a prior
commit, an existing archive destination, mismatched or replaced tmux identity,
worktree changes reported by ordinary `git status --porcelain`, detached or
worktrees, worktrees outside the canonical `repos/*.git` bare clones, symlinked
paths or roots, and unknown entries in the worktree group. Cleanup for one slug
is serialized, and every worktree receives the same ordinary cleanliness and
path checks before any is removed. It then delegates each removal to non-force
`git worktree remove`, preserves the branches, and uses a same-filesystem,
atomic no-clobber move from `work/<slug>/` to
`archive/<slug>/`. If Git refuses a worktree for another reason, cleanup stops;
already removed worktrees remain available through their retained branches,
and the command can be retried after resolving the refusal. The helper requires
GNU `mv` with `--no-copy` and `--update=none-fail`; it verifies option support
before removing worktrees.

The per-slug lock serializes `dev-session` commands, not external writers. A
process that writes after the last cleanliness check can still race worktree
removal, so all worktree writers must be stopped before finalization.

The terminal lifecycle and final state do not need a separate commit before
`finalize`; the earlier committed active snapshot is sufficient. The helper
does not stage or commit. Inspect the reported move and commit only the exact
`work/<slug>/` and `archive/<slug>/` paths in the shared top-level repository,
including the final tracking content, as one archive commit. The managed tmux
session remains available for that commit. After the commit,
`bin/dev-session stop <slug> --as-is` verifies the terminal archive and clean
task paths before closing the exact workspace-owned tmux identity it resolves
at stop time.

## Worktree helpers

Create a project worktree through the canonical bare repository:

```sh
bin/dev-session worktree add api-token-rotation vpsadmin
```

This uses:

- bare repo: `repos/vpsadmin.git`;
- branch: `<slug>`;
- worktree path: `worktrees/<slug>/vpsadmin`;
- base ref: `origin/HEAD`, falling back to `origin/master`;
- `git fetch origin` before creation.

Before fetching or creating a branch, the helper resolves the repository and
requires it to be a bare clone directly under the canonical `repos/` root.
An in-root alias to another canonical bare clone is accepted; a symlink to an
external repository is refused.

Useful options:

```sh
bin/dev-session worktree add api-token-rotation vpsadmin --base origin/main
bin/dev-session worktree add api-token-rotation vpsadmin --name vpsadmin-master --branch master
bin/dev-session worktree add api-token-rotation vpsadmin --no-fetch
```

Remove a worktree without deleting its branch:

```sh
bin/dev-session worktree remove api-token-rotation vpsadmin
```

Worktrees with changes reported by ordinary `git status --porcelain` are
refused unless `--force` is used. Detached worktrees are always refused. Git
remains the authority for whether its non-force worktree removal can proceed;
resolve any refusal and retry, or use the explicit force option when discarding
the worktree is intentional.

## Test and automation options

Global options are accepted before the command:

```sh
bin/dev-session --workspace /tmp/ws --tmux-socket dev-session-test start demo --no-attach
```

Set `VPSFREE_DEV_SESSION_CODEX` to override the command used for the left pane.
Use `--no-codex` to start a shell in the left pane instead.
