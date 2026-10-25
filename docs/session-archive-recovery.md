# Archive cleanup and recovery

Normal `dev-session archive` and the portal's Archive button use explicit
archival. They write `lifecycle: archived`, which records closure without claiming
that work was integrated or discarded. Automatic archival keeps its completion
policy and merge proofs. `--require-merged` selects that strict policy for a
manual command; `--abandoned` retains the discard policy.

Sensitive sessions follow these same ownership and recovery rules. Their tracking
transition is verified locally without a Git history prerequisite or commit;
sensitivity survives both moves. See [sensitive sessions](dev-sessions.md#sensitive-sessions).

## Explicit archival and local recovery

### Readiness before shutdown

Before stopping agents, explicit archival checks the session's tracking,
repository ownership, worktree layout, native conversation identities, removal
permissions, helpers and coordination state. Inspect the same checks without
changing the session:

```sh
dev-session archive-check SESSION --as-is --json
```

The JSON result identifies blockers by code and path, with a total blocker count
and validation progress. A valid check returns exit status 0 even when blockers
remain. It does not stop agents, pause goals, capture queues, pin Git refs or
create an archive journal. The executor repeats validation under lifecycle locks.
File operations and checks that require stopped writers can still fail later;
resume those operations through their existing archive journal.

The portal prepares explicit archival in the background. If checks fail, it asks
the retained lead to repair session-owned local blockers and checks again after
the repair turn completes. The lead keeps its saved model and reasoning effort.
Preparation continues while validation advances or the blocker count falls. It
stops after three repair turns without progress, when the lead needs operator
input, or when its identity or availability cannot be verified. The portal shows
the blockers, repair count and recent check history.

Use **Cancel preparation** to revoke automatic continuation before archival
starts. An already accepted cleanup turn may finish. The browser can be closed:
the portal owns preparation and resumes its durable record after restart. Native
delivery retries use the original message identity. Preparation holds no lifecycle
reservation or package lock while the lead works. Other sessions remain available.
Portal shutdown leaves an ordinary retryable receipt for older packages; a capable
portal can resume preparation only while its original receipt still matches.

Preparation cannot authorize shared infrastructure changes, Git history rewrites
or edits to lifecycle evidence. Existing archive journals resume directly without
lead repair. Automatic and `--require-merged` archival keep their existing policy.
Keep generated hook dependencies in ignored checkout-local paths such as `.gems`;
loose files beside the checkouts block cleanup.

### Shutdown and retained content

Explicit archival stops the managed terminal client, pauses active goals,
interrupts retained agents and their proven loaded native descendants, and saves
queued prompts with their complete structured input in private `archived-prompts/`
storage. Saved prompts stay held through revival; they are not automatically
queued again. Conversations remain in Codex history. Native parent links establish
utility ownership; sharing a working directory alone does not authorize retirement.
Parents stop before their descendants. Discovery repeats after interruption to
catch children spawned during shutdown, then retires children before parents.
Threadless sessions still require proof that no conversations or retained team
contradict their recorded ownership.

A threadless session can retain its verified shell until tracking is saved.
The capture journal exists before the full conversation absence check, so fresh
CLI attempts and browser attempts use the same accepted operation proof. A failed
check leaves the journal retryable and worktrees intact. Archive can finish its
journal after closing the terminal from which it was invoked; closed-terminal
status output does not interrupt completion.

Git-registered checkouts beneath the exact session group belong to canonical bare
repositories or the shared workspace repository. Explicit archival pins their
committed heads under `refs/dev-session-archives/` and removes the checkouts,
including staged, unstaged, untracked and ignored files. Commit changes before
archiving if they must survive. Branches remain available. A small schema-3
inventory records repository, checkout, branch and committed head for revival;
there are no file or index snapshots. Revival recreates a clean checkout at that
head, attaching an unchanged branch or using a detached head if the branch moved.

Tracking retains Markdown files and portal.yml manifests. Other files and their
artifact registrations are removed before a fresh operation seals its tracking
checksum. Existing sealed operations keep their original contents and policy.

### Predecessor file-snapshot archives

The following file/index capture behavior applies only to inventories written
by predecessor packages and their interrupted operations.

Git-registered checkouts beneath the exact session group belong to canonical bare
repositories or the shared workspace repository. This includes nested containers
and detached or locked checkouts. Explicit archival does not fetch, compare published heads,
require merged work or require clean checkouts. It pins checkout heads and index
objects under `refs/dev-session-archives/`, then saves working files and index and
operation state in private `archived-worktrees/` storage. A clean checkout with
no untracked or ignored entries uses retained Git objects instead of a file copy;
its saved permissions also preserve empty directories. Other checkouts use a full
file snapshot. Git reconstruction requires raw file bytes to match retained blobs;
file types must also match the Git tree. Filtered or converted content and regular
files representing Git symlinks keep a full snapshot. Staged, unstaged,
untracked, ignored and binary contents survive. Captures are synced and checked
against the source before forced Git removal. Git recovery checks every retained
tree and file object, including its raw object hash, before removal. Branches are retained.

Wrong repository ownership, symlinked checkout paths, conflicting operation
identities and changed recovery contents still refuse destructive cleanup.
Stop external editors and builds before archival: session locks cannot stop an
unmanaged writer. A failed filesystem copy or removal leaves the operation
retryable with its recovery contents retained.

### Journals and revival

Installed workspaces write schema-4 archive journals and schema-3 cleanup
sidecars. Their `tracking_saved` phase proves a durable local tracking transition;
Git synchronization runs separately. Schema-2/3 archive journals keep their
commit prerequisite. Recovery inventories accept predecessor schemas 1 and 2,
whose records distinguish Git reconstruction from full copies, and committed-only
schema 3. Finalized manifests remain schema 3. The manifest's `archive_operation_id` selects private
recovery without exposing local storage paths. A registration with no available
local head stays recorded without an invented final SHA. Existing schema-2
complete/abandoned journals keep their saved policy on retry.

Installed workspaces write schema-5 revival journals with the same local tracking
milestone. Older schema-3/4 journals retain synchronous bookkeeping. Before starting the retained
conversation, it recreates each saved checkout. Schema-3 inventories restore
committed content only; predecessor inventories restore their saved files and Git
index. An unchanged retained branch is attached again; an advanced or missing
branch produces a detached checkout at the saved head without resetting a shared
ref. Restoration has private intent and completion receipts and can resume after
interruption. New manifests and journals require this capable executor; finish
pending operations before switching packages. Older archives keep their previous
revival behavior and do not gain worktree snapshots.
Once a restoration completion receipt exists, retry cannot replace the checkout
again. It removes a leftover intent when the contents still match, and refuses
if the operator has since changed them.
An incomplete Git restore reconstructs raw file bytes on retry before applying
saved permissions and index state. Checkout filters and line-ending settings do
not change those saved bytes.

## Completion and abandoned cleanup

Strict completion discovers the same canonical checkout inventory and requires
clean worktrees. Every registration proves its exact local and origin feature head
against its own `default_branch`, the integration target. One repository may have
registrations targeting both master and a release branch. Auxiliary checkouts
select a recorded target containing their exact head, preferring origin/HEAD when
it is among those targets. An attached target checkout uses that target only.
When no target is registered, origin/HEAD supplies the fallback.

Additional feature branches need publication and ancestry proof without an
invented initial-base exception. Target checkouts may lag their fetched target.
Abandoned archival skips publication and merge proof but requires an existing
shared branch, tag or origin tracking ref to retain detached commits. These two
policies use non-force removal and create no recovery snapshots.

Record checksums cover paths, entry types, file contents and symlink targets.
Permission changes alone do not change the recorded content. Archival and revival
retain the files through ordinary moves and Git tracking. They accept checksums
from existing mode-sensitive journals as exact legacy proofs without rewriting
their evidence. Those journals still require their recorded modes; resolve an
existing mismatch before retrying. Newly created operations use content-only
checksums and require an executor with this support. Finish pending operations
before changing the selected workspace package.

The registered coordination workspace's shared `master` branch can advance
through tracking commits, including the archive's own commit. Its sealed final
head stays unchanged and must remain an ancestor of local `master`. Complete
archival also requires it merged into current origin `master`; abandoned archival
keeps its publication exemption. This exception requires the exact workspace Git
common directory; other repositories, feature branches and auxiliary checkouts
keep their existing head checks.

Before cleanup, the operation records checkout, Git administration, common
repository and container identities, exact heads and selected retention refs in
a sealed inventory. It checks that inventory, cleanliness, tracking projection
and retained-head proof before removal and each remaining destructive phase.
Completion and abandoned cleanup use non-force `git worktree remove` and `rmdir` for verified empty
containers. Stop external writers first; helper locks do not serialize arbitrary
Git commands, editors or native clients.

Archive, delete and revive retain a shared package transition lock, then acquire
the creation lock and exclusive session lock in that order. Confirmed requests
wait for session access and recheck their target and operation ownership after
acquisition. Revival keeps this policy when starting its retained runtime and
reacquiring locks. Different sessions can proceed while package switches and
recovery retain the exclusive transition lock. For operations with a deadline,
lock waits count toward that deadline. Archive has no total wall-clock deadline.

The portal shows “Waiting for session access” while an archive waits. Its
supervised worker and accepted receipt remain the same across portal restarts;
waiting does not create a lifecycle journal or authorize another attempt.
Conversation mutations retain a shared session lock until completion. Portal
lifecycle execution does not hold a conversation mutex while waiting for the
exclusive session lock, so an existing mutation can finish and release access.

Successful local archive completion also releases the session's machine
reservations. Interrupted archives retain their reservations until the same
operation finishes. Running or uncertain coordinated deployments must settle
before archival starts. Queued jobs are canceled and remain canceled after
revival; deployment history and recovery artifacts are retained.

Automatic scans skip busy sessions and preserve their cached inactivity evidence.
They recheck eligibility on a later scan. A busy lock is not a failed activity or
merge proof. Other session writers retain their existing contention policy.
Managed browser archives run in independent `workspace-archive-RECEIPT-ATTEMPT`
user services. The unit identity must match the retained portal authorization.
Portal restart reconciles the unit instead of starting another worker. Private
progress records are presentation only; the lifecycle journal remains authority.
An inactive unit with a queued start job is still running from the portal's
perspective. An inactive unit without that job is terminal, including a worker
cancelled before its executable started.
Portal receipt schema 2 stores finish time separately from journal progress and
continues to read schema 1.

The portal's archive CLI dispatch passes the resolved session and operation ID
to the worker authorization check. A worker must belong to the exact service
named by the saved receipt, including its attempt number. This authorization is
specific to archive; delete and revive continue to require the portal service.
An authorization refusal occurs before archive journal creation or cleanup.
Retrying an unchanged package cannot fix a dispatch error. Preserve its portal
receipt, deploy a corrected package, and use the ordinary Retry action.
A completed receipt may retain evidence that its journal previously existed.
That historical evidence does not block a new confirmation once fresh inspection
finds no pending journal. Failed or paused receipts still require recovery.

Local tracking receipts live under private `tracking-sync/` state. An archive
receipt binds its operation and exact tracking digest before runtime retirement.
Local-first archival accepts valid tracking files that have never been committed,
including partially committed tracking. This applies to explicit archival and
every automatic tier. Automatic archival still requires its normal inactivity,
hold, ownership, cleanliness, publication and merge proofs. The local receipt makes the transition durable while Git bookkeeping is
pending. Older synchronous journals retain their saved
commit requirements.
The per-workspace retry timer commits only owned paths under the existing shared
tracking lock. Each attempt has a 60-second deadline covering lock waits, fetches
and hooks; fetches also retain their 30-second limit. Timed-out commands receive
a five-second termination grace. Owned-index cleanup has a separate ten-second
command timeout. Hooks are not bypassed. Failed attempts retain their error and
backoff and release lifecycle locks. Revival coalesces the receipt under the
session lock; deletion removes it after its own transition finishes. Sensitive
sessions do not enqueue Git tracking.

Committed-only inventories require `archiveRecoveryPolicy: 3`; this reader
also supports predecessor inventories. New capture and receipt formats require
the matching `archiveRecoveryPolicy`,
`trackingSyncPolicy` and `archiveExecutionPolicy` runtime capabilities. Package
selection and activation refuse a target that cannot read retained state. Keep
recovery and receipts when recovering forward; do not remove them to select an
older package.

Tracking commits cooperate through `dev-session-tracking.lock` in the shared
Git common directory. Fetches occur before that short lock; each waiter then
rechecks master, cached origin ancestry, current HEAD and owned staged paths.
Normal hooks, owned-path failure cleanup and post-commit proof stay inside it,
preserving unrelated index and working-tree changes. This does not serialize
arbitrary operator Git commands.

## Retrying an interrupted operation

Run the same installed `dev-session archive SLUG --as-is` command again. Include
`--abandoned` only when it matches the recorded mode. Keep both the ordinary
journal and its private `SLUG.archive-cleanup.json` sidecar under
`worktrees/.locks/`. Their immutable operation identity, root, tracking digest
and timestamp must agree. Never remove receipts to bypass a refusal.

The command writes the sidecar before the ordinary journal. An interruption
between those writes leaves a prepared intent. The matching archive command
repeats the normal confirmation and preflight, verifies the exact source tracking and sealed
inventory, then publishes the original journal. Other mutations and package
transitions remain blocked until that retry completes.

If Git removal succeeded before its progress write, retry accepts the checkout
only after proving both the path and Git registration absent. A recreated path,
even at the same HEAD, is refused. Retention proof continues after removal: a
deleted or rewound ref that no longer reaches the sealed commit blocks recovery.

Git can reuse a removed worktree's internal administration name for a new
checkout. Recovery accepts that reuse only when the directory has a different
identity, its reciprocal Git pointers and registration identify a checkout
outside the archived session, and its common repository is unchanged. It reads
the replacement's identity without changing its files, Git state or runtime.
An unverified or dangling administration directory still blocks recovery.

After the final journal phase, the command marks the sidecar complete before
removing the journal. If interruption leaves only that completed receipt, retry
proves the committed archive and retained conversation/team/runtime retirement
before clearing it. It does not start another archive or repeat the tracking
commit. A prepared intent with moved or changed source tracking is refused.

## Legacy archive recovery without a cleanup sidecar

The temporary compatibility adapter accepts valid schema-2 journals written
before sidecar support. Before the tracking move, it retains the old immediate,
attached checkout layout and known manifest obligations. It does not adopt new
nested, detached or unregistered checkouts into that operation. Later phases
retain the existing exact tracking-tree, head and conversation checks.

New operations always create a sidecar, including an empty cleanup inventory.
Finish pending cleanup through the selected capable executor before switching
packages. An older executor that does not understand the sidecar is unsupported;
the narrow candidate recovery that delegates to a selected predecessor refuses
such an operation.

Generic lifecycle maintainers own the old no-sidecar adapter. Remove it only after
an explicit inventory proves there are no unfinished dependent journals or
intents and no supported predecessor operation, retained package or upgrade
recovery path can create another. A successful rollout or an elapsed date does
not satisfy that gate. Schema-2 journals themselves remain a supported format.

## Retained conversations and selected-executor recovery

Strict completion and abandoned archival verify the exact retained root and every nonremoved member
before publishing its operation, at cleanup, and at conversation retirement.
Active same-directory discovery must match the retained set, even when the root
is already archived. Discovery includes native subagents, exec/App Server and
unknown-source threads as well as interactive conversations. The shared discovery
owner combines saved lists with complete public loaded-ID enumeration and exact
metadata-only reads. The positive retained active-set query uses the App Server
state database index to avoid a saved-rollout scan; loaded discovery remains
independent. Threadless absence uses the separate negative proof below.
The final retirement lookup uses the index only after exact root proof.
A newly loaded thread need not have a saved row, rollout or turn.
Exact retained fresh identities can be loaded-only; unknown same-CWD
identities block, and positively verified unrelated CWDs do not. Failed metadata
reads, disappearance or contradictory saved/loaded identity make the sample
unknown. The native loaded manager omits internal sessions; its absence is never
an archived-idle proof for another source. Pending creation,
replacement or removal, unmaterialized members, active turns, requests, runnable
queued input or unresolved submissions block progress.

For an already archived retained `vscode` conversation, the selected 0.160.0
authority must positively prove the exact archived identity and `notLoaded`
metadata status, absence from every public loaded-thread page, empty or terminal
latest-turn history, no known requests and resolved submissions. It repeats the
turn, metadata, file and loaded-thread proofs after those reads. Any loaded
archived identity remains a blocker, even if idle; this absence proof does not
apply to internal or other sources. Active and fresh conversations keep the
ordinary queue-emptiness check.

The selected native queue API refuses cold archived conversations. Archive can
retain dormant queue rows, so archived proof establishes no currently runnable
input without reading or clearing that storage. Later explicit resume, including
collaboration resume without unarchive, can make those rows runnable. Stop
external writers first. These checks sample current state; native clients can
load a thread between checks or afterward.

After tracking is committed, archival reconciles and archives retained members,
proves every nonremoved member archived with resolved submissions, then retires
the root through the ordinary thread executor. It never recycles, adopts,
recreates or forces a conversation. Partial member or root acknowledgements
leave the normal journal retryable. Exact metadata and archived-rollout proof
reconcile work already completed before an interruption.

For a supported old no-sidecar journal paused at `tracking_committed`, use the
candidate recovery command documented in the [session guide](dev-sessions.md).
The candidate must match the supplied source and selected runtime contract.
It validates the selected executable's actual version and generated protocol
contract, including compatible 0.160.0, without selecting a different profile
or Codex. An active idle root and an archived root are both supported after the
same retained-set proof. Recovery archives and proves members before invoking
the selected ordinary lifecycle executor with only its portal helper replaced.
The schema-2 journal, exact tracking/head/root proof, profile token and normal
transition/session locks remain authoritative.

For schema-4 local-first journals with schema-3 cleanup sidecars, the same
candidate recovery command can finish an archive whose tracking has moved and
whose checkout and container removals are all recorded. It verifies the exact
journal, sealed inventory, retained heads and archived tracking under the normal
session locks. The candidate's ordinary archive executor then completes the
saved operation, including conversation and runtime retirement. Its generation
check remains bound to the selected predecessor while the host holds the
transition lock. The profile and Codex executable remain selected throughout.
This path cannot start an archive or remove a remaining checkout; resume those
operations with their ordinary capable executor.
If interruption leaves only the completed schema-3 cleanup receipt, candidate
recovery reconstructs its exact final journal identity through the cleanup
owner, revalidates the saved state and finishes the ordinary receipt cleanup.
An incomplete receipt without its journal remains a refusal.

## Deleting a failed direct creation before a package switch

A failed direct creation can block package selection when the selected
executor cannot finish its deletion. Use the candidate's public recovery entry
only after authorizing deletion of that exact session:

```sh
/path/to/candidate/bin/workspace-host recover-failed-creation-delete \
  --source /path/to/exact/source --workspace NAME --session SLUG \
  --request-id UUID
```

The supplied source must build the same candidate package that runs this
command. That candidate must differ from the selected package. During deletion,
the command holds the package transition lock and verifies the selected profile
token, native executable and protocol. Ordinary deletion still asks for
confirmation, checks resource ownership and records its lifecycle journal.
The selected profile and services stay in place.

The runtime contracts must match exactly, with one exception: a candidate
declaring `turnRetrySessionPolicy: 1` may recover a predecessor that omits this
field. Both may also declare `1`. Any other difference blocks recovery.

Recovery requires the exact failed new-session request, direct team snapshot,
retained root and roster. The creating manifest must explicitly record both
`initial_goal_attempted: false` and `initial_goal_sent: false`. An interrupted
terminal-start journal belongs to this creation only when its slug and tmux
identity match the creating CLI journal and retained runtime identity.
Other start, fork or lifecycle conflicts remain blockers.

A failed direct creation can leave its exact managed terminal before runtime
authority is published. If authority is absent and that terminal is present,
the ordinary deletion owner revalidates the failed-creation context, terminal
ID, workspace, sockets, client version and original creation nonce under its
existing locks. It publishes the ordinary authority with state `creating`,
then applies the unchanged trust and deletion checks. It does not replace an
existing authority, create one without a terminal, or mark creation ready.
After interruption, the same authority remains available to ordinary recovery.

The ordinary deletion owner repeats these checks under its existing locks.
After preflight and root quiescence, it durably records `thread_retiring` and
atomically moves the unchanged start journal to private recovery as `start.json`.
It then performs normal member and root retirement. For a member whose start
outcome is unknown, the preserved roster retains its project and attempted-start
provenance. This does not prove that the native thread is absent or remove its
project. Private recovery retains the start, creation, team and tracking records.

Root retirement first proves the recorded thread's archive state. If it is
not archived and the index has no candidate, retirement also checks loaded
threads and their metadata. The original root must be the only thread matching
the session directory and source policy. Missing, ambiguous or changing
identities still block removal. When both discovery paths omit the root,
retirement requires positive proof of the original persisted, non-ephemeral
`vscode` root with an empty preview and exact `notLoaded` status. Its canonical
active rollout must be a regular materialized file; metadata and file identity
must remain stable through history, terminal-turn idle and resolved-submission
checks. Discovery absence alone never permits archival, and force does not
bypass this identity proof. Attempt records are cleared only after successful
archival or positive proof that the root is already archived.

After interruption, repeat the command with the same source, session and
request ID. It validates the original removal operation and private recovery
when tracking has moved. Missing or conflicting provenance blocks recovery.
Completed removal evidence makes an exact replay a no-op while the failed
receipt remains available. A missing target alone never proves completion.

After the command returns and releases the transition lock, reconcile the
original request through the selected portal's ordinary
`GET /api/session-creations/UUID`. This status request can retire the failed
creation receipt after proving completed deletion. Do not invoke creation retry
or start another session. If reconciliation cannot prove completed deletion,
stop and retain the receipt and recovery records. After it retires the receipt,
use the normal forward package switch. Its unfinished-operation and capability
checks still apply. A ready replacement is unaffected.

## Threadless conversation absence

Threadless readiness combines fail-closed public indexed queries for unassigned
threads and every native project, fresh saved-rollout scope, complete loaded-ID
metadata and public directory-submission checks before and after discovery.
Project enumeration is repeated to detect membership drift. Explicit project
selectors propagate an unavailable native database instead of accepting an empty
index response. Every query includes active or archived state and all supported
source kinds and providers.

Saved rollouts may be imported without an index entry. The reader checks their
first `session_meta` record within 1 MiB, with strict identity and duplicate-key
validation, then rechecks file identities, prefixes and directory entries.
It does not read turn bodies; append-only body updates do not change the scope.
A same-directory import blocks readiness. A positively identified different
CWD is harmless. Compressed-only files or unusable headers need exact public
metadata bound to that saved path; unavailable or contradictory scope refuses.
The native plain sibling takes precedence when both representations exist.

This is fresh sampled proof, not a writer exclusion or a saved inventory cache.
Restore or replace a Codex home only through the existing stopped-writer and
selected-home procedure, then obtain new proof. The reader never imports,
resumes, repairs or adopts a conversation. Normal session/package locks and
external writer exclusions retain their existing responsibilities.

## Old automatic observation conversion

Generic lifecycle maintainers own the temporary conversion of schema-1 automatic
observations without a semantic fingerprint version and with root-ID-only
identity. Conversion retains a hold only after positively matching the same
retained root, then starts one fresh semantic baseline. It does not reuse the
old `updatedAt` grace or infer conversation absence from a missing manifest.
Read-only status never performs this conversion.

Remove the adapter only after inventorying active and archived tracking, private
observations and holds, unfinished lifecycle/creation/browser receipts,
retained supported packages and restore/revival paths. Record dependent formats,
counts and replacement paths. Removal requires zero dependent inputs and zero
supported ways to reintroduce them; successful deployment or absence from
`work/` alone is insufficient. Semantic fingerprint version 1 and its typed
continuity/proof distinction remain the normal supported contract.

## Ordinary retained tracking

Ordinary schema-1 tracking may retain the exact root without creation or goal
metadata. It needs explicit repository and artifact lists, complete Codex
provenance, receipt checks and current proof of the root, team, runtime authority
and submissions. Incomplete creation evidence keeps its original recovery path.
Strict completion and abandoned archival require an idle conversation and repeat these proofs. Revival preserves
the exact root, absent creation metadata and unknown historical bases.

Repair unresolved older tracking offline before exposing it to ordinary writers
or automatic archival. New manifestless start/adoption/revival and partial
worktree registration refuse before journal, tracking or runtime mutation.
Accepted predecessor journals retain their exact projections and recovery limits.
A repair cannot certify a browser receipt whose expected journal and owning
success result have both disappeared.

## Compatibility removal inventory

Generic lifecycle maintainers own these temporary readers and predecessor recovery
paths. Inventory active **and archived** tracking, unfinished lifecycle/creation/
revive/browser receipts, private observations/holds, retained supported
package generations, and upgrade/restore/revival paths. Record each input format,
its dependent count and supported replacement path. Removal requires zero
current dependent inputs and zero supported reintroduction paths.

| Temporary input/reader | Dependencies that must be absent before removal |
| --- | --- |
| Old no-sidecar archive recovery | No unfinished predecessor journal/intent and no retained supported executor or upgrade recovery able to create it |
| Unversioned browser targets | No unfinished ctime-derived pre-journal receipt and no supported old portal/import path that can restore it |
| Old automatic observations | No supported unversioned/root-ID-only observation or hold dependency and no old worker/restore path able to write it |

Workspace maintainers own their offline repair tools. Keep such a tool while
unresolved active or archived records, or a supported restoration path, need it.

Do not use a deployment date, successful rollout, or zero active raw
legacy records as the removal gate. Schema-2 archive journals and honest
unknown-base schema-1 manifests remain ordinary permanent formats.
