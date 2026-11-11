---
name: dev-session-monitor
description: Run long verification through a fresh runtime-owned watcher when available, with active-parent native delegation as fallback. Use for authorized verification expected to exceed one minute and integration tests or builds of uncertain duration; keep known quick checks inline.
---

# Development monitoring

Use a fresh verification watcher to reduce routine monitoring work. Keep the
parent model and reasoning effort unchanged: the parent selects commands, interprets results,
diagnoses failures, makes changes, and decides whether to rerun or accept checks.
This skill authorizes a bounded monitoring subagent, not additional development
or deployment work. If you are already the assigned watcher, follow only the
watcher instructions below; do not delegate again.

## Parent handoff

Resolve the watcher policy before delegating. For each fresh managed watcher,
read the selected installed package's validated catalog. Select the exact model
and reasoning effort from the separate
`utilities.verification_watcher` record, together with the matching native
utility configuration from that same catalog digest. Resolve package-wide
`execution_access` from that package. Retain the launch's exact package, catalog,
model, effort and utility identity; do not change an existing watcher or saved
team settings. A team's retained catalog digest is not a resolvable utility
package pin; do not search the store by digest or infer a replacement lineup.

Give the watcher only the information it needs:

- Its monitoring role, this skill path, and the exact authorized command or CI
  run and attempt. Include canonical cwd, tested revision and input identity;
  do not pass the development conversation, credentials or environment values.
- Where to keep complete logs and which status source proves completion. The
  watcher owns a new local run. An existing run needs independently accessible
  status and logs; do not relaunch it to obtain a tool handle.
- Any caller or project deadline and escalation condition. Do not invent a
  timeout for quiet builds or tests. Cancellation scope is descriptive; canceling
  a monitor operation grants no authority to interrupt a command.
- The result format below and the requirement to return evidence without edits,
  diagnosis, retries, deployment, acceptance decisions or nested agents.

### Registered runtime operation

When `monitor_operation` is available to the exact current session lead, use its
`start` action for authorized lead-owned monitoring. Supply a fresh stable UUID
or 32-hex `id`, a bounded `brief`, and `source`. Local source names the exact
`argv`, `cwd`, `inputIdentity`, `revision` and `invocationId` equal to `id`.
CI source names provider, repository, workflow/run ID, positive run attempt and
tested commit. A rerun or different command needs a new operation.

The runtime creates a fresh standalone utility and freezes its selected installed
package, catalog, model, effort, instructions and execution policy. Verify the
acknowledged operation/source, utility identity and launch receipt before relying
on this path. A lost acknowledgement requires status inspection and an unchanged
retry with the same ID; never launch a replacement watcher or repeat the command.
Recovery retains the operation's snapshot even if the installed defaults change.

After confirmed registration, the runtime owns outcome delivery to the exact
lead, including when its turn ends. A busy lead receives steering. A stopped or
held session, user/approval wait, provider pause or unknown submission can delay
delivery. Check `status` for launch, outcome and delivery separately. A saved
event or queued input does not prove lead activation; require the native turn
receipt and actual turn/history evidence before reporting that it resumed.

`cancel` with the operation `id` revokes unsent continuation. It does not stop the
utility or command and cannot recall accepted input. If external effects remain
unresolved, the exact lead may use `resolve` with `operationId`, a stable
`resolutionId`, bounded nonempty `evidence` and `detail`. This records one
immutable resolution after native quiescence and receipt proof; it clears only
the external-effects fence. It sends no wake, rewrites no outcome, and cannot
clear unknown launch/submission or active-native fences. Retry the same evidence
with the same resolution ID; conflicting evidence is refused. Resolution evidence
must account for the source command and every known operation-owned code cell
or tool session. Keep known uncertainty fenced; an idle turn or the source
command's exit does not settle those effects.

A portal restart can reconcile retained events. A native restart holds execution
until ordinary explicit lead activation; it never restarts the utility or command.
Do not use lifecycle helpers to force completion. Retirement of a thread canceled
before bootstrap still requires exact native archive proof; retain refusal or
recovery state if that proof is unavailable. Completion never authorizes session
or utility archival, deletion or cleanup.

Automated native checks cover the retained thread and turn, prompts, queues and
submission ledger. They do not provide a complete cell or tool-session inventory.
The runtime trusts the utility to report that work; an unreported surviving cell
remains a limit of this contract. Native archive behavior with a surviving cell
has not been verified.

### Native delegation fallback

