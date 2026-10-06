# Archive cleanup and recovery

Archival discovers Git-registered worktrees beneath the exact session
group, including checkouts in containers such as `integration-targets/` and
detached auxiliary checkouts. The canonical bare repositories and the shared
workspace repository establish ownership. A checkout inside another checkout,
foreign or unregistered repository, symlink, locked or prunable registration,
unknown file, or dirty checkout blocks cleanup.

Every registered feature branch remains an obligation even after its checkout
has been removed. Complete archival proves exact local and origin feature heads
merged into the recorded origin default. An additional attached feature branch
needs the same proof without an invented initial-base exception. Default-branch
checkouts may lag the fetched default. A detached checkout must be reachable from
that fetched default. Abandoned archival skips publication and merge proof, but
still requires an existing shared branch, tag or origin tracking ref to retain
each detached commit. Archival creates no rescue ref and never removes branches.

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
Cleanup uses non-force `git worktree remove` and `rmdir` for verified empty
containers. Stop external writers first; helper locks do not serialize arbitrary
Git commands, editors or native clients.

Ordinary archive, delete, revive and hold commands retain a shared package
transition lock and their existing session locks. Different sessions can proceed
while package switches and recovery retain the exclusive transition lock. The
operation deadline includes waiting for generation and session locks.
Tracking commits cooperate through `dev-session-tracking.lock` in the shared
Git common directory. Fetches occur before that short lock; each waiter then
rechecks master, cached origin ancestry, current HEAD and owned staged paths.
Normal hooks, owned-path failure cleanup and post-commit proof stay inside it,
preserving unrelated index and working-tree changes. This does not serialize
arbitrary operator Git commands.

## Retrying an interrupted operation

Run the same installed `dev-session archive SLUG --as-is` command again. Include
`--abandoned` only when it matches the recorded mode. Keep both the ordinary
schema-2 journal and its private schema-1 `SLUG.archive-cleanup.json` sidecar under
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

Ordinary archival verifies the exact retained root and every nonremoved member
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
transition/session locks remain authoritative. New sidecars require a capable
ordinary executor and remain outside this temporary recovery adapter's inputs.

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
Archive requires an idle conversation and repeats these proofs. Revival preserves
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
