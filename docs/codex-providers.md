# Codex providers

The limits sidebar shows the provider selected for the registered workspace.
Open its info dialog and choose a provider to preview that provider's limits.
Changing the select box only reads limits. **Switch to this provider** submits
a separate request for every retained root and team thread in the workspace.
Other registered workspaces keep their own selection.

Subscription previews use the authenticated native account's limits. Reset
credits are available only when that subscription is active. Load balancer
previews separate API-key and upstream aggregate quotas, account pool remaining
percentages, and reported request, token and cost totals. Missing metrics are
unavailable; they do not mean unlimited capacity.

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
Neither the portal nor the catalog contains its contents. Metrics use bearer
authentication against `/v1/usage`; model preflight uses `/v1/models`.
Redirects are refused. Native inference uses Responses and a token-file auth
helper. Browser requests cannot supply endpoints or executable paths.

`codexInitialProvider` applies only before saved workspace selection exists.
Removing a catalog with saved state fails closed. Configure the initial provider
to match current routing before enabling the feature on existing threads.
Global Codex config, authentication and subscription backups remain untouched.
No catalog and no saved state preserve the previous runtime behavior.

## Waiting and application

A durable request belongs to `workspace-codex-provider@WORKSPACE.service`.
Closing the popup or browser does not cancel it. While **waiting**, ordinary
work continues. The popup reports active turns, unresolved submissions, pending
questions, queues, goals, unfinished lifecycle operations or unowned threads.
**Cancel waiting switch** cancels that exact request. Cancellation is refused
after **applying** starts.

Cancellation confirmation appears only in the popup where you requested a switch
or canceled a waiting switch. Closing the popup or changing its preview selection
clears that confirmation. Waiting, applying, failure and recovery status remain
visible across tabs and page reloads.

Application holds the package-transition lock, repeats ownership and idle checks,
and probes authentication and retained models without inference. It records
private settings and terminal identities, quiesces managed terminal clients,
and validates both forward and recovery settings before stopping the proven native
daemon. Direct native code-mode and retained team MCP helpers are accepted only
with exact executable, parent and member-argument proof after all threads are idle.
Other processes and helper descendants, including VM runners, block the switch.
Portal and tmux services remain running.

The selected native package cold-resumes retained IDs with explicit provider,
model, effort, sandbox, approval policy, runtime roots and saved team policy.
Effective resume responses prove routing; historical provider metadata alone
is insufficient. Isolated writers exit before shared daemon startup.
Shared startup waits for the exact native service and protocol readiness, even
when the loaded inventory was empty. Originally loaded threads and terminal
clients are restored. Member environments retain their member address. Unsupported
settings, including custom named permission profiles, refuse the transition;
implicit built-in profiles use their verified effective sandbox policy.

Cold capture reads both turn contexts and idle settings snapshots in native
chronological order. Approval policy, reviewer and profile changes after the
last turn take precedence. Nondefault service tiers remain explicit; native
`null` and `default` tier values mean the same default scheduling policy.
Compiled unrestricted and ordinary read-only snapshots are supported without
a preceding turn. Other compiled filesystem snapshots require an identical
preceding turn policy; unsupported changes refuse before loading the thread.

## State and recovery

Private schema-1 state lives below
`$XDG_STATE_HOME/dev-workspaces/providers/WORKSPACE/`, bound to the workspace
root and socket. Atomic state includes active selection, operation UUID,
target catalog digest, phase, snapshot reference and UUID receipts in one write.
Receipts prevent canceled retries becoming new requests; retention is bounded to
2,048 distinct UUIDs and the whole state to 1 MiB. The enabled worker stays available
between requests so requests accepted during completion cannot miss it. A changed catalog fails a
waiting request; select the provider again.

Before daemon stop, failure leaves original routing running. Later failure
compensates each thread through native resumes to its original provider and
settings. Recovery preserves history; it never overwrites native databases or
rollout files. Unverified recovery leaves **recovery-required** and blocks writes.

```sh
workspace-host provider status WORKSPACE
workspace-host provider recover WORKSPACE
```

Recovery starts the same worker without interrupting an existing worker.
A durable notice marks terminal restoration as pending until it succeeds. If
restoration fails or is interrupted, use `dev-session sync SLUG --as-is` after
routing has reached a terminal phase. Do not erase state or restart an
unsafe service to bypass a refusal.

Packages consuming saved state must advertise provider schema 1 and transition
policy 1. Applying and recovery exclude package changes. Waiting requests survive
compatible updates. Cancel waiting requests before suspension or unregistration.

## Deployment

Deploy the matching host module and catalog, then select the matching application
through the workspace user profile. Keep the native Codex version unchanged.
Check package-transition restart safety separately: application rollout can
restart services that provider switching leaves running. Verify both previews
and an idle switch in each direction. Returning to the subscription uses the
selector. Package updates remain forward-only; recover software by selecting
a corrected compatible package.
