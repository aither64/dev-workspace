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

The workspace menu on index and session pages has **Notifications** and **Sound**
checkboxes. **Notification settings** opens the full panel with detailed status,
Test and recovery controls, also available in session Settings. The
**Notifications** checkbox shows confirmed browser setup. A mixed, disabled
checkbox means setup is uncertain; open **Notification settings** to check it.
Opening the menu reads the current state without requesting permission or
changing a subscription. Changing **Notifications** uses the same Enable or
Disable action as the full panel. Disabling applies across sessions in this
browser, while title attention and reservation markers keep updating. Per-session
notification settings remain unchanged and apply again after browser Enable.
Events from the disabled period are dropped permanently. Enabling again starts
delivery for future events; it does not replay a backlog. A push already accepted
by the provider may still reach the worker, which drops its old registration
epoch even after notifications are enabled again.

The **Sound** checkbox in the menu controls the same setting as **Play sound** in
settings. It applies to notifications from this portal in the current browser
profile, across sessions and tests. It defaults on when no choice has been
saved. Turn it off for visual-only notifications. When checked, the browser uses
its default sound behavior; it does not guarantee a sound. Browser and system
notification settings still apply. The control reports when the browser lacks
native visual-only support or the worker update is unconfirmed.

The choice is saved only after browser storage confirms the write. Reopening
these controls reads the last committed choice, including one saved in another
tab. A failed save reports the failure and rechecks the stored choice. An
unreadable or corrupt preference requests silent display while still attempting
the notification. Disable and re-enable retain the choice; clearing this site's
browser data resets it. Changing the setting affects the worker's next display
attempt, including a push already queued by the provider. It cannot change an
already displayed notification, enable delivery or mute a session.

Use **Send test notification** after enabling. The server waits ten seconds so
you can switch to another tab or app before checking delivery. Open
the full panel from **Notification settings** in the workspace menu or use
session Settings. The test result distinguishes arrival in this browser, whether
the browser accepted or rejected the display request, and whether it retained a
matching notification record. None of these confirms appearance on your screen.
The last push-service result may belong to another notification; it cannot
confirm this test. The page checks for a result until ten seconds after the
test's one-minute expiry and resumes the saved check after reopening. Missing
or unavailable results remain unconfirmed.

New or refreshed notification controls check for an update to the installed
worker. Updates activate automatically while other portal tabs stay open; no
page is closed or reloaded, and unsent drafts stay intact. The page waits up to ten seconds for the updated worker to control it, then
checks detailed Test support.
An unsuccessful or unconfirmed update leaves those results limited; refresh the
page to try again. Updating does not unregister the worker, replace a subscription
or reset browser data. Each explicit test gets a fresh notification entry;
duplicate delivery of that test coalesces.
Only older test entries for the same browser registration are closed. Session
notification tags and acknowledgement are unchanged.

**Disable on this browser**
first saves a browser-local off gate, then removes the server registration and
unsubscribes locally. It leaves the browser's
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
If the off gate cannot be saved, cleanup still runs, but Disable remains
incomplete and retryable. It cannot certify that delayed pushes will be blocked.
Denied permission requires a change in the browser's site settings.

Enable requires support from the server, the worker actually controlling this
page, and the browser's Web Locks API. An unconfirmed update or unsupported
component leaves setup incomplete. Reloading checks again; it does not bypass
these requirements. Updated tabs serialize subscription and cleanup actions.
A durable action token prevents an older completion from saving a registration,
changing the current controls or unsubscribing a newer setup. Fresh activation
requires an absent server endpoint and a new registration ID. Repeating Enable
while the same registration remains enabled preserves its ID and pending delivery.

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

The worker checks the browser-local gate before processing a push and again
immediately before display, after status and sound reads. For a managed browser,
the worker drops deliveries while off and deliveries with a missing or mismatched
registration epoch, without a popup or neutral fallback. Gate-storage errors also
prevent display. A bounded authenticated status read verifies the meaning of a
current enabled delivery. Expired,
resolved, muted, superseded, acknowledged or unverifiable delivery uses
**Session update available**. A missing or inaccessible session uses
**Workspace update** without session details. IndexedDB prevents an older
revision replacing newer known state; duplicates replace the same notification
with `renotify: false`. A worker crash can lose display. No exactly-once delivery
guarantee is claimed.

The browser's existing version-1 attention database keeps at most one pending
test descriptor and the latest test outcome per test identity, with no
database upgrade. These optional fields contain event IDs, revisions, expiry,
fixed stage names and an allowlisted browser error category. They contain no
endpoint, subscription key, provider response or notification payload. Older
clients or workers leave the new result unknown. Local diagnostic storage
failure does not prevent display when the delivery gate can still be read. The same database
also holds one browser sound preference, separately from the 512 attention and
test records. It uses the existing version-1 object store without a database
upgrade or a server-side preference. A fixed retention rank, rather than a
timestamp, protects this preference from pruning by already-open older pages.

The same object store holds one reserved delivery gate with `enabled`,
`actionToken` and `registrationId`. Its fixed retention rank protects it from
pruning; ordinary attention writes cannot replace it. Bounded transactions wait
for commit and compare the action token before enabling a registration. No
database upgrade is needed. An installation without a gate retains legacy
delivery behavior until its first explicit updated Enable or Disable action.
The gate persists across tabs, reloads and worker restarts. Supported attention
writes and pruning never delete it, and managed mode never silently returns to
legacy delivery. A context that has read a managed gate treats unexpected absence
as a storage failure. Explicitly clearing this site's browser data resets its
locally retained delivery state.

Once accepted by a provider, a message cannot reliably be recalled. Browser
Disable drops its old epoch in the capable worker. A mute or focus race within
the current enabled epoch can still produce a visible neutral notification.
An already-started display or shown OS notification cannot be recalled. The
browser's required `userVisibleOnly: true` subscription can have vendor behavior
outside the application's display calls. Clicking a session
notification closes it and focuses an existing tab for the same portal and
session, without reloading it, changing its member or section, or losing its
unsent draft. If several tabs match,
the worker prefers one already showing the target member. A missing final slash
still matches the same session. If none matches, it opens the validated
conversation route through ordinary portal authentication. A failed focus does
not open a duplicate tab. The click itself does not acknowledge attention;
normal visible, focused observation keeps its session identity checks.

The worker caches no portal page or API response. The root worker route retains
compatible version-1 payload handling across updates. `registrationId` is an
additive transport field, including Test; it does not change global attention
identity or persisted server events. Old workers and loaded controls cannot
provide the disabled-period drop guarantee. Use updated controls and confirm
their actual controller before enabling. Keep delivery off and recover forward
with capable assets if an older worker cannot honor the gate.
Newly captured requests with `browserNotificationsEnabled` need the
capable bundled session helper; predecessor helpers refuse that added receipt
field. Existing receipts with the field omitted remain supported. The
forward-only package and creation-transition fence keeps portal and helper
paired; recover with a capable successor package. New clients retain conversation
access when an older server does not
provide notification endpoints. Private notification failure disables delivery
without affecting Codex or treating a lost mute as enabled.

See [notification operations](browser-notifications-operations.md) for provider
egress, private-state backup, disable and recovery.
