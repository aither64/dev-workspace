# Development Tmux Sessions

The user-profile `dev-session` command manages one tmux session per development
initiative. It selects a registered workspace from the current directory; use
`--workspace NAME` before the subcommand to select one explicitly. The session
name is the resolved slug, and the tool follows the workspace layout from
`AGENTS.md`.

## Starting a session

Pass a short name by default:

```sh
dev-session start api-token-rotation
```

This works anywhere under the registered root, including `work/` and
`worktrees/`. From another directory, or whenever you want to be explicit,
name the workspace before the subcommand:

```sh
dev-session --workspace example-workspace start api-token-rotation
```

If only one workspace is registered, it is the fallback outside its root. Once
two or more are registered, an outside call requires `--workspace`.

For a new Codex session, the command asks for the first request before it
creates tracking files or tmux panes. In a script or another noninteractive
shell, put that request in a file:

```sh
dev-session start api-token-rotation --goal-file request.txt --no-attach
```

The request file is required only while creating a shared conversation. An
existing session resumes without asking for the request again.

An unfinished initiative may predate the portal or lose its old process. Stop
the old tmux session after checking that its writers are quiet, commit the
active `plan.md` and `state.md`, and restart the exact slug with a fresh
request:

```sh
dev-session start 2026-06-06-api-token-rotation --as-is \
  --goal-file request.txt --no-attach
```

The helper preserves `plan.md` and `state.md` byte for byte. It creates a new
shared conversation and registers existing canonical worktrees in the portal
manifest. The plan and state must match the committed workspace tree.
`portal.yml` must be absent from both the working tree and the committed tree;
an existing manifest is never adopted as retained tracking. Restarting always
creates a Codex conversation; `--no-codex` is refused. A directory that cannot
be proven as a canonical, attached worktree is left untouched, reported as a
warning, and omitted from the manifest until it is repaired. A conflicting
registration still stops synchronization. The helper also refuses a
conflicting managed session. This is a restart of the same active initiative,
not a new initiative or a recovery of the old conversation.

If a unique existing slug already matches that name, `start` resumes it. If no
existing slug matches, it creates today's slug. On June 6, 2026, a new
`api-token-rotation` session resolves to `2026-06-06-api-token-rotation`.

Use `--new` to force today's slug even when older matching sessions exist:

```sh
dev-session start api-token-rotation --new
```

Use `--as-is` when the argument is already the slug:

```sh
dev-session start 2026-06-06-api-token-rotation --as-is
```

When the workspace portal is available, `--model` and `--effort` select the
initial Codex settings. The browser offers the same choices from the App Server
model catalog.

The first window is named `dev` and contains three panes:

- left: login shell with cwd set to the workspace repository root; the helper
  launches `codex` in this shell, so exiting Codex returns to the shell prompt;
- right top: shell, with cwd set to `work/<slug>`;
- right bottom: shell, with cwd set to `worktrees/<slug>`.

Managed tmux panes and worktree windows receive these environment variables:

- `DEV_SESSION_SLUG`;
- `DEV_SESSION_WORKSPACE`;
- `DEV_SESSION_WORK_DIR`;
- `DEV_SESSION_WORKTREES_DIR`;
- `DEV_SESSION_PORTAL_BASE_URL`, the reusable portal origin;
- `DEV_SESSION_URL`, the resolved link for this session.

Do not use `DEV_SESSION_URL` as the input for another session. Its
value already contains the current slug. The registry-backed dispatcher passes
the base URL explicitly and exports both values into each managed pane.

