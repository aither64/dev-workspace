# Workspace portal

The workspace portal keeps long-running development sessions available through
both a browser and tmux. One installed package generation supplies the portal,
Codex CLI, session helper, user units and any configured extensions.

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
source viewer. The viewer shows line numbers and syntax highlighting. A link
ending in `:10`, `:10:5` or `#L10` selects line 10. Clicking a line number updates
the address; **Copy link** copies that address. Links without a line reference
open the beginning of the file.

Active repository links show the current worktree contents, including
uncommitted edits and newly staged files. The file must be tracked in a verified
session repository. After archival, the viewer reads the recorded final commit
and displays its revision. A link is a live reference, so later edits can move
the referenced line. The viewer reports a missing line instead of selecting a
different one.

Shared workspace links show current contents of Git-tracked files, including
uncommitted edits and newly staged files. They can open documentation, the root
`AGENTS.md`, scripts and notes without a session registration. The configured
workspace must be the Git repository root. Files under `work/`, `archive/`,
`worktrees/` and `repos/` are excluded from this shared reader so session files
keep their repository registration and artifact publication checks.

Tracking files are limited to `plan.md`, `state.md` and artifacts declared in
`portal.yml`. Their links continue to work when tracking moves into `archive/`.

Repeated artifact paths are allowed. The portal shows one link per normalized
relative path, using the first declaration's label and position. For example,
`report.json` and `./report.json` refer to the same artifact. Every declaration
must still have a nonempty label and a safe relative path. Browsing and validation
leave the manifest unchanged.

Untracked repository files, Git metadata, symlinks and other host files are not
available. Text previews are limited to 512 KiB and 12,000 lines. Markdown files
appear as source; binary files receive a notice instead of a text preview.

The viewer route is `/files/<slug>?repository=<id>&path=<relative-path>#L10`,
or `/files/<slug>?artifact=<relative-path>#L10` for a tracking artifact. The
repository ID is the same opaque ID used by repository review. The read-only
`GET /api/sessions/<slug>/file` endpoint accepts the same query parameters and
returns `path`, `repository` when applicable, `source`, the archived `revision`
when applicable, and a `content` object with the existing review preview fields.
Source values are `worktree`, `archive`, `artifact` and `archived-artifact`.
Shared files use `/workspace-files?path=<relative-path>#L10` and
`GET /api/workspace/file?path=<relative-path>`. This endpoint returns the same
preview fields with `source: "workspace"` and no repository or revision. It
accepts exactly one `path` value and no other query parameters. The viewer labels
these files as current workspace contents; archived conversations still open
their current shared files rather than a historical version.
Existing URLs whose website path contains the absolute workspace file path
redirect to the viewer. The original conversation text remains unchanged.

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
repositories show committed comparisons only. Untracked content is available
through an authorized review snapshot, not the source viewer.

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

Active or unknown turn state blocks saving. A failed or uncertain update keeps
the dialog open and rereads the thread. The controls never send a compensating
update automatically. Reads begun before a successful save cannot overwrite its
confirmed pair.

Browser refresh timings come from codex-web's `conversation/assets/refresh.js`.
That policy contains resource intervals, deadlines, retry backoff and notice
delays. Background refreshes keep the last values visible. Initial loading
appears after 750 ms and manual older-history loading after 250 ms. Refresh
failures appear after 30 continuous visible seconds; returning to a hidden tab
or restoring a page starts a fresh grace period. Access errors and failures of
user actions appear immediately.

Automatic history repair retries with capped backoff without loading banners.
A manual older-page failure offers Retry for that page. Expired cursors trigger
reconnection and a check of retained history. Servers without paging keep the
existing legacy behavior.

Requests, queued messages, activity timing, session details and index status
refresh separately from the transcript. A slow queue reconciliation does not
delay the first conversation page. Failed refreshes retain their last result;
request and queue warnings offer Retry after the shared grace period. Transcript receipt and upload clearing require positive
server evidence across all retained pages, including automatically loaded older
messages. While the server is still checking conversation metadata, the
mode is shown as pending and plan actions stay hidden. The tab retries metadata
reads with backoff. Activity reads have a five-second budget while active and
a thirty-second budget while idle; focus and reconnection prompt a fresh read.

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

