# Coordinated workspace operations

The user-profile coordinator schedules three operations: fast-forward Git
integration, machine activation and workspace profile activation. Sessions keep
their own branches, reviews and conversations. They prepare independently and
submit exact reviewed snapshots under existing user authorization.

Builds, input updates, rebases, conflict resolution and verification run before
resource claims. An admitted worker holds only the resources its mutation needs.
Different repository targets and machines can proceed concurrently. Eligible work
uses FIFO order; waiting on approval or dependencies does not block unrelated work.

Workers wait for temporary contention on package admission, session admission and
shared Git locks within their original attempt. They recheck ownership, approval
and prepared evidence after acquiring admission, and Git baselines after acquiring
the tracking lock. A busy lock does not require snapshot refresh.

Merge preparation enters the repository's declared Nix shell, including its
shell initialization, before retaining the hook environment. Bundler-based
Overcommit dependencies and the runtime SSH transport are checked before a job
can be admitted. Dependency installation happens in the retained source outside
claims. Target checkout creation and merge/push hooks use that same prepared
environment, with the runtime transport path retained. Workers install no
dependencies while holding Git resources.

## Machine ownership during review

A reservation belongs to a session and physical machine. It survives successful
deployment, idle agents, browser closure, UI review, feedback and repeated fixes.
It also covers workspace profile selection on that machine. Other sessions can
build and merge while their deployment waits.

Normal release requires every exact final owned feature head to be integrated,
all deployed input revisions to be integrated, settled operations and deliveries,
unchanged live identities, and an explicit declaration that deployment and review
work is finished. Integration alone does not release a machine. Deployment or
review does not supply merge approval.

Archive and deletion retain deployed or uncertain reservations as recovery
blockers. Revival preserves ownership. An uncertain effect retains its execution
claim too; a dead process, failed SSH command or timeout cannot prove no effect.
The first implementation refuses exceptional release without integration. Resolve
it through an explicitly authorized, reviewed recovery operation rather than
editing or deleting the ledger.

## Session commands

Commands select the registered workspace normally and require the exact slug:

```text
dev-session coordination status SESSION --as-is --json
dev-session coordination prepare SESSION --as-is --request FILE --json
dev-session coordination approve SESSION --as-is --request FILE --json
dev-session coordination reserve SESSION --as-is --request FILE --json
dev-session coordination submit SESSION --as-is --request FILE --json
dev-session coordination refresh SESSION JOB --as-is --request FILE --json
dev-session coordination cancel SESSION JOB --as-is --request FILE --json
dev-session coordination reconcile SESSION JOB --as-is --json
dev-session coordination finish SESSION RESERVATION --as-is --request FILE --json
workspace-host coordination status --json
```

JSON requests use `schema: 1` and a caller-generated lowercase UUID `requestId`.
An exact retry returns its retained receipt; changing input under the same ID
refuses. Updates use `expectedRevision`. Exit 0 reports acceptance or status, not
necessarily successful execution. Exit 2 means invalid input, 3 stale evidence or
baseline, 4 missing authorization or ownership, 5 recovery required, and 75 a
temporary admission conflict.

Preparation takes `kind`, session evidence (`kind`, `path`, SHA-256 `sha256`), and
the registered `repository` for a merge or catalog `target` for a machine. Profile
preparation uses the session's registered workspace branch. Evidence lives under
the owning session's active tracking directory. Preparation retains committed
sources, artifacts and a content-addressed snapshot; it returns that snapshot.

Approval records `kind` (`merge` or `deploy`), exact `scope`, `reviewedSnapshot`,
and `source: {kind: "user_instruction", reference, quote}`. Merge scope entries
contain `repository` and full `target` ref. Deployment entries contain canonical
`machine`, `actions` (`switch`, `profile_activate`) and `reboot: false`. Quote the
actual user direction. An optional `iterationScope` must be explicitly authorized;
the prepared snapshot's `changeScope` must match it. A digest without an existing
owner-prepared snapshot cannot become an approval.

A reservation request contains `target`, `mode` (`deploy_first` or `integrated`),
`approvalId` and `integrationObligations` (`repository`, full `target`). Current
registrations are also frozen as obligations. Removing a registration cannot
erase them. Reservations are requested near the first prepared deployment, and
are granted only with its eligible first operation.

Submission contains `kind`, `dependsOn`, `approvalId`, `reservationId` and the
complete returned `snapshot`. Dependencies name earlier jobs of the same owner.
Merge-only jobs use a null reservation. A null approval stores waiting work and
cannot execute. `refresh` supplies `expectedRevision`, a fresh snapshot and its
applicable approval; it cannot replace an admitted or uncertain attempt.

`workspace-host switch --source PATH` prepares outside locks, finds an exact
retained approved profile snapshot and machine reservation, submits it once and
waits outside locks. It preserves an existing job's dependency graph. If another
deployment is pending, use explicit submission to specify order. This command
cannot manufacture review or approval. Coordinated source selection requires a
clean committed source; `workspace-host prepare --source PATH --json` can also
inspect a local source before it is committed.

## Completion and cumulative deployments

`finish` takes `expectedRevision`, `deploymentWorkFinished: true`, a nonempty
acceptance `evidence` reference and `finalHeads` entries with `repository`, full
`target` and exact `head`. Those heads must match current owned registrations and
be contained in freshly fetched targets. Unregistered inputs use their sealed
catalog `defaultTarget` and canonical clone; the coordinator proves their actual
deployed revisions are already integrated. It never invents feature registrations
for unchanged dependencies.

