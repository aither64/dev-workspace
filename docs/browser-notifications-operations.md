# Browser notification operations

The [feature contract](browser-notifications.md) owns event, mute, presence and
worker behavior. This procedure covers the private runtime state and outbound
provider transport. It does not authorize deploying a package or changing any
session's lifecycle.

## Host prerequisites and verification

Allow outbound HTTPS to the exact supported provider hosts:

- `updates.push.services.mozilla.com` for Firefox;
- `fcm.googleapis.com` for Chrome and supported Chromium builds;
- `jmt17.google.com` for Chromium's upstream staging GCM service.

Chromium 147.0.7727.15 [defines that staging host](https://raw.githubusercontent.com/chromium/chromium/147.0.7727.15/components/push_messaging/push_messaging_constants.cc)
and [selects it for non-stable channels](https://raw.githubusercontent.com/chromium/chromium/147.0.7727.15/components/push_messaging/push_messaging_utils.cc).
Its [unbranded POSIX build reports an unknown channel](https://raw.githubusercontent.com/chromium/chromium/147.0.7727.15/chrome/common/channel_info_posix.cc),
which takes that route. This exact upstream host is supported without allowing
other Google subdomains. Staging ownership does not guarantee service
availability; verify the actual subscription and test delivery. Never rewrite a
browser-issued endpoint to another provider or path.

The sender accepts HTTPS on port 443 only, disables environment proxies and
redirects, validates every resolved address as public, then dials a validated
literal while TLS verifies the original hostname. Private, loopback, link-local,
metadata, reserved and documentation addresses are refused. The transport has
bounded DNS/connect/request deadlines, headers and response reads. A new
provider needs a source allowlist change and a subscription/test in the target
browser. The provider does not need inbound access to the portal.

Browsers need trusted HTTPS access to the private portal for registration,
fresh-status checks and clicks, and their own connection to the vendor push
service. Verify the explicit Enable and Send test actions with the actual
browser build. Check another active site, all portal tabs closed and the browser
minimized or unfocused. Verify that a click selects an existing session tab without reloading it,
changing its member or section, or losing an unsent draft. With no matching tab,
verify that it opens the intended conversation through normal authentication.
A synthetic handler test cannot confirm which tab Firefox selects after a real
macOS notification click.
Record queue acceptance, the last provider result, exact test worker receipt,
display API resolution or rejection, matching browser notification record,
physical OS appearance and click results separately. The last provider result
is registration-wide and may belong to another notification. A worker receipt
saved before display cannot establish that the display API resolved. A resolved
API or browser record still cannot establish physical appearance. Synthetic
HTTP/worker tests and Linux displays do not prove macOS display.

The successful Test reply adds only an exact event descriptor: `id`, `identity`,
`revision` and `expiresAt`. The client matches all four to the worker's bounded
local outcome. Reopen the controls after a background test to resume its check.
An old server or worker without these fields provides limited results, not proof
of delivery. New or refreshed controls update the existing root registration.
The successor skips waiting during installation and claims clients on activation,
so other tabs can stay open. The page confirms its own controller, rather than
only the registration's active worker, within a bounded ten-second check. Failed
updates remain unconfirmed and can be checked again by refreshing the page. No
unregister, resubscribe, VAPID reset or document reload is part of activation.
Existing loaded scripts in older tabs are not replaced; use new or refreshed
controls for current notification behavior.

The **Notifications** checkbox in the workspace menu and the full-panel buttons
use the same Enable/Disable actions. **Sound** and **Play sound** use the same
browser-local choice. Loading the page or opening the menu does not request
permission or change subscriptions. The worker may check for an update.
Permission and subscription changes require an explicit action. Disabling
notifications for this browser leaves per-session mute preferences, title
attention and reservation markers intact. If cleanup is incomplete, use
**Notification settings** to retry Disable. The menu shows a brief notice and a
mixed checkbox.

Browser Disable commits its durable off gate before server cleanup or local
unsubscribe. Provider-queued pushes carry their registration ID and are dropped
by a capable worker while off or after a fresh activation replaces that ID.
Disable does not queue events for replay. A new Enable accepts only future events;
continuous idempotent registration preserves its current ID and pending outbox.
Gate-storage failure still permits cleanup but cannot confirm durable blocking.

The existing version-1 IndexedDB store has a reserved gate containing `identity`,
`enabled`, `actionToken`, `registrationId` and the fixed `at` retention rank.
Its transaction-commit compare-and-set and origin-scoped Web Lock fence delayed
Enable/Disable results and destructive side effects across updated tabs. The
server's GET and the actual controlling worker advertise `deliveryEpoch: 1`.
Only after that server capability is known does fresh registration send
`requireFresh: true`; an existing endpoint returns HTTP 409 instead of adopting
its ID. Enable performs bounded cleanup and retries once. Old strict JSON
decoders never receive this optional precondition from updated controls.
Unsupported server/controller/storage/locking leaves setup incomplete. Preserve
the gate during forward recovery; deleting it would discard the off fence.

The browser-local **Play sound** setting uses the existing attention database.
Checked leaves `silent` out of the display options; unchecked sets
`silent: true` for Test, session and fallback notifications. Storage reads and
writes are bounded and wait for transaction completion; unavailable or malformed
state requests silent display. A rejected silent display is never retried with
sound allowed. No subscription, provider payload or private server record is
changed. Old loaded scripts do not implement this choice; use new or refreshed
controls and confirm the worker update.

Check native visual-only support and the saved choice in each browser profile.
[Firefox 132 added `Notification.silent` support](https://developer.mozilla.org/en-US/docs/Mozilla/Firefox/Releases/132).
Run one explicit Test with each choice and a background session event with
visual-only selected. Record physical appearance and sound separately. Browser
API support and recorded options do not prove OS sound behavior, and allowing
sound cannot override browser or system settings.

## Private state

`browser-notifications.json` lives beside `lifecycle-operations.json` in the
existing private per-workspace portal directory beneath the user state root.
The directory is mode 0700 and the file mode 0600. It contains endpoint
capabilities, subscription keys, VAPID private/public keys, browser IDs,
incarnation-bound preferences, baselines and pending deliveries. Do not print,
commit, upload as an artifact or put this file in Nix store.

Back it up with the private user runtime state. Package updates retain the VAPID
key. The store limits 128 browsers, 4096 session baselines/preferences, 512
pending deliveries and a 4 MiB snapshot. Full or corrupt state disables delivery;
it does not affect session execution. Uncertain writes fence further sending
until repaired and the portal restarted. Restore private state from a known
consistent backup instead of deleting it to clear an error. Deleting it can lose
mutes and replace the VAPID key.

If keys are lost, existing browser subscriptions cannot use the new key. In each
browser use Disable on this browser, then Enable again and Send test. Retain
compatible old keys for existing subscriptions during a planned rotation; this
version does not offer an automatic rotation command. Expired subscriptions
require explicit enablement again. Do not include endpoint URLs or provider
response bodies in diagnostics; the UI exposes only generic result categories.

## Disable and recovery

The browser's Disable action independently removes the server registration and
the local subscription. `POST /api/browser-notifications/disable` accepts
exactly one `registrationId` or `endpointDigest`, each a lowercase 64-hex string.
The digest is SHA-256 of the exact subscription endpoint. Deletion matches only
the current workspace and exact trusted portal origin, including when the
endpoint is no longer allowed for registration. An absent record succeeds
idempotently. Only matching registrations and their outbox entries are removed;
the lookup makes no outbound request and creates no registration or new private
state format. Existing clients can continue deleting by registration ID.

Before unsubscribing, the browser keeps the current subscription's endpoint
digest in one bounded cleanup receipt and retains any saved registration ID
separately. A saved ID may belong to an older subscription after a replacement
registration response is lost. When replacement registration succeeds, Enable
retires the previous ID before saving the new one. If retirement is uncertain,
the previous ID and current digest remain for an explicit Disable retry.
Disable removes both through at most two idempotent requests, each with exactly
one selector. It retains neither the
endpoint nor subscription keys. Server failure does not prevent local
unsubscribe. The browser verifies local absence and retains unresolved server
cleanup across reload for an explicit Disable retry. Enable remains unavailable
while cleanup is pending. Fully disabled requires both relevant server removals
and local absence to be confirmed.
Browser storage failure can prevent retaining the selector across reload; the
current page keeps it in memory for retry.

To disable delivery without stopping sessions, create the owner-only file
`browser-notifications.disabled` in the same private portal directory. The
running sender notices it on its next two-second pass, cancels unsent work and
retains keys, registrations and preferences. Settings reports that delivery is
disabled. Remove that marker to recover; events observed while disabled do not
replay. A push already accepted by its provider may still display under this
server-only marker; it does not change a browser's local gate. Use browser Disable
for the browser's permanent old-epoch drop policy.

For a package defect, use a corrected successor package through the supported
forward-only workspace selection policy. Keep private state and version-1
worker/payload handling intact. Deploying old assets cannot retract an installed
worker or provider-queued message. Before decommissioning the feature, disable
server delivery, remove every browser registration/subscription using the
browser control, then retire the installed worker after pending messages expire.
No notification cleanup may stop, archive, delete or release a session.
