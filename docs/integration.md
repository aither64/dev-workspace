# Coordinated integration

This driver continues retained integration runs. New operations use the
[resource claim service](coordination.md); sessions own their integration steps.

The integration driver prepares approved operations, admits their dependency
graph together, monitors results and records completion. Background mode lets the owning
session remain idle while approved operations continue. It uses the coordinator's
ordinary merge and deployment workers. Physical resources remain free during
preparation, review, builds and input regeneration.

```sh
dev-session coordination integrate SESSION --as-is --request integration.json --json
dev-session coordination integration-status SESSION RUN_UUID --as-is --json
dev-session coordination integration-resume SESSION RUN_UUID --as-is --json
```

`integrate` and `integration-resume` wait outside admission locks. Use `--no-wait`
to advance once and return status. Interrupting the command leaves admitted jobs
running and retains the journal; resume the same run. Status does not reconcile,
create a session owner or change the ledger.

## Background continuation

For agent-driven work, submit the integration with `--background`:

```sh
dev-session coordination integrate SESSION --as-is --request integration.json --background --json
dev-session coordination integration-status SESSION RUN_UUID --as-is --json
dev-session coordination integration-pause SESSION RUN_UUID --as-is --json
dev-session coordination integration-resume SESSION RUN_UUID --as-is --background --json
```

Before accepting a new run, the driver checks every repository for unpublished
local default commits and dirty owned worktrees. A failure names the repository
and leaves no new journal or operation. Successful admission returns a run ID in
`prepare`; merges have started only after merge jobs appear in status.

Background mode requires `CODEX_THREAD_ID`/`CODEX_SESSION_ID` to agree with the
session's retained lead. Conflicting identities refuse. An unknown or external
conversation cannot rely on that lead's App Server to wake it. Use ordinary
foreground integration, or explicitly add `--to-session-lead` to hand responsibility
to the managed lead. This does not rebind the session or change saved policies.

For an existing background run, an external conversation can stay attached:

```sh
dev-session coordination integration-resume SESSION RUN_UUID --as-is --foreground --json
```

This command supervises the same run under its driver lock. Operation claims
remain separate, so workers can execute submitted jobs. Background continuation
stays disabled while supervision owns the run. Failure returns to the caller; ordinary errors pause continuation. After a
package-generation change, resume the same run through the selected command.
The supervised marker prevents implicit background takeover. Explicit resume
selects foreground supervision or a verified managed recipient. Keep the caller observed through the development
monitoring procedure. A detached external conversation has no automatic wake-up.

Acceptance returns the run ID before preparation begins. Record it in session
state, continue any independent work, then end the turn. A systemd worker prepares
and advances the approved operations without an attached CLI, browser or agent.
The coordinator wakes on state changes; its five-second tick recovers missed
signals and exited workers. Waiting runs do not consume model turns.

The Operations page shows background runs, automatic waiting and reasons for
intervention. Reservation grants and intermediate successes stay in that view.
The retained session receives one native message when its operations finish or
need agent action. Read authoritative status when it arrives. A message supplies
no merge or deployment approval and preserves the retained model and policy.
The lead reviews the result and updates initiative completion; background run
completion alone does not close the initiative or release a review reservation.

### Agent pickup

An attention or review notice has a `handoff` record in integration status.
`pending` means no agent has acknowledged responsibility. Native delivery receipts
alone do not change it. The matching managed lead acknowledges the exact event:

```sh
dev-session coordination integration-ack SESSION RUN_UUID --as-is --request ack.json --json
```

The schema-1 request contains a fresh UUID `requestId`, the status record's
`eventId`, and its integer `generation`. The caller's Codex identity must match
the retained recipient. A stale generation or another conversation refuses.
An exact request retry returns the original receipt. Acknowledgement lasts five
minutes; renew with a fresh request while handling the action. After an unclaimed
or expired acknowledgement, the coordinator retries the same logical event with
a new native delivery attempt. Reminders back off to at most one per fifteen
minutes. They may produce duplicates and never replay operations. The Operations
page distinguishes delivery from agent response. Pickup does not gate operation
completion, tracking publication or machine release.

Preparation with no submitted operations can be canceled explicitly:

```sh
dev-session coordination integration-cancel SESSION RUN_UUID --as-is --json
```

