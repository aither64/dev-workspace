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

- left: `codex`, with cwd set to the workspace repository root;
- right top: shell, with cwd set to `work/<slug>`;
- right bottom: shell, with cwd set to `worktrees/<slug>`.

`start` creates `work/<slug>/plan.md`, `work/<slug>/state.md`, and
`worktrees/<slug>/` when missing. Existing plan and state files are never
overwritten.

## Attaching and syncing

```sh
bin/dev-session attach api-token-rotation
bin/dev-session sync api-token-rotation
bin/dev-session stop api-token-rotation
bin/dev-session remove api-token-rotation
bin/dev-session list
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

`remove` cleans up a development session:

```sh
bin/dev-session remove api-token-rotation
```

It kills the managed tmux session when one exists, removes clean git worktrees
under `worktrees/<slug>/`, and removes the empty worktree group directory.
Branches are kept. `work/<slug>/plan.md` and `state.md` are kept by default.

Dirty worktrees are refused unless `--force` is passed:

```sh
bin/dev-session remove api-token-rotation --force
```

Use `--all` only when the durable notes should also be removed:

```sh
bin/dev-session remove api-token-rotation --all
```

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
