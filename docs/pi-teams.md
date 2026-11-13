# Pi team policy and delivery adapters

Pi team support uses the existing retained roster, session authority, explicit
reports and monitor operations. The additive adapters in `teamruntime`,
`agentteams`, `monitoroperation`, `communication`, `progressobserver` and
`pihosttools` supply typed policy, delivery and persistence boundaries. Production
composition must supply current authority before these adapters can write Pi
records or attempt delivery. They do not register tools or start workers.

The codecs and domain adapters have focused fake tests. Live team execution
requires the integration gates below. Existing Codex executors and schema-1
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

`CheckPiDelivery` consumes current read-only participant projections inside that
ownership. A ready root does not make a creating or unmaterialized member ready.
Busy, waiting, stopped, held or cold recipients remain pending/refused through
the existing owner; the adapter never loads, resumes, enqueues, steers or
changes settings to manufacture availability.

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
| `outcome` | Conservative interpreted Accepted, NotSubmitted or Unknown after an attempt. |

Accepted requires the original message ID, a persisted native user-entry
anchor, no queue/steering claim, and no contradictory error. Missing or
contradictory evidence stays Unknown. The raw native result and error remain
available to the owning controller. No host pending-record acknowledgment
becomes native Accepted.

The assignment envelope preserves the existing wording and report instruction.
`ActionContext` remains `team:<slug>:<from>:<to>`. Message IDs retain their exact
lowercase UUID or 32-hex form. Those forms are distinct; no trimming,
normalization, hyphen insertion/removal or replacement ID is allowed. The
runtime owns full-envelope/options conflicts and durable duplicate receipts.
The team adapter adds no deduplication store.

Accepted, Unknown and terminal NotSubmitted never authorize another input
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
refuse before the handler. The inert worker factory
`createReportingTools(callHost)` supplies the same two definitions. It has no
transport, worker activation or automatic report hook. A future authenticated
callback must preserve this binding and revalidate delayed calls after member
replacement/removal. The factory returns held/unavailable/attempted evidence;
invoking a tool does not establish native acceptance.

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

Four required live gates remain with their owners:

1. Portal member materialization/publication, current writer admission and
   package compatibility preflight for the schema-2 records.
2. Runtime authenticated callbacks and native delivery/Lookup witness transport.
3. Distinct utility authority, creation/input receipts and complete settlement.
4. Portal registrations and validated/applied provider and account policy.

The independent modules cannot advertise installed team readiness while those
gates are absent. Initialization must publish the same reserved address,
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
port. Its implementation must hold ordinary package-transition admission,
recheck the exact selected package after waits, then establish current
runtime/session/generation authority before invoking a domain callback. Domain
locks are acquired afterward. A missing implementation refuses before creating
state. The communication store's `Schema2Write` closure uses that same admission
before legacy delivery mutates an already upgraded aggregate; a missing closure
also refuses. A skipped callback does not report a successful write.

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
It holds no monitor file lock while invoking a native callback. The distinct
runtime `UtilityRef` and utility digest helper must actually exist and pass their
own fixtures before an adapter is connected. Bind is not a reservation or input
operation. After successful mapped creation, the runtime's single digest helper
uses the complete frozen utility ref/input/settings to produce the original
input reference. `ReserveUtilityInput` stores that reference before Send; it
never hashes presentation text or fabricates a native ID or second ledger.

The inert `BindUtilityTools` factory accepts only `monitor_operation` status and
report for its captured exact utility, original input and generation. It grants
no retained-member report, peer, question, Plan or communication identity. Before
a domain handler, the authenticated callback bridge must prove that original
input durably Accepted with its native witness. Tool origin alone is not proof.
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

These are source codecs and domain adapters, with refusal when required owners
are absent. Actual target preflight/older-candidate compatibility, registered
writer admission, native utility execution, callback transport and installed
launch permissions remain separate proofs. Busy enqueue, automatic negative
retry and fresh-reviewer fallback remain unavailable. Retained independent
reviewers keep their normal path; absence of one does not permit self-review or
waive mandatory review.
