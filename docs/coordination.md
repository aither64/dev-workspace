# Resource claims and session notifications

The coordinator serializes integration into shared branches and reserves machines
for deployment. Sessions develop independently in their own worktrees. Repository
claims are short merge locks; machine claims are deployment reservations. Sessions
run commands, verify effects and explicitly release ownership. Queued requests
notify their waiting session when granted.

## Acquire resources

Use a schema-1 request with a fresh lowercase UUID `requestId`:

```json
{
  "schema": 1,
  "requestId": "11111111-1111-4111-8111-111111111111",
  "label": "Merge approved feature into master",
  "repositories": ["project"],
  "machines": []
}
```

Repositories must be registered to the session and present in the installed
catalog. Machines accept configured names, IDs or aliases. Supply at least one
resource. The complete request fits 16 KiB; labels fit 512 bytes. Keep credentials
out of labels and requests.

```sh
dev-session coordination acquire SESSION --as-is --request claim.json --json
dev-session coordination claim-status SESSION --as-is --id CLAIM_UUID --json
```

The command returns immediately with `held` or `waiting`. Run the shared mutation
only while the exact claim is `held`. Requests acquire all resources together;
a waiting request owns none of them. Earlier eligible conflicting requests have priority,
while disjoint resources can be used concurrently. A request blocked by held
resources cannot prevent their holder from acquiring other free resources. A queued grant sends a native
notification to the session's retained conversation. Read authoritative status
before acting on a notice. A notice supplies no merge or deployment approval.
Threadless sessions can use the local CLI but have no notification destination.

An exact request-ID retry returns its existing claim, including after release.
Changed input or another owner refuses. Use a fresh ID for intentional new
ownership. A second claim does not implicitly reuse resources held by the same
owner. Request the complete set initially or use separate disjoint claims.

## Execute and release

Independent feature work needs no repository claim. This includes edits, commits,
rebases, dependency updates, feature-branch pushes, hook preparation, builds,
tests, review and waiting for approval. A merge waiter can continue such work in
its own worktree. Git retains its ordinary file and ref locks.

Prepare the reviewed feature, clean target checkout, environment and hook trust
before requesting a merge lock. Obtain integration approval first. Acquire only
the repository being integrated, immediately before these steps:

1. Recheck the exact feature head, clean target checkout and current remote target.
2. Perform the approved fast-forward merge and required hooks.
3. Push normally and prove the resulting remote ref.
4. Release immediately, before CI monitoring, records, artifact registration or
   disposable checkout cleanup.

If the target moved, release and reconcile outside the claim. If a hook fails or
needs intervention, stop commands and inspect effects before release, then repair
and verify outside ownership. Git can exit zero after a failing post-merge hook;
inspect hook results before pushing. A rejected push or interrupted command needs
an actual ref check. Retain ownership for an uncertain shared mutation only while
actively reconciling its effects. Do not wait for operator input with a known-safe
repository held. Never hold merge locks during development, tests or review, and
do not reserve several repositories for a sequential integration batch.

Linked worktrees share Git configuration. Repository tools own configuration and
plugin trust; the coordinator never signs, changes trust or runs commands. A trust
mismatch returns to preparation and does not justify a development-wide claim.
Required hooks and signature checks remain enabled.

Machine reservations are separate: build and copy artifacts before ownership when
supported, then acquire only the physical machine near activation. Keep its
reservation through deployment, live review and feedback. Do not bundle repository
claims into a deployment reservation.

This is cooperative locking. Tools do not intercept arbitrary commands. The claim
certifies exclusive integration or deployment ownership, not command success.
Shared tracking writes use their short `dev-session-tracking.lock`; they do not
need a durable repository claim for independent session bookkeeping. Integration
from that same shared checkout takes the repository claim first, then tries the
tracking lock without waiting. If it is busy, release the claim and retry after
the writer finishes. Hold both only across the final target check, merge/hooks,
push and remote proof. Preserve unrelated dirty paths and index entries in the
shared checkout; disposable targets must be clean.

```sh
dev-session coordination release SESSION --as-is --id CLAIM_UUID --json
dev-session coordination cancel-claim SESSION --as-is --id CLAIM_UUID --json
```

Release settles a held claim. Cancellation settles a waiting request. Both are
owner-only and idempotent; cancellation cannot release held resources. Machine
ownership can remain held during human review, feedback and repeated deployment.
Release requires no integration proof and grants no merge approval. Session
completion still follows its normal exact-head integration requirements.

For workspace profile activation, hold a claim covering the configured profile
machine and run ordinary `workspace-host switch --source PATH` with
`DEV_WORKSPACE_CLAIM_ID=CLAIM_UUID` and the exact `DEV_SESSION_SLUG` and
`DEV_SESSION_WORKSPACE` identity. Workspace-host checks ownership under its
transition locks. Package compatibility, activation journals and forward recovery
remain workspace-host responsibilities.

## Interruption and lifecycle

Claims survive idle agents, disconnections and service restart. They have no
automatic expiry. The coordinator does not observe command execution or infer
completion. After interruption, inspect the original local and remote work,
stop anything still mutating the resources, then explicitly release. Do not
release ownership merely because SSH or a client process exited.

Held claims block lifecycle cleanup until their owner releases them. Lifecycle
fencing cancels waiting requests. Suspension retains ownership and waiting
requests, with no new grants while suspended. Status reads do not grant resources
or run recovery. The Operations page shows up to 128 claims, with up to eight repository and machine
names per claim; the CLI retains complete records. Machine
claims also appear in the session's Reservations tab.

## Storage and compatibility

Bounded private schema-1 `claims.json` records coexist with the original ledger.
The existing admission mutex protects both old and new resource ownership.
Admission reserves a queued claim's future notification capacity in the ledger.
Notifications use the existing durable outbox; failed delivery retries without
changing or releasing claims. A released claim stays released even if an older
notification arrives later.

Packages advertise `share/dev-workspace/claim-contract.json`. Once claim records
exist, selecting a package without that capability refuses. Compatible selection
preserves live repository/machine mappings. Recovery selects a compatible forward
package; never delete records to bypass this check.

New work uses `acquire`. Existing snapshots, jobs, integration journals,
reservations and uncertain effects retain their formats, recovery payloads and
original release requirements. Legacy jobs and reservations exclude conflicting
claims, including the same owner's legacy reservation. Retained preparation,
mechanical refresh and tracking publication acquire repository ownership around
their mutations. Collection preserves payloads in a claimed common Git directory. New claims cannot release
legacy reservations. A retained job may refresh its prepared snapshot by supplying
its exact `legacyJobId`; this does not permit submitting an independent new job.
Use the [retained workflow guide](coordination-legacy.md),
[integration guide](integration.md) and [operations guide](coordination-operations.md)
for those records.
