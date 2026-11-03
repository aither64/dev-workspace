# Resource claims and session notifications

The coordinator serializes integration into shared branches and reserves configured
machine targets for deployment. Sessions develop independently in their own worktrees. Repository
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

Repositories must be registered to the session. Their canonical clones live in
`repos/<project>.git`; the reserved `workspace` registration uses the workspace
Git root. The coordinator validates the real common Git directory and canonical
SSH origin. A registration's optional `github` identity and any configured catalog
remote must agree with that origin. Repository names must be unambiguous.
Registering a repository does not require a catalog update or package rebuild.
Schema-1 machine names, IDs and aliases remain available for retained
operations through the compatibility catalog. New machine requests use schema 2
below. Supply at least one resource. The complete request fits 16 KiB; labels fit
512 bytes. Keep credentials out of labels and requests.

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

Aliases sharing a common Git directory and separate clones sharing a canonical
remote contend for the same repository. An explicit registered `default_branch`,
including `main`, remains authoritative; catalog defaults do not replace it.
Claims select resources, carry no target branch and grant no merge approval.
Queued requests retain their original resource keys and refuse changed mappings.

An exact request-ID retry returns its existing claim, including after release
or removal of the clone. Changed input or another owner refuses. Use a fresh ID for intentional new
ownership. A second claim does not implicitly reuse resources held by the same
owner. Request the complete set initially or use separate disjoint claims.

## Configured machine identity

A schema-2 machine request selects an exact machine from a registered configuration
repository and names an installed protocol-1 adapter:

```json
{
  "schema": 2,
  "requestId": "22222222-2222-4222-8222-222222222222",
  "label": "Deploy configured target",
  "repositories": [],
  "machines": [{
    "repository": "configuration",
    "selector": "region/machines/development",
    "adapter": "confctl"
  }]
}
```

Use the same `acquire`, `claim-status`, `cancel-claim` and `release` commands.
Machine acquisition alone does not reserve the configuration repository for a
merge. Ordinary new machines need no catalog entry or package rebuild.

The runtime reads machine metadata at the registered feature branch's exact
committed revision.
It supplies the canonical local repository path to the adapter; the request
cannot supply a path, revision or shared-label override. The resolver uses local
locked inputs without builds, remote fetching, lockfile writes or target probes.
A missing branch, flake or prepared input prevents admission. The runtime never
falls back to the default branch.

By default, identity is the canonical registered SSH repository origin and exact
configured machine name. Clones, registration aliases, workspaces and branches
therefore contend for the same target. Different repositories can each contain
`vps` without contending for the same lock. If configuration supplies
`metaConfig.labels.dev-workspace.lock-id`, its nonempty string value selects a
shared identity instead. Configure the same stable label wherever intentional
cross-repository overlap needs one lock. Names and labels must be nonempty,
case-sensitive UTF-8 strings of at most 256 bytes, without control characters or
surrounding whitespace. An explicit null or malformed label prevents resolution.

The visible identifier is
`machine:github.com/example/configuration:region/machines/development`.
Shared labels display as `machine:shared:LABEL`. Reserved delimiters and other
UTF-8 bytes are percent-encoded. Display text is not a claim input. Internal keys
hash the canonical structured identity; routing, hostnames, SSH addresses,
commits and local Git paths do not enter that identity. The coordinator does not
discover physical overlap between different logical identities.

New claims and waiting grants check each source pair, its canonical origin and
exact configured selector, against all held and waiting bindings and accepted
legacy aliases. The adapter ID must match, and the resources must be equivalent
under the package policy. A changed label or adapter cannot bypass an existing
hold or waiter by producing another key. Accepted legacy aliases retain this
requirement even without an active old claim. Terminal records retain no ownership.
A waiting request keeps its original structured binding; a newer commit with the
same mapping may be granted, while its initial source revision remains audit
evidence. An approved compatibility bridge makes namespaces contend without
transferring or rebinding a held claim.

Before an intended committed deployment batch, check its exact held typed binding
once:

```sh
dev-session coordination verify-machine SESSION --as-is --id CLAIM_UUID \
  --repository configuration \
  --selector region/machines/development --json
```

This read-only local check returns the claim ID, canonical resource, identifier
and current source revision. It grants no ownership, changes no record and
performs no deployment. Build, artifact, live-state and interruption checks still
follow the ordinary deployment procedure. Run them under the original claim and
release requirements.

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
4. Release immediately, before CI monitoring, record updates, handoff or
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
reservation until deployment verification and integration are complete under the
release rule below. Do not bundle repository claims into a deployment
reservation.

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
ownership follows the deployment and integration rule below. Release grants no
merge approval. Session completion still follows its normal exact-head
integration requirements.

For workspace profile activation, hold a claim covering the package policy's
exact profile machine identity and run ordinary `workspace-host switch --source PATH` with
`DEV_WORKSPACE_CLAIM_ID=CLAIM_UUID` and the exact `DEV_SESSION_SLUG` and
`DEV_SESSION_WORKSPACE` identity. Workspace-host checks ownership under its
transition locks. Package compatibility, activation journals and forward recovery
remain workspace-host responsibilities.

## Machine retention

Promptly release each deployed physical machine once both conditions are met:

1. Deployment and its effects have been verified, with no unfinished or uncertain
   command effects.
2. Its complete deployed feature lineage is integrated into the approved remote
   default branches, including deployed dependencies.

