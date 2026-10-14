# Session attention and browser notifications

Session titles can start with `?` for an unseen blocking answer or approval
request, or `✓` when the lead has finished a response and every ready team
member is idle. Input takes priority. The symbols describe the current response;
they do not certify review, deployment, integration or initiative completion.
Sleep, team waits, optional questions, failed turns and uncertain activity do not
create completion or input events.

`R` means the session holds one machine reservation; `R2`, `R3` and so on count
distinct held machines. Review and recovery holds remain included. The title
uses the coordinator's complete holder summary and exact session identity. A
failed read keeps the last known count with a stale-status notice. Only a fresh,
matching read can clear it. The Reservations tab names the machines and reasons.

A visible, focused visit to any section of the session acknowledges events the
page has already observed. The index, another session and a hidden or unfocused
window do not acknowledge them. Acknowledgement clears the title marker and
closes that browser's matching notification; it never answers a request or
releases a machine. Unresolved requests remain in the conversation. The read
status is retained in origin-scoped IndexedDB and shared with matching tabs and
the worker. Browser profiles, devices and portal aliases have separate read
status and subscriptions. Replacement sessions cannot inherit it. A retained page
requires reload if the session identity or root thread changes; it cannot
acknowledge, publish presence or save notification settings for the replacement.

## Enabling delivery

**Browser notifications for this session** appears in New session, Fork,
Implement plan and Settings. It defaults on, without requesting browser
permission. Clearing it mutes the session for every subscribed browser while
keeping title markers. Its optional creation field is retained through request
retries; older clients omit it and retain the default. Archive/revive preserves
the same session's choice. Replacement sessions start with a fresh choice.
Enabling delivery again does not replay events from the muted period.

**Enable browser notifications** is a separate explicit action. It asks for
notification permission, registers the worker and subscribes with the server's
public VAPID key. Notifications show the session name, relevant member and a
generic reason: **Your answer is needed**, **Approval needed** or **Response
finished**. They contain no prompts, commands or excerpts. Session names may
appear on the lock screen. The browser vendor's push service handles encrypted
delivery and can observe delivery metadata.

Use **Send test notification** after enabling. The server waits ten seconds so
you can close all portal tabs or switch windows before checking delivery. A
provider-accepted result means the push service accepted the message; confirm
that the browser and operating system displayed it. **Disable on this browser**
removes the server registration and unsubscribes locally. It leaves the browser's
site permission unchanged. The two removals are independent: server failure does
not prevent local unsubscribe. If server cleanup remains uncertain, the browser
keeps a small deletion selector for an explicit Disable retry, including after
reload. It contains an ID or endpoint digest, not subscription keys or the
endpoint itself. A saved ID can refer to an older subscription; Disable also
targets the current subscription's digest and waits for both server removals.
Finish pending cleanup before enabling again. Fully disabled means those
removals and local absence are confirmed. A lost registration response or saved ID
reports incomplete setup; Disable uses the endpoint digest without registering
the subscription again.
Denied permission requires a change in the browser's site settings.

The browser must support service workers, Notifications and the Push API in a
trusted secure context. Firefox and Chrome/Chromium use Web Push to wake the
worker with every portal tab closed while the browser is running. Some Chromium
builds lack a usable push service; successful subscription and an actual test
are required to establish delivery. A fully quit browser is outside this
delivery contract. Browser and OS notification settings can suppress display.

## Event and delivery contract

The server's existing activity observer watches the lead and ready retained
members independently of browser connections. Native turn/item/request identity
defines events. A successful lead turn can wait for the team to become idle;
member completion alone cannot trigger a completion notification. First
observation and uncertain continuity establish a baseline instead of replaying
old idle turns. An already persisted event remains unread across an observer gap
when its session identity and corresponding turn or request are still current.
Expiry limits desktop delivery without clearing that title marker. Stopped, held,
archived and unready sessions are ineligible.
Notification observation never activates or answers a conversation.

The private version-1 store atomically saves baselines and an outbox with VAPID
keys, registrations and session preferences. One pending delivery per browser
and session coalesces to the current event. Expired or irrelevant work is pruned
before admission, and a full outbox can still replace an existing session key.
New lead activity cancels unsent completion. A focused page may publish a
twelve-second presence lease; sending
waits while it is fresh, and suppresses the event once that page confirms it
displayed the event. Expired leases permit delivery without a tab.

Desktop delivery expires after five minutes for completion and fifteen minutes
for input. An unread title marker remains until a focused visit, new work or
request resolution supersedes it. The
sender rechecks ownership, event relevance, mute and registration before sending.
It retries network failures, HTTP 429 and server failures with bounded backoff,
up to six attempts within the original expiry. HTTP 404/410 retires the
subscription. Permanent provider failures appear in browser Settings. Provider
Topic and notification tag coalesce messages for the same session.

The worker always displays a visible notification for a delivered push. A
bounded authenticated status read verifies its current meaning. Expired,
resolved, muted, superseded, acknowledged or unverifiable delivery uses
**Session update available**. A missing or inaccessible session uses
**Workspace update** without session details. IndexedDB prevents an older
revision replacing newer known state; duplicates replace the same notification
with `renotify: false`. A worker crash can lose display. No exactly-once delivery
guarantee is claimed.

Once accepted by a provider, a message cannot reliably be recalled. A mute or
focus race can still produce a visible neutral notification. Clicking closes
the notification and focuses or opens its validated same-origin conversation
route. Ordinary portal authentication remains required. The click itself does
not acknowledge attention; the destination must become visible and focused.

The worker caches no portal page or API response. The root worker route retains
compatible version-1 payload handling across updates. Old clients ignore the
feature. Newly captured requests with `browserNotificationsEnabled` need the
capable bundled session helper; predecessor helpers refuse that added receipt
field. Existing receipts with the field omitted remain supported. The
forward-only package and creation-transition fence keeps portal and helper
paired; recover with a capable successor package. New clients retain conversation
access when an older server does not
provide notification endpoints. Private notification failure disables delivery
without affecting Codex or treating a lost mute as enabled.

See [notification operations](browser-notifications-operations.md) for provider
egress, private-state backup, disable and recovery.
