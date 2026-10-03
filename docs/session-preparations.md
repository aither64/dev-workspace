# Session preparation requests

New-session callers can submit a canonical random version-4 UUID in
`clientRequestId` with `POST /sessions`. A short `name` is optional on this path.
The server saves the accepted request before asynchronous naming and ordinary
session initialization. Explicit-name requests without an ID retain their
existing behavior. Forks, plan sessions and CLI name syntax are unchanged.

## Admission and replay

The frozen input contains the original UTF-8 prompt, ordered attachment IDs,
upload scope, date, normalized custom name, submitted team/model values and
their presence, and an exact resolved team preset. Presets include member and
lead settings and instructions. For managed selections, admission validates
local structure and policy; the initialization worker checks live model/effort
availability. It never replaces unavailable captured settings with defaults.

A versioned input digest binds the request ID. Repeating the same ID and input
returns the original operation before consulting current catalog or upload
state. Changing prompt whitespace, attachment order, scope, date or settings
returns a conflict. Different IDs are separate requests even with equal prompt
text.
Repeating POST does not retry a failed operation.

JSON acceptance is HTTP 202 with `requestId`, `receiptId`, `attempt`, `url`,
state, phase, timestamps and `initialRequest`. Form callers receive a 303 to
`/creations/<request-id>/`. The initial request is the raw textual prompt,
independent of attachment-expanded text sent to the session. The preparation
page renders it before browser scripts load.

`GET /api/session-creations/<request-id>` returns current progress and, after
exact receipt installation, a server-generated `canonicalUrl` and final slug.
`POST /api/session-creations/<request-id>/retry` requires the current
`receiptId` and `attempt`; stale offers conflict. Before handoff a retry starts
a new preparation attempt, or resumes an attempt already published but not
launched. After handoff the ordinary creation receipt owns
attempts and progress. These endpoints require canonical unescaped UUID paths;
mutations retain the portal's exact-origin and package-generation checks.

An unconfirmed preparation or upload-claim write returns HTTP 503 with the
exact `requestId` and `code: "preparation_persistence_unconfirmed"`. The saved
identity remains reserved. Repeating the identical submission confirms the
published write and repairs its pending admission; retry uses the current
receipt and attempt. Other HTTP failures do not establish this recovery state.

## Browser drafts and progress

New session puts the initial request first, with team selection available below
it. Options contains the optional custom short name and advanced lead settings.
Leaving the name empty requests an automatic dated name. Custom names retain
their case, underscores, syntax and 48-character limit. A request may contain
only attachments. The CLI preview requires a custom name; CLI, fork and plan
session naming are unchanged.

For a managed team, leaving both lead settings empty uses the installed team
defaults captured by admission. Model discovery is needed to offer new explicit
overrides, but its failure does not prevent admission with team defaults or
recovery with already saved exact overrides. Initialization checks captured
settings against live availability without choosing another model or effort.

The browser verifies a random version-4 request ID in the tab's sessionStorage
before sending. Its version-2 tab draft retains text, date, settings, upload
scope and the exact serialized submission body, including field presence and
ordered attachment IDs. The first attempt saves that body before POST. A lost
response, HTTP failure or reload keeps the inputs and uploads locked. Recover
saved request checks status first; a missing operation permits only identical
resubmission of the saved body. A 503 with the exact saved `requestId` and
`code: "preparation_persistence_unconfirmed"` also permits identical recovery
POST after the status lookup. Other 503 responses do not. Neither path permits
editing or recycling that ID.

After an attempted submission fails, **Start a separate request** opens the
current form in a new tab without an opener. The original request may still
finish; this action does not cancel or replace it. The original tab retains its
exact request, settings and file selection for recovery. **Copy saved text**
copies its raw prompt. The new tab uses current settings, a new request ID and a
fresh file scope. Copying text, reattaching files and submitting are deliberate
steps; nothing transfers or submits automatically. A restored attempted draft
retains its original recovery behavior, even in the new tab.

Each draft owns a separate upload scope and stores its selection in the same
tab. Fresh tabs do not reuse the older shared localStorage scope pointer.
Older unsubmitted text/settings drafts receive a new request ID; older shared
upload pointers/selections are left untouched without assigning their ownership
to the new draft. When an older draft has cached file-selection evidence, the
form asks you to attach those files again. Unsubmitted server files still follow
their normal expiry rules. Reattach the originals to select them for that request.
Attempted drafts cannot be migrated to a different request or reconstructed from
current catalog choices.

Acceptance and storage cleanup are separate checks. Only a response identifying
this request, exact receipt, attempt and preparation URL can clear its draft and
scope/selection pointers. Removal is verified by readback. Storage failure
keeps the recoverable identity visible; keep the tab open and restore browser
storage before retrying. No failure deletes files or sends a legacy name-only
request. An unreadable attempted draft is retained for diagnosis.

The preparation page renders the accepted raw prompt as escaped text before
scripts load. It polls once per second, shows naming, reservation and initialization
progress and measures elapsed time from original acceptance across retries.
Retry sends the current exact receipt ID and attempt. Navigation requires a
ready status with the original request/receipt identity and an exact dated
same-origin session path. Gone, replaced, conflicting and cancelled operations
stay on their preparation page. The legacy slug-based initialization page
continues to serve forks, plan sessions and explicit-name callers.