Cancellation holds the run and session locks, requires `prepare`, and refuses
any submitted job, submission step or deployment completion. It retains the
original request, preparation steps and receipts, records `outcome: canceled`,
and ends continuation without releasing reservations or claiming integration.
Use it when a reviewed replacement changes scope before any operation started.
Submitted or uncertain effects retain their existing recovery interface.

Pause takes effect before the next step or queued operation is admitted. Already
claimed operations settle through their existing workers and recovery rules.
A pause does not cancel effects, release a reservation or stop an in-flight
build. Resume explicitly after reviewing the reason and fixing the relevant
inputs. Conflicts, stale deployment artifacts, failed checks and uncertain effects
stop automatic advancement. Only existing certified mechanical refreshes continue
automatically; resumption cannot expand approval or replace reviewed evidence.

Each background run has a schema-2 journal; manual runs retain schema 1. Step
request IDs and coordinator receipts bind events to their run even if a submission
reply is lost. Per-run locking prevents overlapping continuation. A preparing
worker checks pause, suspension, lifecycle ownership and package generation before
its next admitted mutation. Archive and deletion hold continuation; revival keeps
that hold until explicit resumption. A profile switch transfers the exact run to
the selected compatible generation. Operation ledgers, snapshots, adapter protocols
and attempt envelopes retain schema 1.

Run completion writes its result and emits its completion notice before final
tracking publication. Informational delivery does not gate completion or release.
Pending notices remain visible and retry independently; a failed attempt uses a
new native message identity and can produce a duplicate.

`--background` and `--no-wait` are mutually exclusive. Existing synchronous calls
and manual `--no-wait` runs keep their behavior; `--no-wait` alone does not arrange
background continuation. A manual run cannot change execution mode under the same
request ID. Use `integration-resume --background` for a retained background run.

### Upgrading from journal schema 1

Packages read both journal formats. A profile upgrade can preserve pending manual
runs: `package.json` keeps the exact schema-1 integration descriptor accepted by
predecessor releases. The separate `share/dev-workspace/integration-contract.json`
advertises `schema: 1`, `journalSchemas: [1, 2]` and `certificationSchema: 1`.
Older package metadata readers ignore this file; current transition guards require
it when schema-2 journals are retained. Missing or unsupported contracts refuse
that selection. Upgrading preserves the pending manual run for its owner to resume.

Retained schema-2 journals intentionally prevent selection of a package that only
understands schema 1, including after run completion. Recover with a compatible
generation; do not delete journals to enable a downgrade.

## Request and approval

A schema-1 request has a lowercase UUID `requestId`, `mode`, `repositories`,
`evidence` and `approval`. Modes are `merge_only`, `integrate_first` and
`deploy_first`. Repository entries contain `repository`, full `target`, exact
`base`/`head`, a checksummed `review` reference and `dependsOn` repository names.
Dependencies name earlier entries. Use integrate-first when both integration and
deployment have already been authorized.

The original independent review receipt binds schema 1, `decision: accepted`,
the actual `reviewer`, repository, target, base, head and a `migrations` list.
The lead records this receipt from the completed independent review and retains
the original report. It does not authenticate reviewers or replace initial review.
Each evidence reference has `kind`, `path` and SHA-256 `sha256`, under the owning
session's tracking directory.

`approval` is the actual `user_instruction` source with its reference and quote.
Deployment uses a separate section with `target`, `approval`, `evidence` and an
optional existing `reservationId`. A `completion` section explicitly supplies
`deploymentWorkFinished`, acceptance `evidence` and optional existing replacement
mappings. Deployment/review authorization does not imply integration approval.

Optional `checks` entries name the repository, flake `output` and a checksummed
result `receipt`. Reusable receipts contain schema 1, `exitStatus: 0`, the complete
Nix `derivation` path and `inputs: {derivation: PATH}`. Nix binds its dependency,
tool and source inputs into that path. A different derivation runs the check.

## Mechanical refresh

The installed integration catalog is separate from the coordination catalog.
`lib.mkPackage` accepts optional `integrationConfig`; its default has no recipes.
Catalog recipes are immutable protocol-1 executables. Optional `refreshes` selects
recipe names and parameters by registered repository. A request can include an
explicit `recipe: {name, parameters}`. The run freezes those selections.

Refreshes operate in private scratch worktrees. They preserve original sources,
retain old feature refs, run declared hooks and publish only the clean owned
feature branch with an exact lease. A conflict, changed scope, migration, new
behavior needing review or unprovable transformation stops the run for the lead.
Resolved substantive changes need their applicable review and approval before a
new prepared operation can execute.

