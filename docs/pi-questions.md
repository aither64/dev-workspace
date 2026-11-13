# Pi optional questions and Plan identity

The Pi workflow modules retain host-owned question and Plan intent. They are
currently unregistered: their existence does not enable question publication,
answer dispatch, Plan enforcement or accepted-plan execution. Existing Codex
question, composer and Plan routes retain their own behavior.

## Question records and capture

`piquestions.FileStore` binds every publication to a trusted conversation,
logical member, session/conversation incarnations, publication tool-call ID and
stable branch selection. Worker generation is current execution proof, not an
idempotency namespace. The owner supplies current authorization and lineage
validation; missing validation refuses the operation.

A publication contains one to three questions. Host-assigned item and choice IDs
come from their original indexes. Question content is immutable at revision 1;
changed content needs a new publication. Repeating the same caller/tool call
and content returns the retained question. Changed content conflicts. Closing
requires the same publication caller and does not erase accepted answers.

Publication persists before returning pending and never waits for a user answer
or takes the native message lock held by the publishing turn. Titles allow
4096 UTF-8 bytes; each question allows up to 32 choices with 512-byte labels.
Free text remains available. A preselected choice and elapsed time never submit
an answer or grant permission for dependent work.

Answer capture atomically persists an immutable response ID, exact typed
answers, monotonic sequence and delivery intent before acknowledging it. A
same-ID duplicate returns its retained result; different content conflicts.
A correction needs a fresh ID and the exact latest response as predecessor.
Concurrent corrections from one predecessor cannot both win. Previous replies
remain ordered evidence, including replies already used by an agent.

Snapshots apply replies in order and replace only the answers listed in each
reply. Answering one item leaves the others available. Editing an answered item
requires an explicit correction. The group predecessor controls reply order;
it does not erase answers to other items.

Choice plus text and free text alone are accepted. The deterministic ordinary
message envelope includes original questions and correction attribution as
JSON-escaped data after a fixed sentence. The entire envelope must fit 20,000
UTF-8 bytes; answer text never becomes a slash command or prompt template.

## Persistence and browser behavior

Private schema-1 records are stored beneath:

```
<UserStateRoot>/pi-questions/<workspace SHA256>/<slug>/<session incarnation>/<member>/<conversation incarnation>/questions.json
<UserStateRoot>/pi-plans/<workspace SHA256>/<slug>/<session incarnation>/<member>/<conversation incarnation>/plans.json
```

Workspace hashes use the complete canonical workspace path, as existing backend
bindings do. Each record binds the full conversation identity. Short sibling
locks serialize atomic replacement; files are 0600, directories 0700, and file
and directory synchronization precede success. Strict readers reject duplicate,
null, missing or unknown fields, invalid identities and oversized records.
A file is limited to 4 MiB; exhaustion refuses new writes without deleting
retained evidence. There is no automatic collection or legacy converter.

Public snapshots expose display data and allowed actions, not native identities,
paths or branch tokens. Snapshot revision prevents older browser responses from
rolling state back. Local storage retains drafts and exact pre-network request
identity only. A failed or stale request retains the draft; reload does not
submit it. Status checks are observational. Corrections require an explicit
action and never replace a reply silently.

`createQuestionCard` in codex-web owns presentation and keyboard behavior only.
`mountPiQuestions` owns the Pi controller and uses injected request/storage/card
ports. Pinned and inline cards have separate DOM instances. The ordinary
composer remains independent. Server truth determines pending, held, delivered,
stale and closed status; delivery phase retains Unknown/NotSubmitted details.

## Native delivery and holds

`piquestions.Service` requires the ordinary host admission owner plus proved
current-caller, idle/hold observation, prepare, Send and Lookup ports. None is
registered here. Busy and held recipients keep pending responses without
calling Send. The service does not start workers or clear holds.

Before the first Send it freezes the runtime's receipt reference, original
response operation ID, exact message and action context. Store locks are
released before native calls. After a lost acknowledgment, it only looks up
that original identity. Unknown and terminal NotSubmitted never resend or mint
another ID. Unknown/held predecessors prevent dependent progression. Accepted
means admission to the native conversation, not proof that the agent read it.

Ordinary later turns do not stale open questions. A branch change, replaced
recipient/incarnation or unavailable lineage proof disables new delivery.
Read-only snapshots may still show stale records. Process restart can rebind
the same logical recipient only with current proof; it cannot choose a different
recipient. Retired recipients refuse new capture.

## Plan and Default

`piplan` stores completed, caller-bound proposals and acceptance intent. The
proposal digest is SHA-256 of the exact displayed UTF-8 bytes, including line
endings and whitespace. A proposal requires proved completed native origin;
Markdown shape alone is insufficient.

