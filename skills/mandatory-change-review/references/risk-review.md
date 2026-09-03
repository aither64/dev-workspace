# Risk and Compatibility Review

Review security boundaries, data safety, public contracts, deployment,
rollback, and mixed-version operation.

## Security and Data Safety

- Check for vulnerabilities, authorization bypasses, tenant-isolation failures,
  unsafe host/node interactions, and security regressions in the context of the
  whole system and its cross-project interactions.
- Check untrusted-input validation, privilege boundaries, secret handling,
  failure behavior, repeated or partially completed operations, and whether
  errors can leave conflicting or unsafe state.
- For new or changed migrations that have not been merged, released, or
  deployed, reject schema-existence guards such as `table_exists?`,
  `column_exists?`, `index_exists?`, `if_exists`, or `if_not_exists` when they
  merely accommodate a stale disposable database. Require the exact predecessor
  schema and reset disposable databases instead. Allow guards only for
  documented supported predecessor schemas with tests for every path; retain
  real data-integrity and conversion checks.
- Require transfer tests for containers, VPS datasets, backups, and replacement
  datasets to prove data integrity with known contents or checksums, not only
  metadata.

## Contracts and Deployment

- Identify public and cross-project API, generated-client, CLI, protocol,
  message, Nix module, configuration, and persisted-state changes. Verify both
  backward and forward compatibility where old and new components can coexist.
- Inspect actual consumer pins and deployment topology. Check required update
  order, rolling upgrades, rollback behavior, and whether an older version can
  read or safely reject state produced by the new version.
- Require explicit rationale and operator actions for intentional incompatible
  changes. For vpsAdminOS, call out and justify any change requiring coordinated
  updates of all machines or nodes.
- Check schema/data migrations for safe conversion, constraints, failure and
  rollback behavior, and compatibility with the immediately preceding deployed
  schema.
- Check host, node, daemon, and operational tooling changes for destructive or
  irreversible actions, exact target resolution, partial-failure recovery, and
  preservation of unrelated state.
- Verify that tests cover representative mixed-version, authorization-failure,
  rollback, retry, partial-completion, and invalid-state scenarios whenever the
  affected contract can encounter them.
