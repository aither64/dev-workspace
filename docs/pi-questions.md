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
