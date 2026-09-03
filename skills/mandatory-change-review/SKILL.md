---
name: mandatory-change-review
description: Run the required adaptive review of committed vpsFree.cz development changes after quick verification and before integration tests. Use for feature, bugfix, refactor, or cross-project work that changes code, schemas, APIs, protocols, configuration, documentation, tests, deployment behavior, or security posture; skip only dependency-only or generated update sessions without relevant code changes.
---

# Mandatory Change Review

## Purpose

Use this skill after all intended changes are committed and quick local
verification has passed, but before long integration tests are started. The
review is advisory, but Blocking and Important findings must be addressed as
described below before continuing.

The coordinating agent launches an adaptive team of one to three standalone
reviewers with fresh context. Every reviewer must use model `gpt-5.6-sol` with
reasoning effort `max`, perform its assigned review directly, and not launch
nested reviewers or subagents.

## Invocation Mode

First decide which role you are in:

- If you are coordinating the development session, follow the Main Agent
  Workflow.
- If you are a spawned reviewer, read Shared Reviewer Instructions and the
  reference for your assigned lane. Do not launch another reviewer.

## Review Lanes

Launch the general reviewer for every required review. Add each specialist when
its trigger applies:

- **General:** always. Read
  [references/general-review.md](references/general-review.md).
- **Architecture and repetition:** hand-written implementation, test, build,
  workflow, or configuration logic changed; or the change affects an
  abstraction, extension point, reusable component, or cross-project
  interface. Read
  [references/architecture-review.md](references/architecture-review.md).
- **Risk and compatibility:** the change affects authentication,
  authorization, tenant isolation, security boundaries, persisted state,
  schemas, public or cross-project contracts, protocols, host/node behavior,
  destructive or irreversible operations, deployment, rollback, or
  mixed-version operation. Read
  [references/risk-review.md](references/risk-review.md).

Documentation-only changes normally use only the general lane. If more than one
lane applies, launch the reviewers concurrently when capacity permits and
sequentially otherwise; lack of a free parallel slot is not a reason to omit a
required lane.

## Main Agent Workflow

1. Confirm the review is required. Skip only when the session contains no
   relevant code or design change, such as dependency-only updates, generated
   lockfile refreshes, or other mechanical metadata updates.
2. Make sure all intended changes are committed in every affected repository.
   Do not review a half-staged or partly uncommitted implementation.
3. Run quick verification first, using the local project guidance. Do not start
   long integration tests yet.
4. Determine the applicable lanes using the triggers above. Read every
   applicable lane reference before preparing the review.
5. Prepare a review packet containing:
   - requested outcome and acceptance criteria;
   - initiative slug, plan/state files, affected repositories and worktrees;
   - base and head commits for every repository;
   - intended commit split and any deliberately bundled changes, with a
     concrete rationale for why they are inseparable;
   - relevant dependency pins or configuration changes;
   - quick verification commands and results;
   - known compatibility and deployment assumptions;
   - for reusable or cross-project components, the owning component, public
     interface, and consumers discovered from imports, dependency pins,
     wrappers, manifests, documentation, and current repository state.
6. Launch one fresh standalone agent per applicable lane. Set
   `fork_turns: "none"`, `model: "gpt-5.6-sol"`, and
   `reasoning_effort: "max"`. Give each agent the review packet, its lane, this
   skill path, and instructions to read the lane reference and perform the
   review itself. Do not pass hidden conclusions or ask for a rubber stamp.
7. Collect all findings. Investigate conflicts using the code and repository
   evidence; do not decide by majority vote. Merge duplicates, retain the
   highest severity supported by evidence, and identify the originating lane.
8. Fix Blocking findings before integration tests. Fix Important findings or
   discuss and record an explicit decision before continuing. Advisory findings
   may be accepted at the coordinating agent's discretion but must be recorded.
9. If a fix materially changes code, architecture, contracts, behavior, tests,
   or commit history, rerun the general lane and every specialist lane affected
   by the new diff against the updated heads.
10. Record reviewer lanes, model and effort, reviewed commits, findings,
    decisions, fixes, and any reruns in the initiative `state.md`.

## Shared Reviewer Instructions

Review committed changes across all affected projects. Inspect diffs, commit
history, local `AGENTS.md` files, relevant tests, documentation, and project
context before forming conclusions. Review the commit series, not only the
final tree, and compare it with the user request and initiative plan/state.

Stay focused on the assigned lane, but report a concrete serious issue from
another lane if you encounter one. Do not assume that another reviewer will
notice it. Architecture findings must describe a plausible maintenance or
failure scenario instead of relying on pattern names or line counts.

Use these severities:

- `Blocking`: must be fixed before long integration tests.
- `Important`: may proceed only after a fix or an explicit recorded decision.
- `Advisory`: a smaller improvement or residual risk that does not block
  progress.

Start with findings ordered by severity, using file/line and commit references
where possible. If there are no findings, say so clearly and list residual
risks or test gaps. Keep summaries brief; the value of the review is in
concrete findings and compatibility, architecture, and security reasoning.