Use this path for member-owned checks or when runtime registration is unavailable.
For installed `full_access`, launch a standalone CLI watcher with
`--sandbox danger-full-access --ask-for-approval never` (or the executor's
corresponding explicit policy); do not add sandbox writable roots for Nix cache,
GC roots or daemon socket directories. Generated schema-6 utility TOMLs carry
that same execution policy. Verify the actual launch policy before treating a
watcher as ready. The watcher is a utility,
not a team member. Create its first native operation with fresh context
(`fork_turns: "none"`). Its lifetime is one operation: every later long or
uncertain operation receives a fresh watcher and never reuses this identity.

For an unmanaged session, use only an explicit applicable caller or site policy.
If none supplies the watcher lineup, visibly fall back to parent monitoring; do
not infer a model, effort, or native utility from this generic skill. Keep known
quick checks inline. Do not start or wait for CI the user has excluded.

Use one watcher for a related batch of checks when practical. With native
delegation, keep the parent turn active. Do independent useful work while the
watcher runs, then use interruptible native waits within the available tool
limits. Do not poll the same logs alongside the watcher or ask repeatedly for
unchanged status. Native messages do not establish a supported way to start a new
turn for an idle parent. Do not end the parent turn while promising automatic
continuation from a native watcher. Continue the authorized workflow when its
result arrives within the active turn; no user confirmation is needed for that
handoff.

If the skill's delegation tools, required managed or caller-supplied watcher
lineup, or a free agent slot are unavailable, say so once and monitor in the
parent with blocking waits and bounded output. Use the same fallback when an
existing run has no transferable observation interface. Do not silently
substitute a different watcher model, interrupt unrelated agents, or stall
verification waiting for capacity.

## Watcher instructions

Run only the assigned operation, or observe the specified existing run. Preserve
the exact command's exit status when capturing output. Use blocking waits where
possible and bounded status reads otherwise; do not repeatedly dump full logs.
For CI, track the specified revision and run IDs rather than a moving branch's
latest run. Collect available failure evidence without rerunning failed jobs.

Before reporting terminal `passed` or `failed`, account for every operation-owned
code cell and tool session. Finish or join all other operation-owned work.
Await the reporting call, then finish the current reporting cell before ending
your turn. If any identity or completion remains uncertain, report `incomplete`.
Include exact known cell, session and run identifiers and unknown effects in
`stillRunning`, with supporting `evidence`. An idle turn or the source command's
exit alone does not establish that this work ended. This grants no new authority
to kill a command or rerun it.

For terminal `passed` or `failed`, omit `stillRunning` or set it to an empty
string. Do not write `none` or a description of completed work there; put that
information in `detail` and `evidence`. A canceled command or CI run can report
`failed` with its authoritative exit status or conclusion once all effects are
accounted for, leaving `stillRunning` empty or omitted. Any unfinished or
uncertain work requires `incomplete`, with exact known identities and unknown
effects in `stillRunning`.

Report completion or an escalation condition promptly. If launched through
`monitor_operation`, call its `report` action from your registered native thread.
Use slot `terminal` with outcome `passed`, `failed` or `incomplete`, the exact
exit status or CI conclusion, elapsed time and bounded evidence. For escalation,
use slot `escalation`, outcome `incomplete`, a reason and exact `stillRunning`
identity. Reports take no operation ID or recipient: the runtime derives them
from your registered caller. Identical same-slot retries retain the original
event; changed reports are refused. An operation admits at most one escalation and one
terminal event. Inspect status after an uncertain acknowledgement; do not invent
a new report or wake-up about delivery failure. Native-only watchers return the
compact report below to their active parent.

After a registered operation escalates, retain ownership of the exact run and
continue only the observation and cancellation already authorized in the brief.
Report authoritative terminal evidence when available. This API has no reply or
reassignment channel; do not wait for new guidance or start another command or
attempt. If the authorized scope cannot continue, report terminal `incomplete`
with the still-running or unknown effects. Native-only watchers can use their
ordinary active-parent channel for guidance.

Canceling or resolving a monitor never grants command interruption authority;
silence alone is not a reason to kill a process. Do not leave a running operation
unaccounted for or claim that a timeout proves test failure.

Do not edit project files, write initiative tracking, investigate root causes,
change commands, install dependencies beyond the assigned command, retry, deploy,
approve results, or spawn agents. Local logs and result artifacts requested in
the brief are allowed. Follow any applicable project escalation requirements.

Return a compact report:

- Result: passed, failed, or incomplete; exit status or CI conclusion.
- Tested revision, command/run identity, and elapsed time.
- Log/artifact paths and a bounded excerpt identifying failures or escalation.
- Any still-running operation and any explicitly authorized cancellation taken.

Return raw evidence and uncertainty, not a diagnosis. The parent owns all
resulting decisions; native-only reporting requires its active turn. Retain
evidence; deleting an agent or its history does not reduce usage already incurred.