Certificate verification accepts identical trees or an exact reconstruction of
the reviewed patch over non-substantive tracking changes. It does not accept
stable patch IDs alone. An absorbed change must remain present; a reverted effect
fails reconstruction. Generated pins use the installed recipe's read-only semantic
verifier, including source identity and unrelated inputs/follows.

Review reuse and check reuse are separate. `certified_equivalent` replacement
receipts bind the original independent review, exact ranges, reconstruction or
recipe proof, and verification evidence. Intermediate deployments need their
retained exact comparison base and a proved bridge from the original review.
Changes outside those boundaries retain `reviewed_change` and its compatibility
review/later-deployment requirements.

Identical Git trees do not guarantee identical machine artifacts. A configuration
that embeds its Git revision requires preparation from the exact final commit.

## Admission, results and recovery

Ordinary commands and workers share the session lifecycle guard; lifecycle writers
remain exclusive. Logical writes use the ledger mutex, expected revisions and
idempotent request IDs. `--admission-timeout SECONDS` defaults to 30 seconds; zero
requests immediate admission. Timeout returns exit 75 without entering the
mutation callback. Waiting is cancelable and uses a monotonic clock.

Notifications run separately from scheduling, under a shared generation/lifecycle
guard. They retain event identity and native options. After a recorded delivery
failure, the next attempt uses a new native message ID and can deliver a duplicate.
Slow delivery does not hold exclusive session admission or block unrelated claims.

Private versioned journals retain every request identity and payload before
mutation. Resume reuses receipts and jobs, including partially successful batches.
It never blindly replays an uncertain activation or forces a reservation release.
Packages without the integration capability cannot replace a generation with a
pending run. Existing schema-1 jobs, snapshots and attempt envelopes are unchanged.
Profile recovery remains forward-only.

In deploy-first runs, resume through the stable command after profile selection.
The selected generation prepares new merge snapshots and approvals; it retains
the preceding deployment receipts. Preparation and verification bind to the
reviewed or certified head and stop if that feature changes. If a pin recipe
committed before its reply was lost, resume verifies the retained candidate
without generating another commit.

Status includes phase, exact heads, job states, queue time, verification/preparation
time and active administration time. Detailed timings live in the private journal.
The session's compact integration summary and result record operation completion;
the driver's final status also covers tracking publication. Records use the
owned-path tracking commit, preserving unrelated shared files and index entries.
Informational notification delivery does not gate integration
completion or machine release; pending events remain visible and retry through
the coordinator independently. Feature refs and sessions remain retained.
Each run writes its summary, result and equivalence receipts beneath
`work/SESSION/integration/RUN_UUID/`. These short writes require session admission
and the original tracking incarnation, including when resuming finalization.
Sensitive sessions retain these records locally and do not make a tracking commit.
File registration is unnecessary; the deprecated artifact add command only prints
a notice and performs no mutation.

Scheduling still verifies native thread ownership synchronously. Slow metadata
requests can delay scheduler ticks even though notification delivery is separate.

## Ruby verification

Package verification depends on separately cached Ruby checks. Portal-only source
changes preserve those derivations; Ruby commands, helpers, fixtures, contracts
and tools invalidate them. Session tests use four isolated processes and
coordination tests use two. An aggregate check requires complete unique test
coverage, including the suite's declared skips.

For diagnosis, run an unsharded suite in the Nix environment:

```sh
nix develop --command ruby test/run_ruby_suite.rb \
  --shard 0/1 --timings /tmp/session-timings.json dev_session_test.rb
```

The flake exports individual `ruby-*` checks; the package exposes `rubyChecks`.
The development shell provides build/test tools without building verification
outputs merely to prepare hook dependencies.

Optional bounded handoff metadata extends schema-2 journals without changing
operation, ledger or native request formats. Schema-2 predecessors can read it
but do not enforce recipient verification or agent acknowledgement. A rollback
therefore loses those handoff guarantees while preserving operation authority.
They also ignore the supervised marker and can resume a runnable continuation
without its attached caller. Pause or settle supervised runs before rollback
when that supervision boundary must be preserved.
Retained schema-2 journals still require a compatible reader. Canceled runs retain
their explicit outcome; older displays may label their terminal phase completed.
