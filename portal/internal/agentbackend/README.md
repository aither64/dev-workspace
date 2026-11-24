# Host backend identities and readers

`agentbackend` is a standard-library-only identity leaf. Native adapters can
consume its session, conversation and execution references without importing
the session store or either native runtime. Its typed errors distinguish absent,
unsupported, conflicting, unready and stale evidence through `errors.Is`.

## Adapter contracts

The portal owns common workflows and presentation. Adapters translate native
evidence into common operations and values. Team, Fork and transcript presentation
use shared portal components. The neutral
[`Operations`](https://github.com/aither64/codex-web/blob/master/conversation/backend/operations.go)
and [presentation types](https://github.com/aither64/codex-web/blob/master/conversation/backend/types.go)
belong to the codex-web dependency selected in [go.mod](../../go.mod).
Pi implements them in [piruntime](../piruntime/controller.go), with admission
and browser scope checks in [pi_workflows.go](../web/pi_workflows.go).
Keep native protocols and receipt storage behind their owning adapters.

### Identity, support and admission

[ConversationRef](identity.go) identifies the retained session/member incarnation
and its opaque native conversation. `ExecutionHandle` adds the current execution
generation. A worker replacement can change that generation without changing
the conversation or creating a new idempotency namespace. Verify the current
binding and generation for each operation; stale responses cannot update a
replacement member. Neither a typed identity nor a native ID grants permission.

`Operations.Verify` is required; optional operation groups remain nil when
unsupported. Capability support is independent of current authorization,
readiness and availability. A supported read can fail without becoming an absent
capability, and missing measurements are not zero. Unsupported operations must
remain explicit; never route them silently to another backend. Genuine semantic
exceptions need an owning contract, rationale and consumer coverage. An unimplemented
adapter or UI path does not justify a capability exception.

Read admission retains host generation, lifecycle, roster/member and source-scope
checks, cancellation and ordinary lock release. Read-only refreshes must not
create or load a worker, apply settings, reconcile workflows or send input just
to obtain a transcript or statistic. Optional watches invalidate observations;
they do not authorize mutation or establish missing historical evidence.

### Activity and shared consumers

Adapters provide the common `ActivitySnapshot` meanings before values reach
Team aggregation or browser presentation:

- Sent messages count assistant replies/plans; received messages count user
  prompts. Tool calls are separate. Preserve current-turn and retained-history
  count scopes rather than deriving activity from conversation count or row kind.
- `CountsComplete` describes count coverage; `CoverageComplete` describes timing
  coverage. Preserve partial/unclassified intervals, approximate timing, stale
  state and genuine observed zero. Never-started, unsupported, unavailable and
  removed members cannot supply invented measurements.
- Closed waiting time and `OpenWaitingMS` are separate; the shared consumer adds
  the open interval once. Map native inclusive totals at the adapter boundary,
  validating nonnegative values and consistent intervals rather than clamping
  contradictions into apparent evidence.
- A current idle interval uses valid `ObservedAtMS` and `StateSinceMS`, with an
  extension only while the observation is fresh. Polling or tab changes do not
  restart that interval or add it again to accumulated totals. Missing/future anchors remain
  unavailable; a real state or worker change can start a new interval.

[Pi activity timing](../../../docs/pi-runtime.md#activity-timing) owns the native
`activity_v1` mapping and attachment-limited coverage. Older workers without that
capability stay unsupported; a UI change cannot manufacture past timing. The
[portal guide](../../../docs/workspace-portal.md#team-activity-statistics) owns
the shared display. Retain per-member failures without losing healthy rows.

### Input modes and receipts

Use the typed `SendRequest`/`SendResult` operation, preserving the original input
ID, text, settings, digest and explicit `steer` or `followUp` mode. An empty mode
retains legacy admission. Steering is consumed at the native processing boundary;
follow-up waits until the agent would otherwise stop. Neither promises immediate
tool interruption.

`Pending` is positive same-process enqueue acknowledgement. `Accepted` requires
the adapter's exact native acceptance evidence and durable receipt; neither
means turn completion or completion of external effects. `NotSubmitted` must be
positively proved before `Retryable` has meaning. `Unknown`, a lost response or a
new generation never permits a replacement send or a new input ID. An original
receipt enquiry is lookup-only, remains distinct from enqueue state, and preserves
the same immutable request. See [Pi input acceptance and recovery](../../../docs/pi-runtime.md#input-acceptance-and-recovery)
and [team delivery](../../../docs/pi-teams.md#one-attempted-delivery-and-explicit-reports)
for the owning native witness, callback and recovery rules.

### Portal-owned durable input

The optional shared
[`DurableDeliveries` contract](https://github.com/aither64/codex-web/blob/db79231a4e5513bf439493efab75378bc7d84c04/docs/reference.md#application-owned-durable-deliveries)
is implemented by `web/pi_input_delivery.go`. It separates local inbox admission
and status access from native Send. Local resolution joins strict saved
binding, creation/catalog/tracking, current authority, lifecycle/provider and
access evidence under package-transition admission. It calls the runtime's
disk-only `ReadInputSelection`, pins the original ordered files, then rereads the
local tuple before committing. It acquires no runtime shared/exclusive lock and
performs no worker, host, transcript or workflow observation.

The composer retains its initial verified scope and selection digest separately
from the latest status metadata. Polling cannot adopt a new selection. A changed
digest requires an explicit page refresh or successful settings/mode save followed
by a fresh same-scope local confirmation. Failed, wrong-scope or late reads cannot
confirm it. Drafts, files and original-ID recovery remain intact. This conservative
rule also applies when only execution generation or binding revision changed.

The private workspace-scoped portal `input-delivery.sqlite` database is queue
schema 1, independent of native receipt schemas. It uses one retained SQLite
connection, indexed exact identities, WAL and FULL synchronization. The original
Admission, browser selection digest, text/prepared wire, file scope/order and
native ref remain immutable. A short conditional update records the actual
dispatch runtime/handle/generation before the checked native call. That claim is
the no-replay boundary, including a crash before any socket write.

Server dispatch uses foreground scheduling, ordinary fresh effect admission and
workflow attachment, exact local selection/attachment rechecks and real native
identity/instruction proof. The checked-input owner repeats the decisive native
settings/policy check. A still-queued request can continue through a runtime-owned
proof of a complete CURRENT successor chain only when its logical target, native
ID, incarnations, access, settings, policy, instructions and file wire remain
exact. Work waits while a successor is preparing or unloaded; dispatch cannot
implicitly start or load it.
After the claim, pending/dispatching/unknown records use only original-ref lookup.
For a positive Pending result, the next lookup is due after 200 ms or at the end
of the first second after the persisted dispatch claim, whichever comes first.
Later Pending results, Unknown results and failed checks wait two seconds.
Restarts and reads do not renew the fast window. The existing 250 ms worker tick
and lookup cost also affect when Sent appears in the portal; native acceptance
is measured separately.

Queue collection retains outstanding work and a bounded recent status window;
exact identities remain available for duplicate detection and recovery. Explicit
owner-proved retirement leaves the original Unknown and immutable admission
unchanged, releases that row's ordering and capacity barrier, and joins the recent
terminal/disposition window. Unproved or incomplete retirement cannot release it.
The inbox has no age-based deletion, editing, reordering or cancellation API.
Attachment pins survive ambiguous or failed delivery until their existing owner
can prove safe release. Native team, communication, questions/Plan, creation and
utility producers retain their existing receipt owners and never enter this inbox.

Definitive local admission refusals retain an exact-request rejection identity in
the same SQLite authority. Admit and Reject serialize the same key and recover or
conflict with an existing queue/refusal identity before writing. Only a durable
matching `notQueued` proof releases the browser's saved admission; it preserves
the draft and files for an explicit new submission. Generic errors, missing rows
and authorization failures do not establish this proof. Refusal identities remain
available for duplicate detection and package retention, without a work row or
fabricated native receipt.

### Inbox package retention and recovery

The existing package preflight inventories the isolated queue through its owning
strict reader and emits `portal_delivery` source claims. Retained work, disposition
and refusal identities require
`piPortalDelivery: 1` in the target package, during both preparation and activation
rechecks. Malformed, orphan or unsupported queue state refuses the transition.
Older native workers do not read the queue, and their member/receipt artifact
whitelists and schemas remain unchanged.

Every operation on the source SQLite connection, including initialization,
reconnection failure, commit checkpoint and final close, runs under shared package
transition admission. There is no expiring pool connection or autonomous close.
Terminal portal shutdown cancels/joins dispatcher work before attempting a bounded
shared close. If an exclusive switch already owns transition admission, the inert
connection remains until process exit; no deferred task checkpoints it later.
Uncommitted result persistence leaves the existing dispatch claim for original-ref
reconciliation on the next start.

Preflight runs under the host's exclusive transition boundary. Its standalone
inspection API requires that same writer/checkpoint/close exclusion. It copies
the database and any WAL into a private disposable snapshot, omits reconstructible
SHM, and opens SQLite only on the copy. It never initializes, checkpoints or repairs
the retained source. Back up database and WAL together while offline, or use a
compatible SQLite online backup operation. Recover forward with a queue-capable
portal; discarding queue or native receipt state can lose original identities.

### Communication provenance and filters

Project communication through `TranscriptCommunication` and the existing retained
communication reader. Saved domain records own message IDs, endpoints, from/to
labels and delivery status. Correlate native entries with exact accepted input
identity/digest when available. Recognizing a retained envelope header is only a
presentation fallback; text, timestamps or a tool invocation cannot grant sender
authority or prove delivery. Preserve canonical native bytes, IDs and cursors.

Sessions/All include cross-session messages; Messages/Team exclude them. Team/All
retain genuine team traffic, and errors stay visible. A retained message uses one
`session-message:ID` row ordered by its recorded creation time; reload, older-page
and status refresh must preserve deduplication and update that same row. Keep
lead exchanges out of member views. A generic tool row has no invented endpoints
or successful-delivery label. See [session communication](../../../docs/session-communication.md)
for grants, exact endpoint identities and delivery recovery. Pi leads use the
communication CLI. A native `session_communication` tool would require a separate
update to the retained tool policy and its upgrade path. Transcript display
changes do not add that capability.

### Adding or removing a backend

Implement the existing identity and optional operation contracts needed by the
feature, then adapt native evidence to the common types. Extend an interface only
for a demonstrated shared operation; no new plugin registry or parallel UI is
needed. Keep native storage, SDK behavior and receipt ownership in the adapter,
and domain authorization/recovery with their existing owners.

Exercise representative adapters through the actual shared consumers, including
rendered page composition: activity/Team, transcript labels and filters, input
modes and receipt enquiries. Reuse existing Go fixtures and browser contracts
such as [pi_activity_test.go](../web/pi_activity_test.go),
[composer_handoff_contract_test.cjs](../web/composer_handoff_contract_test.cjs)
and [message_delivery_browser_test.cjs](../web/message_delivery_browser_test.cjs).
Cover absent capability, failed reads, stale generations, cancellation, partial
coverage and uncertain receipts with zero unintended mutations. Adapter-only
success does not prove the shared consumer is wired correctly. Use existing
fixtures for these checks; live provider calls are unnecessary for
presentation-only changes.

Removing a backend from new selection does not remove retained dependencies.
Keep its readers, original receipt/recovery owners and required packages while
bindings, history or unfinished operations depend on them. Preserve old-reader
refusal and compatible forward recovery; do not reinterpret retained records as
Codex, discard them or resend an input with an unknown outcome. Rollback must
respect the retained formats and recovery owners.
The host reader and package fences below remain part of that obligation.

## Host readers and retained compatibility

The adjacent `session` package owns the read APIs:

- `BackendBindingStore.Load` reads Pi binding schema 1 from
  `<UserStateRoot>/session-backends/<full lowercase workspace SHA256>/<slug>.json`.
  The hash covers the canonical workspace's UTF-8 bytes. The durable binding
  selects Pi even when runtime files are absent.
- `LoadPiRuntimeAuthority` reads Pi projection schema 2 from the existing
  `<AuthorityDir>/<slug>.json`. `ValidateBinding` checks the durable tuple and
  revision. A live projection is derived evidence, not the backend selector.
- `SelectBackend` consumes typed, validated host evidence. Existing Codex
  readers remain responsible for legacy validation. Positive legacy evidence
  selects Codex; absence alone does not. An orphan Pi projection, mixed legacy
  and Pi evidence or a mismatched durable tuple refuses selection.

Readers require private owner-UID mode-0700 directories and mode-0600 regular
files, bounded strict JSON and exact identity fields. Only genuine missing state
wraps `ErrNotBound`; callers must propagate other loader errors before selection.
Reads create no directories, records, locks or identities and change no bytes.

`BackendBinding.Resolve` requires a ready binding and addressed conversation.
`PiRuntimeAuthority.ValidateHandle` additionally checks a ready projection, the
current generation and the exact stable conversation reference. Session and
conversation incarnations survive worker restarts; execution generations do not.
Submission identity uses the stable reference plus the original action, input ID
and payload/options digest. A new generation cannot authorize resubmission of an
unknown request. These types do not implement a submission ledger or establish
native acceptance.

Selection and handle validation grant no roster permission, native history
ownership or execution authority. Callers must separately verify current host
generation, lifecycle/admission, member authorization and native process proof.
A creating conversation cannot dispatch, whether its native ID is absent or
already allocated. No reader advertises creation or execution support.

This reader boundary preserves schema-1 Codex bytes, receipts and recovery.
It supplies no writer, converter or migration. Pi creation and writes require
the separate durable package/stale-wrapper fences and creation-receipt ordering;
schema-2 reader refusal alone cannot prevent an older blind writer overwriting
runtime state. No live profile, lifecycle or native operation is needed to check
this boundary. Run its disposable fixtures from the repository Nix environment:

```sh
nix develop --command go -C portal test -mod=readonly \
  ./internal/agentbackend ./internal/session
```

The host/CLI guard continuation reads the same durable binding namespace through
`libexec/workspace-session-backends.rb`. Legacy writers, lifecycle commands and
web recovery/creation paths refuse Pi evidence before native or tracking effects,
and repeat the decisive proof after waiting for admission. Host preparation,
activation and restoration scan durable bindings as well as accepted creation
receipts and retained journals; loss of runtime files does not clear that gate.
Existing lock order and generation checks remain in place.

The runtime contract advertises binding schema 1/policy 1, authority schema 2 and
`piCreationSchema: 5`. The guard reads ordinary schema-4 creation and the explicit
no-input schema-5 fork variant. It validates the complete receipt, immutable
request, journal and completion shapes against the durable binding. Fork members
use an immutable origin reference and native copy proof; normal creation retains
its initial-input witness. See [Pi fork proof](../../../docs/pi-runtime.md#native-independent-fork). The guard refuses incomplete or contradictory records;
matching target metadata alone cannot make a Pi-bearing transition succeed. A binding with neither
retained creation fence is a conflict. See
[Pi creation package compatibility](../../../docs/pi-creation.md#package-compatibility)
for the retained-record and target-package requirements.

Predecessor packages still reject schema-4 records before their ready-state skip,
including candidate activation preflight. The actual ready journal, immutable
request and completion proof remain required after receipt-cache retirement.
Keep those records for retained Pi dependencies; a metadata key or sidecar cannot
retrofit old binaries. Compatibility checks neither repair state nor grant native
dispatch. Recovery stays with the original creation and receipt owner.

Optional trusted startup configuration forwards `DEV_WORKSPACES_PI_AUTH_DIR`
verbatim through `--pi-auth-dir` to `web.Config.PiAuthDirectory`. There is no
default, credential read or login at startup. This field grants no launch
support; a Pi adapter must validate its configured directory before loading a
worker. Missing or invalid Pi configuration must leave Codex available.
The launcher state root is also explicit: `DEV_WORKSPACES_PI_HOST_STATE_ROOT`
passes through `--pi-host-state-root` to `web.Config.PiHostStateRoot`. It has no
default and does not alter the frozen H0 launch request.

Run Ruby fixtures through `test/workspace_host_test.rb` or
`test/dev_session_test.rb`, using Minitest's `--name` filter for focused cases.
Individual component files rely on fake classes loaded by those suite entries.
For tmux fixtures, use `nix develop --command env TMPDIR=/tmp ruby ...`: the
extra Nix shell temporary-directory prefix can exceed the Unix socket path
limit. Each fixture still creates and removes its own disposable directory.