## Naming and reservation

The portal owns one naming-only Codex App Server for its service lifetime.
The text-only adapter uses that private socket; ordinary conversations keep
their existing App Server and dynamic catalog. An injected adapter remains
available for tests. Startup, readiness or runtime failure leaves naming
unavailable and uses the existing deterministic fallback.
The adapter receives at most 8192 raw UTF-8 bytes and
returns one JSON object containing only `name`. A generated name must contain
three to six lowercase ASCII alphanumeric words separated by hyphens and fit
within 48 bytes. The adapter must honor cancellation and the ten-second deadline,
including queue time. No more than two adapter calls run concurrently per
workspace. Attachment names, paths and contents are excluded.

The adapter calls codex-web's public `RunEphemeralTurn` with exactly
`gpt-5.5`/`low`, fixed instructions and a bounded name schema. Each call uses
an empty private mode-0700 cwd outside the workspace and a separate mode-0600
instruction override file containing exactly codex-web's
`EphemeralInstructionFileContent`. The adapter writes that provider-owned constant
once to its unique `instructions.txt` file and keeps it unchanged through helper
teardown. Both file overrides use that path: the pinned server requires readable,
nonempty contents even when explicit naming instructions take precedence. The
fixed text also supplies the compact-file prompt; it does not disable compaction.
The call supplies no attachments or team policy.
The worker retains ownership of the whole deadline and concurrency limit.
Scratch state is removed after the call; durable session state is untouched.

