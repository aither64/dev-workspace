# Codex providers

Each session can use its own provider or inherit the workspace default. Open
the limits dialog, choose **This session** or **Workspace default**, preview a
provider, then choose **Switch to this provider**. Changing the preview only
reads limits. **Use workspace default** removes a session's explicit selection.
Changing the default applies to inherited sessions; explicit selections stay put.

The switch requires affected conversations to be idle and drained. It forks
their retained histories with the selected provider and preserves one visible
conversation, model, reasoning effort, permissions and team settings. Original
manifest and roster IDs stay fixed; a private binding selects each current native
child. Queued input, active goals and unresolved submissions must settle before
the fork. Other sessions keep working.
The shared native daemon, portal, tmux and development clusters keep running.
The lead and every retained team member switch, including idle members.
Temporary subagents stop when their parent is closed. Their saved history remains,
but interrupted delegated tasks may need to be assigned again. New subagents
inherit their parent's selected provider. Switching uses the selected upstream
Codex binary without an additional provider-switching patch or rebuild.

New-session, plan and fork forms show the provider directly in the form.
**Workspace default (Codex subscription)** follows the current workspace default;
the name in parentheses updates with the actual default. Choosing an individual
provider pins the session to it, including when it equals the current default.
Fork selection belongs to the new session independently of the source provider.
You can also use:

```sh
dev-session start my-task --provider openai
dev-session fork my-task alternate-approach --provider openai
```

Omitting the option inherits the workspace default. The CLI creation journal
retains the original selection before provider binding, so an interrupted retry
cannot change it by omitting or replacing the flag. An explicit selection binds
to the accepted creation incarnation, then to its exact root thread. Creation
retries retain that selection. Browser drafts keep the provider through model
refresh and reload; an accepted request keeps its exact submitted provider. Reusing a deleted session name cannot inherit
the previous session's override. Retained members use their session's provider.

Browser creation drafts use schema 3. The current portal reads older schema-2
requests and preserves their submitted bytes. Older portals refuse schema-3
drafts before submission, including editable drafts. After a rollback, keep the
tab's storage and restore a compatible portal to continue the saved request.

Explicit forks retain provider intent in a schema-2 fork journal; inherited
forks keep schema 1. Both use the existing provider-state format. A package must
advertise `codexProviderForkCreationPolicy: 1` while provider-bearing fork
receipts remain, including ready receipts and the window before a CLI journal
exists. Package admission refuses older readers before selection: their portal
cannot start with these receipts. Finishing pending forks alone does not remove
this boundary. Retain a compatible package and recovery evidence; never remove
journals or receipts to bypass a refusal.

The sidebar shows limits for the current session's provider. The dialog can
preview either provider independently. Subscription previews use the native
account's limits. Load balancer previews separate API-key and upstream aggregate
quotas, account pool remaining percentages, and reported request, token and cost
totals. Missing metrics are unavailable, rather than unlimited capacity.
Quota metrics do not gate a switch. Model preflight checks the target catalog.

## Progress and recovery

Requests belong to `workspace-codex-provider@WORKSPACE.service` and survive
closing the browser. Status appears before native inventory and provider checks.
It identifies the affected scope, current step, thread, completed count and
elapsed time. Specific reasons distinguish session locks, unfinished lifecycle
operations, unavailable authentication/models and unsupported saved permissions.

Native checks use metadata without loading transcript pages. The worker locks
only affected session runtimes and shares the package-generation lock. A session
switch does not wait for unrelated turns, goals, queued input, stale submission
receipts, utility threads or VM processes. A workspace-default change waits for
inherited session creation to finish before freezing its scope. Package updates
and accepted lifecycle operations retain their normal ownership rules. A shared
creation-admission lock prevents an inherited creator from entering between the
default switch's root inventory and its durable applying fence.

