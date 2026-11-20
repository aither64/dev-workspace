# Monitor inventory diagnostic

`workspace-portal monitor-lifecycle diagnose-inventory` measures the existing
maintenance monitor proof for one named session. It uses the cold transition
client and the same five-second context, check order and refusal rules as
maintenance admission. Its result is advisory; it does not authorize admission
or explain the cause of an earlier failure.

Supply these exact coordinates from the currently selected host registration:

- `--registry`, `--workspace-name`, `--workspace`, `--user-state-root` and
  `--runtime-dir`;
- `--session-slug`, `--root-thread-id`, `--socket` and `--authority-dir`;
- `--host-profile` and its resolved `--selected-package`.

The command requires matching `DEV_WORKSPACES_CONFIG`, `DEV_WORKSPACES_STATE`,
`DEV_WORKSPACES_RUNTIME_DIR` and `DEV_WORKSPACE_CODEX_HOME`. It derives the expected
socket, authority directory and native home using the host's existing workspace
path rules. It validates the registry and retained manifest before constructing
the client, then rechecks tracking and profile selection after the proof. Keep
the operator's session identity; the named session is only the measurement scope.
A locally compiled helper does not select or replace an installed package.

The proof can initialize or import the ordinary submission database, enable WAL
and restore missing predecessor fences. These are existing ledger bookkeeping
effects, so this command does not promise read-only local storage. It does not
reconcile receipts, change event outcomes, create a maintenance operation,
dispatch input, load or resume a conversation, archive a session, or control a
worker, profile or service. It uses the real ledger paths and preserves unresolved
attempts and all admission blockers.

The JSON result contains fixed stage/method names, per-measurement monitor
ordinals, elapsed and duration milliseconds, remaining budget and context state
at entry, immediate typed or owning result classes, decoder body-read counts and
the aggregate result. Immediate results are recorded before later error joins.
Unclassified errors remain `other`; the command does not classify error prose.
It omits native error text, paths, native thread IDs and transcript content. The
normal delivery logger is disabled for this selection.

Output contains at most 128 stage rows and 64 KiB. Truncation stops collecting
rows, never proof work. Local storage work uses cooperative cancellation and can
overrun the five-second context; wall time is reported without forcibly stopping
or relabeling that work. Client timings include connection, admission and decode
cost, not just daemon execution. A nonzero exit refuses the proof; retain that
original result without treating it as permission to retry admission.

Keep output private and assess the named session's sensitivity before retaining
it. Compilation, review and a fixture pass do not authorize a live invocation.
