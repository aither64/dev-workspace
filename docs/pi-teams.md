# Pi team policy and delivery adapters

Pi team support uses the existing retained roster, session authority, explicit
reports and monitor operations. The additive adapters in `teamruntime`,
`agentteams`, `monitoroperation`, `communication`, `progressobserver` and
`pihosttools` supply typed policy and refusal boundaries. They do not register
tools, start workers, write a new roster or activate team delivery.

The current source supports an independent fake-tested slice. Live team
execution requires the integration gates below. Existing Codex executors,
record formats, reports and utility launch remain unchanged.

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
fresh watcher. `CheckPiUtilityLaunch` currently refuses every production launch
because the distinct runtime authority and record variant are not implemented
by this slice.

Utility reports retain existing immutable escalation/terminal slot rules.
Terminal passed/failed projections need authoritative source completion,
settlement of every owned effect, and durable report publication. Command exit,
assistant final/idle or SDK abort alone is insufficient. An incomplete report
can retain uncertain command/cell identities. Cancellation of continuation
grants no command interruption or new attempt. These checks publish nothing
and do not clear an existing recovery hold.

## Integration and verification

Four required live gates remain with their owners:

1. Portal member materialization/publication and versioned existing-record codecs.
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

No persistence format changes are included here. Future backend variants need
explicit reader/writer fences and retained recovery before use. Forward recovery
keeps original policy/history/package references and unresolved receipts; it
never falls through to Codex, deletes evidence or silently resends input.