`start` creates `work/<slug>/plan.md`, `work/<slug>/state.md`,
`work/<slug>/portal.yml`, and `worktrees/<slug>/` when missing. When
`workspace-portal` is installed, it also creates a Codex App Server thread,
records its ID, assigns the session name, and sends the initial request. It
waits until the App Server has persisted the rollout and verifies the exact
first user message before it opens the terminal Codex client. Once that check
passes, the thread ID is authoritative and the helper resumes that exact
thread while refreshing its working directory and runtime environment. While
an exclusive creation journal
is still `creating` and its initial goal is unsent, the helper reconciles the
unique working directory instead. It resumes the sole candidate, or replaces a
recorded memory-only thread that vanished during an App Server restart. It
refuses multiple candidates and never replaces a ready thread. Before the one
allowed initial `turn/start`, it records a durable attempt marker. A retry of
the same unmaterialized thread fails closed until exact matching history appears
or an App Server restart permits a fresh creation replacement. Existing plan
and state files are never overwritten. New state files begin with exact YAML
front matter containing `lifecycle: active`; that
anchored field is the only lifecycle authority. An
exact slug that is present under `archive/` or active workspace state cannot be
reused. The helper also checks the top-level Git index and history, so removing
an archive from a later checkout does not remove its slug tombstone. It disables
repository-configured process hooks for these read-only checks and fails closed
when the archive history cannot be read.
If a running session was created without a shared portal thread, stop it before
starting the same active slug with portal interaction; the helper refuses to
attach a new browser thread to an old terminal conversation.

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
dev-session attach api-token-rotation
dev-session fork api-token-rotation alternate-approach
dev-session sync api-token-rotation
dev-session stop api-token-rotation
dev-session archive api-token-rotation
dev-session archive api-token-rotation --abandoned
dev-session revive api-token-rotation
dev-session delete api-token-rotation
dev-session list
dev-session current
dev-session url
```

Lookup commands accept a short name when it resolves to exactly one known slug
from `work/`, `worktrees/`, or managed tmux sessions. If multiple slugs match,
the command fails and prints the candidates. Use the full dated slug directly
to avoid ambiguity. The `--as-is` option is only needed when a command must
treat an input as a literal slug before that slug is known to the workspace.

`fork` creates a new dated session with a native copy of the source Codex
conversation. It copies no worktrees, tracking content, artifacts, or
development clusters. Use `--model` and `--effort` to override the inherited
Codex settings.

`sync` creates one managed tmux window for every git worktree under
`worktrees/<slug>/*`. It removes only managed windows whose worktree path no
longer exists, and it leaves user-created windows untouched.

`stop` only kills the exact managed tmux session and leaves all files and
worktrees in place. It asks you to type the exact session slug before making
the change. Use it when you want to pause terminal access without archiving the
initiative.

`current` prints the active slug and nothing else. It resolves the slug from the
managed tmux session environment, the caller's exact managed tmux pane, or a
cwd under `work/<slug>` / `worktrees/<slug>`. It never falls back to tmux's
server-current session when called outside a tmux pane. It exits with an error
when no active session can be found or when those sources disagree. Codex
instances should run it before creating a new initiative slug. The helper
accepts session identity only when both `DEV_SESSION_SLUG` and the
canonical `DEV_SESSION_WORKSPACE` match. It filters managed sessions
owned by other workspaces from listing and short-name lookup.

Managed sessions created before workspace identity metadata was introduced are
not adopted automatically. Inspect and stop such a session manually after its
writers are quiet, commit its active tracking, then restart the same slug with
`start --as-is` and a new initial request. Existing symlinked workspace paths
are recognized and normalized automatically.

`url` prints the permanent portal page for the selected initiative. It accepts
the same short-name and `--as-is` forms as the other lookup commands. The page
continues to work after archival because the portal scans both `work/` and
`archive/`.

`delete` discards a development session after explicit confirmation:

```sh
dev-session delete api-token-rotation
```

The command requires an interactive terminal and asks for one `y/N`
confirmation. There is no noninteractive confirmation flag. The command
independently inventories each canonical worktree owned by the exact session,
including its repository identity, branch, head, and dirty state, then retires
the matching Codex thread, releases configured development clusters, removes
the worktrees, stops the managed tmux session, removes runtime authority, and
moves the tracking directory and creation journal to private recovery storage.
It records each completed phase in a private journal, so rerunning the same
command safely continues an interrupted deletion instead of repeating
irreversible work.
Each persisted phase is printed in plain language. The browser uses the same
journal to show progress and elapsed time while the command continues in the
background.
The journal is also a slug tombstone: starting, reviving, attaching, changing
worktrees, or starting new cluster work for that slug is refused until the
original `delete` command finishes. Thread retirement records its intent before
contacting Codex, durably records a unique cwd-bound thread before archiving it
when creation lost its ID, and recognizes that exact thread when it is already
archived. A known thread
must remain reachable; an unavailable portal or App Server stops deletion for a
later retry instead of orphaning the conversation. Successful retirement prunes
that thread's durable send and queue attempts.
Recovery state is stored under:

```text
$XDG_STATE_HOME/dev-workspaces/removed/<workspace-id>/
```

The session disappears from active and archived discovery. Git branches are
retained. The recovery directory is private to the user and contains metadata
that identifies the original workspace and slug. It must be outside the
tracking directory, creation journal, initiative worktrees, and cluster state
or socket directories; deletion refuses an `XDG_STATE_HOME` that would place
recovery data inside state it will destroy.
If the tracking directory was
already committed, `delete` fetches `origin/master`, requires the shared
checkout to be a compatible `master`, rechecks the fetched remote immediately
before the commit, and commits only the exact tracking deletion. It preserves
unrelated staged and unstaged changes and does not push.
This prevents a fresh checkout from rediscovering a successfully deleted
session.

Worktrees with changes reported by ordinary `git status --porcelain` are
refused unless `--force` is passed. Detached worktrees and paths outside the
exact initiative group are always refused. Cleanup delegates removal to
`git worktree remove`; if Git refuses a worktree, resolve the reason and retry.
Missing or stale `portal.yml` repository entries do not block deletion when the
actual worktrees can be proven to belong to canonical workspace repositories.
Symlinks, unmanaged entries, foreign repositories, path escapes, and ambiguous
ownership are always refused. `--force` permits dirty worktrees and interrupts
an active Codex turn. If a non-forced attempt stops at an active turn, rerunning
it with `--force` records a one-way upgrade to that authorization and resumes
the same journal. Forced deletion still requires the interactive `y/N`
confirmation:

```sh
dev-session delete api-token-rotation --force
```

Use `delete` only when you mean to discard the whole session. Completed and
abandoned initiatives normally use `archive`, which preserves their durable
record in the portal.

## Archiving and reviving an initiative

Use `archive` only after the initiative is fully integrated and has no
session-owned work, or after the user explicitly abandons it:

```sh
dev-session archive api-token-rotation
dev-session archive api-token-rotation --abandoned
```

Before archiving completed work:

- merge every registered feature branch's exact final head into its configured
  remote default branch;
- make sure plan and state have an earlier commit under `work/<slug>/` whose
  state front matter has `lifecycle: active`;
- resolve all review, CI, merge, approval, deployment, and cleanup work owned by
  the session;
- stop shells, editors, builds, and background processes that can still write
  into an initiative worktree;
- remove credentials, caches, reproducible bulk captures, and transient
  outputs, keeping plan, state, and intentionally durable evidence.

The command asks for one yes/no confirmation. It writes `lifecycle: complete`
or `lifecycle: abandoned` itself, so no separate terminal tracking commit is
needed. The browser offers the same two modes with one confirmation dialog.
Agents must not treat a completed response, a handoff, "finish the work", or
"implement the plan" as permission to archive a session.

For completed work, `archive` fetches every registered feature branch and
default branch. The local and remote feature tips must be identical, and that
exact commit must be an ancestor of `origin/<default_branch>`. The command
reports every branch whose merge status cannot be proven. A squash merge or
partial cherry-pick does not satisfy this rule. Coordination-only initiatives
with no registered branches remain valid. Legacy live worktrees without
`portal.yml` are inferred from their canonical bare repository and checked as
branches. `--abandoned` skips only this merge proof. A retry fetches and
reproves the exact heads stored in the archive journal before every remaining
destructive phase; another merged commit on the feature branch cannot replace
the head originally approved for archival.

Before changing state, `archive` proves the thread has no active turn, pending
request, or queued message; verifies the tracking commit and clean attached
worktrees; checks the atomic move; and verifies that the shared workspace can
make an exact-path commit. It then quiesces the terminal client, releases both
development cluster types, records immutable repository heads, removes
worktrees without force, atomically moves tracking into `archive/`, commits
only the tracking transition, retires the Codex thread, and removes the tmux
session and authority. Branches are retained. The host-wide transition gate
prevents portal mutations and cluster starts from racing this operation.

Each irreversible phase is journaled in private runtime state. If a command or
deployment interruption stops the operation, run the same `archive` command
again with the same mode. It resumes completed phases without repeating them.
The CLI prints phases as they are persisted. The browser exposes the same phase
and offers Retry when an archive, delete, or revive operation fails or pauses.
The journal binds the exact projected archive tree and retained Codex thread;
retry refuses changed lifecycle, manifest, artifacts, or conversation identity
before it commits tracking or retires the runtime.
The durable journal also blocks workspace package changes, unregister,
suspension, Codex reconciliation, and cluster mutations outside the exact
lifecycle-owned cleanup. The per-slug lock serializes helper commands, not
external writers, so all worktree writers must be stopped first.

If an initiative needs more work after archival, revive it:

```sh
dev-session revive 2026-06-06-api-token-rotation --as-is
```

`revive` asks for one confirmation, with a stronger warning for an abandoned
initiative. That confirmation becomes part of the transaction, so a retry
after tracking has moved does not ask again or consult the obsolete archive
location. The archive move and terminal tracking must already be committed.
Revive refuses a conflicting
active directory, worktree group, live session, dirty tracking transition, or
ambiguous identity. It atomically moves `archive/<slug>` back to `work/<slug>`,
changes the lifecycle to `active`, removes `finalized_at` and stale
`final_head_sha` values, verifies the full restored tracking tree before the
commit, commits only that transition, and preserves branch, base, repository,
and Codex identity.

When the archive retains a Codex thread, revive restores and starts that exact
conversation without forking it or resending the initial request. It does not
recreate worktrees. Legacy archives without `portal.yml` get a fresh shared
conversation through the same durable thread-creation recovery used by new
sessions, so a lost App Server response cannot create a second thread. Adding
retained feature branches through the normal worktree command reconstructs
their registration. Supply `--base REF` if a retained branch's intended base
cannot be inferred uniquely. Revive is journaled and retryable in the same way
as archive.

## Worktree helpers

Create a project worktree through the canonical bare repository:

```sh
dev-session worktree add api-token-rotation api-service
```

This uses:

- bare repo: `repos/api-service.git`;
- branch: `<slug>`;
- worktree path: `worktrees/<slug>/api-service`;
- base ref: `origin/HEAD`, falling back to `origin/master`;
- `git fetch origin` before creation.

The helper also records the canonical project identity, GitHub repository,
feature branch, default branch, and starting base commit in `portal.yml`.
Removing an individual worktree or using bulk cleanup records its last commit
before cleanup. Archival verifies the project identity and records the last
commit of every remaining worktree, so archived pages retain trustworthy
immutable comparison links.

Before fetching or creating a branch, the helper resolves the repository and
requires it to be a bare clone directly under the canonical `repos/` root.
An in-root alias to another canonical bare clone is accepted; a symlink to an
external repository is refused.

Workspace implementation work uses the top-level repository through its
reserved project and worktree name:

```sh
dev-session worktree add api-token-rotation workspace
```

This creates `worktrees/<slug>/workspace` from the top-level repository. The
helper verifies that the worktree belongs to that exact repository and applies
the same cleanliness, attached-branch, final-commit, and non-force removal
checks used for project worktrees. The name `workspace` is reserved for this
repository, so an independent project cannot use it as a worktree alias. Other
non-bare repositories remain refused.

Useful options:

```sh
dev-session worktree add api-token-rotation api-service --base origin/main
dev-session worktree add api-token-rotation api-service --name api-service-master --branch master
dev-session worktree add api-token-rotation api-service --no-fetch
```

Remove a worktree without deleting its branch:

```sh
dev-session worktree remove api-token-rotation api-service
```

Worktrees with changes reported by ordinary `git status --porcelain` are
refused unless `--force` is used. Detached worktrees are always refused. Git
remains the authority for whether its non-force worktree removal can proceed;
resolve any refusal and retry, or use the explicit force option when discarding
the worktree is intentional.

## Runtime ownership

The workspace flake installs the public commands and user systemd units in a
dedicated user Nix profile. A private registry maps workspace names to roots
and hostnames. Per-workspace portal, App Server, authority, and tmux state lives
below `$XDG_RUNTIME_DIR/dev-workspaces/`; the public command derives those
paths and does not require callers to repeat them. Run
`dev-session validate` to validate every persisted portal entry, including its
plan, anchored lifecycle, active/archive placement, and manifest, before
deployment.

Host-specific runtime options are private implementation details fixed by the
dispatcher. Callers can select only a registered workspace, not replace its
runtime paths. The App Server uses the Codex package bundled with the current
application profile. `workspace-host` checks its protocol and model catalog
before activation and retains the tested Codex store path with each profile
generation. Profile switches and rollbacks gate new mutations, quiesce native
terminal clients, verify all threads are idle, restart App Server and portal
pairs, and restore the clients. A compatible profile update that finds an
active turn is retried every five minutes. Use `--no-codex` to start a shell in
the left pane instead.

New conversations use GPT-6 Astra with `xhigh` reasoning. When an explicitly
chosen model does not support `xhigh`, its advertised default reasoning effort
is used. An explicit reasoning choice always takes precedence.

`--goal-file FILE` provides the initial Codex request and seeds the Goal
section in a new plan. It is required when a noninteractive caller creates a
shared conversation. `--json` prints the resolved slug, portal URL, Codex
thread ID, and identity-bound tmux attach command. `--exclusive` requires a
goal file and records its digest in a workspace-local creation journal before
creating initiative state. It may resume a manifest
whose creation state is `creating` or `ready` only when the complete request
identity matches. The unique `work/<slug>` directory is used to reconcile one
matching App Server thread, but App Server does not offer an exactly-once
creation key. A pending retry resumes the sole candidate, creates a replacement
when an App Server restart provably left none, and refuses multiple candidates.
It sends the initial goal directly only when metadata identifies the expected
fresh, idle, turnless thread; accepted or ambiguous active turns fail closed.
An incomplete tmux session is replaced only when its creation environment
identifies the exact workspace and slug and its creation identity matches the
nonce durably recorded before tmux was started. The nonce is removed from the
journal after the managed runtime authority is published and creation becomes
ready. The journal remains in
the lock directory after the manifest reaches `ready`, allowing the
HTTP result to be replayed without repeating a known initial turn. The portal
passes a full dated slug with `--as-is`, preserving request identity across
midnight.

Fork creation uses the same identity rule through a short-lived fork journal.
It records the validated source thread before creating destination tracking, so
an interrupted fork can resume without source tracking and can adopt or remove
only the tmux session carrying the recorded identity. Explicit overrides are
resolved against the source thread's inherited settings before the journal is
published; both the request and its complete resolved model/reasoning pair are
recorded. Retrying a short destination name reuses the journaled dated slug even
after midnight, and package transitions wait until the fork finishes.
Journal-owned destination tracking and writer temporary files are reconciled
idempotently after an interruption, but retry refuses modified tracking or
unexpected files.
Starting tmux for an existing session uses a corresponding short-lived start
journal. This makes stopped-session restarts retryable without authorizing a
same-name tmux session created by another process.

The private package implementation requires the authority directory, tmux
socket, Codex command and socket, client version, and portal command to be
present as absolute paths. The registry-backed dispatcher supplies them to
both terminal users and the portal, so they share one command and one session
model.

See [Workspace portal](workspace-portal.md) for the browser interface, manifest
format, security model, private CA, and deployment responsibilities.
