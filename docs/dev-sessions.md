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
overwritten. New state files start with `- Lifecycle: active`. An exact slug
already present under `archive/` cannot be reused.

Fill in a substantive plan and initial state and commit them in the workspace
repository before the first project-code commit or external mutation. Keep
active tracking committed at meaningful plan, review, test/CI, deployment, and
handoff checkpoints.

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

`stop` only kills the managed tmux session. It leaves all files and worktrees in
place.

`current` prints the active slug and nothing else. It resolves the slug from the
managed tmux session environment, the current managed tmux session, or a cwd
under `work/<slug>` / `worktrees/<slug>`. It exits with an error when no active
session can be found or when those sources disagree. Codex instances should run
it before creating a new initiative slug; if it prints a slug, continue in that
session.

`remove` cleans up a development session:

```sh
bin/dev-session remove api-token-rotation
```

It removes clean git worktrees under `worktrees/<slug>/`, removes the empty
worktree group directory, and then kills the managed tmux session when one
exists. This order makes it safe to run from inside its own managed session:
the session is killed only after cleanup has completed. Branches are kept.
`work/<slug>/plan.md` and `state.md` are kept by default.

Dirty worktrees are refused unless `--force` is passed:

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

- set the single `- Lifecycle:` marker in `state.md` to `complete` or
  `abandoned`;
- make sure plan and state have an earlier commit under `work/<slug>/`;
- resolve all review, CI, merge, approval, deployment, and cleanup work owned by
  the session;
- remove credentials, caches, reproducible bulk captures, and transient
  outputs, keeping plan/state and intentionally durable evidence.

`finalize` performs all safety checks before cleanup. It refuses missing
tracking files, an active or ambiguous lifecycle, tracking without a prior
commit, an existing archive destination, unmanaged tmux ownership, dirty git
worktrees, and unknown entries in the worktree group. It then removes clean
worktrees, preserves their branches, moves `work/<slug>/` to
`archive/<slug>/`, and kills the managed tmux session last.

The helper does not stage or commit. Inspect the reported move and commit only
the exact `work/<slug>/` and `archive/<slug>/` paths in the shared top-level
repository.

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

Dirty worktrees are refused unless `--force` is used.

## Test and automation options

Global options are accepted before the command:

```sh
bin/dev-session --workspace /tmp/ws --tmux-socket dev-session-test start demo --no-attach
```

Set `VPSFREE_DEV_SESSION_CODEX` to override the command used for the left pane.
Use `--no-codex` to start a shell in the left pane instead.
