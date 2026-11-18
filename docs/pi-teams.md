# Pi team policy and delivery adapters

Pi teams use the existing retained roster, session authority and explicit delivery
receipts. The portal registers team assignment and reporting through authenticated
worker callbacks. It reserves the complete selected roster and initializes each
additional member on their first real assignment. Browser and CLI requests use the
serving portal's team API and its current admission checks.

The codecs and domain adapters have focused fake tests. Existing Codex executors and schema-1
record values remain supported; communication can advance its shared aggregate
to schema 2 under the writer admission described below.

## Retained settings and caller authority

`agentteams.ProjectPiPolicy` projects the selected saved model, effort,
behavior, purpose, instructions and access without selecting another role or
substituting catalog defaults. Saved role access and installed execution access
are separate fields. An independent reviewer keeps their saved policy and
private history; read-only instructions alone do not promise an OS sandbox.
Provider/account/model/thinking validation and actual applied settings remain
with the provider/runtime owners.

New Pi presets map Astra roles to `gpt-6.2-astra` and Sol roles to `gpt-6.2-sol`.
The observer uses `gpt-6.2-sol` with low effort. The projection preserves the
remaining role settings and original catalog provenance, and freezes its own team
digest. Saved rosters and Codex presets remain unchanged. The team table shows
Saved model/reasoning; the conversation summary shows the currently applied
settings.

Add member reserves a new address using the installed role policy. The reservation
saves the member’s instructions, access, provider, account and supported
model/reasoning pair. It sends no bootstrap prompt and leaves existing members and
the original creation request unchanged. The member initializes on its first real
assignment. Readiness requires that assignment’s original Accepted receipt and
native witness. Adding a member preserves the lead’s saved tool ceiling.

An original Solo session may have no stored roster. The portal validates its
original Solo creation and current ready lead authority, then displays the lead
with an empty team view. Reading the page or Team API writes no roster. The first
explicit Add creates roster schema 3, revision 1, and records prior roster revision
0 in the reservation. Other missing or malformed rosters are refused.

Add saves the reservation, appends the exact creating member to the backend
binding, then advances the authority revision. If the response is lost, retry with
the original request UUID and unchanged selection. That request completes only its
own reservation. It cannot choose a replacement incarnation, create a duplicate
member or repeat an input. A package without the schema 3 reader cannot be selected
while this state is retained.

Before reservation, the current worker package must support protocol 2 and the
required role and workflow contracts. Add reuses a healthy attachment to the exact
current generation. A temporary connection failure follows the existing refusal
and recovery path. Missing package support requires a runtime upgrade. A lead
retained with two tools keeps those two tools even when a new member’s validated
policy declares four.

The host captures `teamruntime.PiCaller` from an authenticated worker channel.
It contains a `piruntime.RuntimeRef` and an `agentbackend.ExecutionHandle`.
Worker arguments, process IDs, environment variables, claimed native thread
metadata and model-selected sender names cannot establish that caller.

Structural validation rejects mismatched session, incarnation or generation.
Authorization still requires `PiAdmission.WithDelivery`: its implementation
holds existing package/session/team admission through the callback and checks
the current caller, recipient, retirement state, materialization, holds, loaded
worker, applied provider settings and actual native support immediately before
the effect. Callers enter it without already holding a team lock. Missing
authority invokes no callback and performs no native operation.

`CheckPiDelivery` consumes current participant observations inside that ownership.
A ready root does not make a creating or unmaterialized member ready. Idle,
working and streaming recipients may receive an explicit steer request. Waiting,
stopped, held or cold recipients remain refused through the existing owner;
lookup never loads or resumes a member.

## One attempted delivery and explicit reports

The native port has only `Deliver` and `Lookup`. The runtime implements `Deliver` over
its trusted Bind/Send operation, with persistent native admission evidence.
The team adapter calls it at most once during an admitted callback. Its result
separates:

| Field | Meaning |
| --- | --- |
| `disposition: held` | Policy/recipient state prevented a native attempt. |
| `disposition: unavailable` | Required support or authority is absent. |
| `disposition: attempted` | The native port was called; acceptance is still a separate fact. |
| `native` | Exact native `SendResult`, retained even on error or contradictory evidence. |
| `outcome` | Conservative interpreted Pending, Accepted, NotSubmitted or Unknown after an attempt. |