**Cancel switch** cancels the exact request while it is waiting or preparing.
Cancellation interrupts preparation checks and restores terminal clients before
releasing the affected scope. The control disappears when native switching begins;
its journal then owns completion and recovery. A failure before any native
close leaves routing unchanged. **Dismiss** hides a terminal result while retaining
its failure evidence. Finished attempts show their final duration instead of a
running timer. Status separates the current step, retained member, count and reason;
raw thread IDs appear under technical details. A later failure shows **recovery-required** for
the affected scope. **Retry provider switch** continues the accepted target;
it does not silently switch other sessions back to an earlier provider.

```sh
workspace-host provider status WORKSPACE
workspace-host provider request WORKSPACE PROVIDER UUID --session SLUG
workspace-host provider request WORKSPACE PROVIDER UUID
workspace-host provider recover WORKSPACE UUID
workspace-host provider cancel WORKSPACE UUID
workspace-host provider dismiss WORKSPACE UUID
```

Use a fresh operation UUID for a new request and retain it when retrying the
same request. Omit `--session` to change the workspace default. To return a
session to inheritance, request the current default provider with
`--session SLUG --inherit`. Status and receipts distinguish those intentions.

Terminal clients restore automatically. A terminal restoration failure leaves
the selected provider in place and reports that `dev-session sync SLUG --as-is`
is needed. The portal separately rejoins its event and activity subscriptions.
That durable notification survives another provider request or portal restart.
Do not erase provider state or snapshots to clear a recovery refusal.

## Native transition

