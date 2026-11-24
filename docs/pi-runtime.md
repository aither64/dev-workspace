# Pi SDK runtime

The Pi adapter separates conversation input from runtime and host authority.
`piruntime.Controller` takes trusted session and member identities; its nullable
conversation operation slots do not grant permission to use them. The host
launcher supplies the session process boundary described in [Pi session host
launcher](pi-runtime-host.md). Worker protocol 2 uses one Node host per
development session. Each loaded conversation keeps its own SDK session,
history, settings and resources within that host, using the configured canonical
native authentication directory. Protocol 1 predecessors retain their separate
worker processes until an explicit forward drain and successor transition.

The manager verifies the private supervisor handshake against independent host
observations before loading a worker. Installed host permissions, trusted portal
composition and route admission are prerequisites for enabling Pi in the portal.
Source fixtures do not establish an installed usable session.

Transcript snapshots and history pages preserve user and assistant text as
conversation messages. Tool requests and results appear as labelled activity with
the supplied tool name, arguments and output; thinking and other nontext assistant
events remain activity. The Messages filter hides activity rows; All shows them.
Tool parameters join a result only through an unambiguous native call ID available
in the same snapshot. Requested or Preparing does not mean a tool is running;
Completed or Failed comes from the native result. Older results without that flag
remain labelled Result. Tool details use plain text and retain image placeholders.

Recorded native entry times are preserved. When entry time is absent or invalid,
the adapter can use the native message's recorded millisecond timestamp, including
for live output. Missing times remain unavailable. No observation time or tool
execution duration is inferred. Busy snapshots report `active`, with `held` taking
precedence. An assistant result with no text or only whitespace and native reason
`error` or `aborted` becomes one literal diagnostic row, preserving its identity
and order. The complete diagnostic is bounded to 4096 UTF-8 bytes before HTML
escaping. Projection changes no native history, receipt, input digest, delivery
outcome or retry permission. The adapter validates opaque history positions:
malformed positions return `transcript_cursor_invalid`; a valid position for
changed history or identity returns `transcript_reset_required`.

Pi keeps each saved item’s display identity across overlapping history pages,
including its existing block suffixes. Turn IDs still describe workflow grouping.
The current streaming response appears after retained history; the next successful
current snapshot replaces or removes it. Loading older history cannot replace or
retain that live response. These display rules leave native history and delivery
receipts unchanged.

## Activity timing

The optional backend Activity operation measures an existing member from public
`agent_start` events through `agent_settled` events. Tool execution and provider wait
count as working wall time. An explicit native retry delay is waiting; queued
input alone is not. Held periods remain unclassified. Elapsed totals use a
monotonic clock, while reported timestamps preserve the observed epoch times.

Retained linear entries supply message/tool counts and the latest persisted
turn bounds. An assistant message timestamp is not a completion timestamp.
Historical working or idle totals are not inferred. Coverage starts when the
current worker attaches and remains partial after reload. The current
runtime's immutable `piActivity: 1` contract enables `activity_v1`; an older
runtime leaves Activity unsupported and keeps its strict snapshot shape.

The portal maps native `activity_v1` counters to the shared member view: assistant
replies are sent messages, and user prompts are received messages. Native waiting
time includes the open interval; the adapter separates it from closed waiting
time so the browser counts it once. Invalid negative counters or contradictory
waiting intervals make statistics unavailable. This mapping leaves the native
worker protocol and retained history unchanged.

## Native independent fork

The optional `ForkController` requires `piFork: 1` in the current frozen source
and destination runtime packages. `CaptureFork` records immutable source evidence;
it does not reserve a source or grant authority to fork it. Before copying, `Fork`
rechecks the admitted source identity, saved settings, instructions, policy and
complete linear history. Every loaded source member must be quiescent, with no
queued input or unresolved work. The operation takes the existing native runtime
locks in path order without waiting or upgrading a shared lock, and excludes
native effects until the destination copy completes.

The destination reserves its native ID and schema-3 fork origin before calling
the pinned SDK's public `SessionManager.forkFrom`. It uses a separate session host
and private member directories. History is context only: receipts, callback
requests and approval records are not copied. The bridge verifies the new header,
parent path and complete copied prefix, then syncs the file and its directory.
`VerifyFork` uses destination-only proof and permits later destination appends;
it does not require a live or unchanged source. Retrying a lost reply reconciles
the exact reserved ID. Partial or ambiguous native files stay held without deletion,
replacement or another ID. Existing schema-1 and schema-2 mappings retain their
formats; normal creation and bootstrap verification reject the fork variant.

Portal Fork captures the whole saved team, including the original Solo before its
first Add. Every nonremoved materialized member must already be loaded, persisted
and quiescent. An unloaded or archived member refuses the operation with its
address before the destination is reserved. Never-started reservations remain
unstarted; removed members retain their metadata without a native copy. The
selected source address stays selected in the destination. Each copied member
gets independent native, session and member identities, with the same saved
provider, account, model, effort, access, instructions and tool ceiling.

Fork writes creation schema 5 and roster schema 4. Its request, journal and
completion evidence contain no goal digest or initial input. Ready members carry
an immutable fork origin and a verified persisted copy instead of an Accepted
bootstrap receipt. Normal schema-4 creation and roster schemas 2 and 3 keep their
original validation. Later Add and first assignment preserve fork provenance and
use genuine new input receipts. The destination desired provider policy starts
at revision 1; inherited applied settings retain their source policy/catalog
provenance until an explicit Configure.

After an interrupted copy, the portal tries `VerifyFork`, then destination-only
`RecoverFork`. Recovery reads the exact reserved destination and never starts a
source copy. A complete matching result needs no live source. Only proven absence
permits a new copy after source authority, roster, policy and snapshot checks.
Recovery errors retain the original IDs and hold partial or ambiguous files.
Source and target package checks require the corresponding creation, roster and
native readers before a package switch; installing a reader alone rewrites no
saved session. Historical source timing stays partial or unknown, and destination
observation starts with its own worker attachment.

## Input acceptance and recovery

