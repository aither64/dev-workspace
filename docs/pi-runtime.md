# Pi SDK runtime

The Pi adapter separates conversation input from runtime and host authority.
`piruntime.Controller` takes trusted session and member identities; its nullable
conversation operation slots do not grant permission to use them. The host
launcher supplies the session process boundary described in
[Pi session host launcher](pi-runtime-host.md). Each loaded conversation uses a
separate SDK process, private history/settings/resources and the configured
canonical native authentication directory.

The manager verifies the private supervisor handshake against independent host
observations before loading a worker. Installed host permissions, trusted portal
composition and route admission are prerequisites for enabling Pi in the portal.
Source fixtures do not establish an installed usable session.

Transcript snapshots and history pages use the shared `userMessage` and
`agentMessage` presentation kinds, including live assistant output; tool results
remain activity. Busy snapshots report `active`, with `held` taking precedence.
An assistant result with no text or only whitespace and native reason `error` or
`aborted` becomes one literal diagnostic row, preserving its identity and order. The complete diagnostic
is bounded to 4096 UTF-8 bytes before HTML escaping. It changes no input receipt,
delivery outcome or permission to retry. Opaque history positions are validated
by the adapter: malformed positions return `transcript_cursor_invalid`, while a
valid position for changed history or identity returns `transcript_reset_required`.

## Input acceptance and recovery

The adapter publishes a private, fsynced intent before dispatch. Ordinary text
input has one pending submission per worker. The SDK user event occurs before
native history append, so the adapter waits until the exact new entry can also
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

## Native identities and settings

New Pi sessions require an explicitly selected Solo starting team with only the
lead role and no additional members. Unsupported multi-role selections are
refused before a creation receipt or native effects. Completed historical
creation proofs remain readable. Incomplete unsupported creations stay held with
their original identities and evidence; recovery does not issue replacement
Create or Send calls.

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
or lost durable completion remains Unknown and held. Inspect changes no state and
loads no worker; a normal active callback can inspect its enforced policy, while
Apply still refuses until that callback drains. An authorized Load reapplies the
unchanged retained policy and verifies it before input.

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
Creation requires an explicit Solo selection, a custom name and text. Model and
thinking overrides preserve the package's provider, account and full fallback
policy. The form checks local configuration and capabilities; it does not verify
provider access or that the installed runtime is usable.

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

Interrupt requests the bound SDK abort. Native idle is not descendant-quiescence
proof. Unload requires idle/no unresolved receipts/settings and verifies the
exact worker/process-group exit plus session-cgroup inventory. Unattributed or
surviving descendants retain a durable hold, even if a later observation sees
no process; no implicit cleanup or recovery clears it.

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
includes worker.mjs, protocol.mjs, session.mjs, accounts.mjs, package.mjs,
framing.mjs, persistence.mjs, lineage.mjs, host-bridge.mjs, tool-policy.mjs and the
shipped host-tool factory modules. The private version-1
pipe protocol has bounded messages, correlation and exact execution identity.
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
`pi_package_root`. Both versions are 1. Credentials, provider endpoints and
machine-specific state paths are absent from these reusable manifests.
The `pi-worker` package/check is available independently of application launch.

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

Package and fake-protocol checks establish immutable input resolution and
source contracts. Installed host UID/groups, sudo policy, exact namespace,
Nix access and canonical-auth visibility still need the reviewed installed
verification gate. Building a package does not activate it or enable Pi.