The following archive/unarchive sequence belongs to retained `scoped-v1`
operations. New provider changes use the [collective fork](#collective-provider-fork)
and wait for idle, drained sources.

Codex cannot change the provider of an already loaded thread. Unsubscribing has
a 30-minute inactivity grace period, so the worker uses native archive/unarchive
to close and reopen the selected histories on the same daemon. This temporary
native close does not archive the development session, tracking or worktrees.

Before closing, it validates ownership and captures effective settings. Native
closure stops execution while retaining queue entries and delivery identities.
Goal status remains native-owned: cold resume restores the current persisted
state. The worker never replays goal activation, which could restart a goal
that completed during preparation or after a lost reply. Native archive closes the selected root and its temporary subagents. Only retained
leads and members enter provider preflight, reopening and verification. The
worker does not wait for temporary agents or try to migrate their provider.
Cold resumes explicitly retain provider, model, effort, sandbox, approval
policy, runtime roots and saved team instructions. Member environments retain
their member address. Effective native responses prove routing.

Deleting a session supports its selected provider even when the workspace
default differs. Retirement checks all providers for conversations using the
same session directory and refuses ambiguous ownership. It still verifies the
recorded root and waits for idle state before native archival.

The worker restores only work interrupted by the close. Continuation messages
have durable client identities and ask Codex to inspect tool results before
retrying actions. It never answers an approval or question; an interrupted
request can be asked again after continuation. Unsupported named permission
profiles or cold filesystem settings refuse before closing retained threads.
Cold workspace-write settings support the standard compiled policy, including
writable roots, network and temporary-directory flags, and protected metadata
paths. Equivalent duplicate rules and ordering do not change that policy. Custom
read/deny/glob restrictions or missing metadata protection refuse before closure.
Persisted collaboration settings and disabled plugins retain their values.

Native model discovery uses the daemon's configured provider. Model-only private
servers supply cached catalogs for the selected session or creation provider,
without loading threads or restarting the shared daemon. Conversation pickers
and model validation use the same provider catalog, including CLI root creation,
team presets and team changes. Creation dialogs resolve the destination catalog
independently of the source session.

Native v2 children remain controlled by their parent. Their interruption does
not prevent a provider switch. They are not resumed by the switch worker and
are not included in the retained-team completion condition.

## Operator catalog

The host module writes a nonsecret schema-1 catalog at
`/etc/dev-workspaces/codex-providers.json` when
`services.dev-workspaces.codexProviders` is nonempty. Subscription ID `openai`
is built in. Each load balancer has a unique ID, label, HTTP(S) URL ending in
`/v1`, and an absolute token-file path:

```nix
services.dev-workspaces = {
  codexInitialProvider = "openai";
  codexProviders = [{
    id = "pool";
    label = "Development pool";
    baseURL = "https://pool.example.test/v1";
    tokenFile = "/home/operator/.codex/pool.token";
  }];
};
```

The token must be a private regular file readable by the workspace user.
The catalog and portal never contain its contents. Metrics use bearer
authentication against `/v1/usage`; model preflight uses `/v1/models`.
Redirects are refused. Native inference uses Responses and a token-file auth
helper. Browser requests cannot supply endpoints or executable paths.

`codexInitialProvider` applies only before saved workspace selection exists.
Removing a catalog with saved state fails closed. Match the initial provider to
existing routing before enabling the feature. Global Codex configuration,
authentication and subscription backups stay intact. No catalog and no saved
state preserve ordinary runtime behavior.

## State and package compatibility

Private provider state lives below
`$XDG_STATE_HOME/dev-workspaces/providers/WORKSPACE/`, bound to the registered
workspace root and socket. Schema 2 adds exact-root session selections and
scoped operations. Receipts bind UUID, provider, scope and inheritance intent.
Their retention is bounded to 2,048 UUIDs and state to 1 MiB. Private per-operation
snapshots journal close, reopen, verification and continuation boundaries.
Exact-operation `.control.json` files hold cooperative cancellation and dismissal
flags separately, so earlier strict schema-2 readers can still load terminal state.
An active operation continues to fence package replacement until it finishes.

Schema-1 state remains readable. An unfinished schema-1 operation keeps its
original workspace-wide engine and compensation rules, including its daemon
restart. Finish or cancel that operation before requesting a scoped switch.
Snapshots are never reinterpreted as another strategy.

Packages retain `codexProviderStateSchema: 1` and
`codexProviderTransitionPolicy: 1` for admission by an installed legacy runtime.
Packages managing schema 2 must additionally advertise
`codexProviderScopedStateSchema: 2` and
`codexProviderScopedTransitionPolicy: 1`. Once schema 2 exists, package selection
refuses candidates lacking those capabilities. Recover software with a corrected
compatible package; workspace package selection remains forward-only.

Creation preparations and receipts with provider selections or destination model
settings require the scoped capabilities too, even before provider state is
written. The host checks both before package activation so an older portal cannot
start against fields its strict reader does not support. Completed CLI creation
journals retain their validated provider selection during host inspection.

The worker emits bounded `PROVIDER_FAILURE` diagnostics to its systemd journal.
They identify operation, direction, stage, step and exception category without
credentials, native transcripts, helper output or private settings. Public
progress reports stable reason codes rather than raw exception text.

## Deployment

Deploy the matching host catalog/module and select the application through the
workspace user profile. Keep the native Codex version unchanged. Application
deployment can restart services even though provider switching does not.
Verify a disposable busy session, its queued input and continuation, an unrelated
working session, creation selection, model catalogs and interrupted recovery.
Use both provider directions. Keep live production sessions out of development
transition tests.

The disposable managed VM fixture includes
`TestManagedProviderForkPortalIntegration` under the
`naming_runtime_integration` build tag. It uses the selected package's CLI,
portal, tmux and native daemon with synthetic loopback model responses. The test
checks a root and retained member, session inheritance and workspace-default
switches, terminal continuation, and recovery of the same operation after a
known child is durably recorded but before collective commit. It does not test
an interruption after commit or installed-package activation.

Run it inside the reviewed VM with its immutable package/source manifest and
fixture environment:

```sh
"$MANAGED_NAMING_SMOKE_BINARY" -test.v -test.count=1 -test.failfast -test.timeout=300s -test.run='^TestManagedProviderForkPortalIntegration$'
```

## Binding codec

The Go `codexproviders` package and Ruby `ProviderBindings` module provide the
schema-3 codec and typed binding resolution used by ordinary provider readers.
The package advertises binding state schema 3, fork transition policy 1 and runtime
authority schema 3. Reading state alone grants no native execution permission.
Accepted schema-1 and scoped operations retain their original recovery engines;
Pi keeps its own identities and lifecycle contracts.

Schema 3 adds a positive uint64 `bindingRevision` and `bindings` keyed by session
slug. Each session binds its original root, existing creation/tmux incarnation,
last affected revision and conversations keyed by their original native IDs.
Conversation rows record retained member address, active native ID, provider and,
for a switched ID, a sealed operation ID and evidence digest. Original manifest
and roster IDs remain identity anchors. Removed-member history can remain without
granting current membership; a replacement at the same address has a different
original ID. The state envelope owns the workspace name, root and shared socket.
Binding keys use the canonical workspace root.

Schema 3 retains an empty `bindings` map when encoded. Legacy formats omit the
binding fields. In this codec, optional routing identities must be
absent or valid and nonempty. Receipt inheritance requires a session; a
creation-only selection omits `rootThreadId`. Production legacy readers retain
their existing acceptance rules.

`ResolveCurrent` requires caller-validated current eligibility, identity,
provider, socket and expected session revision. These are preconditions supplied
by session/roster readers; they do not establish authentication, authorization or
the live socket generation. `ReceiptTargets` accepts the original key, a
`bindingRequired` precondition validated by the owning manifest reader, and an
immutable evidence reader. It returns receipt targets from the active ID back to
the original ID, each once. A required but missing binding causes an error. An
unmarked session with no binding can retain its exact original route. If a
session binding exists, a missing conversation or identity mismatch causes an
error without fallback. Historical lookup does not require current readiness,
authorize execution or prove that a native receipt is absent.

Lineage uses schema-1 sealed documents in the existing private provider operation
directory. Exact-byte SHA-256 seals bind the operation, workspace/root/socket,
previous and committed revisions, native snapshot digest and complete participant
rows. Each row binds the original key, old and new IDs and providers, and the
preceding seal. Missing or corrupt evidence, identity mismatches, conflicting
targets, cycles or nondecreasing revisions cause an error. Callers must still check
original send options, attempted/unknown outcomes and native receipts at every
returned target; read errors are not absence. No receipts or histories are copied
to the active child.

`PrepareBindingCommit` checks expected global and per-session revisions, the
complete supplied frozen current participant set, one-to-one updates and explicit
selection intent. It returns one bounded candidate or an error without modifying
the input or performing I/O. The owning writer must prove roster completeness and
revalidate the same preconditions under locks. A binding change increments the
global revision once and stamps only affected sessions; unrelated bindings and
receipts remain unchanged. Selection-only changes preserve IDs, lineage and
binding revisions. Recording an explicit session selection may use the existing
schema-1-to-schema-2 conversion; a workspace no-op retains schema 1. Neither
operation creates schema-3 bindings. The codec accepts original identity
rows, but it does not create initial reservations or registrations. Those belong
to the writer. An unchanged candidate does not establish a migration. The strict
operation codec retains legacy and `scoped-v1` shapes and validates
`fork-v1` operations and their strict journals. Ordinary readers use the strict
codec for marked state and fork operations, including control overlays.

Both languages use the same corpus of valid and malformed inputs. State is
bounded to 1 MiB, sealed evidence to 16 MiB, sessions to 4,096, and receipt
retention and operation traversal to 2,048 entries. The codec rejects duplicate
keys, unknown fields, null or coerced revisions, and uint64 overflow. A package
managing marked state must support the worker, manifest requirements, execution
and receipt consumers, package compatibility and forward recovery.
Loading a package does not migrate live state.

### Collective provider fork

New effective-provider changes use the fork transaction. Go holds exclusive
affected runtime locks, then team operation locks in sorted
session order. Under those locks it rechecks the complete current roster, socket
generation, creation incarnation and execution blockers. Affected conversations
and their proved descendants must be idle and drained. Unclear ancestry or state
refuses preparation; unrelated threads on the shared server are outside the scope.
A trusted callback supplies the environment for each full session/member binding
key. The helper freezes a private copy before effects and refuses missing,
mismatched or unsupported settings. This captures the owning configuration;
it does not infer environment from native metadata.

After capture, the reserve callback records and pauses the exact owned terminal
panes while Go retains the runtime and team locks. It rechecks frozen history,
idle state and submission evidence after the pause and before native intent.
Input that arrived between capture and pause refuses the fork. Retries prove
the same paused panes before another unattempted participant; partial pauses
remain fenced. Cancellation before native intent restores only the saved source
terminals after Go releases its locks. Failed or unknown forks keep their fence.

Synchronous Ruby callbacks from the same package exclusively write provider
state and binding evidence under `request.lock`. The parent does not hold that
lock while waiting for Go, and callbacks do not reacquire runtime or team locks.
Origin reservation retains the old selections and IDs. The first-intent callback
checks cancellation, persists journal intent, then advances state to `forking`.
Cancellation checks the durable journal even if a crash left state at
`preparing`. Go dispatches only after a successful live acknowledgement and an
exact reread. A lost acknowledgement or an existing intent never authorizes
another fork. Each returned child ID is durable before dependent reads; only
verified children can enter commit. An unknown outcome stays fenced without
searching for, adopting or retrying a child. The transaction never rolls back to
an ancestor or synthesizes conversation input.

At commit, Ruby revalidates the frozen affected revisions against fresh state.
It uses the current global revision and preserves unrelated registrations.
Durable `committing` state serializes binding writers while Ruby seals the
immutable native proof and publishes all bindings and selections in one state
write. Recovery after a lost state acknowledgement reuses the original seal and
reconciles the exact state. Projections recover forward at the committed child
IDs; terminal restoration failure keeps committed routing in place. Empty
workspace scope, no-op and inheritance changes that need only a selection update
preserve the existing schema and bindings without creating a self-link.

The public dispatcher accepts an immutable UUID, target, scope and catalog
receipt, then the worker freezes the affected roots under creation ownership.
The package-internal Go owner captures the accepted source under runtime and team
locks before synchronous Ruby callbacks publish reservation, intent and commit.
Retries consume that exact snapshot; missing evidence after reservation refuses.
Same-provider selections keep the current native IDs and binding revisions.
Existing legacy and `scoped-v1` recovery retain their original engines.

### Consumers of retained bindings

A marked session keeps the original manifest root and roster member IDs. Its
current native IDs come from the strict binding state and ready schema-3 runtime
authority. The portal resolves the same visible conversation to those current
IDs under package, runtime and team ownership. Current resume preserves native
model and effort, verifies provider and roster eligibility, and rejects stale
authority or a reserved operation. Startup instruction reconciliation uses the
same current lead while retaining the original retry identity and policy.

Watch recovery requires the existing grant for the same socket generation and
exact current conversation. Refreshing a subscription does not create or transfer
a watch or cursor from an ancestor. Normal conversation resolution establishes
the subscription for a newly selected current conversation.

A browser fork captures the complete current source tuple before persisting its
receipt. Destination model lookup and uploads consume that accepted operation;
uploads fork from its captured native source. A legacy receipt without a capture
continues to use its original source ID. A fresh native fork requires equality
with the current source capture. An accepted intent has an unknown outcome and
cannot dispatch again. Known root and member children reconcile their exact
saved identities even after source archival or removal; an unattempted member
still requires fresh source proof. Completion after journal retirement requires
the existing exact completion evidence.

Package preparation checks the candidate's complete provider contract before
source admission. Candidate activation repeats compatibility under transition
exclusion. A named package-transition reader validates retained binding state
and reservations while preserving the lifecycle, Pi, recovery, cluster and claim
guards. Retained marked manifests, state and captures require integer contract
values `codexProviderBindingStateSchema: 3`,
`codexProviderForkTransitionPolicy: 1` and `codexRuntimeAuthoritySchema: 3`, in
addition to the applicable legacy, scoped and creation capabilities. Preparation
is read-only preflight; its result cannot replace activation's fresh checks.