Acceptance binds proposal ID/revision/digest, acceptance ID, fixed operation ID,
original/target mode and enforced policy revision. Same-ID duplicates retain
their original result even after a newer proposal. A fresh acceptance of an old
proposal refuses; another tab cannot create a second operation for one plan.

Plan support remains absent until the native owner proves effective policy
across built-in, custom, MCP/codemode and delegated tool paths. A saved mode
string or policy revision is insufficient. Default does not broaden member
access. Read/render and unsupported controls have no native effects.

Under ordinary admission, a proved implementation first persists acceptance,
applies and confirms Default, then sends exact accepted plan data with the same
operation identity. Mode acknowledgment loss permits observational
reconciliation, not repeating mode changes. Native uncertainty holds the
acceptance. It never rolls back a potentially submitted implementation or
creates a new operation. Acceptance authorizes that implementation request;
it does not authorize merge, deployment or lifecycle operations.

## Integration requirements

The portal owns routes, browser entrypoints, authority and compatibility checks.
The runtime owns worker callbacks, lineage, receipt/digest mapping and policy
across all tools. The application package supplies the modules and assets.
Host-tool factories are pure caller-bound adapters and use
no external extension or startup installation. Missing callbacks fail rather
than return successful stubs.

Before any real store writer is registered, the existing package transition
preflight must recognize retained `pi-questions` and `pi-plans` state and require
target `piQuestionsSchema:1` and `piPlanSchema:1`, respectively. Metadata must be
published only with implemented readers/writers. Unknown/malformed retained
state refuses a transition; genuine absence adds no requirement. This is a
portal integration gate, not implemented or advertised by these modules. Forward-only
recovery must retain the readers and original receipt owners.

Pure Go/browser/tool-factory fixtures use fake ports. No provider login, native
model call, activation or live callback is required for them. Live publication,
answer delivery, Plan enforcement and accepted-plan dispatch each require their
own proved integration before their controls or tools become available.

## Enforced tool policy integration

The optional `piruntime.ToolPolicyController` is separate from provider settings
and the required runtime controller. Its presence does not enable Plan. A real
policy proof requires the current loaded worker, exact active and callable tool
sets, a registered set within the trusted ceiling, the installed policy hooks,
matching retained policy and no pending or contradictory state.

The first Plan mapping permits read, grep, find, ls, optional questions and plan
publication. Default adds only tools allowed by saved host access. MCP,
codemode, deferred/router tools, ambient extensions and unsupported delegation
remain excluded. The native module checks the real SDK registry and callable
set, blocks disallowed tool calls including nested attempts, and adds explicit
mode instructions through the public extension hook. Instructions supplement
enforcement; this is not an OS read-only sandbox or a shell regex filter.

Tool names are exact sorted unique ASCII names, with at most 128 entries per
list and 128 bytes per name. The registered ceiling is fixed for the loaded
worker and retained policy. A mode change cannot narrow or broaden it; a changed
access/tool composition needs a separately authorized complete composition
contract. Load reapplies the retained exact policy and does not invent a new
ceiling.

The policy revision and expected CAS value are independent integers from 1 to
9007199254740991. The mapping digest is SHA-256 of the exact compact JSON array
`["pi-tool-policy",1,Mode,Active,RegisteredCeiling]`, without a trailing newline.
Revision is deliberately excluded. Same-revision replay needs fresh exact live
verification; conflicting or skipped revisions refuse before mutation. A max
revision can be verified again but cannot increment.

Schema-2 native conversation mappings add only `ToolPolicy` and
`PendingToolPolicy`. Schema 1 or a nil policy stays unavailable. Initial policy
creation requires a complete revision 1 target; exact creation replay is allowed
only while that original revision 1 target remains provable, without pending
state and with the existing creation/native evidence. A later policy revision
refuses Create replay even if its mode/digest repeats. Pending or partial setup
remains held and does not authorize reinitialization. No migration or inferred
Default is supplied.

A mode transition persists Pending before native mutation. Applied requires
verified sets/hooks and synced owned mapping before acknowledgment. Partial
mutation, lost response or failed durable completion is Unknown/held. Inspect
never loads a worker, applies policy or clears a hold. An authorized Load may
reapply an exact retained complete policy only when no pending state exists.
The `piToolPolicy:{policy:1,conversationSchema:2}` target-package guard and old
reader fixtures are required before live policy writers are registered.

`piplan.ToolPolicyAdapter` binds consent to
`pi-plan-policy-v1:originalMode:originalRevision:originalDigest:defaultDigest`.
This token has five ASCII fields, at most 192 bytes, canonical decimal revision
and lowercase 64-character digests. Plan targets the next safe revision;
already-Default requires the same revision and equal mapping digests. The full
trusted target is recomposed and checked against the frozen digest. A later
same-digest revision is not the accepted target.

