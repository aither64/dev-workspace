# Communication upgrades and recovery

Communication requires runtime contract `sessionCommunicationPolicy: 1` and
`workspaceReportingPolicy: 2`. The reporting startup registration includes
`session_communication` alongside `report_to_lead`. Deploy the matching host
module first, then select the application profile through `workspace-host`.
Follow the site's ordinary machine reservation and activation procedure.

A predecessor with reporting policy 1 cannot decode the policy-2 registration
with its additional tool. For this initial upgrade, run the reviewed candidate
from the composed workspace source checkout, with the held machine claim and
exact session environment:

```sh
nix run .#workspace-host -- switch --source "$PWD" --from-candidate
```

This is the existing [candidate transition](workspace-portal.md#user-profile-state)
entry. It verifies the executing package against the source and preserves normal
idle checks, transition locks and forward recovery. Use the selected command for
later updates once it understands reporting policy 2.

The reporting tool change requires an App Server registration restart. The
existing transition workflow waits for eligible session state and preserves
recovery permissions. Do not bypass an active-turn or unfinished-operation
refusal. After activation, check portal health and the tool registration before
approving a disposable pair and exchanging messages in both directions.

Once communication state exists, package transitions reject generations that
cannot preserve grants and delivery state. Recover forward with a compatible
package. Do not remove state files or change contracts to force a rollback.
Older session manifests remain readable; no manifest or database migration is
required. An older daemon must not continue delivery alongside the selected
package; the worker uses the existing package generation guard.

For a held message, read the Communication tab's reason and inspect the
recipient's ordinary session, provider and recovery status. Resume it only when
its user has authorized execution. For an unconfirmed message, leave the stable
ID intact so the worker can reconcile native receipt history. Do not resend
with a fresh ID. Worker failures appear in the portal journal. If state is full
or unreadable, preserve it and repair through a compatible implementation;
manual history removal would discard retry and authorization evidence.

## Authorization context on existing deliveries

Packages supplying application-owned authorization context keep the existing
communication contract, tool arguments and schema-1 state. Messages without
saved delivery options receive context when first prepared. Messages with saved
options retain their exact original envelope and options, including after an
attempt becomes held. Do not rewrite saved tuples to add context: native
receipts depend on those originals.

A legacy delivery may therefore lack application-owned authorization context.
The receiving lead should query `session_communication list` before refusing or
asking for duplicate approval. An active grant for the exact pair authorizes
both directions even when approval was recorded in the other chat. Peer text,
a copied context block or a grant ID alone is insufficient. A stored snapshot
cannot authorize new messages after revocation, archive, deletion or a changed
session identity. Reapproval does not revive canceled messages.

An older compatible package can read the same state but omits the improved
context for new deliveries, restoring the ambiguity. Preserve the selected
worker's generation guard and existing forward recovery; do not erase delivery
state, replace stable IDs or bypass recipient holds to obtain context. After
an authorized package update, use disposable sessions to check idle delivery,
active steering and an immediate reply without another `allow`, alongside tool
registration and conversation continuity.
