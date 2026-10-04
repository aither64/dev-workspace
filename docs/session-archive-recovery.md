# Archive cleanup and recovery

Archival discovers Git-registered worktrees beneath the exact session
group, including checkouts in containers such as `integration-targets/` and
detached auxiliary checkouts. The canonical bare repositories and the shared
workspace repository establish ownership. A checkout inside another checkout,
foreign or unregistered repository, symlink, locked or prunable registration,
unknown file, or dirty checkout blocks cleanup.

Every registered feature branch remains an obligation even after its checkout
has been removed. Complete archival proves exact local and origin feature heads
merged into the recorded origin default. An additional attached feature branch
needs the same proof without an invented initial-base exception. Default-branch
checkouts may lag the fetched default. A detached checkout must be reachable from
that fetched default. Abandoned archival skips publication and merge proof, but
still requires an existing shared branch, tag or origin tracking ref to retain
each detached commit. Archival creates no rescue ref and never removes branches.

Before cleanup, the operation records checkout, Git administration, common
repository and container identities, exact heads and selected retention refs in
a sealed inventory. It checks that inventory, cleanliness, tracking projection
and retained-head proof before removal and each remaining destructive phase. Cleanup uses
non-force `git worktree remove` and `rmdir` for verified empty containers. Stop
external writers first; helper locks do not serialize arbitrary Git commands,
editors or native clients.

## Retrying an interrupted operation

Run the same installed `dev-session archive SLUG --as-is` command again. Include
`--abandoned` only when it matches the recorded mode. Keep both the ordinary
schema-2 journal and its private schema-1 `SLUG.archive-cleanup.json` sidecar under
`worktrees/.locks/`. Their immutable operation identity, root, tracking digest
and timestamp must agree. Never remove receipts to bypass a refusal.

The command writes the sidecar before the ordinary journal. An interruption between
those writes leaves a prepared intent. The matching archive command repeats the
normal confirmation and preflight, verifies the exact source tracking and sealed
inventory, then publishes the original journal. Other mutations and package
transitions remain blocked until that retry completes.

If Git removal succeeded before its progress write, retry accepts the checkout
only after proving both the path and Git registration absent. A recreated path,
even at the same HEAD, is refused. Retention proof continues after removal: a
deleted or rewound ref that no longer reaches the sealed commit blocks recovery.

After the final journal phase, the command marks the sidecar complete before
removing the journal. If interruption leaves only that completed receipt, retry
proves the committed archive and retained conversation/team/runtime retirement
before clearing it. It does not start another archive or repeat the tracking
commit. A prepared intent with moved or changed source tracking is refused.

## Legacy archive recovery without a cleanup sidecar

The temporary compatibility adapter accepts valid schema-2 journals written
before sidecar support. Before the tracking move, it retains the old immediate,
attached checkout layout and known manifest obligations. It does not adopt new
nested, detached or unregistered checkouts into that operation. Later phases
retain the existing exact tracking-tree, head and conversation checks.

New operations always create a sidecar, including an empty cleanup inventory.
Finish pending cleanup through the selected capable executor before switching
packages. An older executor that does not understand the sidecar is unsupported;
the narrow candidate recovery that delegates to a selected predecessor refuses
such an operation.

Generic lifecycle maintainers own the old no-sidecar adapter. Remove it only after
an explicit inventory proves there are no unfinished dependent journals or
intents and no supported predecessor operation, retained package or upgrade
recovery path can create another. A successful rollout or an elapsed date does
not satisfy that gate. Schema-2 journals themselves remain a supported format.