Pending means the native runtime admitted the request to its queue. Accepted
requires the original message ID and exact persisted native user-entry witness,
with no contradictory evidence. A queue acknowledgment alone does not establish
Accepted. Missing or contradictory evidence stays Unknown; the owning controller
retains the raw native result and error.

The assignment envelope preserves the existing wording and report instruction.
New team messages use `ActionContext` `pi-delivery-v1:steer`. Historical
`team:<slug>:<from>:<to>` contexts remain unchanged when reconciling their original
receipts. Message IDs retain their exact
lowercase UUID or 32-hex form. Those forms are distinct; no trimming,
normalization, hyphen insertion/removal or replacement ID is allowed. The
runtime owns full-envelope/options conflicts and durable duplicate receipts.
The team adapter adds no deduplication store.

Pending, Accepted, Unknown and terminal NotSubmitted never authorize another input
attempt. In particular, `Retryable` does not override a frozen negative
receipt. Reconcile the original saved `SubmissionRef` through the runtime's `Lookup`;
do not infer its digest from visible text or inspect a private ledger.
Read/recovery admission belongs to the central owner; a held receipt must not
need a delivery callback to remain inspectable.

`PiDelivery.Report` sends to the own-session lead only when explicitly called.
A final assistant answer does not forward itself. `BindTeamTools` captures
the caller outside JSON and accepts only:

- `report_to_lead`: `message_id`, `message`;
- `team_message`: `message_id`, `to`, `message`.

Unknown fields, duplicate JSON keys, invalid IDs and identity/authority fields
refuse before the handler. The worker factory `createReportingTools(callHost)`
supplies these definitions. Production registration captures the authenticated
caller outside tool JSON and revalidates delayed calls after member replacement
or removal. Reporting is explicit; a final assistant answer does not forward
itself. Results distinguish held, unavailable and attempted delivery, including
native Pending. Calling the tool alone does not establish acceptance.

## Communication, observers and utilities

`WithPiDeliveryPolicy` checks a current read-only exact-pair grant projection
under the existing communication owner's admission locks. Only lead endpoints
may use it. Revoked grants and held recipients refuse before the callback.
The existing store must support typed backend endpoints before it can supply
that support proof; absent/mixed unsupported variants stay unavailable. No
Codex root ID is fabricated for a Pi endpoint, and no new grant store exists.

`PiDue` feeds proved, current activity into the existing observer `State.Due`
rule. The owner supplies the relevant handles and freshness bound. Missing,
stale, wrong-generation or held observations cannot wake an observer or lead.
Observer work itself does not count as development. Existing pause, interval
and pending-result behavior stays with the existing scheduler.

Fresh watchers use `PiUtilityHandle` and `PiUtilityLaunch` projections owned by
their existing monitor operation. These are distinct from retained-member
conversation handles. The frozen snapshot records original operation/source,
installed package/catalog/model/effort/instructions/access and concurrency.
Retry compares that snapshot; it does not resolve a new policy or create a
fresh watcher. `CheckPiUtilityLaunch` refuses production launch until distinct
runtime utility authority and its native receipt engine are connected. The
domain record alone cannot authorize that launch.

Utility reports retain existing immutable escalation/terminal slot rules.
Terminal passed/failed projections need authoritative source completion,
settlement of every owned effect, and durable report publication. Command exit,
assistant final/idle or SDK abort alone is insufficient. An incomplete report
can retain uncertain command/cell identities. Cancellation of continuation
grants no command interruption or new attempt. These checks publish nothing
and do not clear an existing recovery hold.

## Integration and verification

The integration boundaries are:

1. Portal member materialization/publication, current writer admission and
   package compatibility preflight for the schema-2 records.
2. Runtime authenticated callbacks and native delivery/Lookup witness transport.
3. Distinct utility authority, creation/input receipts and complete settlement.
4. Portal registrations and validated/applied provider and account policy.

Tool and team availability follows the capabilities of the current worker.
Utility launch remains unavailable until its distinct authority and receipt
engine are connected. Initialization must publish the same reserved address,
incarnation and native identity after persistent proof, in retained selection
order. No bootstrap prompt or replacement native ID is invented after a loss.

