# Workspace portal

The workspace portal keeps long-running development sessions available through
both a browser and tmux. One installed package generation supplies the portal,
Codex CLI, session helper, user units and any configured extensions.

The [Codex provider guide](codex-providers.md) describes the limits popup's
provider previews, session selections and workspace defaults, including automatic
pause/resume, operator catalog and recovery contracts.

## Session attention and notifications

The portal shares a validated session catalog between display, observation and
recovery readers. It checks file identity and change metadata before reusing
parsed records. Directory discovery is coalesced for one second; direct session
lookups and resume permission still check current records. A reverse index finds
the recovery owner of a watched thread, so reconnecting does not parse the entire
workspace once for every conversation. Missing or ambiguous ownership denies
resume. These process-local caches disappear when the portal stops.

Activity and transcript history use a shared passive connection pool, separate
from live subscriptions and interactive requests. Long conversations backfill
statistics through small item pages. Statistics remain partial while that work
continues; the transcript remains usable. Completed-turn totals and verified fork
history are cached, while active items and appended records continue updating.
No total conversation byte or turn ceiling applies. Individual messages retain
the transport safeguard; an oversized item fails its own history request.

After a failed activity read, the portal keeps the last good snapshot with its
original observation time and marks it stale. Retries use exponential backoff
from about two seconds to a maximum of one minute. Stale data cannot trigger a
fresh notification. Successful reads replace it. This does not change persisted
session, recovery or activity-checkpoint schemas, or the bundled Codex version.

Session titles show unseen input (`?`), a finished response (`✓`) and persistent
machine reservations (`R`, `R2`, …). Browser notifications use server observation
and Web Push with every portal tab closed. The [feature guide](browser-notifications.md)
explains permission, per-session mute, event identity and delivery limits;
[operations](browser-notifications-operations.md) covers provider egress and
private-state recovery.

Session settings use the same switches for automatic retries, session browser
notifications, Play sound and Keep open. Click a switch's label or focus it and
press Space to change it. These preferences save when changed; the model and
reasoning dialog keeps its separate **Save** action.

## Optional Codex questions

Optional questions appear in their original Codex message and in a panel below
the scrolling output. The message box remains available. Options appear as radio
lists, initially selecting the recommendation or first option. Select one and
optionally add text, or choose **Other** to write your own answer. Select **Send answer** to submit. Both views show the same selection,
text, delivery status and accepted answer.

Press Enter in a question's text answer to use its current **Next**, **Submit
answers** or **Send answer** action. Use Shift+Enter for a newline.

The panel starts as a narrow bar showing the number of pending questions. Click
it to expand one question at a time. Use the numbered question buttons or arrows
to navigate without sending or losing drafts;
**Send answer** sends the current answer and advances. Collapse hides the panel
without dismissing questions or losing drafts. New questions update the count
without opening the panel or moving focus. Reloading starts collapsed.

**Dismiss** removes a question from the pinned panel without sending anything.
The panel also clears when the turn ends or an ordinary follow-up is accepted,
matching the CLI lifetime. Unanswered questions remain answerable from the
original message. Answered questions show the actual answer, including answers
received from another client. Unsent drafts remain available inline after
unpinning or reloading the page. Browser storage keeps them separate for each
conversation and member thread. If delivery is uncertain, use **Retry answer**;
it checks the same submission instead of creating a second reply.

The portal uses codex-web's shared question controller and native reply envelope.
Optional questions use ordinary authorized message delivery and do not create
pending approval requests or mark Codex as blocked on user input.

## Workspace configuration

Each workspace can contain `.dev-workspace.json`:

```json
{
  "schema": 2,
  "displayLabel": "Example development",
  "hostLabel": "build-host",
  "sshHost": "build-host.example.test",
  "portal": {
    "hostname": "workspace.example.test",
    "aliases": ["old-workspace.example.test"]
  },
  "developmentClusterProviders": ["service"]
}
```

The portal hostname and aliases are configuration, not application constants.
`workspace-host register` reads them when no command-line override is supplied.
The registry stores the resolved values so routing remains deterministic until
the next explicit registration update.

The canonical hostname remains the portal's published base URL. At service
start, `workspace-host` passes only the registered aliases as additional exact
HTTPS origins. The portal permits browser conversation, upload and mutation
requests from that fixed set; it never derives trust from a request Host or
Origin header.

Provider IDs must exist in the immutable package extension catalog. Workspace
files cannot choose executable paths or labels.

## File links

Absolute file links in conversations and Markdown artifacts open a read-only
source viewer in a new tab. The viewer shows line numbers and syntax highlighting.
**Copy content** copies the complete UTF-8 preview without line numbers.
**Download** saves the original bytes, including binary files and files whose
preview is omitted, up to 10 MiB. Larger files cannot be downloaded through this
endpoint. A link ending in `:10`, `:10:5` or `#L10` selects line 10. Clicking a line number updates
the address; **Copy link** copies that address. Links without a line reference
open the beginning of the file.

Active repository links show the current worktree contents, including
uncommitted edits and non-ignored untracked files. The repository must be verified
for the session. Tracked files remain available even when an ignore rule matches
them. After archival, the viewer reads the recorded final commit
and displays its revision. A link is a live reference, so later edits can move
the referenced line. The viewer reports a missing line instead of selecting a
different one.

For repository links, untracked eligibility follows Git's standard excludes,
including repository ignore files, local exclusions and the configured global
ignore file.

Shared workspace links show current contents of tracked and non-ignored
untracked files. They can open documentation, the root
`AGENTS.md`, scripts and notes without a session registration. The configured
workspace must be the Git repository root. Files under `work/`, `archive/`,
`worktrees/` and `repos/` are excluded from this shared reader so session files
keep their session lookup and repository checks.

Session tracking links can open every regular file beneath the session's
`work/<slug>/` folder, including Git-ignored and unregistered files. Save files
there directly; no Git repository, worktree or live Codex connection is required.
After archival, the reader uses the preserved `archive/<slug>/` folder. The same
session-relative URLs continue to open the retained files after archive and
revive. Untracked project files outside this folder are unavailable after their
worktree is removed; the source viewer does not save them.

Paths are relative to the session tracking folder, such as `design.md` or
`reports/result.json`. Existing artifact declarations and links remain supported
without rewriting `portal.yml`. Labels are not displayed in Files and do not
control file access. Retained manifests still validate their captions and paths;
`name` remains a fallback when `label` is absent or blank. Repeated declarations
do not create duplicate file rows or expand the accessible root.

Older declarations and URLs starting with this session's `work/<slug>/` or
`archive/<slug>/` prefix resolve to the session-relative file only when the
literal nested file is missing and the session-relative file exists. An existing
literal nested file takes precedence; other access errors do not fall back.
Git metadata, symlinks, directories as files, special files and paths outside the
session root are unavailable, including through old preview or download routes.

`dev-session artifact add` is deprecated. It accepts its old arguments and
options, including `--expected-owner`, and prints:

> File registration is no longer needed. Session files appear automatically in Files.

It exits successfully without opening or changing the manifest, its labels,
receipts or session files. It does not require `--label` or a running conversation.

Text previews are limited to 512 KiB and 12,000 lines. Complete Markdown can use
a sanitized rendered preview, and PNG, JPEG, GIF and WebP images use their checked
media type. Raw shows the original text. SVG, HTML and scripts remain passive
source; they are never executed or embedded as active pages. Binary or limited
content shows a preview-unavailable notice while retaining Download within its
separate size limit.

The viewer route is `/files/<slug>?repository=<id>&path=<relative-path>#L10`,
or `/files/<slug>?artifact=<relative-path>#L10` for a session tracking file. The
repository ID is the same opaque ID used by repository review. The read-only
`GET /api/sessions/<slug>/file` endpoint accepts the same query parameters and
returns `path`, `repository` when applicable, `source`, the archived `revision`
when applicable, and a `content` object with the existing review preview fields.
Source values are `worktree`, `archive`, `artifact` and `archived-artifact`.
The `artifact` and `archived-artifact` values identify session tracking files,
including unregistered files. Session responses can also include an optional
`preview` object: `{kind: "markdown", html: "..."}` for sanitized Markdown, or
`{kind: "image", url: "/artifact-previews/<slug>/<path>"}` for a supported raster
image. The original `content` remains unchanged. Missing preview metadata falls
back to raw source. Preview and image URLs use the same confined session root.
Shared files use `/workspace-files?path=<relative-path>#L10` and
`GET /api/workspace/file?path=<relative-path>`. This endpoint returns the same
preview fields with `source: "workspace"` and no repository or revision. It
accepts exactly one `path` value and no other query parameters. The viewer labels
these files as current workspace contents; archived conversations still open
their current shared files rather than a historical version.
Existing URLs whose website path contains the absolute workspace file path
redirect to the viewer. The original conversation text remains unchanged.

**Files** has a directory tree on the left and the selected file on the right.
On narrow screens the panes stack. Each regular session file appears once under
its filename, without registration labels or badges. The tree loads on first
open; **Refresh** updates it. `#artifacts` opens Files too. Ordinary conversation
refreshes do not scan the folder or reload the tree. The tab shows a count only
after a successful listing.

A plain click opens the file inline and keeps the conversation draft. Filename
links retain a real standalone viewer URL, so modifier-click, middle-click and
the browser's Open link in new tab action still work. Markdown and supported
images open in Preview; other text opens in Raw. The icon controls have tooltips
and accessible names for Raw/Preview, Copy and Download. Switching Raw/Preview
uses the already fetched content. Copy uses the complete original text, including
its line endings, and is unavailable for binary or limited previews. Download
remains a real link to the original bytes.

The panes scroll independently. Refresh preserves open directories, scroll
positions and the selected path when it is still present. A removed selection
clears its actions. Loading and errors stay in the file pane; an older selection's
late response cannot replace the current file or its controls.

`GET /api/sessions/<slug>/files` returns `{files: [{path: "..."}]}` with sorted
session-relative paths. It enumerates metadata without reading file contents or
consulting Git. The whole result is limited to 5,000 files or 4 MiB. An oversized
or failed refresh keeps the last successful tree and reports the error rather
than publishing a partial list. Opening, previewing or downloading a file checks
the same confined session root again.

Downloads use `GET /api/sessions/<slug>/file/download` and
`GET /api/workspace/file/download` with the same query parameters and eligibility
checks as their preview endpoints. They return an attachment with an
`application/octet-stream` content type and a 10 MiB limit, independent of the
preview limit. Oversized downloads return HTTP 413. Archived repository downloads
read the recorded final Git object. No new file access is granted by downloading.

## Development cluster controls

