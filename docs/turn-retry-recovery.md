# Automatic turn retry recovery

The portal retries new managed turns that fail with the native
`serverOverloaded` or `cyberPolicy` code. The
[portal guide](workspace-portal.md#automatic-turn-retries) describes the controls
and schedule. This guide covers durable state, admission and recovery.

When session retry policy is available, Settings contains separate switches for
capacity and cybersecurity errors. Both apply to the lead and all current and
future retained team members. The conversation notice shows the current or
latest retry chain. Retry now accelerates a scheduled attempt; Stop retrying
stops that chain without changing the session preferences. Dismiss hides only
that result in this browser. The notice appears inside the conversation viewport
and leaves no space when hidden.

## Ownership and persistence

Schema-1 records live at
`$DEV_WORKSPACES_STATE/turn-retries/<workspace-sha256>/<slug>/<thread-sha256>.json`.
The canonical workspace, retained root, native thread and socket are part of
each record's identity. Records retain the enrollment boundary, legacy preference,
failure watermark, chain, fixed deadline, exact automatic submission identity,
quota timestamps and bounded settings evidence. Instructions and application
context remain private; status responses expose neither.

State, loaded bindings and admission snapshots carry a required nonzero
provenance epoch. Cybersecurity charges carry unique automatic submission UUIDs;
equal timestamps do not make two charges interchangeable. Missing epochs,
anonymous charges and inconsistent pending identities fail closed.

Directories use mode 0700 and records mode 0600. Writers hold a per-thread
interprocess admission lock and replace each record atomically, syncing both
file and directory. A record is limited to 1 MiB, each settings snapshot to
256 KiB, and a session to 128 thread records.

The record holds up to 64 outstanding or current settings snapshots. To make
room for new evidence, admission retires only the oldest positively accepted
snapshots bound to a turn. Unbound queue entries, uncertain submissions and
pending identities remain protected. Retiring a contributor from the current
turn disables automatic retry for that exact turn and removes its cached
execution proof. If that turn later fails, it requires manual continuation. If
protected evidence fills the count or byte budget, admission refuses before
sending.

The admission lock is shared by portal, CLI and direct team mutations. Lock
ordering is package transition, session runtime, roster operation when needed,
conversation mutex, then per-thread admission. Atomic state writes and native
queue/submission-ledger operations occur inside that admission fence. They do
not acquire an outer roster or conversation lock. The retry scheduler waits for
its next scheduled attempt outside these locks. Two scheduler workers serve
retained roots and ready team members without requiring a browser connection.

Session preferences and optional activity checkpoints live separately at
`$DEV_WORKSPACES_STATE/turn-retry-policy/<workspace-sha256>/<slug>.json`.
This strict schema-1 sidecar is limited to 1 MiB and 128 thread entries. Its
identity includes the retained root and the tracking directory's device/inode
incarnation. A mismatch preserves the record and holds automatic execution.
The sidecar contains no prompts, settings snapshots or duplicate submission
receipts. A short sibling `.lock` protects atomic read-modify-write checkpoints;
native requests never run under that lock.

Bootstrap reads the lead and all nonremoved retained members under the session
runtime's exclusive lock. Any legacy per-thread opt-out disables both session
preferences; otherwise both start on. Missing legacy records use the old
default-on choice.
Unreadable state or an incomplete inventory holds automation. Once saved, the
session choices govern both classes for current and future retained members;
the legacy per-thread preference is no longer an execution gate.

Preference changes use a separate exclusive runtime transaction with a total
30-second budget. Two matching cold observations establish each class's enable
boundary before one complete policy write. The same active independent turn may
qualify if it later fails; a terminal turn is consumed. Turnover or an incomplete
read refuses the change. A request that repeats the saved choices returns the
current revision without changing the policy, baselines or checkpoints. This
also covers a repeated request whose reply was lost. Polling never takes
exclusive access.
Turning a class off saves affected chain and uncertain UUID identities before
lazy ledger cancellation. Turning it back on cannot revive those identities.

Manual Send, Queue, Implement plan and team delivery do not read or write this
sidecar. Missing, corrupt, full or unwritable policy and activity metadata may
hold automatic execution, but cannot add a manual-delivery refusal or prevent
exact receipt settlement.

First enrollment reads a conservative terminal baseline. A failure already
present at that boundary is consumed. A proven active turn can qualify when it
later fails; a completion racing enrollment is excluded. Re-enabling consumes
the current failure as a fresh baseline. An unchanged independent active turn
remains eligible. Portal restart reads the saved boundary and deadline rather
than enrolling the same failure again.

## Settings and execution proof

Managed admission saves authorized provider, common and role instructions,
reporting binding, execution policy and application context before work can run.
The accepted native receipt or exact client-ID/content history binds that
admission to a turn. The pinned Codex 0.163.0 rollout's exact `turn_context`
supplies model, effort, collaboration mode and execution policy for that turn.
Enqueue-time defaults are not evidence of eventual queue execution settings.
Steering retains the active turn's actual settings and accounts for every
contributing managed message's application context.

The loaded binding must remain authorized and match the native socket
generation. Missing or conflicting context, unidentified user input, changed
bindings, a native child or an unproved native-origin turn leaves the failure
for manual continuation. The retry worker never resumes a thread or writes
settings to repair missing proof. Saved queues and recovery-held conversations
stay paused until their existing explicit activation path permits execution.

These guarantees cover managed portal, CLI and retained-team operations that
share the admission and package-generation fences. An independent direct native
writer does not participate in those locks. The native protocol has no atomic
compare-and-start precondition, so direct native mutations can race a retry or
change loaded settings outside this contract. Observed mismatches and
unidentified input block continuation; historical context and display caches
cannot prove absence of every concurrent external writer.

A managed settings intent advances the durable epoch before applying settings,
even when no retry is pending. It consumes the current active or failed turn
and cancels its chain. Failed or uncertain settings replies do not undo this
boundary. The settings intent invalidates all earlier admission proofs. Queued
inputs keep their order and may execute normally, but their failures require
manual continuation. Newly admitted managed input can qualify using the new
epoch and exact execution proof. Old snapshots needed for reconciliation remain
unchanged and cannot acquire the new binding.

After a successful explicit settings change, the settings owner can preserve
the inherited loaded binding for new work only when cold reads show the same
exact active, in-progress turn before and after the change. The prior binding,
provider, socket, authorized policy and common instructions must still match.
This preserves the warm thread's MCP, instructions and access without another
resume or rebind. Completion, queue turnover, cold state, uncertainty or a failed
check leaves the new epoch without a binding. A later explicit managed idle
action can establish one. Persisted native session IDs cannot prove that the
same thread instance remained loaded. Earlier admissions remain ineligible.

## Lost responses and cancellation

Automatic continuations use the existing schema-3 native submission ledger.
The private retry record retains one UUID, action identity, source failure and
immutable settings across restart. A lost response enters reconciliation using
that exact identity. A new UUID is never a recovery mechanism.

Exact delivery reconciliation runs before capturing new retry provenance or
binding settings. An accepted original UUID returns its existing receipt without
acquiring newer settings or causing another native send. This includes history
recovery after browser acknowledgment. An uncertain submission with missing
admission proof cannot be treated as new input. Keep the original UUID when
recovering an uncertain response.

Plan implementation reserves its original message identity before changing Plan
mode to Default. An exact prepared entry in the generic submission ledger, with
no queue attempt, can acquire its first managed admission after that explicit
settings change. This preserves the original UUID, text, plan action, options
and steering intent. It does not rewrite earlier retry snapshots or admit a
submitting or queue-owned attempt as new work. Exact receipt recovery runs
before plan freshness and ordinary activity checks. Every other unresolved
submission still receives the ordinary checks.

Browser mutations wait for a temporary exclusive session-runtime lock within
their existing request deadline. After acquiring the lock, the server checks
the current package, lifecycle, provider and conversation authority again. A
timeout or changed authority refuses admission without sending input or changing
settings. Lifecycle commands retain their existing immediate lock refusal
behavior. Recover an uncertain response using the original browser submission
identity.

Automatic dispatch reserves room for accepted-result bookkeeping before sending.
If an already accepted result cannot retain another eligibility snapshot,
reconciliation saves its exact pending identity, result and quota charge in a
blocked state that requires manual continuation. Freeing space or restarting
does not enable automatic retry for that result. Persistence errors leave the
durable uncertain tuple and generic receipt available for exact reconciliation.

Stop retrying suppresses future continuations without changing the session
preferences. Turning a class off also cancels its chain. New input, queue
admission/start, settings
changes, interruption and explicit lifecycle operations cancel pending work.
Cancellation cannot establish that an already-dispatched turn did not run.
Possible submissions retain their identity and quota charge until the native
ledger/history proves the outcome. Stop, retirement and package/provider
transitions preserve that uncertainty rather than erasing it.

The worker settles its exact saved submission identity in a separate internal
reconciliation pass before ordinary mutation admission. That pass cannot send,
resume, steer, change settings or activate queues. Fresh execution then reacquires
the full ordinary admission checks. Other unknown submissions and cold or stopped
execution holds continue to block new work.

Stop and Retry now use exact session-incarnation, thread, chain and policy
revision tokens. Retry now also binds the pending UUID and only shortens its
saved deadline. It does not send input, change quota or create a new identity.
Stop keeps its token across attempts in the same chain and saves cancellation
before acknowledgment. Duplicate actions are harmless for their exact owning
identity. A new chain or policy revision requires refreshed status; Retry now
also requires a refresh when the pending UUID changes. An uncertain submission
can reach its own receipt-only recovery and Stop before
ordinary activity checks. This path cannot start fresh work. Old boolean and
tokenless retry controls require a page reload.

Confirmed starts count exact accepted automatic UUIDs once, after the retry
ledger saves the receipt. Quota reservations, polling, button presses and
refusals do not count as starts. One possibly submitted attempt is shown
separately. The compact checkpoint keeps the current or latest chain's count,
last confirmed UUID and timing; it is not an execution ledger. Before replacing
an accepted pending tuple, the worker checkpoints it or holds further automatic
progress. A missing checkpoint shows partial or unavailable evidence rather
than inferring starts from the attempt or quota counters. Optional checkpoint
failure cannot undo acceptance or a saved Stop. Terminal elapsed time freezes
at its observed end; missing original timing remains unavailable.

Cybersecurity quota counts possible automatic sends in the rolling five-minute
window, including dispatches interrupted before their result was saved. Receipt
reconciliation does not charge again. Backward clock movement cannot expire
charges or advance a deadline early. Quota exhaustion remains terminal for the
same failure after the window expires; a timer does not restart it.

After positive proof that an exact UUID was never submitted, a resumed dispatch
reserves that UUID again at the current conservative time. It does not reuse an
expired pre-crash charge or replace another ID's charge. Acceptance, including a
receipt recovered later, anchors the same charge once at receipt observation
time so preflight or network delay cannot expire it early. Duplicate receipt
reconciliation does not add a charge or move that anchor again.

## Forward recovery

Packages publish `turnRetryStateSchema: 1` in their runtime contract. Once any
retry record exists, package selection refuses a candidate without matching
support. This requirement applies even after cancellation because enrollment,
opt-out and unresolved identities must survive. Existing recovery, roster,
workspace-manifest and native submission formats retain their versions.

Packages publish `turnRetrySessionPolicy: 1` alongside unchanged
`turnRetryStateSchema: 1`. Once any `.json` file exists below
`$DEV_WORKSPACES_STATE/turn-retry-policy`, package preparation, activation and
candidate self-activation require that exact integer capability, even without
legacy retry records. Ruby checks presence only. Empty, malformed, oversized
or wrong-incarnation sidecars still require support; Go validates their payloads.
Empty directories and lock or temporary files alone do not require the new
capability. State-inspection failures refuse the transition. Keep policy
sidecars with retry ledgers and receipts during supported recovery. Do not
delete or rewrite policy to select an incompatible package. Recover forward
with a compatible package.

Recover by repeating the supported workspace switch or selecting a corrected
newer compatible package. Keep retry records and submission receipts together.
Do not delete or edit a record to bypass an unknown outcome, restore default-on
behavior or make an older package selectable. A malformed or unknown-version
record blocks automation and reports unavailable state; preserve it for
diagnosis. Repair the producing/reading package through the normal reviewed
forward-recovery workflow.

For diagnosis, read the conversation's retry status and retained native history.
The journal reports bounded observation/admission failures without including
private settings or model input. A scheduled deadline with a held conversation
is pending work, not proof that a retry ran. A running receipt proves acceptance
of the visible continuation, not completion of its task or exactly-once external
effects from earlier model actions.