The adapter publishes a private, fsynced intent before dispatch. Ordinary text
input without a delivery mode has one pending submission per worker. The SDK user
event occurs before native history append, so the adapter waits until the exact new entry can also
be read through an independent public SessionManager reopen. Repeated text is
correlated using the live event object and new entry lineage, never text alone.
The worker opens that existing owned history read-only, syncs the file and its
containing directory, and closes both before emitting the witness. The host then
syncs the native witness into its existing submission record before returning
Accepted. An open, sync or close failure after dispatch yields Unknown and holds
the worker; it never permits another input to repair the failure.

Pi's native writer itself does not call `fsync`; the adapter's barrier covers
the verified input prefix under the filesystem's sync contract. It does not
prove physical power-cut behavior or sync later assistant/tool output. Older
receipts retain their original evidence scope. Missing history or an entry on
later verification keeps the operation held without repair or resend.

Records use schema 1 and retain the stable conversation, original operation ID,
action, full immutable input/settings digest and dispatch generation. Generation
is evidence and does not partition duplicate detection. IDs accept exactly
lowercase 32 hexadecimal characters or the lowercase 8-4-4-4-12 hexadecimal
spelling. Spellings are preserved; compact and hyphenated IDs are distinct.
Lookup and same-ID retries never dispatch again, including terminal NotSubmitted.
Changed input or settings under the same ID conflicts.

The shared receipt decoder also supports the separate schema-2 utility owner
and rejects records in the wrong owner root. This codec and its typed utility
references do not enable utility launch or substitute a retained member identity.

A crash before the committed witness can leave Unknown even if native input or
a tool effect exists. Missing acknowledgements after receipt commit are
recoverable by lookup. Unknown holds the affected conversation and requests its
exact worker abort; abort does not prove arbitrary external effects stopped.
SDK exceptions cannot establish NotSubmitted. The adapter makes no exactly-once
claim for external tools.