Each cluster card shows its state and readiness above the services. Its toolbar
offers Start or Stop and Release when the provider supports them. Start uses the
existing cluster's saved topology and network with the provider's normal build
and boot behavior. Stop keeps its disks and configuration. Release stops the
cluster and deletes its persistent disks and cluster state after confirmation.
First-time cluster setup and maintenance recovery remain CLI operations.

Controls show elapsed progress and provider errors beside the toolbar. The page
refreshes cluster state without reloading or changing the selected service tab.
An admitted operation continues if the browser disconnects. After a failed or
lost response, check refreshed status before trying again; the portal never
automatically repeats a mutation. Busy or unavailable status disables controls.

Password fields contain a Reveal/Hide icon at the right edge. Copy sits beside
each input and aligns across rows. Passwords begin hidden, and copying works
while they are hidden.
Both icon controls have accessible names and keyboard support.

## Repository comparisons

The repository overview puts each card on its own row. The review controls
stay visible above **Local commits**. Its closed summary shows the commit count
and net changed-file, line and binary counts for the complete frozen base/head
comparison. Local history loads in the background so **Compare** becomes
available without opening it. The summary shows **Loading totals…** until the
totals arrive, or **Totals unavailable** if they cannot be loaded. If only the
totals fail, the commit list and **Compare** remain usable. The disclosure stays
open during status refreshes while the card is retained; a full page reload
closes it. Capture failures appear next to the actions even when history is
closed.

When a repository with an origin has workflow runs, its card shows a closed
**Workflows** disclosure with fixed Total, Queued, Running, Successful and Failed
counters. Total is the
number of runs returned for the exact selected revision, up to 100; it includes
skipped and neutral results. Queued includes requested, waiting and pending
runs. Failed includes cancelled and other unsuccessful terminal results,
including completed runs without a successful conclusion. Skipped and neutral
runs count only toward Total. Expanding the disclosure keeps each returned
run's original status, conclusion, workflow name and link. Its open state
survives refreshes of the same card and resets on a full page reload. A
successful empty lookup shows **Workflows · 0 total** without a disclosure;
missing or failed lookup data shows **Workflows · unavailable**, with any origin
error shown on the card. A later nonempty result starts a new closed disclosure.

Successful comparison responses always include `files` as a JSON array. An
empty staged or unstaged snapshot, committed branch comparison, or commit diff
uses `files: []` with zero changed-file statistics and no preview. An empty
snapshot keeps its capture identity and supports recapture. The browser also
accepts `files: null` from an older portal generation as an empty list; other malformed
values and HTTP failures remain errors. Empty views issue no file-preview
requests. Unverified submodule identities remain separate from changed files.

An open comparison retains loaded file content and editors while the repository
card refreshes or the reader scrolls. Collapsing a file hides its editor without
discarding the loaded result. Changing the layout or file version may recreate
an editor from retained content. Closing or replacing the comparison releases
these results.

**Load all diffs** expands every listed file and uses the same bounded queue as
scroll loading: two concurrent requests with up to four files each. Progress
shows completed files and failures, and Retry repeats only failed files. Binary
and limited previews complete with metadata. Loading every diff can use more
browser memory; the existing per-file preview limits still apply. The browser's
native Find searches rendered CodeMirror content, which can virtualize lines
and hide unchanged regions, so it may miss text outside the visible diff.

An active repository card can capture **Staged changes** from HEAD to the index
and **Unstaged changes** from the index to working files, including non-ignored
untracked files. If a file changed in both layers, the index version appears
between them. Capturing changes does not stage or discard files. Snapshot links
last only while the portal process retains them and cannot be saved as durable
comparison IDs. An open snapshot keeps its captured content when the repository
changes; an expired link returns a conflict and must be recaptured. Archived
repositories show committed comparisons only. Untracked content can also be
opened directly in the source viewer while the verified worktree exists.

Before publishing, capture checks HEAD, index entries and bytes, non-ignored
untracked paths, and candidate file identities again. It refuses conflicts,
sparse or skip-worktree entries, unsafe paths, and special files. Split indexes
use Git's read-only expanded view; unsupported records fail rather than produce
a guessed comparison. The process keeps at most 32 snapshots, with 64 MiB of
admitted raw previews per snapshot and 256 MiB total. File previews remain
limited to 512 KiB and 12,000 lines per version; comparisons remain limited to
5,000 files. A working file over 512 KiB has frozen metadata and a **not
compared** notice, without an exact content diff or line count. The response
marks aggregate line counts incomplete. Unchanged large files are not opened
for preview. Candidate discovery uses read-only index stat records without
running configured clean filters; racy index timestamps remain candidates.
Staged gitlink changes retain their commit identities. Unstaged capture does
not inspect nested submodule worktrees. It lists each indexed gitlink's frozen
path, mode, and object ID separately, outside changed-file totals. The list
does not claim that a submodule is dirty.

Repository registrations can include `github: owner/name`. The GitHub origin
validates that value, builds HTTPS links, and loads the remote branch, exact
head, and workflow runs. If a GitHub lookup fails, local commits and diffs
remain available and the card shows the error. Without a GitHub registration,
local review remains available without external links or remote status.

Repository status JSON retains `github`, `githubError`, `compareUrl`,
`branchUrl`, and `actionsUrl`. It adds `origin`, `originError`, and `originLinks`;
history and comparison responses also include `origin` when available. The
manifest schemas, CLI registration flags, and saved review IDs remain unchanged.
Older portal versions can read the same manifests and saved comparisons. GitHub
is the only supported origin; there is no runtime origin URL or plugin setting.

## Conversation history in the portal

The Codex tab reads the newest 100 transcript items on first load for the lead,
team members and archived sessions. Moving upward within 200 pixels of the
transcript top loads one older page, including when the view cannot scroll any
farther. Another page requires another upward action. Items already on screen
retain their DOM nodes as new output arrives; loading older items keeps the
reader's scroll position. If more than one page arrives while a
browser is disconnected, the tab reads bounded pages until the new and retained
history meet. An active item that has moved out of the newest page is also
checked for its final update.
The portal uses codex-web's `createTranscriptHistory` and `readTranscriptPage`
browser helpers for merging pages and deciding when legacy fallback is allowed.

The browser uses `GET /api/sessions/<slug>/thread/page` for the lead and the
equivalent member conversation route for members. The existing `/thread` route
remains available to older browsers. The new browser uses it when paging is
explicitly unavailable, or when the page route is missing and an authorized
legacy read succeeds. Authorization failures, timeouts and invalid cursors
remain errors. A changed cursor keeps loaded messages visible while a fresh
bounded history repair checks their continuity.

In lead and member conversations, the composer shows the last confirmed model
and reasoning effort. The pencil icon opens a dialog with both selectors and **Save**
sends the pair together. Closing the dialog or pressing Escape discards edits.
An open dialog keeps its draft during polling and reconnects. Reloading starts
from the server-confirmed pair.

The Plan/Default switch sits with the attachment and Send controls on the left.
The model summary, pencil icon and Interrupt control sit on the right, centered
vertically within their row.

Model and reasoning settings can be saved during active, idle, interrupted and
unknown turn states. Native settings become defaults for the next turn; the
current turn continues with its original settings. Stopped or recovery-held
sessions save the choice without loading a thread. Explicit activation applies
it before queued messages or a saved goal can resume. Archived sessions remain
read-only. A failed or uncertain update keeps
the dialog open and rereads the thread. The controls never send a compensating
update automatically. Reads begun before a successful save cannot overwrite its
confirmed pair.

Browser refresh timings come from codex-web's `conversation/assets/refresh.js`.
That policy contains resource intervals, deadlines, retry backoff and notice
delays. Background refreshes keep the last values visible. Initial transcript
loading appears after 750 ms and manual older-history loading after 250 ms. Routine index
status loading is silent, and membership is published before optional repository,
activity and cluster enrichment. The additive `runningClustersKnown` display flag
distinguishes unknown counts from measured zero; older assets keep the integer `runningClusters` fallback.
An absent flag retains legacy numeric behavior. Unknown counts do not grant
controls or generate notifications. Refresh failures appear after 30 continuous visible seconds; returning to a hidden tab
or restoring a page starts a fresh grace period. Access errors and failures of
user actions appear immediately.

Hidden conversation tabs pause transcript, request, queue, timing, details, team
and provider reads. Returning to a tab starts fresh reads even if an old request
has not settled. Late results cannot replace the current view. Provider catalog
refreshes share one in-flight read. With typed pending invalidation, ordinary
transcript updates respect a 15-second interval after a successful pending read;
new questions, local responses and reconnection force a refresh. Older servers
keep the existing refresh behavior.

Team status unavailable marks stale peer-activity observations. A successful
HTTP response clears it only when the returned observations are fresh.

Instrumented non-streaming read responses include `X-Portal-Read` for correlation
and numeric `Server-Timing` stages. Timings cover the measured lookup, verification and details dependencies;
nested stages overlap. Limits-cache waits are measured separately. Admission and
native connection/write waits remain unmeasured. Slow or failed reads emit at most
one `portal_read` log record per second across the portal. These records contain
fixed lane/stage names, durations, status and cancellation category, without
session or native identifiers, paths, queries, request/response contents or error
text. Response timing ends when headers are written; the log total includes the
handler's completion. These diagnostics do not change authority or time budgets.

Index snapshot warnings use the same grace period as failed HTTP refreshes,
including when the server successfully returns its last known state. Repeated
warnings do not restart the grace period. A healthy snapshot clears the warning.

User messages use the same sanitized Markdown renderer as assistant messages.
Attachment decoration runs before presentation, so generated file-reference
envelopes do not appear in the rendered message. Canonical message text and
receipt digests stay unchanged. Team assignments and reports have a distinct color
and identify the sender and recipient when available. **Messages** hides team
traffic; **Team** shows it;
**All** includes it; **Activity** shows other activity. Errors remain visible in
every view. Communication labels are presentation hints; they do not authenticate
identities or grant access.

When you stop following the transcript, **New output** tracks changes to visible
content under each filter, including streaming messages. Hidden entries,
timestamp updates and changes inside closed details do not trigger it; changes
to a visible summary do. Hidden updates leave an existing indicator intact.
Each filter keeps its scroll position and unread state.
Returning to the tail or selecting **New output** clears that view's indicator.
Initial history and older pages do not count as new output.

Message times come from recorded item timing when available. While metadata
loads, the portal keeps an exact time already observed for that entry. For a
new entry without an exact time, it leaves the time blank until enrichment
finishes or the shared 30-second visible grace period expires. A fallback based
on turn timing has a `~` prefix; its tooltip identifies it as approximate.
Exact times replace estimates as soon as they become available. Time and date
grouping use the same observation. Returning to a hidden page starts a fresh
grace period. Older servers without the metadata-loading signal display
available estimates immediately.