The child uses the package's pinned native Codex executable and full unchanged
original model catalog at `share/workspace-portal/codex-models.json`. A startup
`model_catalog_json` CLI override selects the native static catalog manager;
ordinary catalog refresh cannot replace it. The helper validates the immutable
file and requires `config/read` to identify its exact path with `sessionFlags`
origin before creating a thread. A model name or a path written to user config
after startup cannot establish that profile. See the provider's
[ephemeral utility contract](https://github.com/aither64/codex-web/blob/master/docs/reference.md#ephemeral-utility-turns)
for the file digest, supported pair and preflight rules.

The portal starts this child once, with its existing host-selected Codex home
and account/provider environment, strict config loading and native remote
control disabled for the child. It copies no credentials and does not change
ordinary remote enrollment. Startup runs in an empty private cwd; global user
policy remains trusted. A failed or exited child is not respawned or replaced
by the ordinary socket. Restart the portal through the existing service/package
procedure to create another lifetime.

On shutdown, the portal stops admissions and cancels/waits its workers while
the child remains available for helper cleanup. It then terminates and joins
only the owned process group before returning. A two-second graceful stop is
followed by group kill if needed. Repeated cleanup is safe, including early
child exit and constructor/listener/HTTP failure. The managed service's cgroup
is the final boundary after forced parent exit; a manual uncontained launcher
does not provide that boundary. Temporary child socket/cwd state is removed
after the owned process has been joined.

The helper disables file/network action tools and rejects questions immediately.
Actual server requests with IDs receive `-32601`; async question/delivery
notifications fail without a fabricated response or UI admission. The supported
Direct profile exposes no model tools, including clock and JavaScript exec/wait.
CodeModeOnly utility models are unsupported because the pinned protocol cannot
terminate their yielded cells after a completed turn.
Global user instructions remain trusted serving-instance policy.
When a turn/start reply is lost, a matching early notification may identify the
owned turn solely for interruption within the original deadline. It cannot
authorize success; missing or conflicting identity is not adopted. Unsubscribe
alone does not stop a running turn.
Operators must pause/stop naming through existing service/package procedures
before MCP reconfiguration, then let fresh calls capture all new literal server
names. Validate actual tool schemas, hostile dispatch, both question paths and
teardown against the exact candidate binary before live enablement. Fake client
tests and deterministic fallback do not prove model isolation.

Native ephemeral mode omits resumable conversations but its diagnostic SQLite
logger can retain plaintext copies of the naming input and thread/turn identities
in `logs_2.sqlite` under the configured SQLite home. The portal keeps that existing
local logger enabled. The naming input is limited to the first 8192 raw UTF-8
bytes and excludes attachment names, paths and contents. Codex 0.160.0 prunes
rows older than ten days at startup; this is not a guaranteed erasure deadline.
Accepted preparation requests and normal session history retain their existing
persistence contracts. Portal operational logs contain outcome and duration,
without prompt text or model output.
Changing authentication, provider configuration, managed policy or the fixed
resource requires restarting the naming child through the portal procedure.

With no adapter, or after an unavailable/invalid result, naming uses the first
nonempty line: lowercase, Unicode diacritic decomposition, removal of combining
marks, then up to six ASCII alphanumeric words joined with hyphens. Other
characters separate words. Truncation keeps whole words; if the first word
cannot fit or no words remain, the name is `session`. Attachment-only prompts
skip the adapter. Operational logs contain duration and outcome codes, not
prompt text or model output.

The selected base is saved before reservation. Automatic collisions add `-2`,
`-3` and subsequent suffixes, truncating whole words to retain the 48-byte limit.
Custom-name collisions fail. A frozen final slug never changes on retry.
Reservation shares creation-then-session locks with the CLI and excludes
tracking, archives, worktrees, receipts, other preparations, journals and
runtime/team state. Lock contention and unreadable occupancy stop the attempt.
An unrelated CLI start or fork cannot claim a frozen portal destination.

## Durable state and uploads

Version-1 preparation records live in private `session-preparations/` files
beside ordinary portal state. They preserve the full input, team snapshot,
digests, naming result, predetermined receipt ID, final handoff request and
deletion-history epoch while recovery can still need them. File replacement
and directory moves are atomic and synced. A successful rename followed by a
sync error reserves the exact published record. Until required directory syncs
succeed, admission and status return the request-bound persistence error above;
no receipt replacement, upload release or new side effect is allowed.
Same-process recovery confirms the published bytes before advancing.

Private in-memory dispatch bookkeeping separates a pending launch from a
dispatched worker. Identical submission repairs a pending admission launch once,
with the original receipt and attempt. A retry that published its incremented
attempt before a sync error exposes that current attempt after confirmation;
retrying it launches the pending worker without another increment. A worker
that already stopped after later I/O instead exposes stopped progress for an
explicit retry. Confirmation alone never repeats its naming call. Explicit retry
retains any published base and reservation; ordinary restart still pauses work.
Existing creation receipts, manifests, lifecycle journals and upload catalogs
retain their schemas.

At most 512 unfinished requests and 10,000 total admitted replay identities are
allowed. Mapping capacity is reserved on admission. Full records are bounded
to twice the existing receipt byte limit plus 16 KiB; compact mappings are
bounded to 2 KiB. The aggregate limit reserves those maxima. Admission rejects
new IDs at capacity and retains existing identities.

Attachment acceptance validates ready files before saving an intent, then
revalidates and atomically claims the draft scope in the existing schema-1
catalog using an initial submission with
`attempt: preparation:<request-id>` and `state: pending`. Pending retention
protects accepted files even before a slug exists. Another request or legacy
submission cannot reuse the scope. Every catalog mutation checks ownership
inside its transaction. Replaying an existing pending claim confirms catalog
file and directory durability before acceptance advances. Unconfirmed catalog
or preparation writes abort collection and retain selected files and ownership
evidence. For a confirmed accepting intent, collection completes the attachment
claim before allowing expiry to run; it leaves dispatch pending for the original
POST or explicit retry. If the claim cannot be completed, collection aborts.
Empty attachment selections do not claim a draft.

After reservation the same scope receives the final slug and epoch. Ordinary
creation proof transfers it to the final thread through the existing initial
attachment binding. Retention is continuous across that transfer. Reconciliation
runs before collection; it does not discard unresolved ownership based on age.
If an older writer adds a competing submission while the draft is accepted,
new recovery fails that preparation and retains both ownership evidence and
selected files. It does not silently pick an owner or release pending retention.

## Recovery and older packages

A saved intent without a catalog claim is not completed acceptance. Recovery
finishes the same intent or reports unavailable files. A pending claim without
an acceptance response resumes from the frozen snapshot. Restart pauses naming;
explicit retry uses any saved base and reservation. Shutdown cancellation never
commits a fallback merely because the server is stopping.

Receipt installation checks the exact predetermined ID, request, resolved team
and epoch. An equal-content receipt belonging to another request is a conflict.
A crash between receipt installation and handoff recording adopts only that
exact receipt, without launching another worker.

Terminal operations compact to immutable input-digest/receipt/slug/epoch/outcome
mappings in `session-preparation-mappings/` before ordinary receipt retirement.
Retirement also waits for confirmation of an already published mapping and both
directories after its move; an earlier failed sync cannot be bypassed by replay.
The CLI scans only unfinished records. A crash after compact replacement but
before the directory move remains recoverable. Receipt eviction or later
session deletion never allows the same request ID to initialize again.

Older packages ignore the preparation files and retain schema-1 pending uploads.
They cannot resume preparation progress, and older CLI code cannot honor the
new destination reservations. Newer recovery refuses a competing destination
created by that code. Compatible forward updates restore
the new endpoints and recovery. Workspace application switches remain
forward-only; operational recovery uses a newer package with the required
behavior and state readers.

## Focused compatibility fixture

After mandatory review, run `bash test/preparation_compatibility.sh` from the
repository's Nix environment. The runner extracts only portal source from exact
baseline `924c0ec28c41dd8b56aaf17f2212b302ca614899` into a temporary directory.
It never switches an installed package. Three nonzero Go selectors write a
current pre-slug claim, execute the baseline reader/collector and mutation APIs,
then restart current recovery against the resulting catalog.

The baseline phase verifies pending-file retention and refused deletion/append
of selected completed uploads. It also executes baseline Create/Append/Complete
for a new file and Prepare for a competing submission. The final phase requires
current recovery and retry to refuse the conflict before naming/reservation,
preserve selected bytes and retain both submission records. Fixture code alone
is not passing compatibility evidence; record its actual execution separately.