Focused Go `TestPi*` fixtures cover policy/caller refusal, one-send results,
original IDs, explicit reports, immutable conflicts, revoked grants, stale
activity and unavailable utilities. Worker factory tests run with
`node --test pi-runtime/src/host-tools/reporting.test.mjs`. Use the repository's
Nix environment and explicit `-mod=readonly` for Go. Passing fakes prove these
adapters, not installed callback/receipt/utility behavior. Cross-process
verification follows committed-deliverable review and the ordinary fresh
verification watcher policy when the live contracts are released.

Schema-2 records require the explicit reader/writer fences and retained recovery
below before production use. Forward recovery keeps original policy/history/
package references and unresolved receipts; it never falls through to Codex,
deletes evidence or silently resends input.

## Schema-2 records and writer admission

The domain stores have explicit schema-2 Pi codecs on their existing paths.
They preserve schema-1 Codex values, field names, paths and native executors.
The Pi record arms contain complete scoped references; an opaque native ID alone
cannot identify a caller, conversation or utility.

| Existing store | Pi record |
| --- | --- |
| `codex-teams/<workspace hash>/<slug>.json` | `PiRoster` retains role policy and creation/first-input references, with explicit session/lead/member incarnations. It contains no Codex thread/project placeholders or duplicate native IDs. |
| `monitor-operations/<workspace hash>/<operation ID>.json` | `PiOperation` freezes the original lead/input owner, verification-watcher policy, distinct utility, create/launch references, report events and optional settlement evidence. |
| `session-communication/<workspace hash>/state.json` | One schema-2 aggregate contains the original Codex grants/messages and explicit Pi endpoint arms. A Pi delivery references original settings/input, without a synthetic Codex receipt. |
| `progress-observers/<workspace hash>/<root hash>.json` | `PiState` hashes the exact key `pi:<session incarnation>:<lead incarnation>` using the existing path function. Pending observation holds its own conversation/input/settings references. |

The schema-2 codecs reject duplicate/unknown fields, missing required fields,
ambiguous owner arms, null identities, invalid UTF-8, trailing JSON and oversized
records. Legacy communication options retain their existing values, including
legacy payload nulls. Old and new operation ID spellings stay distinct in their
original filenames. Record admission reserves space for later recovery evidence
before a native effect.

`monitoroperation.PiTeamWriteAdmission.WithPiTeamWrite` is a central consumer
port. The portal implementation holds package-transition admission and shared
runtime ownership, and rechecks the selected package, full profile identity,
provider reservation, binding and current session generation after waits. Domain
stores acquire their own locks inside the callback. Record bookkeeping does not
require an idle worker; native effects still need their owning verifier. Missing
admission refuses before creating domain state. Pi communication `UpdatePi`
enters this gate once and does not enter `Schema2Write` as well.

The communication store's `Schema2Write` closure checks common package
compatibility. It requires the complete `piTeamRecords` map and the still-held
transition/profile/package scope, without requiring a Pi binding, generation or
worker. This lets authorized Codex operations preserve retained Pi arms after
their bindings retire. Existing lead, pair, provider and receipt checks still
apply. HTTP mutations create a service inside their admitted invocation; each
background dispatch does the same. The closure acquires no transition or runtime
lock and expires when the invocation returns. The store rechecks the schema
under its aggregate lock, so an unadmitted schema-1 writer cannot cross a
concurrent upgrade. Missing admission or a skipped callback refuses the write.

The Ruby command passes its actual held transition File to the communication
child through `pass_fds`. The private child arguments bind the descriptor and
lock path to the exact package, host profile and raw profile identity. The raw
identity is bounded JSON `[dev,ino,ctimeSec,ctimeNSec,mtimeSec,mtimeNSec,readlinkTarget]`.
It is separate from existing profile-token encodings. The child compares the
descriptor's file identity, its own executable/package, selected profile and
complete codec map. It never acquires, unlocks, downgrades or closes the borrowed
lock. The parent retains ownership through child success or failure. Lifecycle
expiry forwards the same File while keeping its existing exclusive runtime
ownership; the child takes no second runtime lock. Direct legacy schema-1 CLI
calls remain supported, while standalone schema-2 writes require this admission.