**Send**, **Steer now** and **Queue next** save the original message and
attachment IDs before clearing that draft. **Send** stays disabled until the
conversation handler is ready. You can then type, attach files and submit
the next message while an earlier request is pending. This tab dispatches one
request at a time in submission order, continuing after acceptance without
waiting for the turn to finish. The tab can retain up to 20 outstanding
submissions per conversation, including the active request. A full buffer or
unavailable browser storage keeps the current draft intact and reports an error
beside the composer.

Locally saved submissions show **Waiting to send**, then **Sending…** during
delivery. **Send** or **Steer now** starts an idle turn or steers the turn active
when the request reaches the server. **Queue next** keeps its native queue
delivery; accepted queued work appears under **Queued messages**. Native FIFO
and **Start next** still apply to that queue. The local dispatch order does not
promise a combined execution order across steers, queued turns and other clients.

Reloading the same tab retains submission order. Waiting messages and uncertain
outcomes remain paused; saved transient retries resume under the policy below.
Use **Resume sending** for the first waiting entry or **Retry original** to check
an uncertain outcome with its original action, ID, text and attachments. A
buffered **Outcome unknown.** entry holds later submissions, including revised
wording, until it is resolved or you explicitly choose another copy. Uncertain
entries cannot be edited, replaced with the draft or discarded. An uncertain
**Queue next** entry uses its original queue ID for retry and observation; it
offers no copy action.

When the server confirms that a message was not sent, **Not sent.** offers
**Retry original** with the same ID, text and attachments. This also applies to
a recovered attempt that was previously uncertain. A lost retry reply can make
the outcome uncertain again. Older responses without this proof remain uncertain.

For a transient **Not sent.** failure marked retryable, the portal retries the
original ID, text and attachments automatically. Delays increase from 250 ms to
five seconds. Retries wait while the page is hidden or offline, settings are
being saved, or another submission or plan delivery is in progress. A buffered
message can retry only at the head. An older transient receipt outside the
buffer can retry only when the buffer is empty and idle. Unknown outcomes never
retry automatically. Reload restores saved retryability. **Implement here**
keeps its plan context and dedicated delivery endpoint, with the same retry
policy and busy checks. These retries leave the current draft intact.

A recognized validation rejection on this tab's fresh first request, such as an
unavailable attachment or oversized prompt, shows **Not sent** with different
recovery controls. **Retry unsent** uses a new ID with the receipt's text and
original attachment IDs; you can edit that text in the receipt. To change files,
update the text and files in the main composer, then choose
**Replace unsent with draft** on the failed first entry. This saves a new ID with
the current selection and original Send or Queue next action at the same position,
then clears that draft. A failed save keeps
the draft intact. **Discard unsent** abandons that rejected entry and releases
the next one. The same validation error during recovery of an uncertain ID does
not establish that its earlier request was unsent.

For an uncertain Send or Steer receipt, **Send another copy…** first waits for
reconciliation of the original ID. If delivery is still unknown, it asks you to
confirm that the original might still arrive. If the check confirms delivery
or proves the message was not sent, the portal uses that result without
creating a copy. The copy gets a new saved ID and retains the original unknown
receipt and its replacement link. It replaces the original
at the head of the buffer; a copy of an older receipt outside the buffer joins
the tail. Later waiting entries keep their order. Repeated recovery and reload
reuse that saved replacement ID. Use **Retry original** to recover an uncertain
copy. A copy still linked from its original receipt cannot create another copy.
The draft and its attachments stay intact. The portal saves both receipts, their
link and dispatch order before sending the copy. Inconsistent records stop
dispatch and show an error. Older receipts outside the buffer do not automatically
hold new submissions. Their retries and copies still allow only one request at
a time in this tab.

If delivery is confirmed but its status could not be saved in the browser,
**Retry saving** saves that status without resending the message. Keep saved
receipts, replacement links and dispatch order when recovering pending work;
an older browser cannot be assumed to understand every pending dispatch state.

Once the transcript or an accepted receipt confirms delivery, a later HTTP
failure does not change that confirmed state. A successful newer send or queue
submission clears the composer's previous error. Earlier uncertain receipts
remain visible independently. Late responses from an older operation cannot
restore a stale composer error. Sending revised wording creates a separate
attempt and does not resolve the earlier one. Capturing a waiting message is not
successful delivery and does not clear that error.

Admitted sends finish and record their receipts even if the browser disconnects,
within the existing operation deadline and server shutdown. A lost reply can
still leave delivery uncertain; absence from history does not make automatic
resending safe. Delivery failures appear beside the composer without a browser
alert, and uncertain delivery alone does not mark the service unavailable.

A late response or transcript acknowledgment never clears a newer draft, even
if its text is identical. After acceptance, sending the same text again creates
a new message. For receipts outside the dispatch buffer, the small **×** hides
the receipt across reloads while retaining its recovery identity; browser
storage must save that dismissal first.

Successful legacy history fallback is silent. History notices and manual retry
controls occupy normal layout space above the transcript.
Automatic history repair retries with capped backoff without loading banners.
A manual older-page failure offers Retry for that page. Expired cursors trigger
reconnection and a check of retained history. Servers without paging keep the
existing legacy behavior.

Requests, queued messages, activity timing, session details and index status
refresh separately from the transcript. A slow queue reconciliation does not
delay the first conversation page. Failed refreshes retain their last result;
request and queue warnings offer Retry after the shared grace period. Routine
queue checks stay silent during initial load, background refresh,
reconnection or manual Retry. The last confirmed queue stays visible while a
read is pending or fails. Before the first successful read, the portal does not
assert that the queue is empty. The warning grace starts with the first failed
read; access errors and manual Retry failures appear immediately. A warning
stays visible during Retry until a successful read clears it. These read notices
are separate from submission receipts and queue mutation errors.

Transcript receipt retirement requires positive server evidence across all
retained pages, including automatically loaded older messages. While the server
checks conversation metadata, the mode buttons retain the conversation's last
confirmed selection and remain usable. Before the first
confirmation, "Checking mode…" appears after the shared initial-loading delay.
Mode refresh failures use the shared warning grace and keep the confirmed
selection visible. A different thread starts without a retained selection.
Plan actions stay hidden until fresh metadata identifies the mode. The tab
retries metadata reads with backoff. Activity reads have a five-second budget
while active and a thirty-second budget while idle; focus and reconnection prompt
a fresh read.

## Automatic turn retries

Automatic retries start enabled for managed lead conversations and
ready team members. Native child threads are excluded. Only new failures after
enrollment qualify; installing the feature does not restart an already-failed
task.

When session retry policy is available, Settings contains separate switches for
capacity and cybersecurity errors. Both apply to the lead and all current and
future retained team members. The conversation notice shows the current or
latest retry chain. Retry now accelerates a scheduled attempt; Stop retrying
stops that chain without changing the session preferences. Dismiss hides only
that result in this browser. The notice appears inside the conversation viewport
and leaves no space when hidden.

On first use, an existing opt-out on the lead or any retained member turns both
session switches off. Later changes apply to all retained members, including
members added afterward. Enabling consumes an already-failed turn; an unchanged
independent active turn may qualify if it fails later. Turning a switch off and
on does not restart a stopped or uncertain chain. Preference changes wait for
temporary session activity within a 30-second request budget; polling does not
reserve exclusive access.

Confirmed retry starts count accepted automatic submissions. A possibly
submitted attempt appears separately. If confirmed-start evidence is incomplete,
the count is partial or unavailable. Terminal results keep their elapsed time
fixed. Retry preferences
and activity metadata are optional: an unavailable record may hold automation,
but Send, Queue, Implement plan and team delivery retain their normal admission
and exact receipt recovery.

A terminal capacity error retries after 15, 30, 60, 120, 240 and 300 seconds,
then every 300 seconds until cancelled. Each interval adds up to 10% positive
jitter, limited to three seconds and a total interval of 300 seconds. A native
cybersecurity turn error retries immediately, with at most 30 possible automatic
submissions per conversation in five minutes. Exhaustion leaves the turn failed
for manual action; expiration of the window does not rearm it.

The retry starts a visible continuation in the saved conversation:

> Continue the interrupted task from the saved conversation. Inspect the current state before repeating actions.

It preserves the failed turn and its results. It does not replay the original
request or tool calls. Retries require proof of the original model, effort,
provider, mode and authorized execution binding. Missing proof leaves the task
for manual continuation. Queue work and recovery holds take precedence.

The native thread must remain loaded, with the exact failed turn still current
and no active work or pending requests. Codex 0.163.0 can report `systemError`
after a terminal failure until another turn starts. Retry accepts this loaded
status as well as `idle`; the status alone does not make a failure eligible.
The server still verifies the error code, settings, queue, submission ledger
and managed admission. It never resumes a cold thread or clears native error
state to make a retry possible.

New input, settings changes, Interrupt and session Stop cancel pending retries.
A cancellation after dispatch keeps checking whether that request started; it
cannot undo an accepted continuation. Use the existing Interrupt control to
interrupt a running turn.

The server schedules work while the browser is closed. The optional activity
notice displays server-reported retry counts, elapsed time and status. It marks
partial or unavailable evidence. The browser never submits a timed retry.
Transport errors, native intermediate retries, ordinary assistant refusals and
unrecognized failure codes do not start an automatic chain. The
[recovery guide](turn-retry-recovery.md) documents persistence, admission,
settings provenance and compatible package recovery.

The execution guarantee applies to managed portal, CLI and retained-team writers
that share the runtime's admission locks. A managed manual send or queue admission
that wins the lock cancels or supersedes the pending retry. If automatic dispatch
owns the lock, manual input waits until dispatch and receipt handling finish. An
unknown automatic outcome keeps new input held until exact reconciliation. Explicit
manual input can still steer active work. In Codex 0.163.0, public `turn/start` can
implicitly steer an active regular turn and apply persistent settings. It has no
atomic compare-and-start operation. Independent direct native writers bypass the
managed locks and remain outside this guarantee. An observed settings or input
mismatch leaves the failure for manual continuation.

## Session recovery after runtime loss

The portal restores access to previously running sessions when tmux runtime
state is lost. It keeps the exact root thread and saved conversation. Native
threads remain unloaded until Send, Continue, queue Start, or a team assignment
explicitly activates the addressed thread. Cold history, pending-request and
queue reads do not subscribe to native events or refresh thread instructions.
Saved active goals and queues therefore remain dormant. A portal-only restart
preserves execution permission when the App Server socket generation matches.