The owning agent runs the normal release command without waiting for separate
UI acceptance or release direction. No background worker or timer is involved.
Failed or uncertain deployment and missing integration evidence keep the machine
held. Idle time and turn completion do not establish either condition.
Default-branch integration still needs separate explicit approval. A later
deployment requires fresh machine ownership.

Use existing rollout and review receipts for exact system/profile store paths,
complete source/input graphs, canonical repository/target mapping and machine
aliases, exact deployed and final intended feature heads, and fresh remote
ancestry evidence. Retain reviewed old/new base/head ranges, range-diff or
patch-equivalence evidence and migration lineage when history changes. Do not
describe a nonancestor deployed commit as literally merged. An independently
proven equivalent successor can establish integrated feature lineage; a
substantive successor requires a new verified deployment. Ordinary session
completion still requires every registered exact final feature head.

Before release, inspect authoritative status for the exact claim owned by this
session, reconcile unfinished or uncertain command effects, verify deployment
and integrated lineage, release that claim and verify its resulting state. The
cooperative owner-only, idempotent release API does not enforce these
prerequisites. Do not introduce release receipts or edit claim records to bypass
a missing condition. Keep short repository merge claims separate from machine
ownership. Release does not archive, delete, stop or clean a session; each
lifecycle action retains its separate authorization boundary.

## Interruption and lifecycle

Claims survive idle agents, disconnections and service restart. They have no
automatic expiry. The coordinator does not observe command execution or infer
completion. After interruption, inspect the original local and remote work,
stop anything still mutating the resources and reconcile effects. Release a
repository merge claim once its critical section is safe. A deployed machine
remains held until all machine-retention conditions are met. SSH or a client
process exiting does not permit release.

Held claims block lifecycle cleanup until their owner releases them. Lifecycle
fencing cancels waiting requests. Suspension retains ownership and waiting
requests, with no new grants while suspended. Status reads do not grant resources
or run recovery. The Operations page shows up to 128 claims, with up to eight repository and machine
names per claim; the CLI retains complete records. Machine
claims also appear in the session's Reservations tab.

The scheduler inventories validated ledger, claim and integration records once
per workspace pass after reconciling uncertain attempts. It verifies a session
only when that owner has claim work, a nonterminal job, an eligible background
continuation, a due notification or a due handoff reminder. Completed runs remain
eligible for reminders and notification delivery. Candidate records select work;
current generation, session incarnation, fences and action-time state still
govern every action under the existing admission locks. State changes wake the
scheduler; the five-second fallback covers lost wake signals.

## Storage and compatibility

The coordinator strictly reads bounded private `claims.json` envelopes in schemas
1 and 2 alongside the original ledger. The first typed request atomically writes envelope 2 while
preserving existing schema-1 rows. Typed rows freeze the resolved source binding
and identity. Exact request-ID replay returns that saved result before source
resolution, including after release or removal of its clone.
The existing admission mutex protects both old and new resource ownership.
Admission reserves a queued claim's future notification capacity in the ledger.
Notifications use the existing durable outbox; failed delivery retries without
changing or releasing claims. A released claim stays released even if an older
notification arrives later.

Packages advertise `share/dev-workspace/claim-contract.json`. Once claim records
exist, selecting a package without that capability refuses. Compatible selection
preserves live repository/machine mappings. The separate
`share/dev-workspace/repository-claim-contract.json` advertises support for
registered repositories outside the catalog. Every held or waiting repository
claim must retain its registered identity and common Git directory. A candidate
without that capability needs an explicit matching catalog entry for each active
repository; otherwise selection refuses. Released and canceled history does not
require the new capability. Retained machine mappings keep their existing catalog checks.

The separate `share/dev-workspace/machine-claim-contract.json` declares both claim
record schemas, identity version 1 and resolver protocol 1. Every schema-2 store
requires this reader capability, including released or canceled history. Reading
does not rewrite records. Never erase terminal rows or downgrade the envelope to
select a predecessor; recover forward with a compatible package.

`lib.mkPackage` accepts optional `machineIdentityConfig`. Its immutable
`share/dev-workspace/machine-identity-policy.json` contains schema 1,
`profileTarget` (one structured identity or null), and finite `legacyAliases`
entries of `{resource,identity}`. For an unchanged legacy configuration, packaging
derives the default policy from its catalog remotes and exact selectors, using
the runtime's identity derivation. The policy bridges known old keys;
it is not an inventory for new machines. Current and candidate profile identities
must remain equivalent, and a typed profile claim must match the configured
identity and current committed metadata. Initial upgrade uses the original
schema-1 profile-machine claim and its explicit bridge. Retained legacy catalog
and capability descriptors remain unchanged so predecessor hosts can admit that
first candidate. Typed requests start after selection.

Conflict and fairness comparisons normalize approved bridges across old/new
claims, legacy jobs and reservations without rewriting their keys or artifacts.
Candidate selection refuses a missing bridge or conflicting ownership and
preserves the original records. A bridge does not waive an adapter-ID change.
To preserve ownership across a deliberate origin, name or label change, retain a
stable shared label or configure an explicit reviewed compatibility bridge before
use. Recover with a compatible forward package; never delete records to bypass
compatibility checks.

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

Legacy automatic reservation release during lifecycle completion is implementation
behavior, not permission to archive, delete or release a deployed machine before
the retention conditions above. Existing journals and recovery contracts remain;
finishing work or preparing a handoff grants no lifecycle authorization.