`PolicyStore.Accept` holds ordinary host admission across selection, policy
inspection and capture. Exact duplicate consent is returned before inspection.
Native inspection occurs outside the short domain lock; capture rechecks the
same proposal, caller and CAS with request-local observations. Shared store
callbacks are not swapped. The implementation service observes a lost mode ACK
without repeating Apply, then uses its original ordinary input/receipt. Unknown
and terminal NotSubmitted never create a replacement operation or resend.

`PolicyStore` accepts an optional immutable host observation for pure UI
projection. Without it, controls remain absent. The observation must bind the
current conversation/handle/binding revision, complete retained/native policy
proof, eligibility, domain/guard operations and host tools. Plan acceptance also
requires the exact latest proposal, caller, digest and lineage. Runtime, policy,
authority, branch, attachment and domain changes must invalidate it before a new
observation is supplied. Production refresh/invalidation composition remains an
explicit portal/runtime dependency; no cache, poller, TTL or Applied-boolean
shortcut is introduced here. Snapshot, Lookup, GET and render do not invoke
native APIs. Every new action still needs fresh admitted inspection/application.

## Retained native origin checks

A later answer or Plan action checks the historical publication through the
optional `NativeOriginInspector`. This is separate from the original live
callback's Accepted-input proof. The saved caller has no original input ID, so
an action must not infer one from the latest turn. The runtime resolves the
current generation only for the same durable conversation, reads its existing
linear history and returns the exact anchored top-level tool call and bounded
arguments. Inspection does not load a worker, prompt, write, repair or clear holds.

The question comparator requires the exact saved item/title/option order and
text, with canonical numeric host indexes. Native arguments contain titles and
option labels, not those host IDs. Only an omitted options field and an empty
array are equivalent. The comparator rejects unknown fields, null, wrong types,
duplicate keys and lone Unicode surrogates. It preserves text without trimming,
sorting or Unicode normalization.
The Plan comparator requires the exact saved text and digest, complete origin
and the caller's exact anchor. Original raw JSON byte spelling is not claimed.

The admitted question and Plan capture wrappers resolve and inspect immutable
selected content outside the domain file lock. Atomic capture then rechecks
scope, content, revision and predecessor. A question closing or a newer proposal
arriving during inspection prevents capture. Exact duplicate results return
before action admission or native inspection. New service Apply/Prepare/Send
paths require content-aware `VerifyOrigin`; original receipt reconciliation,
status, GET and render do not need a new origin proof. Missing origin inspection
keeps new effects unavailable while existing records remain readable.

Live callback publication consumes the already proved host call and does not
make another native call under a domain lock. No caller, record schema, argument
ledger or latest-input marker is added. The real runtime reader and production
composition remain explicit integration dependencies; pure fake-port fixtures
do not enable those capabilities.

## Manual mode selection

Manual Plan/Default selection uses the optional `ModeSetter`, independently of
proposal acceptance. The request contains only `mode`, `expectedRevision` and
`expectedDigest` from the displayed policy. It supplies no caller, proposal,
tool list, target digest or acceptance token. Missing policy support cannot
initialize a policy or fall back to Codex settings.

Under ordinary host admission, `ModeAdapter` resolves the exact current handle,
inspects complete live policy proof and checks saved access and idle eligibility.
The expected revision and digest must both match before trusted composition.
Selecting the current mode requires full target equality and proves a no-op
without Apply. A change uses the next safe revision and the same fixed registered
ceiling, with one Apply call. Only exact current-generation applied proof returns
policy data. A positive rejection has no new proof. Any invoked Apply error,
unknown or incomplete result returns uncertainty without policy data; no retry,
receipt or mode ledger is created.

The optional `PiPlanHandler.Modes` action uses the same authenticated conversation
scope and same-origin protections, with separate `pi-plan.mode` permission.
Applied, rejected and unknown return HTTP 200, 409 and 202 respectively; 202 does
not certify success. Without the optional action, snapshots suppress mode
selection. Central registration, trusted composition and retained-state guards
remain required portal/runtime dependencies.

The browser sends once on an explicit selection, captures displayed CAS values
and never changes the displayed mode optimistically. It stores no mode request
and does not replay one on reconnect. After uncertainty, pushed updates cannot
unlock selection. Only an explicit no-store refresh begun after the request
settles may use a fresh coherent host observation; an older in-flight GET cannot
unlock it, even when the domain snapshot revision or policy bytes match.
The portal must invalidate observations before mutation and restore them only
from complete current proof. A read displays current state and does not certify
the outcome of an earlier request.