The session page reports stopped, recovering, waiting, active or failed state.
Recovery refreshes the mounted page without replacing the composer or draft.
Continue reuses one durable decision across retries, including requests from
another browser. If there is queued work or an active goal, it activates that
work. Otherwise it queues one visible continuation prompt with a stable ID.
Cold Send uses the same native queue and preserves FIFO order.

The restored-session notice has an X button. Dismissing it leaves saved queues
and goals paused and sends no message to Codex. The server remembers dismissal
across reloads, browser visits and portal-only restarts. A new runtime restoration
shows a fresh notice. Sending a message or explicitly starting queued work still
activates the addressed conversation. Recovery errors remain visible.

Notice preferences use separate private thread records, bound to the retained
session incarnation, terminal identity and App Server socket generation. They do
not grant execution permission or change recovery file schemas. Older packages
ignore these records.

Private schema-1 recovery files live under
`$DEV_WORKSPACES_STATE/session-recovery/<workspace-sha256>/<slug>.json`.
They bind the canonical workspace, slug, root and socket, automatic restoration
intent, continuation receipt, and active thread IDs. Execution permission
applies only to the socket's boot ID, device, inode and creation timestamp.
Writers use a private interprocess lock and an atomic, synced replacement;
invalid or mismatched records refuse recovery. The canonical
`dev-session resume` command owns terminal creation and existing lifecycle,
generation and identity locks. Automatic restoration has two concurrent slots.
Timeouts or a missing socket may retry; identity and lifecycle refusals remain
visible for manual Retry after repair.

All portal, CLI, provider and observation clients share a transactional SQLite
submission store at `$DEV_WORKSPACES_STATE/submissions/<socket-path-sha256>.sqlite`.
The first access imports the persistent schema-3 JSON ledger, or its older
socket-adjacent fallback, under the predecessor locks. It retains private backups
and fences both JSON paths before accepting database writes. Interrupted import
publication resumes on retry. Archive recovery exports one thread through the
client API and preserves the existing held-prompt format.

