# Team reporting startup upgrade and recovery

The daemon-start `team_reporting` MCP server replaces late per-thread insertion,
which native Codex ignores for a materialized warm thread. The
[portal reporting contract](workspace-portal.md#installed-team-policy) owns caller
identity, message arguments, host delivery and retry semantics. Existing rosters,
saved prompts/model/effort/access and queued policy records keep their formats.
Legacy bound helper invocations remain supported. This upgrade needs no native
version or codex-web API change.

Complete review, verification, exact builds and snapshot preparation before
activation. Keep the normal system-before-profile order and machine reservation.
A changed startup contract requires normal idle restart, even with an
identical native executable. Once reporting policy 1 is installed, a package-only
MCP command-path change is compatible when both complete invocations retain the
same authorities and tool policy. The host preserves the actual launch marker
and the retained helper dispatches each report through the selected profile.
When a restart is required, busy conversations defer cutover; do not stop them,
expand sandbox access, bypass ownership or select a different package around the
reservation. Normal reconciliation quiesces idle terminal clients, restarts the
shared daemon and restores retained conversations. Browser clients reconnect.
Provider transitions retain their original policy and forward-recovery rules.

After cutover, assign bounded checks through the actual lead to retained members
that were already materialized before the upgrade, then verify a new team.
Actual members must call `report_to_lead`; lead-side collection of final text is
not proof of transport. Retain matching host submission receipts and one lead
message for each stable ID, including duplicate-ID retry. Concurrent reports from
distinct members must keep their own native caller/session identities. Never send
from a member address as an operator to manufacture acceptance.

## Predecessor cannot decode or compare startup argv

A predecessor limited to `agents.*` launch arguments cannot prepare or activate
the new MCP plan. A reporting-policy-1 predecessor with strict argv comparison
also needs the candidate host's comparison for its first live package update.
The reviewed candidate provides narrow managed entries for these upgrades.
These are not bootstrap and cannot create a new owner,
reservation, approval or activation job. Use the actual managed lead, held
reservation and an explicit current predecessor profile token from ordinary
coordination evidence. The executing candidate must equal the output built from
the clean exact registered workspace source.

From that source, invoke the built candidate's `bin/workspace-host`:

```text
workspace-host coordination candidate-prepare SESSION --as-is --json \
  --source SOURCE --expected-profile-token TOKEN --owner OWNER \
  --reservation RESERVATION --request profile-prepare.json
```

The schema-1 request has `requestId`, `kind: "profile_activate"` and the ordinary
checksummed session `evidence`. The selected predecessor keeps ledger/catalog
and generation authority. Preparation uses the candidate's helper/probes and
retains the exact source and artifact. Approve and submit the returned normal
snapshot with the stable coordinator, preserving explicit system dependency and
the same reservation. Candidate preparation grants no deployment permission.

If the predecessor worker refuses the new argv or requires idle solely because
its reporting command path changed, before profile selection, retain
its original result and claim. Once that exact attempt's service has exited,
recover it through the same reviewed candidate:

```text
workspace-host coordination candidate-reconcile SESSION --as-is --json \
  --source SOURCE --expected-profile-token TOKEN --owner OWNER \
  --reservation RESERVATION --job ORIGINAL_PROFILE_JOB
```

Recovery requires the unchanged predecessor token, original owned profile job,
exact prepared candidate/source/artifact, mutation-intent phase and exited worker
proof. It holds the original worker lock and enters ordinary `activate-prepared`
with that attempt. Session/transition locks, exact machine claim, idle preflight,
compatibility and baseline checks all still apply. Busy refusal retains the same
claim for later recovery. No new activation job, direct profile selection or
forced resource release is permitted. Preserve the unknown-to-applied disposition
and the prior successful system job.

After profile selection, recover forward through the selected stable generation
and original attempt with ordinary coordination reconciliation. A changed token,
source, owner, reservation or artifact refuses the candidate entry. Do not use it
for merges, machine activation, unrelated recovery or default-branch approval.