Pi receipt reads use current session authority under shared runtime ownership.
Their original full input/utility reference and original launch generation stay
unchanged, including recovery while busy or held. Lookup does not create, load,
send or configure a worker. Reconciliation releases read ownership before its
later write gate and compares the same saved domain record before publishing.
Conversation configuration uses exclusive runtime ownership and exact retained
access. The original root format lacks lead access; it can be recovered only
from an already validated catalog with the exact original catalog/team digests
and explicit lead role access. Missing historical proof refuses that mutation.

Roster readiness uses a detached capture of the original Accepted initialization
proof. Its invocation-local verifier rechecks the exact current binding revision,
generation and reserved member/creation tuple inside write admission, before
roster locks. It performs no native verification or publication there, and
cannot be reused after the callback. Actual member-ready publication retains its
separate exclusive CAS and completed-root proof.

The complete target `piTeamRecords` capability is:

```json
{"policy":1,"roster":2,"monitorOperation":2,"communication":2,"progressObserver":2,"utilityReceipt":2}
```

This map describes codecs. It does not prove native utility or callback support.
No production writer may be registered until package preflight scans every
retained domain/runtime obligation, validates the whole map, and tests show that
an actual older candidate refuses the retained state. Keep the recognized
schema-4 creation journal
while any dependent Pi record, communication endpoint/message or utility mapping/
receipt survives, including after binding retirement. A binding-only scan is
insufficient. Unreadable state blocks a transition. There is no extra marker,
retirement command, cleanup path or downgrade repair in these codecs.

Roster discovery validates every `.json` file as authoritative state. Within a
registered workspace's owned roster directory, it ignores visible ancillary
files, such as backups, whose names end in neither `.json` nor `.lock`, only
after `Lstat` confirms a regular file owned by the effective user with mode
`0600`. It does not open their contents or change them. This keeps backups out
of the canonical roster inventory.

Known lock files retain their private-owner, mode and zero-size checks. Unknown
`.lock` files, hidden entries (including unfinished `.team-*.tmp` and
`.pi-record-*` writes), orphan namespaces, unsafe entries and invalid canonical
records still block a transition. Other retained domains keep their existing
inventory rules.

Within a registered monitor namespace, preflight validates the `observers/`
subdirectory using the monitor store's strict schema-1 decoder. The directory
must belong to the effective user with mode `0700`; canonical observer files
must be private regular files with mode `0600` and fit the existing size bound.
Preflight binds each record to its filename ID and workspace without creating
locks or calling `GetObserver` or `ListObservers`. Observer records produce no
Pi claims or record-content output. Direct CI observers retain their separate
capability contract.

A parent native or Pi monitor operation with the same ID blocks the transition.
Orphan stores, unsafe or unknown entries, nested directories, and hidden
`.observer-*` or `.operation-*` write residues also block it. Preflight
preserves these records for their owning recovery procedure.

The first Pi communication write upgrades the existing workspace aggregate
atomically under its existing lock and admitted writer. It retains every legacy
approval, ID, endpoint, timestamp, option, receipt and pending delivery. It does
not reapprove or resend legacy traffic. The same package can continue legacy
Codex delivery through its existing adapter while preserving Pi arms. A package
without schema-2 support remains incompatible while that aggregate is retained.

`ReconcilePiMessage` has only a read/recovery authorization and Lookup port. It
uses the original saved input/settings/envelope/context, including for held or
busy inputs. It has no Send, Load or Configure method. Uncertain proof holds
Unknown; terminal negative remains held under its original reference. Revoking
a grant cancels only unsubmitted work and retains accepted/uncertain obligations.

## First real assignments and utility settlement

`ReservePiInitialAssignment` saves the first real authorized assignment's exact
message ID in the existing roster. It returns the existing team envelope/context
and resolved settings to the admitted initialization owner. Exact replay does not
consume another roster revision. There is no synthetic prompt, JSONL writer or
replacement input ID. The runtime must produce the original persistent witness.
`PiInitialAssignment.Publish` delegates references to the existing member-ready
publication port after delivery/init ownership has been released. That owner
independently verifies the native proof and publishes under its own locks.

A ready flag or `initialInputId` alone is insufficient. Every codec write that
preserves or introduces a ready member requires `PiRosterReadyVerifier`. Its
read-only final binding/live-projection revision and exact incarnation check runs
inside current writer admission before team/file locks. It must not reacquire
transition/runtime locks. Expensive native proof and member publication happen
outside those codec locks; a stale proof must be revalidated under admission.