Completion retains both proof categories. The machine frontier records final
owned heads and actual deployed unowned revisions separately for system and
profile activation. Later prepared deployments must include those integrated
changes. Immediately before machine activation, the worker also fetches the
configuration repository target and proves that the prepared source includes it. An older prepared deployment returns `needs_refresh` rather than undoing
previously completed work.

Rebased deployed features need exact reviewed replacement lineage in
`finish.replacements`. It binds the repository/target, deployed and final ranges,
applicable integration approval and a checksummed independent review receipt.
See [operations and recovery](coordination-operations.md). Original deployed
artifacts and source roots remain retained.

## Runtime and compatibility

Ruby owns a private schema-1 ledger under the selected runtime's state root.
Transactions atomically replace fsynced JSON under one short mutex. Commands and
physical-lock waits run outside that mutex. Workers publish separate immutable
attempt requests and phase/result receipts. Each runs in an independent systemd
service, so restarting the portal or coordinator does not stop it.

Package candidates must support the retained coordination contract, adapters and
machine mappings. Old packages that ignore coordination state cannot be selected
once that state exists. Profile failure after selection recovers forward to the
same approved candidate; the original machine reservation remains held.

The installed `coordination.json` catalog comes from optional `coordinationConfig`
in `lib.mkPackage`. It owns immutable adapter executables, exact machine selectors,
aliases, canonical SSH repositories, full default targets and flake `inputPaths`.
Input mappings are keyed by source repository and contain lists of input paths.
The separate extension catalog and session manifest formats remain unchanged.
The pure Ruby `lib.coordinationCommandLibrary` is also used by packaged adapters
for bounded protocol capture and diagnostics; it does not import the ledger.

The Operations page is an authenticated, bounded, read-only view. It shows
machine ownership, blocked jobs and notification delivery separately. An idle
owner still reserves its machine. The workspace menu links to `/operations`
above Automatic archival. Reserved machines appear as cards with their owning
session and the reason for the hold. Running and waiting operations appear
separately from recent completed results. System configuration deployment and
workspace application updates have distinct labels. A completed step does not
mean that review or integration has ended or that the machine has been released.
IDs, source revisions and diagnostics are available under Technical details.
Session-list badges mark held machine reservations, including review and recovery;
queued requests and brief execution claims do not receive badges. Ownership is
matched to the retained active or archived session incarnation. The portal's
`reservationHolders` summary covers all current holders before history truncation.
The established tracking observation identity also matches cached holders to
session cards, so a replacement session cannot inherit the old badge. Visible
pages refresh every five seconds. Failed reads preserve the last known status
with a warning for the same incarnation; a successful read confirms release.

A single-machine badge uses its configured short name, such as BUILDHOST
RESERVED. Multiple-machine badges show the distinct count. The tooltip lists
every held machine and its status. The session's Reservations tab, between
Clusters and Artifacts, shows its held machines, pending requests and related
operations. Links to `#reservations` open that tab. Session results are matched
to the same observation identity as badges; a recreated session cannot inherit
an earlier incarnation's history. The tab reads only while it and the document
are visible. Read failures retain the last successful view with a warning.

### Operation dates

Dates describe when the coordinator records creation, a state change, completion
or the first reservation grant. They do not measure the exact moment a remote
worker changed the machine. Dates use UTC in status responses and the browser's
local timezone in the interface. Missing dates appear as Time unavailable.

Optional private `coordination/timing.json` metadata records entity UUID,
revision and state. It is published after each relevant authoritative ledger
commit, under the existing mutex, and is exposed only when those values match
the current record. Creation time is recorded only for new records; reads and
idempotent retries never populate it. Existing records stay undated until a new
event occurs. A reservation's grant time remains unknown if it predates timing
support. File modification times and worker phase receipts do not backfill dates.

The ledger schema, deployment history, worker envelopes and receipts remain
unchanged. Older packages ignore timing metadata. A predecessor can therefore
continue an admitted operation across forward package selection; any timing
that becomes stale is omitted. Missing, corrupt, oversized or unwritable metadata
affects dates only. A crash after ledger publication can leave an event without
a date. Timing failure never rolls back successful work or affects scheduling,
ownership, completion proofs or scheduler wake-up.

Notifications use the existing Codex submission ledger, exact retained thread,
model, effort and lead policy. Busy threads receive steering; idle threads receive
a new turn. The coordinator preserves message IDs and reconciles lost replies.
Each snapshot has one retained notification per reported state; repeated resource
waits or reconciliation reuse that event. Native options and receipts live in
immutable checksummed artifacts, so large retained instructions do not fill the
operation ledger. Retries verify and load the original options.
A crash after recording submission but before the native RPC can leave delivery
ambiguous. That event remains visible and retries only reconciliation; it never
creates another message ID or conversation to escape the ambiguity.

Selected-package registration reconciles services under the transition lock and
refuses a pending package transition or held machine review reservation before
changing the registry. Private profile activation still requires its exact claim.
Managed lead and member technical bindings point to the installed guide without
changing retained role instructions, model, effort or access.

Lifecycle preparation runs outside the ledger mutex. Publication of a new
ordinary journal or archive cleanup sidecar checks capacity and binds its exact
operation while holding that mutex. Competing admissions cannot consume the
checked space before publication. Existing bindings remain recoverable at the
admission ceiling.

The guarantees cover cooperating commands in one user runtime. The local operator
is trusted; arbitrary external Git, confctl, SSH and old store binaries can bypass
coordination. Remote clients remain subject to the portal's authentication and
authorization rules.
