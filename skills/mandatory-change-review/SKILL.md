---
name: mandatory-change-review
description: Run the required standalone review of committed vpsFree.cz development changes after quick verification and before integration tests. Use for feature, bugfix, refactor, or cross-project work that changes code, schemas, APIs, protocols, configuration, documentation, tests, deployment behavior, or security posture; skip only dependency-only or generated update sessions without relevant code changes.
---

# Mandatory Change Review

## Purpose

Use this skill after all intended changes are committed and quick local
verification has passed, but before long integration tests are started. The
review is advisory, but significant findings must be addressed or explicitly
discussed before continuing.

The review must be performed by exactly one standalone agent with fresh context.
The main agent prepares inputs, launches that reviewer, and then decides with
the user whether to change the implementation, amend commits, or continue. The
standalone reviewer performs the review directly and must not launch nested
reviewers or subagents.

## Invocation Mode

First decide which role you are in:

- If you are coordinating the development session, follow the Main Agent
  Workflow below and launch one standalone reviewer.
- If you are the spawned standalone reviewer, skip the Main Agent Workflow and
  follow Reviewer Instructions directly. Do not spawn another reviewer.

## Main Agent Workflow

1. Confirm the review is required. Skip only when the session contains no
   relevant code/design change, such as dependency-only updates, generated
   lockfile refreshes, or other mechanical metadata updates.
2. Make sure all intended changes are committed in every affected repository.
   Do not review a half-staged or partly uncommitted implementation.
3. Run quick verification first, using the local project guidance. Do not start
   long integration tests yet.
4. Prepare a review packet for the standalone agent:
   - requested outcome and acceptance criteria;
   - initiative slug, plan/state files, affected repositories and worktrees;
   - base and head commits for every repository;
   - the intended commit split, including any user- or plan-requested separate
     commits;
   - any deliberately bundled changes, with a concrete rationale for why they
     cannot be reviewed, reverted, tested, or explained separately;
   - relevant dependency pins or configuration repository changes;
   - quick verification commands and results;
   - known compatibility or deployment assumptions.
5. Launch exactly one standalone reviewer/subagent with this skill and the
   review packet. Instruct it to perform the review itself and not spawn nested
   reviewers. Do not pass hidden conclusions or ask it to rubber-stamp the work.
6. Treat the review result as a recommendation. Fix or discuss significant
   findings before integration tests. Record the result and any decision in the
   initiative `state.md`.

## Reviewer Instructions

Review committed changes across all affected projects. Inspect diffs, commit
history, local `AGENTS.md` files, relevant tests, documentation, and project
context before forming conclusions.

Assess the commit series, not only the final tree. Compare the history against
the user request and the initiative plan/state:

- Verify that each commit is independently reviewable and matches one logical
  purpose.
- Verify that mechanical moves/refactors, behavior changes, generated updates,
  dependency bumps, tests, documentation, and deployment/configuration changes
  are separated when review clarity, repository rules, or the plan call for it.
- Report bundled unrelated or independently reviewable changes as findings even
  when the final combined diff is correct.
- When the plan or user explicitly asks for separate commits, verify that the
  history follows that split.
- Subject-only commits are acceptable only when repository rules allow them for
  that change type, or when the subject fully explains a trivial mechanical
  change; otherwise report missing rationale as a commit-quality finding.

Commit split review is mandatory. Treat a commit as too broad when it contains
two or more changes that could reasonably be reviewed, reverted, tested, or
explained separately while keeping the branch sequence coherent. A commit
message that lists every included change does not make the commit focused.
Report bundled independent changes as `Blocking` by default, because the
author should split them before long integration tests. Downgrade only when the
review packet gives an explicit, convincing reason that the changes are
indivisible.

Use these concrete red flags when reviewing vpsAdmin-style feature branches:

- A managed notification-template installer commit must not also introduce
  Markdown rendering helpers, Telegram HTML/link rendering changes, synthetic
  test-notification behavior changes, or standalone uploader removal unless
  the packet explains why those changes are inseparable.
- A notification-delivery rate-limit commit must not also replace route-match
  schema/API/WebUI attribution, change event persistence semantics, expand
  test-notification authorization or route scope behavior, or rename generated
  defaults unless the packet explains why those changes are inseparable.
- Cross-cutting support edits, migrations, docs, and tests may stay with the
  behavior they support. If a test or support edit primarily validates a
  different behavior, it belongs with that behavior in a separate commit.

Check at least:

- Whether the committed changes match the requested feature, bugfix, or
  operational goal.