The utility domain freezes the original monitor launch brief as its first real
input. `PiDomainAuthority.WithUtility` revalidates each explicit create/load/
bind/send/unload grant and saves monotone create intent before native creation.
It holds no monitor file lock while invoking a native callback. The typed
`PiRuntimeAdapter` consumes the runtime's `UtilityController`, `UtilityAuthority`
and `UtilityReadAuthority` interfaces. Central composition must supply current
effect and read admission; missing ports refuse. The adapter is configured as
the same Manager's utility authority. It calls the controller without holding
outer admission; the Manager enters its configured authority once per effect.
Bind is not a reservation or input operation. After successful mapped creation,
the runtime's single `UtilitySubmissionDigest` helper
uses the complete frozen utility ref/input/settings to produce the original
input reference. `ReserveUtilityInput` stores that reference before Send; it
never hashes presentation text or fabricates a native ID or second ledger.

Launch reserves create intent and the original input once. A lost creation reply
without a saved native mapping remains held; the adapter has no replacement or
mapping-repair path. Once input is reserved, later Launch calls perform only
Lookup, including when Bind or Send failed before returning a receipt. Lookup
checks the full original utility reference and frozen input/settings, with the
required native witness or positive proof of refusal before acceptance.
Current runtime read authority can inspect that original receipt across
a permitted generation change; it never changes the receipt's generation or
digest. Only an authoritative terminal Lookup can resolve a domain projection
recorded as Unknown. A runtime receipt still marked Unknown stays held;
Accepted and terminal NotSubmitted remain immutable. Recovery
keeps the utility held and does not infer current worker activity or settlement.
No recovery path resends. A Send return alone cannot establish acceptance.

The inert `BindUtilityTools` factory accepts only `monitor_operation` status and
report for its captured exact utility, original input and generation. It grants
no retained-member report, peer, question, Plan or communication identity. Before
a domain handler, the authenticated callback bridge must prove that original
input durably Accepted with its native witness. Tool origin alone is not proof.
That proof may precede the original Send reply and domain projection. A configured
`MonitorDomainHandlers.Runtime` uses `PiRuntimeAdapter.Report` to reconcile only
the saved original receipt before short report publication. Its trusted caller
captures the complete `HostCall.Runtime`; model arguments supply no runtime or
input authority. The handler never waits for Send or holds effect/store locks
across Lookup. Missing read or native support refuses.
The reporting factory forwards the SDK native tool-call ID through callback
options, outside model arguments. The bridge owns transport, callback lineage
and drain counters; no new bridge listener or ledger is added by the domain
layer. The shipped `createMonitorTools` factory in `reporting.mjs` permits only
utility `status` and `report`: status returns the bound Pi operation, and report
returns its durable original event-ID receipt. It has no start, cancel, resolve
or caller-selected operation ID. Bound Go factories advertise no tools unless
their handlers explicitly project approved names through `HostTools`.

A durable terminal report and its immutable slot receipt can be returned while
the reporting callback itself is still completing. `PiReportService` publishes
that event independently from settlement. Later reconciliation observes exact
source/input/resource/callback/tool/utility-exit proof under current runtime
admission without holding a monitor file lock, then compares the operation
snapshot before writing settlement. Command exit, SDK idle/final/abort, a report
reply or successful lead delivery cannot replace complete effect/drain proof.
Unknown children, callbacks or incomplete inventory retain the hold and utility
capacity. The legacy quiescence reader refuses utility arms. Pi quiescence also
requires a current utility verifier after recorded settlement; absent Codex
fields cannot stand in for utility exit or drain. Cancel revokes continuation
without stopping commands, clearing
receipts or declaring settlement. Source execution is never repeated to repair
lead delivery.

These are source codecs and typed domain adapters, with refusal when required
owners are absent. Compositional fixtures cover their interfaces and faults;
the actual Manager, worker transport and SDK have separate runtime tests.
The complete admitted utility create/load/bind/send/lookup/report/drain flow
still requires integration in the runtime's existing cross-process fixture.
Actual target preflight/older-candidate compatibility, registered
writer admission, native utility execution, callback transport and installed
launch permissions remain separate proofs. Automatic negative
retry and fresh-reviewer fallback remain unavailable. Retained independent
reviewers keep their normal path; absence of one does not permit self-review or
waive mandatory review.
