# Codex providers

Each session can use its own provider or inherit the workspace default. Open
the limits dialog, choose **This session** or **Workspace default**, preview a
provider, then choose **Switch to this provider**. Changing the preview only
reads limits. **Use workspace default** removes a session's explicit selection.
Changing the default applies to inherited sessions; explicit selections stay put.

The switch pauses affected threads and resumes their interrupted work. Idle
threads stay idle. It preserves thread IDs, queued input, active goals, model,
reasoning effort, permissions and team settings. Other sessions keep working.
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