All portal, CLI and observation clients share schema-3 submission receipts at
`$DEV_WORKSPACES_STATE/submissions/<socket-path-sha256>.json`. The first read
imports the previous socket-adjacent ledger under both interprocess locks when
the persistent ledger is absent. The old ledger remains in place. Stop old
writers before changing paths; mixed writers using different paths are not
supported. Queued Send retries reconcile their queue/history receipt and never
fall through to a second turn or steer.

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
`transport_unavailable` observations or exit status 75. The recovery coordinator
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

### Switching while Codex threads are active

Packages publishing `livePackageSwitchPolicy = 1` can switch while threads are
active when the candidate resolves to the same native Codex executable, launch
policy, complete arguments and native child-thread capacity as every running
workspace's launch marker. Catalog defaults, portal code, session tools and
package paths do not themselves require a Codex restart. The host refreshes
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

Archive, revive, delete and Keep open use target identity version 2. The target
contains the canonical workspace, slug, tracking location (`work` or `archive`),
tracking directory device/inode and retained root conversation, including an
explicit absent root. Adding artifacts, changing settings or atomically writing
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
Full team (`delegated`), Lead-designed team (`lead_designed`), and Lead and
reviewer (`lead_reviewed`). Their role
lists and exact lead model and reasoning effort appear on the new-session
screen. Solo has only the lead, which investigates, designs and edits
application code without automatic specialists. Full team has `lead`,
`architect0`, `implementer0`, and `reviewer0`, with architect-owned design and
delegated application implementation. Lead-designed has `lead`, `implementer0`
and `reviewer0`; the lead writes the substantive design brief and delegates
application implementation. Catalog prompts own these mode-specific policies.
Lead and reviewer has `lead` and `reviewer0`; the lead owns investigation,
design and application implementation, while the retained reviewer performs
independent final review.
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
inspect their messages. Adding, replacing or reconfiguring members requires
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

The new-session form labels presets by size and role counts, such as
`Full team (4): 1 lead, 1 architect, 1 implementer, 1 reviewer`. It also shows
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
catalog. The catalog supplies named presets and exact model, reasoning,
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
implementer. Role names can differ from the built-in names. The package records
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
saved access. The built-in architect and reviewer roles use a read-only sandbox;
the built-in implementer uses workspace-write. A site catalog can grant its
architect role workspace-write access to edit assigned design artifacts.
Changing that catalog does not alter existing rosters. An operator can
explicitly run `team update-access` for one ready member, choosing `read_only`
or `workspace_write`. The command requires the member thread to be idle and
validates the requested access against the member's retained purpose; an
implementer cannot become read-only, and a reviewer cannot become writable.
It preserves the member's thread, instructions, model, and effort. The member's
catalog digest is cleared to mark a manual access override, and the new
sandbox takes effect on its next turn. Team model and reasoning
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

The report tool is bound to the workspace, session, lead thread, member address,
and exact member thread. A new or forked thread receives the tool after its ID
is known. Before each assignment, App Server resumes the member thread with
that binding, including for members created by an earlier package. No roster
format change is needed. The helper checks the current roster identity before
sending. A missing or mismatched binding fails without sending to `lead`.
The helper exposes only `report_to_lead`. It passes the report through stdin to
the configured user profile's session command, which checks that the lead thread still belongs
to this session under the session lock. Neither command puts the report text in
its process arguments. Use a fresh message ID for each report, and reuse that
ID only when retrying delivery of the same report. The helper retains the stable
profile path across package switches, including custom profiles, and checks the
roster again for each call.

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

## Session creation progress

The [session preparation contract](session-preparations.md) describes
request-ID admission, automatic-name fallback, receipt handoff, upload
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
fixed original catalog and `gpt-5.5`/`low`. Ordinary conversations keep their
existing socket and dynamic model catalog. If the naming child is unavailable,
creation uses the deterministic prompt-based name. Its supported profile,
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
