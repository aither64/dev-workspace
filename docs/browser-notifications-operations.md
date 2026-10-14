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
minimized or unfocused. Verify click navigation through normal authentication.
Record provider acceptance, worker receipt, OS display and click results
separately. Synthetic HTTP/worker tests do not prove macOS display.

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
replay. A push already accepted by its provider may still display.

For a package defect, use a corrected successor package through the supported
forward-only workspace selection policy. Keep private state and version-1
worker/payload handling intact. Deploying old assets cannot retract an installed
worker or provider-queued message. Before decommissioning the feature, disable
server delivery, remove every browser registration/subscription using the
browser control, then retire the installed worker after pending messages expire.
No notification cleanup may stop, archive, delete or release a session.
