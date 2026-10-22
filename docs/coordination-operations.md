# Coordination operations and recovery

For resource ownership, use the [claim guide](coordination.md) to acquire,
inspect, release or cancel a request. Claims survive disconnection and remain
held until released through the normal command. After interruption, reconcile
remaining local and remote effects. Release a repository claim after its safe merge critical
section. Promptly release each deployed physical machine once deployment and its
effects are verified and its complete deployed feature lineage is integrated,
as described in the [machine retention rule](coordination.md#machine-retention).
Do not wait for separate UI acceptance or release direction.
Existing snapshot-based operations keep the procedures below and the
[retained workflow guide](coordination-legacy.md).

The remaining sections describe legacy executor recovery. They do not start new
automatic workflows under a runtime with resource claims.

Read [the coordination contract](coordination.md) before operating its ledger.
The selected package installs both guides. Run `workspace-host coordination guide`
to read them from any checkout, including an unmerged deploy-first rollout.
Status reads do not reconcile, schedule or mutate anything. Use session commands
for exact owned operations; keep all recovery evidence and roots.

## Repository identity refusals

If acquisition refuses a repository identity, check the current session's
registration, `repos/<project>.git` and its single canonical SSH origin. Duplicate
names, unsafe projects and disagreement between `github`, origin or a configured
catalog remote require correction before a new request. Preserve an explicit
`main` default branch. Do not add a catalog entry merely to enable a normally
registered repository.

A queued mapping-change refusal leaves its original request waiting. Cancel it
and acquire a fresh request after reconciling the registration and clone. An
exact retry of a released request returns that receipt even if the clone was
removed. Package-compatibility refusals retain ownership: select a compatible
forward package instead of deleting claim records.

## Agent waiting

Prepare and verify work independently. Request a machine only near the first
prepared deployment. Submit approved work, then continue other useful work.
Use the integration driver's `--background` mode when combining approved merges
and deployments. Background handoff verifies the caller's managed Codex identity and runs a cheap
Git preflight. Record the returned run ID and distinguish preparation from
submitted merge jobs. End the turn when no independent
work remains and the recipient is verified. The worker continues and sends a native message for action or
review. Read status when that message arrives and acknowledge its exact handoff event
and generation before handling the action. Renew the five-minute acknowledgement
while work continues. A delivered native message alone does not establish pickup.
For an external conversation, use `integration-resume --foreground` under an
attached observer, or explicitly select `--background --to-session-lead` to hand
responsibility to the retained managed lead. Individual submitted jobs also
run independently and retain their existing state notifications.

Do not keep a turn open to poll reservations or ask the user to restart it when
another session owns a resource. Pause/resume and generation recovery are described
in [background continuation](integration.md#background-continuation). A busy
resource never authorizes a deployment around its reservation.

On `needs_refresh`, refresh the branch or input baseline, resolve conflicts,
repeat the required verification and review, record any preserved approval proof,
and prepare a new snapshot. The coordinator does not decide conflict semantics.
Use a new request ID and current revision to refresh the unclaimed job.

## Interrupted execution

Read `dev-session coordination status` and identify the exact attempt. The
private attempt directory retains `request.json`, `phase.json`, `result.json` and
`worker.lock`; logs are diagnostic only. The systemd unit is
`workspace-operation-ATTEMPT.service`. A running worker refuses manual
reconciliation. A retained exited unit is distinct from a running process.

Run `reconcile` on the original job. Git recovery proves exact remote ancestry.
The coordinator launches safe reconciliation in independent
`workspace-reconcile-ATTEMPT.service` units after a worker exits. Network probes
and forward recovery never run in the scheduler. A stalled probe retains its
original claim; other resources and notification delivery can continue. Failed
probes wait at least 30 seconds before another attempt. State
changes wake it immediately; a five-second tick covers missed wake signals.
Unprovable effects stay blocked and require operator evidence.
A local fast-forward with a failed push stays in place and holds its claim until
the remote effect is resolved. Profile recovery can complete only the original
selected candidate and saved preparation. Machine recovery requires the adapter's
verified receipt and both live-system/profile identities. Ambiguous rollback or
partial publication keeps the reservation and claim. Do not replay activation or
use a fresh job to hide the original uncertainty.

Archive and deletion hold notifications and cancel queued operations before
cleanup. Coordination receipts bind each ordinary lifecycle journal to exact
owner incarnations. Legacy archive completion releases that owner's machine
reservations in the same ledger transaction that records lifecycle completion.
This implementation behavior grants no permission to archive or bypass the
machine retention rule. Verify deployment and its effects and the complete
integrated deployed lineage before releasing its exact session-owned claim.
Release does not authorize archiving, deleting, stopping or cleaning a session.
The reservation stays held during failed or interrupted cleanup. Retrying a
completed archive receipt can release a hold retained by a predecessor executor;
the exact owner binding must still match. Deployment history and recovery
artifacts remain available after release.

Revival freezes its archive association before tracking changes. It reconnects
the same owner without reacquiring reservations or resuming canceled jobs. A
recreated slug has no claim to the previous owner's resources. Running or
uncertain deployments require their original recovery before archival. Deletion
retains its existing deployed-ownership hold. Exceptional transfers remain
separate recovery operations; there is no force-release command.

## First reporting-startup upgrade

A predecessor whose host accepts only `agents.*` launch overrides cannot decode
the reporting startup plan. The reviewed candidate's narrowly scoped
`coordination candidate-prepare` and `candidate-reconcile` entries keep the
selected generation's ledger authority, actual managed owner, held machine
reservation and original attempt. They do not create bootstrap owners or grant
approval. Follow [team reporting upgrade and recovery](team-reporting-upgrade.md)
for exact source/profile-token proofs, ordinary submission and exited-attempt
recovery. Normal selected-generation reconciliation applies after selection.

## Reviewed rebase lineage

For each deployed nonancestor head, `finish.replacements` contains:

```json
{
  "repository": "example", "target": "refs/heads/master",
  "from": "DEPLOYED_SHA", "fromBase": "OLD_BASE_SHA",
  "to": "FINAL_SHA", "toBase": "NEW_BASE_SHA",
  "relation": "patch_equivalent", "approvalId": "UUID",
  "evidence": {"kind": "review", "path": "work/SESSION/lineage.json", "sha256": "HEX"}
}
```

The session review receipt binds `schema: 1`, `ownerId`, `reservationId`, this exact
tuple in `mapping`, `decision: "accepted"`, the independent `reviewer` identity and checksummed
`artifacts` for `range_diff`, `verification` and `migration_review`. Each artifact
has `kind`, `path` and `sha256`. The lead records the independently reviewed result
under the normal mandatory-review procedure; the receipt is not a reviewer
authentication service.

`patch_equivalent` requires matching ordered stable patch IDs for ordinary
nonmerge commits, retained range-diff evidence and reviewed migration behavior.
`reviewed_change` requires a new integration approval for the final prepared
snapshot, explicit deployed-state compatibility review and a later successful
deployment of the replacement. Both ranges must belong to the same owned
repository and configured target. The replacement must be its current exact
final head and already integrated. Unused, conflicting and intermediate mappings
refuse. Retain the original deployed source and artifact history.

## First installation and threadless maintenance

A pre-contract installed package cannot create managed coordination state.
Use the reviewed candidate's private `coordination-bootstrap` with explicit
source, registered workspace/session, exact predecessor profile token and request.
`--prepare` returns the retained source and a complete request containing the
machine snapshot and profile artifact. Preparation builds and copies outside
claims. Execute the returned request from the exact candidate, retaining the
returned source. It admits a threadless bootstrap owner and machine reservation,
then runs machine activation followed by profile activation in independent
services. An exact execution retry recovers the original jobs.

The bootstrap owner has no notification destination and cannot submit new
unattended work. Later `status`, `reconcile`, `cancel` and `finish` can select its
already retained UUID with `--bootstrap-owner OWNER`. The active tracking
incarnation must still match. The selector cannot create an owner, approve or
submit jobs, or waive integration and completion proofs. Archived owners must be
explicitly revived first.

Profile changes remain forward-only after selection. Preserve the supported
coordination contract when preparing a recovery package. System recovery follows
the configuration owner's deployment procedure. Neither recovery nor bootstrap
authorizes a reboot or default-branch integration.

## Suspension and resumption

`workspace-host suspend` refuses active mutation workers and holds queued work
and reservations. `workspace-host resume` restores services from the exact
selected generation before enabling claims and delivery. It preserves all queued
state and does not select a package or release a machine. If suspension fails
after holding the ledger, it restores services and the prior suspension flag;
failed compensation keeps suspension visible and requires `resume`.

## Capacity and retained evidence

Protocol stdout remains complete within its bound, normally 256 KiB. Prepared
Nix environment parsing has a separate 16 MiB bound; large Git diffs are hashed
as streams. Preparation and worker diagnostics retain at most 4 MiB per operation with an initial section
and a tail, and continue draining after the cap. Private logs use mode 0600 and
never prove an outcome. Ledger/portal errors are bounded UTF-8 text.

The ledger has a 16 MiB limit, with at least 2 MiB reserved for recovery and a
conservative budget for outstanding results and cancellation/completion writes.
Admission checks the prospective ledger, including the new records and receipt,
plus the remaining growth of every pending job, undelivered notification and
unreleased reservation. Notification allowances survive job completion. Worker
results, reasons and request receipts have 16 KiB encoded JSON bounds; deployment
history allows one unknown-to-applied disposition per attempt. Completion accounts
for actual coverage and contracts, integration proofs and both frontier projections.
Native notification options and receipts use the existing checksummed artifact
store. The ledger retains their identifiers and a bounded event envelope.
Persisted file limits include the trailing newline. A result, artifact or native
request must fit its complete file bound before publication.
Worker requests seal the current operation; refresh history and prior results
stay in the ledger. Admission checks the complete worker envelope before taking
a resource claim. An oversized snapshot returns `needs_refresh` without an
attempt, so its owner can reduce it and refresh the job.
Those recovery requests may use the reserve after normal admission fills, and
have a 16 KiB request bound. Existing request IDs remain idempotent at capacity. New preparation/admission requires
at least 1 GiB of free space. Capacity refusal preserves existing authority.
Compact artifacts, snapshots, request receipts and history remain available.
Disposable source/merge-target worktrees and their dedicated Nix roots are
collected once their final required reference ends. Never delete unresolved
reservations or receipts to make room. See [payload collection](#payload-collection).

A claim without `request.json` predates any worker launch but still requires
operator recovery; this release does not automatically redispatch it. Likewise,
`finish` retains full integration proofs even when deployment was canceled before
activation. Early release without integration is deferred; lifecycle abandonment
or separately reviewed exceptional recovery remains required.


## Payload collection

Inspect the global per-user coordination store with:

```sh
workspace-host coordination gc --dry-run --json
workspace-host coordination gc --apply --json
```

The default is a preview. `actions` identifies collectible worktrees and root
links, `removed` records successful removals, and `preserved` explains holds or
live references. `retry` means the ledger changed; rerun the same scan. Output
contains paths and reasons, not environment dumps or credentials.

The coordinator requests collection after ledger changes and every five minutes
through `workspace-coordination-gc.service`. It keeps deletion out of scheduling.
Collection preserves pending jobs, live worker locks/services, all unreleased
reservation recovery, unfinished integration journals, pending notification
artifacts and shared references. An operation succeeding does not retire a
reservation still held for deployment review. The selected workspace profile,
its predecessor and generations required by live work keep their package roots.

A preparation not submitted within seven days expires. A new preparation renews
its lease; status and collection previews do not. Older unused preparations and
unsealed interrupted payloads receive a seven-day grace period at first applied
collection. Submitted snapshots stop independently pinning payloads after their
jobs/recovery obligations end. Fresh submission of an expired/consumed snapshot
without a live job reference requires preparation; an exact completed request
retry returns its saved receipt.

A `.keep` file, a native Git worktree lock, local changes, process references,
busy Git resources or unknown layouts preserve a payload. Collection uses Git
to remove registered worktrees; it does not delete user branches. Source
payload deletion is final, but original repository refs and compact evidence
remain available for a fresh preparation. Nix store paths are reclaimed only
by ordinary Nix GC.

`retention.json` is separate schema-1 bookkeeping for leases and interrupted
worktree removal. Existing ledger, snapshot and artifact formats are unchanged.
Collection shares generation admission with session creation. A separate payload
guard excludes new preparations, and package switches retain exclusive generation
admission. Routine collection does not prevent creating sessions;
legacy in-flight preparation commands also postpone collection. Deletion runs
outside ledger transactions. A changed ledger stops the current sweep, and
interrupted worktree removal resumes from its recorded exact path/common Git
directory. Do not hand-edit the bookkeeping to bypass a preservation reason.

Collection also takes each integration driver's existing lock without waiting.
An active driver postpones the sweep; unfinished journals retain hook dependencies
for their source heads between invocations. Once an integration is complete,
its registered, detached scratch worktrees below `integrations/<UUID>/` become
eligible. Collection verifies the recorded repository, workspace ownership,
cleanliness, process references and explicit holds. It preserves the compact
integration journal, feature branches and published history. Unknown files or
layouts survive for operator inspection.

Before removing integration scratch, the collector records its exact path,
common Git directory and HEAD in the journal's optional `payloadGcPending`
field. This is an admitted removal intent: retries continue deleting that exact
scratch checkout even if tracked files or `.git` have already disappeared.
The retained Git registration must still identify its original detached HEAD
and repository; a changed or missing registration preserves a surviving path.
If `.git` is already gone, Git cannot remove the surviving directory. The
collector finishes that admitted directory deletion, then asks Git to remove
its exact retained registration.
Do not reuse a checkout while its removal is pending. Process references and
`.keep` still postpone a retry, but remaining files are already admitted for
deletion rather than checked again for local changes. The field is removed
after completion.
Existing source retention bookkeeping is unchanged; older journal readers
preserve this additive field. An older collector leaves integration scratch and
this pending work untouched; resume collection with the newer runtime to finish.

Worker package roots also have a bounded lifetime. Notification roots are
released after their worker lock and service are idle. Pending messages and
their payload dependencies remain retained; the next dispatch roots its selected
package again. Background integration worker roots survive unfinished journals
and running services. Unknown root ownership or a mismatched package digest
prevents removal.

The collector prunes recognized workspace Nix profile generations through
`nix-env` under the existing profile-transition exclusion. It retains the
selected generation, one predecessor, all newer generations after a legacy
rollback, and package generations referenced by live operations, preparations or
recovery. It rechecks the selected profile identity before each removal and
does not change profile selection. Separate Codex generation roots remain.
Other users' or system Nix profiles are outside this policy. Workspace package
switches are forward-only; retaining the predecessor supports diagnosis and
recovery evidence, not automatic workspace rollback.

These policies release disposable references; they do not impose a byte limit
on active work. A live operation, held checkout, retained feature worktree or
running process can still keep an old closure. Nix GC decides when store paths
are reclaimable. Bulk archives included in a workspace Git input still multiply
its size until their copies and package roots become unreferenced.
