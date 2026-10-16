# Coordination operations and recovery

Read [the coordination contract](coordination.md) before operating its ledger.
The selected package installs both guides. Run `workspace-host coordination guide`
to read them from any checkout, including an unmerged deploy-first rollout.
Status reads do not reconcile, schedule or mutate anything. Use session commands
for exact owned operations; keep all recovery evidence and roots.

## Agent waiting

Prepare and verify work independently. Request a machine only near the first
prepared deployment. Submit approved work, then continue other useful work.
Use the integration driver's `--background` mode when combining approved merges
and deployments. Record the returned run ID and end the turn when no independent
work remains. The worker continues and sends a native message for action or
review. Read status when that message arrives. Individual submitted jobs also
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

Archive and deletion hold notifications and deployed ownership. Coordination
receipts bind each ordinary lifecycle journal to exact owner incarnations.
Completion persists before journal removal; retries after an interrupted removal
are idempotent. Revival freezes its archive association before tracking changes. Explicit revival
reconnects the same owner but does not resolve its deployment blocker. A recreated
slug has no claim to the previous owner's resources. An exceptional release or
transfer needs a separate reviewed operation with exact user direction and live
evidence; there is no force-release command.

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
reservation still held for deployment review. Existing profile generations and
their independent Nix roots continue to retain installed/rollback closures.

A preparation not submitted within seven days expires. A new preparation renews
its lease; status and collection previews do not. Older unused preparations and
unsealed interrupted payloads receive a seven-day grace period at first applied
collection. Submitted snapshots stop independently pinning payloads after their
jobs/recovery obligations end. Fresh submission of an expired/consumed snapshot
without a live job reference requires preparation; an exact completed request
retry returns its saved receipt.

A `.keep` file, local worktree changes, process references, busy Git resources or
unknown layouts preserve a payload. Collection uses Git to remove registered
worktrees; it does not delete user branches. Source payload deletion is final,
but original repository refs and compact evidence remain available for a fresh
preparation. Nix store paths are reclaimed only by ordinary Nix GC.

`retention.json` is separate schema-1 bookkeeping for leases and interrupted
worktree removal. Existing ledger, snapshot and artifact formats are unchanged.
The profile transition guard excludes new preparations and package switches;
legacy in-flight preparation commands also postpone collection. Deletion runs
outside ledger transactions. A changed ledger stops the current sweep, and
interrupted worktree removal resumes from its recorded exact path/common Git
directory. Do not hand-edit the bookkeeping to bypass a preservation reason.

Collection also takes each integration driver's existing lock without waiting.
An active driver postpones the sweep; unfinished journals retain hook dependencies
for their source heads between invocations. Integration scratch worktrees below
`integrations/<UUID>/` and owner-keyed notification worker roots remain outside
this collector. They are retained, including after completion; this command does
not establish a disk bound for those two layouts.
