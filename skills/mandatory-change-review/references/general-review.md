# General Review

Review scope, correctness, commit history, tests, and documentation.

## Commit Series

- Verify that the committed changes match the requested feature, bugfix, or
  operational goal.
- Verify that each commit has one logical purpose and is independently
  reviewable. Separate mechanical moves, refactors, behavior changes,
  generated updates, dependency bumps, tests, documentation, and deployment
  changes when they can reasonably be reviewed, reverted, tested, or explained
  independently.
- Treat independently reviewable changes bundled without a convincing
  rationale as `Blocking`, even when the combined final diff is correct.
- Verify any user- or plan-requested commit split. Subject-only commits are
  acceptable only when repository rules allow them or the subject fully
  explains a trivial mechanical change.
- Check that the branch has clean history. Fixup or tidy commits for behavior
  introduced earlier on the same unmerged branch should usually be folded into
  the owning commit unless separation improves reviewability.
- Require an unmerged feature that evolved through multiple designs to
  introduce its final protocol, schema, and behavior directly. Flag superseded
  versions, migrations, dual-read/write paths, or shims kept only for abandoned
  branch iterations. Preserve compatibility with merged, released, deployed,
  or externally consumed behavior.
- Require repeated updates to the same flake input, gem dependency, dependency
  group, lockfile, Bundix output, or generated metadata in one update stream to
  be consolidated into one dependency-update commit.
- Check commit messages against applicable `AGENTS.md` rules. Messages must
  describe the final result and rationale, not the development process. Do not
  reformat generated messages such as `confctl` commits.

Use these vpsAdmin-specific commit red flags:

- A managed notification-template installer commit must not also add Markdown
  rendering helpers, change Telegram HTML/link rendering, change synthetic test
  notifications, or remove a standalone uploader without an indivisibility
  rationale.
- A notification-delivery rate-limit commit must not also replace route-match
  attribution, change event persistence, expand test-notification authorization
  or scope, or rename generated defaults without an indivisibility rationale.
- Tests, migrations, documentation, and support edits may stay with the
  behavior they support. If they primarily validate another behavior, they
  belong with that behavior.

## Behavior and Coverage

- Check functional correctness, error handling, non-golden paths, and whether
  the implementation follows existing project conventions.
- Check unit/spec coverage, integration coverage across components or daemons,
  and regression coverage for unexpected input, missing or conflicting state,
  rollback/error paths, authorization failures, mixed-version flows, and
  repeated or partially completed operations.
- For tests transferring containers, VPS datasets, backups, or replacement
  datasets, require a known file and contents or equivalent checksum before and
  after the transfer. Metadata-only assertions are insufficient.
- Check that behavior changes update the appropriate documentation, man pages,
  API docs, migration notes, and operational documentation.
- For KB or DokuWiki changes, manually enumerate instructions that perform a
  vpsAdmin WebUI action and require each to be wrapped in a semantic
  `<vpsadmin-nav>` annotation bound to the affected language pages. A green
  discovery heuristic is not proof of completeness; a missing binding is
  `Blocking`.
