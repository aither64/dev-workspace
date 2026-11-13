# Workspace package updates

A workspace update that changes the native Codex launcher or its startup
arguments pauses active agents, waits for them to settle, then interrupts any
remaining work after a timeout. It activates the prepared package and sends
resume commands. Updates that leave the validated native invocation unchanged
keep Codex running. Both paths retain the candidate, profile, claim and
registration checks.

While a maintenance operation is unfinished, it holds new work across the
profile, including registered native units that were initially inactive. Only
the operation's target startup grants allow replacement native processes to
start.

## Prepare and follow an update

Use `workspace-host switch --source SOURCE`. `--pause-timeout SECONDS` selects a
positive grace period, defaulting to 90 seconds. Preparation saves that value;
the worker records one absolute deadline before it sends any pause notice.
Recovery uses the same deadline.

Each native pause request, including any nested reconciliation, has a
five-second limit and cannot extend the saved grace deadline. Interruption and
notice reconciliation then share a 30-second allowance. Explicit recovery after
that allowance expires gets one bounded attempt without restarting the grace
period. The worker checks Cancel between requests and before advancing the
operation. An unanswered request, interruption timeout or uncertain pause
delivery leaves the operation for recovery. Saving results and cleaning up
cancellation notices also have time limits.

The worker records active session roots and active, ready team members, binding
each to its workspace, session incarnation, thread and Stop state. Each active
member is a separate participant. Native descendants remain owned children.
Preparation refuses unknown or ambiguous ownership of loaded threads.
Independent monitor operations keep their existing reporting and cleanup
contract and receive no maintenance resume command.

Preparation refusals identify the failed stage: target or operation identity,
source process/executable/socket capture, inventory, or operation creation.
Inventory reasons distinguish unavailable local metadata, unavailable native
proof and ambiguous ownership. A blocked inventory lists every present known
category in this order: `monitor-operation-unresolved`,
`retained-member-transition`, `unowned-loaded-writer`. Reasons contain no session
or thread identities, paths, or raw native errors. Existing proof, cancellation
and timeout reasons remain unchanged.

A refusal before operation creation saves no operation or startup receipt. An
I/O failure during creation can leave the operation journal without its active
pointer. Retain that journal and follow the source-owned recovery path for that
operation. A diagnostic reason alone does not authorize retry, cancellation or
recovery.

The worker steers a pause notice into the captured active turn. It asks
the participant to save useful context, settle or interrupt children, and finish
without starting more work. A turn that already ended receives no new pause
turn. No maintenance MCP call, acknowledgement or lease is required.

The worker reads turn status to check whether participants and their owned
children have settled. Completed, failed and interrupted turns all count. At the
deadline it interrupts remaining active turns, including a successor on the same
captured thread and identified owned children. These interrupts do not act as
user Stop. Queued prompts and active goals do not extend the deadline, and the
worker never clears their durable rows to obtain an idle result.

After the pause, application writers drain and managed clients quiesce. The
worker stops its captured native services through ordinary systemd shutdown,
using the existing 150-second service-stop bound. Stop or cleanup failure keeps
the operation in recovery and sends no resume commands. Shutdown is not a
transactional rollback of commands or external effects.

The ordinary activation path selects and starts the prepared target. Startup
grants are used once and remain bound to the operation. Process and socket
identity, registration, profile tokens, claims and pending-target recovery keep
their existing checks. Portals and stopped clients can remain held until resume
delivery is settled.

## Resume and Stop

After activation succeeds, each captured participant that the user has not
stopped receives its saved resume command. The command tells the participant to
inspect its saved checkpoint, current state and partial effects before retrying.
Existing queued work or a goal does not replace this command. Maintenance neither
reconstructs queue/goal rows nor automatically restarts children; the responsible
participant decides whether a child should run again.

Each resume has one stable submission ID. If delivery is uncertain, recovery
checks or retries that same submission. It never creates a new ID for the same
resume. Stop suppresses a resume that has not yet been admitted; after admission,
Stop interrupts work through its ordinary path. The operation then restores the
clients it actually stopped and releases the hold on new work. It does not wait
for a particular terminal-pane presentation.

## Status, cancellation and recovery

`workspace-host maintenance status [UUID]` inspects the current or exact retained
operation. `workspace-host maintenance cancel UUID` requests cancellation before
cutover. The worker retires or reconciles its pause notices, resumes captured
participants on the source unless the user stopped them, and restores its
recorded writers and clients. Once the worker has committed to source shutdown,
Cancel is too late.