- Whether commits are logical and focused. Independent changes should not be
  grouped together, and split commits should still make sense in sequence.
- Whether the feature branch has clean git history. Commits that only fix,
  correct, or tidy behavior introduced by earlier still-unmerged commits in the
  same feature branch should usually be squashed into those commits unless
  keeping them separate improves reviewability.
- Whether a new feature that evolved entirely on the feature branch converges
  on one final design. Flag superseded protocol versions, schema variants,
  migrations, dual-read/write paths, and compatibility shims kept only for code
  or state that never reached the default branch. Prefer rewriting unmerged
  history so the final series introduces the final form directly. Preserve
  compatibility with merged, released, deployed, or externally consumed
  behavior, and require a concrete rationale for any other compatibility layer.
- Whether dependency update history is clean. A feature branch should not
  contain multiple commits updating the same flake input. The same rule applies
  to gem dependency update commits: repeated commits that only update the same
  gem dependency, dependency group, `Gemfile.lock`, Bundix output, or generated
  gem metadata for one update stream should be squashed into one dependency
  update commit.
- Whether commit messages follow the applicable `AGENTS.md` rules, wrap and
  format correctly, and describe the final end result and rationale rather than
  the development process. Do not enforce formatting on generated messages such
  as `confctl` commits.
- Whether the design fits existing project architecture and abstractions,
  especially across vpsAdmin, vpsAdminOS, HaveAPI, clients, and configuration
  repositories.
- For new or changed vpsAdmin API definitions, whether relationships to live
  resources use HaveAPI `resource` parameters and attributes instead of raw
  integer IDs. Prefer `resource Node` to `integer :node_id`, with an explicit
  `name` when needed. Allow raw IDs only when a resource association cannot
  represent the data safely, such as historical records or references to
  resources that may no longer exist, and require a concrete rationale. This
  check applies to the API contract, not database foreign-key storage.
- For vpsAdmin API resources, whether every top-level resource has a standalone
  source file matching that resource. Nested resources may remain in the same
  file as their parent resource.
- For new or changed ActiveRecord migrations, whether schema changes use the
  `change` method and direction-dependent data changes use `reversible` blocks
  within it. Prefer ActiveRecord-managed rollback over duplicated `up` and
  `down` methods that can drift apart. Allow `up` and `down` when the operation
  demands them or they make the migration materially clearer, and require the
  choice to be justified.
- Whether new or changed code uses defensive shape or capability probing
  instead of explicit contracts. Flag runtime method/property/type probes such
  as Ruby `respond_to?`, PHP `method_exists`/`property_exists`, Python
  `hasattr`, reflection checks, optional chaining used to mask uncertain data
  shapes, or helpers that try several possible input shapes unless the code or
  review packet shows a concrete boundary reason: external API compatibility,
  intentional polymorphism, generated/legacy migration data, or validated
  untrusted input normalization. Prefer validating and normalizing data at the
  boundary so internal code knows what it is passing.
- For vpsAdmin API changes, whether plugin-specific functionality stays in the
  owning plugin unless a generic core extension point is intentionally changed.
  Flag plugin-owned API resources, event/type registrations, mail templates,
  sysconfig keys, metrics, routes, or transaction behavior added to core files
  without explicit rationale.
- Whether the changes introduce vulnerabilities or tenant-isolation risks in
  the context of the whole system and cross-project interactions.
- Whether the changes are appropriately tested. Check unit/spec coverage,
  integration coverage where the behavior crosses components or daemons, and
  regression coverage for non-golden paths such as unexpected input, missing
  state, conflicting state, rollback/error paths, authorization failures,
  mixed-version flows, and repeated or partially completed operations.
- Whether tests that transfer containers, VPS datasets, backups, or replacement
  datasets verify data integrity, not just metadata. Expect a known file path
  with known contents, or an equivalent checksum assertion, to be created before
  the transfer and verified after the operation completes.
- Whether appropriate documentation, man pages, API docs, migration notes, or
  operational docs were updated when behavior changes.
- Whether deployment to running systems is safe with older versions still
  present. Consider persisted state, schemas, protocols, generated
  configuration, mixed-version operation, rollback, and whether any all-at-once
  upgrade requirement is justified.

## Output

Start with findings ordered by severity, using file/line and commit references
where possible. Include:

- `Blocking`: issues that should be fixed before integration tests.
- `Important`: issues that may be acceptable only after explicit discussion.
- `Advisory`: smaller design, documentation, or commit-quality improvements.

If there are no findings, say so clearly and list any residual risks or test
gaps. Keep summaries brief; the value of the review is in concrete findings and
deployment/security reasoning.