An operator can [explicitly retire an Unknown input](#retiring-an-input-with-an-uncertain-receipt)
while replacing a held runtime.
Retirement preserves the Unknown receipt and its history. It does not establish
acceptance or repeat the original request.

### Native Steer and Queue

Native Steer and Queue require `piInputDelivery: 1` in the current worker's frozen
application contract. The adapter checks the versioned delivery operations before
exposing transcript `inputDelivery: true` and admitting explicit `steer` or
`followUp` through the ordinary message endpoint. Selecting a new portal package
leaves existing workers on their retained packages. Enable these modes for an
older worker through the [ready-session upgrade](#upgrading-a-ready-session).
Legacy worker version, init, snapshot and send replies keep their exact shapes.

Queue admission returns HTTP 409 `message_pending` with `pending: true`, the
original `clientUserMessageId` and delivery mode. Pending confirms that the native
runtime has queued the input; Accepted requires its entry in native history.
The in-memory registry tracks at most 20 inputs. Prepared or dispatching receipts
without the exact live owner remain Unknown, and restart never reconstructs or
enqueues saved intent. For these modes, a same-ID `retry: true` request only checks
delivery; a missing record remains Unknown. The original text, mode, settings,
retry flag and digest stay frozen. Receipt schema 1 stores the mode in the reserved
action context `pi-delivery-v1:steer` or `pi-delivery-v1:followUp`.

During an active run, the worker passes the exact caller-owned user-message object
to public `Agent.steer` or `Agent.followUp`. Steer is consumed after the active tool
batch; followUp waits until the agent would otherwise stop. If the agent becomes
idle before admission, the worker uses `AgentSession.prompt` with literal text and
template expansion disabled. Queue admission preserves the running callbacks'
original input identity. The consumed native user event must be the exact owned
object. Its serialized entry must also be verified through an independent history
reopen and synced before the receipt becomes Accepted. The linear history proof
allows intervening assistant, tool and system entries, already witnessed owned
user entries, and SDK compaction entries appended to the same parent chain.
Compaction must preserve the captured history prefix. Foreign users, branch or
context edits, changed prefixes and missing objects hold the worker.

Admission returns after enqueue and releases the web, conversation and receipt
locks. The supervisor waits for consumption without those locks, then commits the
witness under short locks. New host callbacks wait for both the native witness and
the committed Accepted receipt; original callbacks retain their exact input and
native origin. A lost response after receipt commit can reconcile to Accepted.
A lost enqueue acknowledgement, commit failure or restart never permits replay.
Abort and hold clear the public native queues, settle unconsumed inputs as Unknown
and preserve already witnessed Accepted inputs. Clearing a queue does not prove
NotSubmitted. Shutdown settles all input waiters before disposing the session.

Activity keeps tracking a held run until the SDK reports that it has settled. It
then uses the terminal status from persisted history; the hold remains and held
time stays unclassified. A hold alone does not establish that the run or its
tools have settled.

Package-transition preflight requires the capability when a current runtime
package or saved native-mode receipt needs it, including runtime reservations
with no members. It refuses targets without `piInputDelivery: 1`, preventing a
rollback from passing these receipt semantics to an old portal. Other workers
retain their frozen packages and remain readable without an upgrade.

### Portal-owned queued input

The optional `QueuedInputController` separates local storage selection from native
execution. `ReadInputSelection` reads the strict CURRENT runtime and owned
conversation record, saved settings, complete policy, instruction digest and frozen
checked-input capability. It makes no host inspection or native call and does not
wait for runtime ownership. This observation grants no effect authority. A valid
unloaded mapping refuses new admission; original receipt and status reads remain
independent. A valid retained package without checked support reports
`CheckedInput: false`.

Checked dispatch requires `piCheckedInput: 1` and the live identity contract from
the retained CURRENT package. The versioned `input_delivery_checked_v1` request
binds the original `SubmissionRef`, literal `SendInput`, delivery mode and full
selection. Under the conversation mutex used for enqueue, it first observes any
existing exact original receipt. Changed settings cannot turn an existing Unknown
into a new refusal or send opportunity. Only a genuinely absent receipt reaches
fresh runtime, settings, policy, instruction, held/pending and native identity
checks before the original receipt/native path. A positive pre-effect refusal is
separate from a native receipt. An error after invoking the native path remains
ambiguous.

Before a portal queue's dispatch claim, a ready CURRENT successor may continue
unsent work only with strict proof that the original full runtime reference is
its ancestor and every selected logical field remains exact. The original
admission, browser selection, input ID, native receipt reference and input stay
immutable. The claim records the actual execution runtime, handle and generation.
An unavailable or unloaded successor leaves existing work queued without starting
or loading a worker. After the claim, recovery only looks up the original receipt,
even if a crash occurred before the wire write. A new generation never authorizes
replay of an ambiguous send.

`input_lookup_v1` never enqueues. It checks the exact original reference,
input/settings digest and receipt-generation lineage. Pending requires that
supervisor's exact live input owner; saved dispatch intent without it is Unknown.
Accepted requires the independent native history witness. A missing receipt is
Unknown. A retained non-fork worker can reconcile Accepted through its existing
native creation verifier when original owner and lineage proof are available.
Unsupported fork or unavailable proof remains Unknown or unavailable. Lookup does
not start or load a worker.

The new `QueuedInputLookupResult` can report `RetiredUnknown: true` only from a raw
owned Unknown receipt and the complete, exact
[retirement proof](#retiring-an-input-with-an-uncertain-receipt). The original
Unknown, reference, input and receipt bytes remain unchanged. This proof permits
distinct later inputs through their ordinary fresh checks; it never permits replay
of the retired input. Missing or synthetic receipts, prepared retirement, ordinary
Unknown and malformed or differently owned recovery records cannot grant it.

## Native identities and settings

New Pi sessions require an explicitly selected starting team from the trusted
catalog. Creation reserves every member in the selected order, initializes the
lead, and submits the original goal once. Other members initialize on their first
real assignment. Initialization does not send a synthetic bootstrap prompt.
Completed creation proofs remain readable; incomplete creations retain their
original identities and evidence. Recovery does not invent a replacement Create
or Send request.

The [Pi team guide](pi-teams.md#retained-settings-and-caller-authority) describes
explicit member addition, retained Solo sessions, roster compatibility and
recovery after a lost response.

A creation mapping reserves a native ID before SDK creation. Empty native
sessions may have no history file. Never submit synthetic input to force a
header, and never open a missing history path as recovery: the SDK can choose
a new identity. Creation verification is observational and requires the owned
mapping plus independent native history and the original Accepted receipt.
An initialized-only proof cannot publish a ready conversation.
Retained CreationID accepts a literal lowercase UUID or 64 lowercase hexadecimal characters;
it is preserved without conversion and does not change native or host request IDs.

`CreateRequest.Instructions` optionally freezes literal role text in a retained
schema-2 mapping before worker initialization. The field is omitted for legacy
empty instructions. A present disk key must be nonempty, valid UTF-8, NUL-free
and at most 4096 bytes. The reader rejects explicit empty or null values,
instructions in schema 1 and instructions in utility records. Creation replay requires identical saved text. Configure never selects
instructions. Load and newly constructed Managers use the original mapping
instead of the latest catalog. Goal text, user entries, submission digests and receipts stay
unchanged.

The worker appends the literal through the normal resource loader, preserving
the default or existing custom prompt, earlier append text, AGENTS and the owned
tool-policy suffix. It verifies the entire observed append and effective SDK
construction at initialization, each snapshot, before input and before Configure.
Only the SHA-256 digest of the observed final literal is acknowledged on the
private worker pipe. The parent checks that acknowledgement from the same process
before publishing the worker or allowing the goal; a missing or changed proof
refuses and preserves the original load intent. Reopen constructs one fresh
append, without recovering an earlier acknowledgement or resending the goal.
`CreationProof.InstructionsDigest` returns the fresh verified digest for nonempty
saved instructions; legacy mappings omit it. Recovery callers can compare it
with the SHA-256 of the exact frozen original role without changing the receipt.

Portal creation uses the lead instructions saved in the original intent. For a
nonempty role, acceptance and later creation effects require the selected
package's full role contract. The first Send, publication of the ready binding
and final completion each require a digest matching the exact frozen role. An
incomplete legacy creation stays held if that proof is missing. Its original
mapping stays unchanged, and the goal is not resent. Completed historical proof
reads and cache retirement remain available without certifying instruction
delivery.

Instruction-bearing mappings require the owning `piRoleInstructions: 1` plus
workflow package/inventory/transition guards before publication and production
use; old strict readers reject the field and downgrade must preserve that refusal.

HistoryRef is an opaque SHA-256 mapping locator, not persistence evidence. Its
versioned encoding is the JSON array `['pi-history',1,backend,workspace,slug,
sessionIncarnation,member,conversationIncarnation,nativeID]`, encoded by Go
encoding/json without a newline. A golden fixture fixes the bytes/hash.

EffectiveSettings contains resolved provider/account/model/thinking/policy
references, never tokens or browser-supplied endpoints. Revision is the target
revision: current+1 applies a mutation; identical full settings at the same
revision replay the prior result; conflicting/skipped/lower revisions reject.
Configuration mutations are idle and serialized. Both current and requested
provider/account pairs must be in the immutable runtime account map. A switch
changes both provider and account reference; replacing the account under one
provider, or changing provider while retaining the account reference, refuses.
The public SDK changes model/thinking on the same native session with
persist:false. The runtime fsyncs its owned effective settings; native global defaults
and other members remain unchanged. Outcomes distinguish applied, rejected before
mutation, and unknown after possible mutation. A pending or uncertain settings
change holds further input and configuration; it never rolls back by guessing.

The SDK runs in the existing canonical `<workspace>/work/<slug>` directory.
Creation/load never creates a missing tracking directory. A crashed loaded
worker with persisted input remains held; loading history cannot resume its turn.

### Live identity proof

`piConversationIdentity: 1` selects `identity_v1` for Bind, Verify and live
capability checks. Its required scalar reply comes from the actual
SessionManager/AgentSession and includes native ID, owned history file, settings,
instruction digest, activity/held/persisted flags and supported input version.
It does not reconstruct or hash transcript history. The manager retains fresh
CURRENT, host/process and native proof checks. A failed advertised operation
cannot fall back to another proof path.

Workers without this declaration keep their exact legacy snapshot path and its
history-linear cost. Installing a new portal does not upgrade retained workers;
legacy snapshot, init and send bytes remain unchanged.

Exact native acceptance still reopens, checks and syncs history before committing
the original witness. Removing redundant identity snapshots leaves that cost in
place and does not establish the latency target. Verification evidence must report
measured retained-worker and new-worker limits. These operations introduce no
native receipt schema or migration.

## Upgrading a ready session

Each Pi conversation receives its workspace, session and member binding from the
authenticated worker identity. The runtime adds this technical context to the
system prompt separately from the saved role instructions, so their text and
digest remain unchanged. Tool shells may omit `DEV_SESSION_SLUG` and
`DEV_SESSION_WORKSPACE`; the trusted binding and a matching `dev-session current`
result identify the session. Existing workers adopt this correction through an
ordinary eligible ready-session runtime upgrade. Updating the portal alone does
not replace their retained package.

The supervisor runs the policy inspection code from the session's frozen runtime
package. Updating the portal changes callback handling and browser text; it does
not replace an existing supervisor. Both must include the correction. Eligible
sessions can adopt the corrected supervisor through the ready-session runtime
upgrade below once they are idle and their effects are settled. Start each new
upgrade with a fresh request UUID. Recovery must reuse that operation's UUID,
expected predecessor generation and frozen target. The upgrade preserves
conversation IDs, history, settings and receipts. An ordinary upgrade cannot
clear a held session or bypass unsettled effects. The explicit retirement
procedure below has separate admission requirements.

Selecting a new application package leaves existing Pi workers on their retained
package. After an ordinary compatible package switch, upgrade an explicitly
selected ready Pi session through the portal's private Unix socket:

```sh
workspace-portal pi-runtime upgrade \
  --workspace '<canonical-workspace>' \
  --slug '<ready-Pi-slug>' \
  --request '<new-request-uuid>' \
  --expected-generation '<current-predecessor-hex64>' \
  --socket '<registered-private-portal.sock>' \
  --origin '<canonical-portal-origin>'
```

Use the generation from the session's current ready runtime authority. Save the
complete command and its request UUID before running it. The portal derives the
target package, native conversations, history and settings from its own selected
profile and session records. The command sends one request and does not follow
redirects or retry automatically.

Workers must be idle, with no pending input, callbacks, unresolved effects or
active resources. Identity or settings drift also refuses the upgrade. Under
the existing transition and session locks, the runtime saves a preparing
successor in its schema-2 request history before unloading the predecessor.
It then proves the predecessor stopped and drained, starts the saved successor,
attaches the portal's host tools and loads the same conversations. Only after
these checks does it publish the new ready authority.

The upgrade preserves native conversation identities, history, saved settings,
original creation receipts, session scratch and retained package roots. It does
not Create, Send or Configure a conversation. Retained loads check native
authentication and the saved model and thinking setting without invoking
redundant setters that would append model changes to history.

A completed ready session retains the catalog and settings recorded at creation.
If a selected package changes that catalog, retained-session admission validates
the saved intent against the immutable original package, including after successor
upgrades. A compatible catalog update may add models and reasoning levels while
preserving every existing model setting, route, account and default. Existing
overrides must keep their values. Saved creation and policy records retain their original catalog
digests. The settings dialog shows only choices supported by both the selected
application and the session's current worker. Model input capabilities may also
expand and model evidence references may change. Existing physical descriptors,
API, endpoint, sampling values and fallback policy must remain unchanged. The
selected catalog must still support the saved policy and account. Other changes
are refused. New and incomplete creations still require their exact catalog and
frozen package. Saved policy digests, receipts and inputs remain unchanged.

The model configuration accepts one to sixteen overrides. Each override names an
exact native model and uses the existing boolean settings. Wildcards and
provider-wide overrides are not supported. Additional override keys require an
existing override map; compatibility does not add a map where sampling behavior
was previously implicit. See the
[installed model configuration](pi-providers.md#immutable-native-endpoint-composition)
for the accepted format. Older packages that require a singleton override cannot
read a multi-override configuration; ordinary package preflight must refuse an
incompatible rollback.

The command prints the operation result and succeeds only when it is ready. A
lost response or nonzero exit can follow partial progress. Keep the selected
target package and repeat the same complete command, including the original
request UUID and expected predecessor generation. Recovery uses the saved target
and reconciles Stop, startup, attachment, Load and authority publication. An
uncertain starting result permits inspection, not another Start. A new request
ID, **Retry creation**, or resending a message cannot recover this operation.

The old authority remains until successor publication. Package preflight refuses
an unfinished successor or an unproved authority change; finish recovery before
another package switch. This operation upgrades one ready session and does not
resume an incomplete initial creation. Initial creation keeps its
[frozen-package recovery requirements](session-preparations.md#pi-requests).

## Retiring an input with an uncertain receipt

An operator can retire one exact Unknown input while replacing its held runtime.
The operation preserves the original receipt bytes and native history,
stops the held runtime through its existing host owner, and loads the same
conversations into the selected compatible package. It does not reconstruct an
acceptance witness, resume the interrupted model request or send the input again.
Earlier external effects remain subject to their own ownership and recovery.

Use the existing upgrade command with all three retirement arguments:

```sh
workspace-portal pi-runtime upgrade \
  --workspace '<canonical-workspace>' \
  --slug '<held-Pi-slug>' \
  --request '<new-recovery-request-uuid>' \
  --expected-generation '<current-predecessor-hex64>' \
  --retire-unknown-input '<original-input-id>' \
  --input-digest '<exact-original-input-digest>' \
  --member '<bound-member-address>' \
  --socket '<registered-private-portal.sock>' \
  --origin '<canonical-portal-origin>'
```

Save the complete command before running it. The recovery request UUID identifies
this upgrade operation; it does not replace the original input ID. The portal
resolves the member, native conversation, target package and saved settings
through the existing session records. A changed input digest, member, generation
or saved target refuses recovery.

Every other member must be idle and free of unresolved work. Independent callback
holds, another uncertain input, pending settings or policy changes, active
resources and identity or history drift still refuse the operation. A held flag
or aborted assistant entry alone is not proof that a runtime has drained.

The runtime saves a private prepared recovery record before requesting host Stop.
It records retirement only after proving that the exact predecessor stopped and
its cgroup drained. It then completes the captured loaded-member bookkeeping and
the saved successor's startup, attachment, conversation loading and authority
publication. Native IDs, history, settings, member bindings, instance scratch and
retained package roots remain available. Independently registered temporary user
services retain their own owner and are not stopped by this recovery.

Success returns `retired-unknown` with the original input ID and digest. Lookup
and same-ID retry still return Unknown and never dispatch. The browser's original
uncertainty remains visible. The corrected worker can accept a distinct later
input using the retained conversation history; loading that history does not
continue the interrupted model request or guarantee exactly-once tool effects.

A lost response or nonzero exit may follow a completed Stop or partial successor
publication. Keep the selected capable package and repeat the same complete
command, including the recovery UUID, original input tuple and predecessor
generation. The prepared or retired record belongs to that operation. Do not
choose another request UUID, edit receipts or holds, or resend the original input
to resolve it.

Recovery requires `piInputRecovery: 1`. Once a prepared or retired recovery record
exists, package preflight refuses a target without this capability. Older readers
retain the Unknown hold and cannot use the disposition to admit work. Recovery
records are retained; rollback does not remove them or reverse an already
completed Stop or publication. Finish an interrupted operation with its saved
request and a capable package.

## Host callbacks and retained origin reads

The optional AttachHost connection uses the existing private supervisor socket;
worker callbacks share the inherited request pipes. Trusted composition supplies
the exact supported tool list through HostToolProvider. Missing or empty support
registers no host tools. A running worker keeps its captured set; reconnect cannot
expand it, and missing support blocks dependent input. Plan and utility operations
still require their separate implemented policy and authority paths.

Before forwarding a callback, the supervisor requires the original input's
durably Accepted receipt and independently verifies the persisted assistant
anchor, unique top-level native tool-call ID, name and arguments. Caller and
input identity come from the worker mapping. Tool arguments cannot supply them.
Original callback input IDs retain their exact lowercase UUID or 32-hex spelling.
Question and plan identifiers follow their separate domain rules.
Native history must remain one complete append-only chain. Tree, fork and switch
hooks refuse; the stable linear selection token derives from the full subject
and exact native root, excluding generation and input ID.

Callback bodies are bounded to 64 KiB, with at most 16 outstanding per worker
and 128 per supervisor, and a 30-second deadline. Attachment setup also obeys
cancellation and the deadline before its acknowledgement. Cancellation or
disconnect after forwarding retains Unknown and a durable hold. Domain stores
own idempotency; no callback ledger or automatic retry is introduced. Unload
cannot settle an active or uncertain callback merely because the SDK is idle.

InspectNativeOrigin reads a saved retained invocation under its current mapped
execution handle. It verifies existing history through public reopen, the full
linear chain, exact anchor/tool identity and selection token, then returns copied
bounded arguments. It does not load a cold worker, publish a callback or infer
an original input from the latest turn. Consumers compare the returned arguments
with their immutable domain content outside store locks. This historical read
does not repeat the original publication's Accepted-input proof.

## Workflow observations and invalidation

The optional `WorkflowObserver` returns a complete observation for an already loaded
retained policy-bearing handle. It combines the independent retained policy and
live native proof with pending input, settings and policy changes, streaming,
tool and callback counts, receipt holds, captured host tools and child/resource
completion evidence. Missing or mismatched proof makes the observation
unavailable. `ObserveWorkflow` never creates or loads a worker, repairs state or
sends input. A busy observation may retain valid policy
proof while `Idle` remains false.

The observation captures complete current `EffectiveSettings` under the same
conversation lock as its policy and native proof. Domain preparation uses those settings with the original operation and
immutable input to compute the existing `SubmissionDigest`, then saves the full
`SubmissionRef` in its existing durable handoff. Preparation creates no native receipt. The
optional `CheckedSubmissionSender.SendCheckedSubmission` checks that exact ref and input against current
settings under the conversation lock before the ordinary send path. A settings,
scope or input mismatch refuses without a receipt or native dispatch; the sender
never substitutes a new digest. Its request uses the same invalidation barrier
and returns at the persisted-input ACK.

Policy and access admission remain required through preparation and Send ACK;
the settings digest is not a Plan acceptance token. A lost ACK, missing receipt,
Unknown or NotSubmitted result, or later settings change is reconciled only through Lookup of
the original saved ref. Recovery never prepares again or resends. Missing settings
or checked-sender support leaves new delivery unavailable, with no ordinary Send
fallback. The existing backend request and receipt bytes remain unchanged.

The optional same-Manager `CurrentRuntimeReader.ReadCurrentRuntime` returns the
literal current `RuntimeRef` from the same strict saved-record reader as
`ReadStart`, including its connected request history. Both ports are disk-only:
they create no state or lock and make no native, auth or helper call. Missing
state, an empty state directory or its validated empty owned `runtime.lock` with
mode `0600` remains absent; other retained artifacts without `runtime.json` refuse. A malformed,
disconnected or mismatched current record also refuses without repair.

The returned ref proves retained identity only. A restarted host can use it for
the existing independent attachment handshake and initial invalidation ACK; live
workflow proof is still required. After waits, the host rechecks the current
session, authority generation and full current runtime ref before publishing an
observation. A changed request invalidates the candidate even if generation is
equal. Missing reader support leaves attachment unavailable; it does not authorize
`Ensure`, `Load` or another runtime start.

A Pi conversation read keeps its shared runtime and package-transition
admission from resolution through the initial verification of the selected
conversation. Verification releases that admission before later reads or event
subscriptions. Request cleanup also releases it if verification does not
finish. Each later operation acquires fresh admission and checks the current
conversation scope and package generation. This keeps an intervening mutation
from consuming the verification deadline and prevents an event stream from
blocking mutations for its lifetime.

After verified successor startup, historical resource records may contribute
completion evidence through the existing strict runtime request history. The
history's current full `Runtime` must match the supervisor. Each resource must join
its original request by full `Runtime` and `RequestID`, with schema 1 and matching
filename, resource/operation IDs and original `ResourceDigest`. Only `stopped` with
a non-null `ExitCode` qualifies. The existing process-group drain and successor startup proofs
establish that terminal boundary; an old PID may have been reused, and the current
cgroup contains the successor. Missing or conflicting history, historical rejected
records and nonterminal resources leave observations unavailable. Resource lookup,
effects and current-generation authority continue to require the current Runtime.

A `HostInvalidationHandler` negotiates `workflowObservations:1` over the existing host
attachment. Initial host-map invalidation and an exact acknowledgement precede
attachment readiness. One ephemeral runtime sequence and mutation-pending count
protect observations; they are unrelated to receipt identity and are not persisted.
A changed sequence or pending mutation refuses publication. Sequence overflow,
event overflow, disconnect or attachment replacement invalidates cached proof.
After first negotiation, disconnected runtimes refuse controlled effects until a
new negotiated attachment is ready. Attachments that never negotiate retain B1
behavior.

Before a controlled effect, the supervisor marks pending, increments the sequence
and waits at most five seconds for the exact attachment/runtime/sequence ACK,
subject to the caller's deadline and cancellation. It waits before taking
conversation, receipt or native effect locks, then revalidates the permit and ordinary authority under those
locks. The invalidation handler may only clear its short host observation map; it
must not acquire domain/runtime locks or call native code. Completion clears pending
and notifies without waiting for another ACK. Natural native events and resource
exit invalidate observations without delaying reaping or durable terminal status.
Ordinary control EOF cancels only its operation admission; closing a successful
input request does not abort the Accepted native run.

Display refresh uses these observations. Every action still
requires current admission and fresh policy checks. Package guards and central
host composition must supply this contract before exposing workflow controls;
source component proof does not enable installed controls. No polling subscription,
TTL, second socket or event ledger is introduced.

## Utility execution and authority

UtilityController uses the same loader, worker pipes, native history, receipt and
unload engine with a distinct utility owner. Its private roots are
`utilities/<original monitor operation ID>/<utility incarnation>/` beneath the
existing session-incarnation state root. A utility never receives a retained
ConversationRef or roster member. Its identity and schema-2 receipt digest have
separate versioned utility encodings; retained schema-1 bytes stay unchanged.

Create, Load, Bind, Send and Unload each require the configured UtilityAuthority
to supply the exact original grant under current runtime admission. Bind grants
no lasting permission. Send accepts only the frozen launch ID, message, action
context, retry flag and settings; changed or additional input refuses. Before the
first input, successful creation reports initialized with no history file. Accepted
still requires the ordinary native and durable host witnesses.

Verify, Read and original-ref Lookup use separate UtilityReadAuthority admission.
Lookup may inspect held or unloaded original work without loading or resuming a
worker. Cold native reads remain unavailable. Utility operations leave Interrupt
unavailable. Explicit Unload is a separate effect, and monitor cancellation does
not terminate workers or commands. Unload requires positive worker/callback/resource drain;
an uncertain report reply holds even when the original input is Accepted.

The authority and host-handler composition must be supplied by the central
monitor owner. Missing composition refuses. Source fixtures do not enable a
production utility or prove the final admitted monitor/report flow.

## Retained tool policy

The optional ToolPolicyController applies complete trusted Plan or Default
targets to retained conversations. A policy-bearing Create uses mapping schema 2
and saves PendingToolPolicy before native setup. It publishes applied state only
after the inline policy hook and exact active/callable tools are confirmed and
the mapping is synced. Interrupted initialization remains held; repeating Create
does not repeat setup. Exact initial revision 1 replay is supported while that
complete target remains current. A later revision makes Create replay unavailable,
even after returning to the same mode and digest.

Policy revisions are exact integers from 1 through 2^53-1. Apply uses an expected
revision and changes only Active within the fixed RegisteredCeiling. It requires
idle input, tools, callbacks and resources, writes pending state before native
mutation and commits the verified applied state before its reply. Partial mutation
or lost durable completion remains Unknown and held.

`Inspect` reads the enforced policy without loading a worker or changing state.
A running native command does not by itself block inspection. Held or unknown
work, pending settings and pending policy changes still block it. `Apply` keeps
its existing descendant-process checks, including for duplicate requests.
A normal active callback can inspect its enforced policy, while Apply still
refuses until that callback drains. An authorized Load reapplies the unchanged
retained policy and verifies it before input.

The worker binds one trusted inline extension through the public SDK, registers
only the trusted ceiling and checks the real active/callable tool sets. Plan permits
the selected read tools and released question/plan tools; Default may add approved
mutators within the same ceiling. MCP, ambient extensions and router/deferred tool
paths are excluded. Tool-call hooks enforce the frozen run's set; instructions do
not replace enforcement. This is not an operating-system read-only sandbox.

Schema 1 remains policy-unavailable and is not upgraded by Apply. Utility policy
is unavailable. Package guards, current host authority and released domain handlers
remain prerequisites; the optional source port does not advertise live Plan support.

## Trusted startup and account attribution

Pi requests from the public New session form use the production Manager and
selected package through the [preparation path](session-preparations.md#pi-requests).
Creation requires an explicitly selected starting team and text or file attachments. The
name can be generated before slug reservation. If naming fails, the request
retains its inputs and offers manual name selection. Model and thinking
overrides preserve the package's provider, account and full fallback
policy. The form checks local configuration and capabilities; it does not verify
provider access or that the installed runtime is usable.

Pi uses the [shared prompt attachment store](../README.md#prompt-attachments)
for initial and later messages. Canonical message text retains the file
references; `DisplayText` and ordered attachment metadata provide the browser
presentation. Validated Accepted receipts and their native user-entry witnesses
supply the original client ID and message digest. The browser uses this proof
to recognize delivery and remove the Sent card, including after reload. An
unknown outcome retains the original input identity; observation never resends
it. Pi uses the same sanitized Markdown renderer as Codex.

An Accepted receipt keeps its original execution generation. After a ready-session
upgrade, the reader verifies that generation in the current runtime's predecessor
chain, then matches the exact native user-entry ID and wire digest. A foreign or
unknown generation, conflicting witness, or equal text under another native ID
cannot acknowledge the browser's saved input. Reading changes neither the receipt
nor native history.

The `snapshot` operation preserves the legacy field set, so older readers can
read an upgraded runtime. Receipt-aware readers use `transcript_snapshot`,
whose strict response contains `snapshot` and `acceptedInputs`. The manager
selects that operation when the current runtime's frozen package contract declares
`piTranscriptSnapshot: 1`. A valid contract without the declaration selects the
legacy read. Missing or invalid metadata and read or proof errors stop the read;
they do not trigger fallback. Existing preparation and rollback gates still apply.

Uploads are generic files. Pi's native read tool can return supported image
content and bounded text; other formats need an appropriate available tool.
Display snapshots replace image blocks with a fixed placeholder before
measuring page size. Native history, IDs, the raw history digest and receipt
verification retain the original content. This keeps image base64 out of browser
snapshots. Image interpretation requires a provider route qualified for image
input; ordinary file storage and references do not depend on that capability.

Manager configuration requires explicit UserStateRoot, HostStateRoot and
AuthDirectory. The latter names an existing native Pi directory, including
its auth.json locks and atomic replacements. There is no directory inference,
credential copy, login or reuse of a rotating grant from another runtime.

AccountBindings is a sorted, nonempty map of native provider IDs to catalog
account references, bounded to 64 entries. Trusted catalog composition supplies
all selectable canonical pairs before startup. The logical credential-store
reference is canonical, with the native provider ID as credential key. Browser
and model input cannot add a binding. This proves configured store/key
attribution, not upstream account identity, entitlement or successful model access.

The manager freezes launch bytes and a per-request runtime-config.json with
scope, instance/request/generation, launch SHA-256, selected user/auth directories
and account_bindings. Their exact bytes and hashes survive interrupted preparation.
It reserves the instance/generation before preparation and records starting
before the host call. Same-ID reconciliation never calls native start again.
Missing or changed frozen configuration refuses.

Public session creation calls `Ensure` once and observes only explicitly pending startup through `Inspect`, using the same full runtime reference and the original four-minute initialization context. A current host startup or an owned control socket that is not yet listening can remain pending; only dial-stage `ENOENT` or `ECONNREFUSED` counts as socket startup. Readiness still requires the validated supervisor handshake and independently observed process identity. A stop request, terminal or unknown state, held work, identity change or other error stops preparation. Observation never repeats host Start, native Create or the original Send, clears retained state or extends the deadline.

An explicit successor supplies a new `RequestID` and sets `ReplacesRequestID` to
the current request. The manager preserves the session, instance, scratch and account
bindings, and reserves one new generation before preparation. It requires the
predecessor's actual stopped terminal host receipt with complete exited/stopped
drain proof, matching current boot and positively empty or absent owned cgroup.
Workers, inputs, callbacks and resources must already have completed their ordinary
drain paths. Loaded mappings, pending changes and Unknown effects refuse; Ensure
does not drain or repair them.

The existing `runtime.json` uses schema 1 for initial replay and schema 2 after an
explicit successor. Schema 2 retains every immutable request entry under its
request UUID and records `CurrentRequestID` in the same file. The whole record is
bounded to 1 MiB; malformed, conflicting or exhausted history refuses without
pruning. A predecessor reader refuses schema 2. Package target guards and current
host admission must exclude an unsupported reader before enabling succession.
Historical Ensure and Inspect use the exact original request and host receipt;
they never contact the successor socket through an old runtime handle.

Each request retains exact `launch.json`, `runtime-config.json` and its own `package-root`
under the instance's `requests/<request UUID>` directory. The legacy instance root
remains unchanged. Before descriptor publication, the manager inspects the
predecessor through the host helper. Under `launch.lock` it rechecks the captured
terminal tuple, current boot/cgroup and frozen descriptor, then releases the lock
before host Start. Start supplies the fresh unit/job/invocation gate; the saved
terminal receipt alone does not prove current systemd idle state. Preparation or
start uncertainty retains the new current request and generation. It never rolls
back the descriptor or creates another request to escape a hold.

Only a verified explicit successor startup may replace a stale fixed `control.sock`.
It verifies the exact current request/config/executable and live boot/invocation/
instance cgroup, then checks schema-2 predecessor lineage. Through the opened
instance directory, it accepts only a same-UID ordinary socket and makes one
no-payload connect probe within 500 ms and the caller deadline. Only definite
`ECONNREFUSED` followed by unchanged device/inode/type/UID permits one unlink and
one bind. A live listener, timeout or ambiguous path/probe refuses. Initial launch
refuses an existing path. Startup makes no helper call or `launch.lock` acquisition,
and does not test the predecessor cgroup for emptiness after joining it.

Conversation/native IDs, history and original input receipts retain their original
identity across succession. Authorized Load reopens that history without resending
an unfinished input; old generation handles refuse dispatch. This supports a new
launch after drain completes separately. It does not automatically resume a crashed
runtime.

ReadStart returns the exact saved current request without native calls or repair.
It reports missing state only if the runtime directory is absent, empty, or
contains only its ordinary empty runtime.lock. Other retained artifacts without
runtime.json hold. ValidatePackage checks the immutable worker paths and fixed
host manifest/supervisor binary without loading the SDK, reading authentication
or invoking the host helper.

Production workers match both account references before setup/configuration,
reject configured apiKey/headers/auth-source overrides in private models.json
and custom registered providers, then require the public runtime's stored-auth
source. Stored status alone is insufficient because it precedes configured
overrides in Pi. Clean custom endpoint/model definitions remain subject to
trusted catalog and adapter support. Fake runtimes are explicit source fixtures.

## History, cancellation and resources

Read/Page exposes escaped text from the selected native branch. Opaque cursors
bind the stable conversation, execution generation and full branch contents;
append, replacement, divergence or wrong scope makes an old cursor stale. Frames
are bounded: each page contains up to 50 native entries and 512 KiB of entry
JSON; an oversized individual entry is unavailable while its native original
remains intact. Watch invalidates a current snapshot and has no replay-log claim.

Each scoped HTTP read reuses one receipt-enriched snapshot and input-capability
result within that invocation. A historical cursor can require an additional
head/page read. Authority and runtime identity are checked again at operation
boundaries; a read result never grants mutation readiness or survives a refresh.
For a current runtime with `piActivity: 1`, a connected workflow watch uses the
existing host invalidation stream and registers its listener before the initial
refresh. Post-append and terminal receipt notifications expose final results.
A missing or unready attachment refuses the watch; connection loss or replacement
closes it. Runtimes without this capability retain their existing native
subscription.

Interrupt requests the bound SDK abort. Native idle is not descendant-quiescence
proof. Shared-host unload requires all attached members quiescent under a short
admission fence, with no unresolved receipts/settings or unknown descendants.
Intermediate unload returns `detached` and `VerifiedDetach`, without process-
exit proof; the last member verifies the exact host/process-group exit plus
session-cgroup inventory before returning `VerifiedExit`. Protocol 1 retains its
independent worker-exit proof. Unattributed or surviving descendants retain a
durable hold, even if a later observation sees no process; no implicit cleanup
or recovery clears it.

Supervisor-started resources retain original operation/resource IDs and exact
PID/start/process-group identity. Same-ID starts do not execute again. A known
exit needs the retained wait result and descendant checks; uncertain effects
stay held. Metrics describe the session cgroup and retained scratch usage,
with explicit unavailable/external coverage. Usage is not a quota and shared
Nix daemon builders, remote tools/VMs and deleted-open files are excluded.
The external service registry must admit versioned Pi handles before using
these methods; compilation does not enable legacy service routes.

## Worker package boundary

The worker uses public SDK exports from one explicit immutable Pi package root.
It never resolves ambient npm modules or probe fixtures. Its module-relative closure
includes worker.mjs, protocol.mjs, session.mjs, activity.mjs, accounts.mjs, package.mjs,
framing.mjs, persistence.mjs, lineage.mjs, host-bridge.mjs, tool-policy.mjs and the
shipped host-tool factory modules. Worker protocol 2 retains schema-1 pipe
frames with bounded messages, correlation and exact execution identity.
The pre-initialization version/EOF smoke creates no SDK session and reads no
authentication data. Source-only fake providers are excluded from production.

The application's strict pi-worker.json parser owns validation of worker
protocol, immutable Node/entrypoint/Pi roots, duplicate/unknown/null/trailing
fields, filesystem kinds and access. This metadata contains no state/auth paths
or credentials. Host execution and a package build alone do not prove supervisor
readiness, native prompt admission or installed tool permissions.

## Packaged inputs and operator prerequisites

The application closure contains the exact unmodified upstream Pi SDK, its
compatible Node runtime and the fixed production worker modules. Startup uses
their absolute Nix-store paths; it does not resolve `pi`, `node` or SDK packages
through PATH, npm or a moving profile. No resource installation is needed at
runtime.

`share/dev-workspace/pi-runtime.json` publishes the fixed host-launcher-v1
entrypoint. Separate `share/dev-workspace/pi-worker.json` contains only
`schema`, `worker_protocol`, `node_executable`, `worker_entrypoint` and
`pi_package_root`. The supervisor manifest remains version 1; worker protocol 2
selects shared-host semantics, and new readers also accept protocol 1
predecessors. The optional `WorkerTopology: shared-session-v1` conversation-
record field makes old strict readers refuse shared PID semantics. It is never
bulk-written during reads. Credentials, provider endpoints and machine-specific
state paths are absent from these reusable manifests. The `pi-worker`
package/check is available independently of application launch.

Pi needs the separately installed [host prerequisite](pi-runtime-host.md).
The portal service also requires explicit `DEV_WORKSPACES_PI_AUTH_DIR` and
`DEV_WORKSPACES_PI_HOST_STATE_ROOT` settings, forwarded as `--pi-auth-dir` and
`--pi-host-state-root`. Neither has an application default. Set the latter to
the installed `services.dev-workspaces.piRuntime.stateRoot` exactly, including
any override. Configure these through the existing
`workspace-portal@<name>.service` environment/drop-in or a deployment-owned
equivalent. Missing or invalid configuration disables Pi while Codex remains
available.

Use one canonical native Pi credential directory, outside member histories,
settings and session scratch. Workers share the directory, including native
locks and atomic credential replacement; each keeps private histories,
settings, models and resources. An initial native Pi grant is an operator
prerequisite. Copying Codex's rotating grant does not replace that step.
The trusted manager resolves the existing user-state root once and freezes
it with the auth directory in each request's private runtime configuration;
workers do not infer those paths from their environment.

A package can supply the optional immutable `piModelConfig` endpoint input
[described in the provider guide](pi-providers.md#immutable-native-endpoint-composition).
The supervisor selects it from the admitted application package. Each worker
keeps private model-cache, settings and history paths, with canonical auth.
The endpoint configuration alone does not supply an available provider catalog
or establish real route access. Its optional model override uses the native SDK
sampling configuration and leaves tool permissions and execution policy intact.
Existing runtimes are not automatically rebound to another package or endpoint.
Package and fake-protocol checks establish immutable input resolution and
source contracts. Installed host UID/groups, sudo policy, exact namespace,
Nix access and canonical-auth visibility still need the reviewed installed
verification gate. Building a package does not activate it or enable Pi.
