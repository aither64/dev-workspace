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

Check at least:

- Whether the committed changes match the requested feature, bugfix, or
  operational goal.
- Whether commits are logical and focused. Independent changes should not be
  grouped together, and split commits should still make sense in sequence.
- Whether the feature branch has clean git history. Commits that only fix,
  correct, or tidy behavior introduced by earlier still-unmerged commits in the
  same feature branch should usually be squashed into those commits unless
  keeping them separate improves reviewability.
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
