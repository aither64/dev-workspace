# Coordinated integration

The integration driver prepares approved operations, admits their dependency
graph together, monitors results and records completion. It uses the coordinator's
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
guard. They retain native options and message IDs and reconcile ambiguous sends.
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
the driver's final status also covers tracking publication. Records use the normal
artifact writer and owned-path tracking commit, preserving unrelated shared files
and index entries. Feature refs and sessions remain retained.
Each run writes its summary, result and equivalence receipts beneath
`work/SESSION/integration/RUN_UUID/`. These short writes require session admission
and the original tracking incarnation, including when resuming finalization.
Sensitive sessions retain these records locally and do not make a tracking commit.
Artifact registration passes `--expected-owner UUID`; the ordinary manifest writer
checks that incarnation under its own session lock before changing the catalog.

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