The runtime contract declares `submissionStoreSchema: 1` and
`submissionStorePolicy: 1`. Once a database or its sidecars exist, package
transitions require both capabilities. Stop predecessor application writers
through the coordinated profile transition before migration; native turns can
continue running. Recover forward with a compatible package. Do not restore a
JSON backup over newer submission records. The generic
[storage guide](https://github.com/aither64/codex-web/blob/master/docs/submission-storage.md)
owns import and delivery semantics.

Slow or failed delivery stages appear in the portal journal with the stage,
thread, browser attempt ID and duration. They contain no prompt text or options.
`DEV_WORKSPACE_DELIVERY_TRACE=1` logs every stage for a controlled diagnostic run.

The runtime contract declares `sessionRecoveryPolicy: 1`. Once recovery records
or persistent receipts exist, normal package switches refuse a target without
that policy. Recover forward with a compatible package. Historical package
commands do not enforce the new gate and are outside the supported rollback
procedure. Workspace manifests, lifecycle journals, team rosters and native
rollouts retain their existing formats.

On the first upgrade, the portal seeds intent only from an exact verified live
terminal and records which root or ready member threads are already loaded.
Stopped sessions remain stopped. Deploy the complete application composition
and matching host substrate before relying on restoration. Existing enabled
user services and user lingering provide boot startup; recovery runs after the
portal has its configured native endpoint. Verify restart behavior with private
native and terminal fixtures, rather than rebooting an occupied development host.

Existing browser and activity subscriptions check the recovery permission before
every implicit native resume, including after a connection loss. A new App Server
socket generation holds those subscriptions until explicit activation. Restarting
only the portal keeps the permission for the unchanged native socket.

Observation distinguishes exact saved queue receipts from unknown submissions.
Verified queued work permits terminal restoration and remains a busy condition
for archival. Continue retires its decision after successful activation; a new
socket generation or explicit stop also clears a pending decision. A lost
activation response retains the same receipt for retry.

Native connection failures and request deadlines cross the helper boundary as
`transport_unavailable` observations or exit status 75. Busy session or creation
locks during `dev-session resume` also return exit status 75. The recovery coordinator
retries them after its backoff. Identity, lifecycle and malformed-state refusals
require explicit Retry after repair.

## User-profile state

The default paths are:

- registry: `~/.config/dev-workspaces/registry.json`;
- installed profile and retained Codex roots:
  `~/.local/state/dev-workspaces/`;
- per-workspace sockets and runtime authority:
  `$XDG_RUNTIME_DIR/dev-workspaces/<name>/`;
- router socket: `/run/dev-workspaces/router.sock`.

Profile switches are forward-only. `workspace-host rollback` refuses to select
an older package; recover by repeating the same switch or selecting a newer
package. Commands waiting on a transition lock reject a generation change and
must be run again.

Portal initialization waits for shared transition admission before reconciling
creation records. SIGTERM cancels that wait, so a profile switch can stop a
portal that has not begun serving HTTP. After initialization, normal shutdown
parks archive preparation before canceling operational workers.

Profile activation also refreshes existing systemd instance and enablement links
that point into older dev-workspace packages. They follow the stable profile or
local unit template, so collecting an older package does not shadow the current
template with a broken instance. Disabled services remain disabled, and custom
unit files or links are preserved.

### Switching while Codex threads are active

Packages publishing `livePackageSwitchPolicy = 1` can switch while threads are
active when the candidate resolves to the same native Codex executable, launch
policy, launch arguments and native child-thread capacity as every running
workspace's launch marker. Reporting policy 1 permits the package-dependent MCP
executable path to change: both complete invocations must validate against their
own package and the same workspace, state root, profile and tool policy. The
retained reporting process resolves the selected profile on every report. Other
argument changes still require idle restart. Catalog defaults, portal code,
session tools and package paths alone do not require a Codex restart. The host refreshes
registration inventory and restarts the portal and router; Codex, tmux and
managed terminal clients keep running. Browser clients reconnect to the portal.

The immutable launch marker continues to describe the package and catalog that
actually started Codex. Inventory describes the selected package. Neither a
catalog update nor a live switch rewrites launch provenance. Existing rosters
keep their saved settings; newly selected presets and members use the selected
catalog.

A predecessor without this policy requires one idle cutover and Codex restart,
even if the native invocation is unchanged. This retires member report helpers
that dispatch to their original package. New helpers bind the configured user
profile and resolve its `bin/dev-session` on every report. Missing or malformed
launch markers, changed launch arguments or a different native executable also
take the idle restart path. The candidate's bundled executable is checked, unless
the operator explicitly supplies `DEV_WORKSPACES_SYSTEM_CODEX`.

Lifecycle journals, unfinished creations, cluster and authority checks, and the
exclusive transition lock still apply to live switches. A failure after profile
selection retains the forward target and pending reconciliation evidence. Retry
the same switch from the stable command; do not select an older generation.

### Cluster transition policy

The runtime contract uses development-cluster state schema 1 and transition
policy 3. A package switch with any registered cluster state requires the target
to declare the same schema and tracking limit, and an equal or newer transition
policy. The check runs before provider adoption, session quiescing or profile
selection, and repeats during activation. Each candidate provider must also
accept the retained cluster through `transition-adopt`.

Policy 3 supports packages whose providers preserve a retained-disk maintenance
hold and its copy receipt. Publish and activate the complete runtime/provider
composition; raising the policy number alone does not add provider support.
Schema-1 clusters can move forward from policy 2 without disk conversion or
reset when the candidate provider accepts their state.

An ordinary switch from policy 3 to policy 2 refuses while any registered
development-cluster state exists. This conservative limit applies even after a
maintenance hold has finished, and to state owned by another provider. Do not
reset retained clusters to bypass it. Recover with the same or a newer reviewed
package that understands the maintenance state. With no cluster state, this
particular check does not restrict the target policy; other checks still apply.

Old package binaries, including their `--from-candidate` recovery and store
helpers, cannot prove a newer maintenance hold. Invoking them is outside the
supported recovery procedure. The policy gate protects normal transitions;
it does not make every historical binary enforce the current contract.

If the installed `workspace-host switch` cannot parse valid persisted state,
run a reviewed candidate that understands that state through its explicit
recovery entry from the source checkout:

```sh
nix run .#workspace-host -- switch --source "$PWD" --from-candidate
```

This entry requires an installed profile and verifies that the executing
package is the exact output built from `--source`. It then applies the same
transition lock, session and cluster preflights, profile selection, and
recovery sequence as a normal switch. Session terminals are quiesced through
the still-selected package and restored through the new package after profile
selection. Use the installed command for ordinary
updates; the candidate entry does not rewrite saved team rosters or creation
journals.

An archive journal paused at `tracking_committed` can block that switch when
the selected package cannot prove an archived conversation or when part of a
retained team was archived before its roster update completed. Recover only
that recorded operation with the candidate package:

```sh
nix run .#workspace-host -- recover-archive \
  --source "$PWD" --workspace NAME --session SLUG
```

This command accepts only an exact archive journal without a cleanup sidecar at
`tracking_committed`, keeps the selected profile and Codex generation unchanged,
and verifies the committed archive tracking while holding the normal session
locks. The candidate validates the selected executable's version and generated
protocol contract; a compatible selected 0.160.0 needs no upgrade. It first
proves an active idle or already archived retained root and
the exact retained team, refusing unknown active same-directory threads. It then
reconciles already archived team members and archives only retained, materialized
active members that pass the normal idle and submission checks. A final exact
proof must cover every member before the command invokes the selected package's
private lifecycle executor with only the portal helper replaced by the candidate
helper. A failed retry leaves the journal in place. Members completed before an
interruption remain archived and are rechecked on the next retry.

Archived proof requires the exact retained `vscode` identity to be unloaded;
an archived but loaded conversation still blocks recovery. The
[recovery guide](session-archive-recovery.md#retained-conversations-and-selected-executor-recovery)
explains the repeated public proof and dormant native queue rows that a later
explicit resume can make runnable.

New archive operations also seal a private cleanup sidecar. Finish these through
their capable ordinary executor; narrow recovery that delegates to an older
selected executor refuses them. [Archive cleanup and recovery](session-archive-recovery.md)
describes prepared intents, retry proof and the old no-sidecar compatibility path.

## Browser lifecycle confirmation and recovery

Lifecycle status reads return published snapshots without waiting for package
locks or native inspection. One background task reconciles them with the owning
receipts and journals; unavailable proof keeps the last known state marked stale.
Retry, dismissal and new confirmations still perform fresh ownership checks.
If another receipt writer wins during that check, the mutation reader checks
the current receipt and journal again within its existing deadline. Display
refreshes can discard a stale proposal; mutation checks cannot treat that
discard as successful proof of the winning receipt.

The index polls cached operation progress once per second while a command runs.
Repository, activity and cluster enrichment refresh separately, at most once
every 15 seconds, with a 30-second backoff after a failed refresh. Hidden pages
pause both polls. A cold cache reports loading; it does not certify an empty
session inventory. `GET /api/index-status?progress=1` reads only these snapshots.
Ordinary index-status requests ask the single background task to refresh when
due, with a six-second pass deadline. Session operation GETs also stay cache-only.
Index-status responses include `loading: true` for a cold cache without a
recorded refresh failure, so browsers can distinguish loading from a warning.

Session details include `pendingLifecycle` from a fresh validated journal check.
An empty string confirms no pending journal; inspection errors fail the read.
An old pending archive page uses this field and archived tracking to reload once
to the current archived page, including before Retry. An idle cached operation
alone cannot clear pending state or authorize another archive request. Older
servers that omit the field and failed reads retain the last display. This is
display reconciliation; it does not complete or replace any operation receipt.

Archive, revive, delete and Keep open use target identity version 2. The target
contains the canonical workspace, slug, tracking location (`work` or `archive`),
tracking directory device/inode and retained root conversation, including an
explicit absent root. Saving session files, changing settings or atomically writing
`portal.yml` leaves the target stable. Replacing the directory or root, or moving
tracking between lifecycle locations, changes it.

The page refreshes its current target through operation, session details and
archival settings snapshots. Opening a confirmation captures that snapshot;
background refresh cannot change an already displayed confirmation silently.
The server checks the confirmed target under the transition gate and again
before launching the command. A changed target returns `target_changed` and the
current session summary. The page shows fresh confirmation without replaying
the action. Keep open restores the confirmed checkbox value and requires another
explicit choice. Fresh deletion confirmation resets the force checkbox.

Lifecycle receipts keep schema 1. New receipts add `targetIdentityVersion: 2`
and an internal attempt counter. Retries retain the same receipt ID; receipt ID
and attempt together reject a late subprocess result. Optional target location
and journal proof metadata bind reconciliation to the existing receipt. The
receipt owns the operation; current-target snapshots refresh the page. Older
receipts without the new fields remain readable; an older portal that does not
understand the additive fields can refuse them. Package selection remains
forward-only.

A matching accepted journal owns retry through its operation ID and immutable
journal evidence. Progress writes and documented deletion force/inventory updates
do not change that evidence. Tracking may move while the original receipt target
stays unchanged; the ordinary command still owns complete journal and tracking
validation. An expected journal that is missing cannot be adopted or superseded.
Completion reconciliation requires the receipt's exact directory/root binding
as well as terminal lifecycle. A replacement with the same slug and terminal
state cannot complete an older receipt. Deletion retains its exact operation-ID
and completion-marker proof.
An allocated journal ID alone does not establish journal acceptance. A failed
pre-journal retry with `target_changed` opens fresh confirmation inline; an
expected missing journal continues to refuse recovery.

A predecessor receipt without a version and without a journal converts only
when recomputing its original hash gives an exact positive match. A mismatch is
unverifiable, including after a harmless ctime change. Fresh confirmation can
replace only the exact failed pre-journal receipt ID; under lock, a changed
receipt, an accepted journal or a missing expected journal refuses replacement.
It cannot change the target of an operation that already started.

An already accepted manifestless predecessor revive journal can create a root
inside its owning command. A
successful command result completes the original receipt. If the portal restarts
after the command removes its journal but before that result is saved, the new
root prevents same-target completion proof. The receipt remains failed with a
missing expected journal; terminal active state alone cannot repair it, and
fresh confirmation cannot replace it. Inspect the failed receipt and keep its
ownership records and original root binding intact.
Reviewed offline repair can establish the active session's metadata, but cannot
certify the lost command result, rebind this receipt or bypass its ordinary
pending-operation checks.

The temporary predecessor lifecycle conversion is owned by portal lifecycle
maintainers. Remove it only after an inventory finds no dependent unversioned
receipts and no supported package/import path that can introduce them again.
Creation's separate source-proof hash remains supported and is not migrated by
this lifecycle conversion. An already open predecessor JavaScript bundle is not
hot upgraded; it may need a normal reload after a stale-target refusal. Pages
loaded with the new bundle perform the refresh and confirmation flow in place.

## Operations overview

The workspace menu links to **Operations** at `/operations`. This read-only
page shows machine reservations, queued and completed jobs, blocking sessions and
agent notification status. Active jobs appear first. Session-list cards name a
single reserved machine or show the distinct machine count, including during idle
review and recovery. The badge tooltip lists the machines and their states. The
session's [Reservations tab](coordination.md) shows its
held machines, pending requests and related operations.
Operations and badges refresh every five seconds while visible; failed reads keep
the previous display with a warning. The coordination contract owns reservation
and recovery semantics; the page provides no release or deployment controls.

## Automatic archival overview

The ⋮ menu at the top of the workspace and session sidebars contains
**Automatic archival**. Its link opens `/automatic-archival`, which
shows policy state, cached scan time, counts and sorted
session rows. Each row gives its rule, earliest eligibility, Keep open state,
known activity and useful blocker messages, with a link to the session. Invalid
legacy tracking and pending automatic operations remain visible. The overview
refreshes every 30 seconds while visible and offers no mass archive or migration
action. The workspace index only links to this page; its operation-status polling
does not fetch the archival overview.

`GET /api/auto-archive` returns the schema-1 workspace status envelope through
the normal authorization and package-generation gates. It invokes the read-only
`dev-session auto-archive status --json` owner. Reads do not observe live turns,
fetch refs, start grace or run a scan. A failed refresh retains prior rows;
missing observations or observations older than two hours are shown as unavailable
or stale.
Per-session status and Keep open continue to use their existing routes.

The internal `workspace-portal session observe` adapter returns schema-1 exact
root/team or verified threadless activity. The [session guide](dev-sessions.md#automatic-archival)
owns its inactivity semantics. Archive's validated executor may carry its exact
operation ID and complete/abandoned mode together, so only that execution's
accepted receipt/journal/cleanup intent is excluded from conflicts. Receipt
inspection stays read-only with the portal owner; full cleanup and tracking
proof stays with the lifecycle executor. Ordinary scans have no such exclusion.
After an accepted move, the owning retry samples the actual archived tracking
identity but still discovers conversations at the original work CWD.

## Ordinary retained readiness

Schema-1 tracking distinguishes an absent `creation` key from an empty,
malformed or unfinished block. A retained root without creation metadata needs
explicit repository and artifact lists, its exact root ID, selected socket and
client version. The creation and browser receipt owners check for conflicting
evidence.

The ordinary observer proves exact root and team identities, public submission state
and complete saved-plus-loaded same-directory discovery. Browser mutation admission
repeats that proof under existing generation and session gates, with the current
runtime authority. Presentation alone cannot authorize a send. Busy conversations
keep the existing operation-specific controls; unknown identity or submission
proof refuses a write and cannot count as archive-idle.

New manifestless start/adoption/revival and partial worktree registration refuse
before mutation. Reviewed offline repair must establish complete ordinary metadata
before those records are exposed to writers or automatic archival. Accepted
lifecycle and creation journals keep their owning recovery, without a general
pending-operation exemption. Cached status never reconstructs historical scope
or reads a maintenance file.

An accepted start retry carries only its exact existing tmux identity; a same-root
revive retry carries its loaded operation ID at `runtime_starting`. The observer
matches those existing owners before and after native reads. Only the matching
start/revive state is allowed; creation receipts, another browser operation and
other lifecycle or cleanup state still refuse. Interaction receives no such
operation exemption.

A positively proved observation subject may include `archiveState:
active|fresh|archived`. This proof-only field remains excluded from the semantic
activity token. Settings, routine manifest writes and archival bookkeeping do
not become conversation activity.

## Extension catalog

Downstream flakes call `dev-workspace.lib.mkPackage` with one `extensions`
attribute set containing three fields:

```nix
extensions = {
  commands = { };
  skills = { };
  clusterProviders = { };
};
```

The fields are:

- `commands`, mapping command names to absolute executables;
- `skills`, mapping skill names to package directories;
- `clusterProviders`, mapping provider IDs to a label and helper executable.

All values that name files or directories must be immutable Nix store
references. The resulting package writes `share/dev-workspace/extensions.json`.
Activation uses only this catalog when linking commands and skills. Workspace
configuration is always read from the registered root's `.dev-workspace.json`.
Cluster helpers receive
the selected workspace through `DEVCLUSTER_WORKSPACE` and keep ownership of
their `status`, `reset`, `cleanup-paths` and `transition-adopt` protocols.

Provider commands whose first argument is `capture-lease` use a full-duplex
relay. Readiness and diagnostics reach the caller immediately, on separate
stdout and stderr channels. The dispatcher retains the package-generation
guard until the provider has exited and been reaped. Other commands keep their
buffered dispatch behavior.

The provider receives new pipes with inherited authority descriptors closed.
Caller EOF, broken channels and interruption close provider input and perform
bounded cleanup of that spawned child. Provider exit cancels a blocked caller
input relay. Abrupt dispatcher loss closes the mediated channels too. Providers
must treat input EOF as lease loss, and capture peers must stop fixture work on
controller loss. A request accepted before loss may already have taken effect.
This transport does not change provider catalogs, state schemas, package
transition policy or command exit mapping.

Protocol stdout EOF ends a lease even if the provider is still running. The
dispatcher closes the caller's protocol output promptly and keeps the generation
guard while it closes provider input and reaps the child. Closing diagnostic
stderr alone does not end the lease.

The package constructor also accepts `userNamespace` and `routerSocket` for a
deployment-specific compatibility generation. `userNamespace` selects the
default user config, state, runtime and profile paths, including portal
lifecycle receipts and deletion recovery, without changing the generic command
or environment-variable names. Keep the defaults for new deployments; use
overrides only while an existing installation is moving its state to the
generic namespace. `activationEnvironmentAliases` can name a
validated legacy activation marker when the installed generation must invoke
that compatibility package. The selected values are recorded in
`share/dev-workspace/package.json` so a deployment-specific migration can
verify both sides of a cutover before moving state.

## Direct Codex thread teams

The Team tab and `dev-session team` control real, independent Codex threads.
The normal session conversation is always `lead` and remains the one available
through `dev-session attach`; member threads do not get tmux panes. Adding a
member creates its persistent Codex thread immediately, before it receives its
first assignment. Members use compact addresses such as `architect0`,
`implementer0`, and `reviewer0`. A configured `analyst` role becomes
`analyst0`, then `analyst1` when another is added. Addresses are never reused
or renumbered.

The roster is a private, workspace-and-session-scoped record. It contains the
root identity, member addresses, thread IDs, lifecycle state, desired next-turn
model and reasoning settings, and frozen role instructions. Message history
stays in each Codex thread. A team message resolves both addresses through the
same roster, and the member transcript API accepts a roster address rather than
a raw thread ID.
There is no global message bus or cross-session member discovery.
The portal's Team assignment API sends as `lead` only. A member can report to
`lead` through its bound host tool, which checks that member's roster and
thread identity; browser input cannot claim to be a member.

The installed workspace policy supplies starting teams such as Solo (`solo`),
Full team (`delegated`, two implementers), Lead-designed team (`lead_designed`),
and Lead and reviewer (`lead_reviewed`). Their role
lists and exact lead model and reasoning effort appear on the new-session
screen. Solo has only the lead, which investigates, designs and edits
application code without automatic specialists. Full team has `lead`,
`architect0`, `implementer0`, `implementer1`, and `reviewer0`, with architect-owned design and
delegated application implementation. Lead-designed has `lead`, `implementer0`
and `reviewer0`; the lead writes the substantive design brief and delegates
application implementation. Catalog prompts own these mode-specific policies.
Lead and reviewer has `lead` and `reviewer0`; the lead owns investigation,
design and application implementation, while the retained reviewer performs
independent final review.
The Full-team family uses `delegated` and `delegated_3` through `delegated_6`.
Their names show the actual implementer count, for example **Full team (6
implementers)**. They contain five through nine members including the lead.
Names are a presentation of the installed catalog; retained creation snapshots
and existing rosters keep their saved names, settings and instructions.

The selected team and its member settings are saved with the creation receipt.
Member threads are created before the lead receives the initial request. A
failed creation can be retried with that saved selection.

Final review starts after the substantive deliverable is complete, all intended
changes are committed and quick checks pass, before long integration tests.
Completed substantive documentation and configuration changes are included.
Routine planning, investigation, findings, session tracking and evidence alone never trigger
automatic review. Earlier review requires an explicit user request, is advisory,
and does not replace final review. A preset reviewer receives no automatic
assignment. See the [session guide](dev-sessions.md#team-members) for orchestration.

After creation, the Team tab can add, remove, and configure members, and
show their activity and statistics. After **Add member** succeeds, the form keeps
the selected role, model and reasoning effort for the next addition. It prevents
another submission while an addition is pending. Each request uses the settings
shown when you select **Add member**. Adding, replacing or reconfiguring members requires
explicit user direction; agents do not automatically grow a Solo roster or add
a designer to a Lead-designed team. The mandatory-review workflow uses a
temporary independent final reviewer when no eligible retained reviewer exists,
including Solo; the separate long-check watcher is also outside the roster.
Neither changes the persistent team. Model and reasoning settings apply to a
member's next turn. Its add selector lists every role in the installed catalog
and starts with that role's model and effort from the retained preset, the
installed development default, or the installed default team. An explicit
selection stays in place during status refresh. Both add settings may be omitted in the HTTP
API or CLI to use those role defaults; supplying only one is invalid. Without
an installed catalog, add requires both settings explicitly. Existing rosters
keep their saved settings.

Removed members stay in a collapsed history section. The Codex tab can
switch between `lead` and ready members to show each complete conversation;
direct messages use the member's current role policy and model settings. Work
can also be assigned from the CLI. An assignment to a busy member uses App
Server's ordinary turn-steer behavior. Members send results and blocking
questions to `lead` through the package-owned `report_to_lead` tool. Each
message has a unique ID that the member reuses only if delivery needs a retry.
The member's work remains in its own transcript.

When the focused conversation is idle or waiting, the Codex panel shows ongoing
work by other ready members. A single peer appears as `architect0 is working`,
`architect0 is sleeping (auto)`, `architect0 is waiting for team (auto)` or
`architect0: in progress`. For several peers it lists up to three addresses with
short states and a `+N` remaining count. Members awaiting approval or an answer
are not included. Peer activity replaces the idle label without an attention
indicator. During waits, the focused member's explanation comes first, followed
by the peer summary on the same line. Approval and answer requests remain first.

The row uses short labels such as `Working`, `Idle`, `Sleeping (auto)` and
`Waiting for team (auto)`. Full descriptions remain in hover titles and
accessible labels on the status and Codex tab. Long status text uses an ellipsis.
Message and tool counters stay on the right beside the elapsed timer.
Tabular digits and a fixed timer slot
keep ticking time from shifting the counters, including duration-unit changes.
Unusually long timing text uses an ellipsis with the full value in its title.
The peer summary reports activity, not a dependency or a promise that another
member will wake the focused conversation.

Team activity refreshes every five seconds while the page is visible, including
when the Team tab is closed. The table and status row share one read. Observations
older than 15 seconds or invalidated by a failed read show `Team status
unavailable` instead of asserting ongoing work. Returning to the page requests
a fresh observation. This read does not activate held or stopped threads.

The new-session form labels presets by size and role counts, such as
`Full team (5): 1 lead, 1 architect, 2 implementers, 1 reviewer`. It also shows
a copyable `dev-session start` command that tracks the selected name, team,
model, and reasoning effort. From an interactive terminal, that command asks
for the initial request; portal uploads are not transferred to the CLI.

Removing a member is permanent for that roster address: archive/revive and
fork retain it as removed rather than making it active again. Session lifecycle
operations and team mutations share the runtime lock, so an active lifecycle
transition rejects an add, removal, configuration change, or assignment.
Removal clears that member thread's submission attempts only after the exact
thread is proved archived or deleted and ordinary submission attempts are
resolved. Archiving a session applies the same rule to each member before
recording its archived roster state. If cleanup or the roster write fails, a
retry proves retirement again and completes the cleanup. The root
conversation's operation marker is retained for lifecycle recovery. A retry
of an already removed or archived member also checks its retired identity and
unresolved attempts before clearing records left by an older package. Forced
session deletion may discard unresolved attempts after interrupting members.

Members can message another ready member in the same session through the CLI:

```sh
dev-session team assign example --as-is --from architect0 --to implementer0 \
  --message 'Please check this design assumption.' --message-id LOWERCASE_UUID
```

Use the sender's actual address and a new stable message ID for each message.
Retry an uncertain delivery with the same ID and exact text. A busy recipient
receives steering through the existing submission path. `report_to_lead` remains
the tool for reports to the lead. Full execution access lets members run this CLI
with the same session locks and socket access as the lead.

The CLI uses the same record:

```sh
dev-session team list example
dev-session team preset example delegated
dev-session team add example implementer
dev-session team add example architect --model gpt-6-sol --effort xhigh
dev-session team configure example architect0 --model gpt-6-sol --effort high
dev-session team update-access example architect0 --access workspace_write
dev-session team assign example --to implementer0 --message 'Implement the approved plan.'
```

Forking materializes a matching member thread for every source member. Archive
and delete archive member threads with the root; revive unarchives them. The
retired virtual-team ledger is ignored for existing idle sessions, which retain
their root conversation and begin with an empty direct roster.

## Installed team policy

An optional `teamConfig` in `lib.mkPackage` builds the installed team
catalog. Schema 6 requires `execution_access = "full_access"`. This package-wide
policy gives every member unrestricted filesystem, network and Nix daemon access,
with approvals set to `never`, including members retained from earlier catalogs.
Role instructions still define which work a member may perform. The Team tab
shows effective full access separately from the saved role access. Schemas 4 and
5 retain their role-based sandboxes.

The package applies effective execution policy when starting, resuming or forking
members and before assignments, browser sends and queue starts. Existing turns
finish with their original context. Saved rosters, prompts, models and efforts
remain unchanged. An uncertain submission keeps its original policy and message
ID until recovery resolves it; changing policy does not authorize resending it.

Native role and verification utility files from schema-6 packages set
`sandbox_mode = "danger-full-access"` and `approval_policy = "never"`.
Native queue additions can execute immediately on an already loaded member.
Both browser enqueueing and recovery assignments apply the installed execution
policy before adding that work. Cold queues remain unloaded until explicit
activation supplies the policy.
Callers still select the catalog's model and effort explicitly. Older packages
cannot load schema-6 catalogs; use the normal forward-only package transition.

The catalog supplies named presets and exact model, reasoning,
behavior, purpose, access, and instruction defaults; it is not a virtual-team
dispatcher. Site configuration may add up to eight roles per team, including
the lead. Member role names use lowercase letters and digits and are at most
32 characters long. Each member role has a purpose of `design`,
`implementation`, `review`, or `general`. The lead uses purpose `lead`.
The generic package requires instructions for every role. A site can provide
defaults for known roles and supply instructions for its own roles. A
development team needs a design owner and an independent reviewer. It can use
an implementation-purpose member, or a lead that owns design and satisfies
the implementation policy when no implementer is present. An existing
implementer must satisfy its policy; the lead cannot substitute for an invalid
implementer. Role names can differ from the built-in names. Catalog schema 5 supports an optional team-level
`initial_member_counts` object, for example `{ implementer = 2; }` in Nix.
Omitted non-lead roles have one initial member. Counts must be positive integers
for existing non-lead roles; the expanded team has at most 63 specialists and
count-bearing teams must fit `max_open_agents`. Members receive consecutive
addresses within their role, such as `implementer0` and `implementer1`. Schema-4
catalogs remain readable and keep their original interpretation and digests.
The new runtime accepts preceding creation snapshots. Older packages reject
repeated-role creation snapshots; recovery requires a compatible newer package.
Existing roster and receipt field layouts remain unchanged. Fresh selections
must fit the existing 64 KiB creating-journal contract, including JSON escaping,
the journal envelope and newline. Acceptance reserves the maximum filesystem
component length for the destination slug and checks lead overrides too. The CLI
writer rejects an oversized complete record before publishing it. Retained
snapshots keep their original validation.

The unmanaged compatibility preset `full` retains one implementer so interrupted
reservations from older packages can retry their exact lineup. Installed catalogs
own the configurable Full-team composition.

The package records
its catalog digest, and the host launch marker records the launch catalog through
the registration-plan digest. Catalog identity alone does not require a restart.
No native child-agent capacity or per-role startup
arguments are required for direct threads.

At creation, the selected preset is copied into the private schema-3 receipt
and durable roster. The expanded snapshot includes `leadInstructions` and each
member's address, model, reasoning effort, behavior, purpose, access, and exact
instructions. Retrying creation uses the snapshot, even if a
later package has different defaults. Forks copy the source prompts and bind
them to the destination session; members added later use the then-installed
catalog. Older receipts without `leadInstructions`, and rosters without
instruction fields, continue with their original role instructions. Newly
created expanded receipts require the newer package and are not readable by an
older package; this is a forward-only
package transition. Host package-switch checks accept both legacy and expanded
snapshots, including an architect's saved read-only or workspace-write access;
they do not replace saved policy with current catalog defaults.
An older Lead-designed snapshot can therefore retain an architect even when the
current catalog omits that role. Shared workspace rules and installed skills
change globally, so they can conflict with saved prompts; no prompt refresh or
existing-roster rewrite accompanies catalog changes. Team mutations remain
unavailable until the creation receipt is ready. If a member's App Server
start has an uncertain outcome, retry reconciles its registered App Server
project and refuses to start another thread when the result is ambiguous. Each
member's project is created with a durable idempotency key derived from the
workspace, session, lead thread, and member address. The roster stores the
project ID returned by App Server before attempting `thread/start`. A lost
`project/create` response can be retried with the same key; a lost
`thread/start` response is resolved by listing threads in that project. If
the project lookup or thread listing is unavailable, creation remains pending.
Fresh headless threads have no rollout until history is written, and App Server
can unload them when the creating client disconnects. Before marking a member
ready, the runtime injects one internal developer bootstrap item and verifies
its exact marker in that thread's rollout; this starts no model turn. Retained
role instructions are attached as developer instructions when the thread starts
and on later turns. They do not cause a separate model turn before the first
assignment. An uncertain injection is reconciled against that marker, never
blindly repeated.
Fork destinations receive the same bootstrap before becoming ready.

The runtime combines each member's retained role instructions with an exact
workspace, session, and member-address binding. It checks the role purpose and
configured access before starting a turn. The Team tab shows each member's
saved role access. In schemas 4 and 5, the built-in architect and reviewer roles
use a read-only sandbox;
the built-in implementer uses workspace-write. A site catalog can grant its
architect role workspace-write access to edit assigned design artifacts.
Changing that catalog does not alter existing rosters. An operator can
explicitly run `team update-access` for one ready member, choosing `read_only`
or `workspace_write`. The command requires the member thread to be idle and
validates the requested access against the member's retained purpose; an
implementer cannot become read-only, and a reviewer cannot become writable.
It preserves the member's thread, instructions, model, and effort. The member's
catalog digest is cleared to mark a manual access override, and the new
role sandbox takes effect on its next turn in schemas 4 and 5. Schema 6 keeps
full execution access regardless of that saved setting. Team model and reasoning
choices remain in the form during background conversation and roster refreshes
until they are saved or the page is reloaded.
Ordinary portal reads leave the thread's persisted instructions alone.
Assignments reapply the retained policy and use a stable
message ID for retries. In the CLI, keep the ID shown after an uncertain
assignment failure and pass it with `--message-id` when retrying.
Before reserving an assignment, the runtime compacts the original turn options
of accepted `team:` submissions. Their request digests and accepted receipts
remain available, so a retry with the same ID and exact text and options keeps
its identity. Unresolved submissions and other contexts are untouched.
Retry the same ID within the same installed package generation. A package
switch can change the retained assignment text or tool path, so an earlier
uncertain ID may be rejected rather than silently replayed. If that happens,
inspect the member transcript to determine whether the assignment arrived.
Send a new assignment with a new ID only after confirming it did not; if the
outcome remains uncertain, stop and investigate before resending.

The shared workspace daemon installs one package-owned `team_reporting` MCP
server at startup. Materialized warm threads do not accept a newly inserted MCP
server through `thread/resume`, so member turns no longer depend on late
per-thread insertion. A changed startup contract requires ordinary idle daemon
reconciliation even when the native executable is unchanged. Reporting policy 1
allows only the package-dependent command path to differ after validating both
complete invocations; the running helper dispatches through the selected profile.
Unrelated MCP
servers remain configured. New members, resumed histories and forks receive the
startup server without changing their saved model, effort, access or instructions.

Only `message` and stable `message_id` are tool arguments. The helper routes by
native `_meta.threadId`, which the pinned native server supplies for model calls
and replaces on direct tool calls. It resolves exactly one current ready,
non-retiring member in this registered workspace's validated rosters. Unknown,
ambiguous, removed, archived or malformed identities refuse delivery. Root
conversations can see the shared tool but cannot report as a member. No session
or sender can be selected through tool input.

Delivery executes on the host through the configured stable user profile's
`bin/dev-session`, with report text on stdin. The helper carries the expected
member thread and lead identity. The session lock verifies the current lead;
the team operation lock revalidates the exact current caller before delivery.
The member sandbox never runs the host locking command. Successful calls return
the bounded native submission receipt. Use a fresh message ID for each report,
and reuse that exact ID, text and options after uncertain delivery. The retained
submission ledger deduplicates retries. Tool errors never echo report text or
child stderr. Completed turns are not automatically forwarded.

Fully bound legacy `team-mcp` process flags and queued per-thread MCP policies
remain supported for recovery. They dispatch through the stable profile and
revalidate their original identities too. No roster migration or native upgrade
is required. See [reporting upgrade and recovery](team-reporting-upgrade.md) for
the first daemon-start cutover, busy deferral and managed candidate recovery.

A fork requires all source members to have finished creation, preserves the
source member snapshot, and refuses an interrupted retry if that roster
changes. Because App Server forks have no project identity,
an uncertain member-fork response is not retried automatically. Archive,
revive, and delete follow the root session lifecycle. Session quiescence checks
the lead and every ready member before archive or a package transition;
deletion moves the retired roster into private recovery storage so the slug
can be reused. Existing idle
virtual-team sessions keep their lead
conversation; the old virtual ledger is not read to construct a direct roster.
Operators can add direct members after those sessions restart.

The catalog defines a verification watcher separately from the team. The
site selects its model and effort for each fresh, operation-scoped subagent;
it is not a persistent team member.

## Team activity statistics

The Team tab shows activity separately from roster state for the lead and every
retained member. Sent messages count assistant replies and plans; received
messages count conversation prompts, including team reports. Tool calls count
distinct native tool items. Lifecycle notifications and tool results do not
increase these counters. Totals span each thread's own history and exclude
inherited fork turns. Utility subagents are not persistent roster members.

Working, waiting and idle durations are separate session lifetime totals. Sleep
and collaboration waits say that execution resumes automatically. Blocking
questions and approvals show the action needed from the user. Idle means there
is no current turn. Unobserved turn time stays unclassified and partial history
is labeled. Counts are rebuilt from native history after a portal restart;
observed timing survives in private activity checkpoints. Stopped and removed
members show the last saved snapshot, without extending its time. Missing
snapshots show statistics unavailable.

The observer watches ready members without a browser connection and never
answers requests or starts turns. Reads share a four-request limit and per-thread
cache. Team polling updates statistics cells without replacing settings forms.
The read-only `GET /api/sessions/<slug>/team-stats` resolves every live member
through the normal conversation authorization. Statistics and pending model
choices use additive private records; roster and recovery schemas stay unchanged.
An older package ignores these records. A forward recovery package retains them;
an older package cannot apply pending cold model choices.

## Session creation progress

The [session preparation contract](session-preparations.md) describes
request-ID admission, naming recovery, receipt handoff, upload
ownership and recovery for New session API callers.

New session needs an initial request or attachments and a starting team. Options
contains a custom short name and advanced lead settings. An empty custom name
generates a dated name; the CLI preview asks for an explicit name. Installed
team defaults appear as exact selected model and reasoning values, including
when model discovery fails. Changing the starting team selects that team's
lead defaults. A manual model selection uses the model's advertised default
reasoning effort; both settings can then be changed. Reloading preserves explicit
unsent choices. Older unsent drafts with empty settings select the team's concrete
defaults, while submitted requests retain their original body for recovery.

This lead-owned composition uses the existing catalog and receipt schemas. Its
catalog must be paired with a runtime that accepts the composition; older
validators still require a separate implementer. Existing saved rosters remain
unchanged, and package updates follow the existing forward-only transition policy.

Automatic naming uses a private Codex child owned by the portal service, with a
fixed naming-only catalog and `gpt-6-luna`/`low`. Ordinary conversations keep their
existing socket and dynamic model catalog. Generated names use two to six words,
preferably two or three. If naming fails,
creation pauses with **Retry naming** and a custom-name field. The accepted
request, files and settings remain saved. The supported naming profile,
shutdown ownership and native diagnostic retention are described in the
[preparation contract](session-preparations.md#naming-and-reservation).

The browser saves each New session request and its file selection in its own
tab. After
an uncertain submission, inputs stay locked and Recover saved request checks
the same operation before resending identical input. Keep that tab open if
browser storage fails. The preparation page shows the accepted prompt before
scripts load, tracks naming through initialization, and redirects only after
the exact recorded session is ready. Fork and plan forms retain their existing
explicit-name flow.

The legacy creation page polls its receipt once per second and shows the
current conversation, recovery, member, prompt, terminal or verification stage.
The elapsed clock measures the whole accepted attempt. A completed stage can
show its own duration; nested durations overlap and must not be added. Team
members still initialize sequentially before the root receives its initial
request. A recovery scan can take longer than a fresh creation.

The portal enables the private stderr transport only for creation with
`DEV_WORKSPACE_CREATION_PROGRESS=1`. The Ruby helper consumes this flag and
explicitly enables it on relevant nested thread/team commands; it does not save
it in tmux, Codex settings, journals or manifests. Older helpers that emit no
progress continue with the broad initialization phase. JSON stdout and ordinary
diagnostic stderr keep their existing contracts.

A frame consists of the record separator byte `0x1e`, the exact ASCII prefix
`DEV_WORKSPACE_CREATION_PROGRESS/1 `, one UTF-8 JSON object, and LF. A complete
frame is at most 4096 bytes. It contains only `stage`, `event`, `elapsedMs` and
optional `member`. Stages are `prepare`, `conversation`, `recovery_loaded`,
`recovery_index`, `recovery_scan`, `team_member`, `prompt`, `terminal` and
`evidence`; events are `begin` and `finish`. `elapsedMs` is an integer from zero
through 86400000, zero at begin and a process-local stage duration at finish.
An optional member is a validated roster address of at most 128 ASCII bytes.
Frames contain no request text, instructions, paths or tool output.

Both nested Ruby capture and the creation-specific Go runner drain stdout and
stderr concurrently. Parsers support split and multiple frames, reject duplicate
or unknown fields and invalid values, and drain oversized records through their
newline before recognizing later frames. Candidate buffers and rejected-frame
diagnostics are bounded. Invalid or truncated frames do not change phase or
replace the child exit status; valid frames already forwarded by Ruby are not
repeated in a failure diagnostic.

Each event is bound by the subprocess callback to its workspace, slug, receipt
ID and attempt. Under the portal's operation lock, it changes only the current
running receipt's phase and update time. Duplicate phases are coalesced and an
attempt permits at most 256 phase writes. Terminal states and newer attempts
ignore delayed events. Progress cannot mark a receipt ready, change captured
settings or replace goal-attempt evidence. Ready still requires the normal
stdout result, matching durable creation proof and upload binding.

## Host module

`nixosModules.host` configures the privileged nginx and TLS substrate. Its
defaults use:

- `/var/lib/dev-workspaces/password/password`;
- `/var/lib/dev-workspaces/auth/htpasswd`;
- `/var/lib/dev-workspaces/pki`;
- `/var/lib/dev-workspaces/tls`;
- `/var/lib/dev-workspaces/public/ca.pem`;
- `/run/lock/dev-workspace-substrate.lock`.

The local operator is trusted to administer the development host. The
root/user split assigns nginx and activation files to their system lifecycle;
it does not contain a compromised operator. Remote requests still require
input validation, authentication and authorization. This trust assumption does
not extend to projects developed in the workspace or their guests.

Persistent outputs stay below `/var/lib`, runtime paths below `/run`, and the
lock file directly below `/run/lock`. Configured output directories must be
distinct and mutually non-nested. Reconciliation refuses malformed or symlinked
configured files and directories. The `tls/current` generation link is intentional.
It serializes writers with `flock`, establishes expected owners and modes, and
publishes generated files and complete TLS pairs atomically. It does not inspect
physical ancestor relationships or attempt to defend against hostile local
mounts and hardlinks.

The shared password remains `root:workspace-portal-owner` with mode `0640`,
and the CA key remains `root:root` with mode `0600`. These defaults keep their
existing lifecycle and reduce accidental changes. The nginx group can read the
generated htpasswd and leaf key. Reconciliation preserves the CA and password,
checks certificate/key pairs and requested names, and verifies leaves using
only the local CA. A weekly system service renews certificates and reloads
nginx. Retained PKI state, TLS generations and migration markers are not removed.

`services.dev-workspaces.auth.bcryptCost` sets the work factor for the generated
nginx Basic Auth hash. It accepts integers from 4 through 17 and defaults to 12.
The reconciler accepts only a hash with the declared two-digit encoded cost and
the current username and password. It creates a new salted hash when the cost
changes, while keeping the password, CA and TLS files. Repeating reconciliation
at the same cost preserves the htpasswd bytes and inode.

A lower cost is suitable only when the password is an independently generated,
secret 64-hex value from `openssl rand -hex 32`. The random credential supplies
the guessing resistance, but lowering the cost still makes each guess cheaper.
Do not apply this exception to human-chosen or reused passwords. Deploy the
module revision and its cost setting together because an older module cannot
evaluate the new option. A compatible older system generation regenerates its
declared cost after rollback, changing the hash bytes without rotating the
password. Let any in-flight reconciler from the previous generation finish
before switching, then rerun the selected generation and check its encoded cost.
If generation fails, the existing complete htpasswd remains in place; repair
the declaration or roll back and retry reconciliation.

The firewall stays closed unless `services.dev-workspaces.firewall.sourceRanges`
contains an allowed network.

## Deployment order

Deploy the host module before registering a workspace. Then build and select the
user-profile package, register the workspace, and verify the router and portal
units. A package transition must contain compatible cluster and runtime-authority
contracts whenever persistent state exists.

Namespace migrations are deployment-specific. Stop writers, preserve state
bytes and metadata, rewrite machine-consumed paths as one journaled operation,
then start the new generation. Rollback requires the reverse migration; old and
new namespaces must not run together.

## Development

The optional `TestFirefoxTabs` uses ordinary Firefox and Geckodriver against a
disposable HTTPS fixture. Supply exact `FIREFOX_BINARY` and `GECKODRIVER_BINARY`
store paths, Node and a headful display; Xvfb is suitable for an isolated run.
Set `PORTAL_FIREFOX_TABS_TEST=1`, then run:

```sh
nix develop --no-update-lock-file -c go -C portal test -mod=readonly \
  ./internal/web -run '^TestFirefoxTabs$' -count=1 -v
```

The runner creates 50 native tabs and records actual document visibility through
foreground switches. It requires a native background discard and restoration,
35 seconds of foreground updates, zero hidden read/stream traffic after
cancellation, timely pending invalidation and recovery without reload. A lost
submission response retains its recoverable browser record; retry uses its original identity and
produces one fixture acceptance record written with fsync. The fixture uses a
stand-in for the native Codex/model backend, so this does not establish native
Codex or production performance. Firefox chrome-context access is confined to its disposable profile and native
discard operation. Driver/browser/display compatibility must be verified when
this optional test runs; syntax and compilation alone do not prove acceptance.
The runner closes only its owned browser/driver and removes its temporary profile.

Run the quick checks with:

```sh
nix flake check --no-build --show-trace
```

Run the complete suite with:

```sh
nix flake check --print-build-logs
```

The NixOS smoke test in `nix/tests/host-module-idempotency.nix` covers
activation, a stable rerun, nginx authentication and proxy access, renewal,
interrupted publication, malformed state, concurrency and configuration
rollback. CI runs it with KVM on `master` and manual dispatch. Dispatch feature
VM tests after mandatory review; package and focused checks run on every push
and pull request. Keep normal dependency-update CI below the 20-minute target
and inspect cold-run timings when changing its workload.

### Cluster provider status and shutdown contract

`runtime-contract.json` publishes the provider contract. A successful `status`
command exits zero and prints its validated JSON state. Exit code
`clusterProvider.statusBusyExitCode` (75) is reserved for a concurrent transition:
providers return it promptly without a status document. The portal keeps the
last valid state and displays a local update notice. Other failures indicate
unavailable status. Readiness and credentials are replaced by the next complete
status response, including an empty response after reset.

The portal requests controls by setting `DEVCLUSTER_PORTAL_CONTROLS=1` in the
helper environment. A found schema-2 response may then include `actions`, an
array containing a unique subset of `start`, `stop` and `release`. Omission
retains legacy Release-only behavior; an empty array disables every action.
Start requires stopped state, Stop requires running or stale state, and pending
maintenance permits only Stop. Providers must omit `actions` for ordinary
readers so older portals can still validate their status documents.

`POST /api/sessions/<slug>/start-cluster`, `stop-cluster` and `release-cluster`
accept `{ "kind": "provider-id" }` and return `{ "ok": true }` after the
operation finishes. The portal selects the registered helper, checks fresh
status, and invokes `start <slug> --existing`, `stop <slug>` or `reset <slug>`.
The control environment also requests nonblocking provider locking: a busy
helper exits 75 rather than queuing a mutation. The provider must validate
retained configuration and selection under that lock for `--existing` and
refuse absent state. Archive's unconditional provider reset barrier keeps its
ordinary blocking lock behavior.

These HTTP operations retain generation, runtime and lifecycle admission checks
and reject overlapping requests for the same session/provider. Start has a
30-minute deadline; Stop uses the release shutdown budget below. Browser
disconnection leaves admitted work running; portal shutdown cancels its helper
process group. A timeout can leave a detached VM runner active, so refreshed
provider status remains the authority and Stop can clean up an owned runner.
This interface adds no persisted state or session journal format.

Schema-2 responses may include optional `webuiSource` and `maintenance`
metadata. The portal validates these objects without adding them to the rendered
cluster card. `webuiSource` contains a 40-character lowercase hexadecimal
`revision`, a boolean `dirty`, and a `kind` of `pinned` or `worktree`.
`maintenance` contains `version` (1 or 2), `mode` (`maintenance`), `phase`,
and boolean `pending`, `copied`, and `active` fields. Supported phases are
`held`, `maintenance_ready`, `copying`, `copied`, `starting_copied`, and
`released`. Only `released` has `pending: false` and `active: true`.
Pending maintenance requires `ready: false` and an empty `services` array.
Metadata requires a found schema-2 cluster. Unknown fields, incomplete metadata,
and trailing output are rejected.

Helpers that omit these metadata objects remain compatible. Portal generations
without metadata support reject responses containing either object. Deploy a
reader that accepts them together with helpers that emit them; removing one
object does not make the other compatible. This wire contract does not change
persisted cluster state or the package transition policy.

The portal gives a release/reset operation
`clusterProvider.releaseTimeoutSeconds` (180 seconds). Providers must budget
guest shutdown, forced reaping, runner cleanup, and command completion within
that ceiling. Providers own their internal shutdown budgets and test them against
the runtime contract. These fields are additive package metadata; they do not
change persisted cluster state or its transition policy.

## Sensitive sessions

Choose **Sensitive session** above **Options** when creating a session. Settings
shows **Sensitive: Yes** or **Sensitive: No**. The badge remains visible in the
session list and session page. Forks and new sessions implementing a
sensitive plan inherit the setting. Records remain local while portal access and
Codex processing retain their existing behavior. See the
[session guide](dev-sessions.md#sensitive-sessions) for storage and recovery.

## Citation links

Web citations in assistant answers appear as numbered links. Hover over a number
to see the source title. Numbers restart in each message, and repeated references
to the same URL share a number. Copy message includes ordinary Markdown links.

The shared codex-web client resolves references from retained web-tool metadata,
including sources outside the visible history page. The portal keeps the answer
visible while sources load and updates its links when resolution finishes. An
unavailable source appears as `[source unavailable]`. Literal examples in code
remain unchanged. Citation links pass through the same Markdown sanitizer as
other message links; user messages and artifact previews do not resolve citations.

The shared client's [citation contract](https://github.com/aither64/codex-web/blob/master/docs/reference.md#web-citations)
owns source extraction, offsets and history resolution. This presentation does
not change persisted conversations or require a Codex version upgrade.

## Progress status

The Settings tab shows automatic progress checks, their interval, the current
concern and the lead's response. Its switch pauses or resumes automatic checks
for an existing observer member. Save interval sets a per-session interval from
1 to 1440 whole minutes; the default is 15. Check now requests one check even
while idle or paused, without enabling automatic checks or waking an idle lead.
Add or remove the member through the usual team controls. Recurring concerns
also appear above the Codex conversation, so they remain visible without opening
Settings. Messages use plain text; observer output cannot add HTML.

`GET /api/sessions/<slug>/progress` returns observer availability, enabled/running
status, effective `intervalMinutes`, the last check time, the concern and
response, its warning state and any observation failure. `POST` accepts
`enabled` and/or `intervalMinutes` to change scheduling. A separate POST with
`{ "checkNow": true }` requests one check using the same ownership and deadline
rules. It refuses an already pending check or another active observer turn.
Writes require the normal exact origin, current package generation and live
session mutation admission. It does
not create a member. Status reads do not start model calls or enroll a session.
See [independent progress checks](dev-sessions.md#independent-progress-checks)
for scheduling, advisory authority and restart behavior.