Cancellation uses the same transition lock as normal resume. Before sending, it
rechecks the recorded source package, profile token and operation. Stop can
record its intent while delivery holds that lock; its native interruption waits
for an admitted resume to finish dispatching. Cancellation cannot send new work
after Stop completes.

`workspace-host maintenance recover UUID` restarts that operation's worker. It
uses the saved phase, deadline, source/target identities and delivery IDs.
Failure alone does not resume agents. Recovery may finish resume delivery after
activation succeeds.
Activation failure preserves the normal pending target and forward recovery.
Before target activation was admitted, the existing worker recovery can restore
the recorded source instead; the outcome reports that the update was not applied.

Source recovery keeps the original coordination attempt, prepared artifact and
claim. Validation requires their operation, candidate and current source token
to agree before activation intent is recorded. The worker must still verify
source shutdown and validate the native executable and registration plans before
recording that intent.

Keep the operation journal and retained package roots until recovery finishes.
The operation's recorded source package (`SourcePackage`) owns its protocol
through profile selection and worker restart. Commands, including active/status
queries, resolve that identity before interpreting the journal. Schema1
operations continue under their retained old package without journal conversion
or changed completion semantics. If the owner is unavailable or its response is
invalid, new work remains held.

## Enablement without a native restart

`workspace-host switch --source SOURCE --enable-maintenance` can enable automatic
pause and resume while the same package and validated native invocation keep
running. This uses a startup selection rather than a full update operation.
`workspace-host maintenance startup-status UUID` reports that selection's pending
or completed state. A regular maintenance status of `none` does not establish
that startup enablement finished.

For each already running unit, the host validates its complete registration
semantics and passes the hash of its launch marker to the controller. The
controller checks the actual invocation, process, boot, cgroup and listener,
then makes a passive native readiness request. It repeats those observations
before saving a ready attempt. This records an existing process; it does not
pretend that the enable command launched one. A compatible launch marker from
an older package remains intact. Initially inactive units still use their
one-use startup grants when they actually launch.

If same-package enablement remains pending, recover its original UUID:

```sh
workspace-host maintenance recover-startup UUID --source SOURCE
```

The selected package, source and authorized profile tokens, complete registered
grants and held machine claim must still match the saved intent. Every native
unit must already be ready; this command does not adopt, stop or restart it.
An exact matching Codex pending record may be present or absent. A malformed or
mismatching record refuses recovery. Missing state never counts as completion.

When the selected package cannot perform this recovery, execute the reviewed
repair candidate with `--from-candidate`. `SOURCE` must build that exact executor:

```sh
"$REPAIR_PACKAGE/bin/workspace-host" maintenance recover-startup UUID \
  --source "$REPAIR_SOURCE" --from-candidate
```

The repair executor does not replace the saved candidate or select itself.
Recovery restores the selected package's auto-archive and tracking-sync timers,
clears only the matching pending record if present, and finishes the original
startup selection last. A failure retains its intent and ready evidence for the
same command. A completed replay verifies the retained process and timer effects
without issuing new grants or restarting timers. Consumed but uncertain attempts
cannot be replaced by an observation of a different process.

This path requires the entire exact operation directory to be absent. Partial,
malformed and foreign full operations refuse it. Regular schema2 and historical
schema1 operations retain their recorded `SourcePackage` owner and ordinary
recovery command. Startup selections keep schema1 and its existing fields;
older readers can read the observed-ready attempt.

## Compatibility and the first upgrade

New operation journals use schema2. The package descriptor declares
`maintenanceStateSchema = 2`, `maintenancePolicy = 2` and
`maintenanceHeldCompletionPolicy = 1`. New native launches do not register the
dedicated maintenance MCP server. General reporting and independent monitor
MCP configuration remain unchanged.

Maintenance checks the actual target for those exact integer values during
preparation, fresh validation and activation admission, including recovery to
the source. The target also checks itself before changing services. Missing,
malformed or unsupported values refuse the operation. While held, a target with
mismatched registration refuses generic restart and retains the same operation's
startup recovery. A matching passive registration refresh does not start portals.
Ordinary nonmaintenance selection and startup-only grant recovery keep their
existing contracts.

A predecessor without this worker needs an initial coordinated idle window and
its ordinary source-bound `--from-candidate` transition. No separate MCP
installation step is required. Finish older active operations with their
retained owner before a conflicting transition. Keep application selection in
the user profile, update the system substrate before the profile, and retain
the ordinary rollback and recovery checks.

These operations keep development sessions open. They preserve saved
conversations, queue/goal state, model settings and responsible-thread ownership.
Verification must cover actual Ruby worker, systemd and profile activation,
including failures that must not send a resume. Focused checks and a local native
fixture alone do not establish those outcomes.
