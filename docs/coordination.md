# Resource claims and session notifications

The coordinator gives sessions exclusive ownership of repository and machine
resources. Sessions run their own commands, manage environments and hook trust,
verify results and release ownership. The coordinator records owners and waiters
and notifies a waiting session when its claim is granted.

## Acquire resources

Use a schema-1 request with a fresh lowercase UUID `requestId`:

```json
{
  "schema": 1,
  "requestId": "11111111-1111-4111-8111-111111111111",
  "label": "Publish reviewed feature",
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

After acquiring resources, use ordinary project tools in the session's worktrees.
Enter its development environment and run required hooks normally. The coordinator
creates no checkout, installs no dependencies and never signs configuration.
Repository ownership covers the canonical remote and common Git directory, so
participating linked worktrees serialize changes to shared Git and hook trust.
Machine aliases share one physical resource.

This is cooperative locking. Git and deployment tools do not intercept arbitrary
commands to enforce a claim. Every participating session must acquire resources
before their shared mutations, including changes to hook trust. Keep required
project checks and user approval gates in the session workflow. Build and copy
large artifacts before taking machine ownership when the tool supports that
separation. Shared tracking commits also retain their ordinary
`dev-session-tracking.lock` exclusion.

Verify effects using the tool's own logs and state. For example, Git can exit zero
after a failing post-merge hook. A claim certifies exclusive ownership only.

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
