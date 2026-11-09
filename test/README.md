# Ruby test layout

`dev_session_test.rb` and `workspace_host_test.rb` remain the complete-suite
entry points used by Nix and CI. They load their feature groups in a fixed
order, so existing commands continue to run the full suites.

`dev_session/` groups session lifecycle coverage by behavior: creation,
worktrees, deletion, archival, tmux, Codex runtime, and retained sessions.
`workspace_host/` groups registry, service, command, transition, and suspension
coverage. For focused session checks, use the aggregate entry point with
`--name '/test_name/'`. Several groups depend on helpers loaded by earlier
groups, such as `NullTmux`; running those files directly can fail before their
assertions run.

`support/` contains the shared test cases and setup. `fixtures/` remains the
shared corpus. Keep reusable setup in `support/`; keep scenario assertions in
the feature file that owns the behavior.

The opt-in [CPU comparison fixture](../docs/cpu-benchmark.md) measures portal
retry/status and coordinator work with disposable synthetic state, including
helper child CPU. It is separate from ordinary correctness and work-count tests.

## Portal browser contracts

`TestShippedBrowserClientMatchesSessionAPI` in `portal/internal/web` runs the
Node `browser_contract_test.cjs` entry point. That entry awaits
`preparation_browser_contract_test.cjs`, which covers New session's verified
tab storage, immutable submission/recovery, attachment scope, acceptance
identity, persistence repair bound to the saved request, preserved rejected
drafts, separate-tab ownership and safe preparation/session navigation. The
existing entry also exercises legacy plan and fork client contracts. Run it
from the repository's Nix environment:

```sh
go -C portal test -mod=readonly ./internal/web \
  -run '^TestShippedBrowserClientMatchesSessionAPI$' -count=1
node portal/internal/web/preparation_browser_contract_test.cjs
```

The Go selector runs one test and the standalone Node entry runs all preparation
contract cases. Neither launches a browser or Codex. HTTP template cases use
`^TestSessionPreparation(PageHasExplicitIdentityAndEscapedRawPrompt|IndexMakesOnlyNewSessionNameOptional)$`.

After independent review, `test/creation_browser.cjs` exercises the shipped
assets in Chromium against a local HTTP fixture. It covers optional/custom
names, unavailable model discovery, lost-response recovery, isolated tab
uploads, attachment-only input, storage failures, progress/retries, escaped
text and legacy plan/creation routes. It also covers the separate no-opener tab
with current settings and deliberate reattachment while the old request remains
recoverable and may complete independently. It requires `PLAYWRIGHT_MODULE`
to name the installed Playwright module, `CHROMIUM_EXECUTABLE` to name its exact browser
executable, and `CODEX_WEB_SOURCE` to point to the pinned codex-web source with
its conversation assets. Run `node test/creation_browser.cjs` from the runtime
repository root. This fixture performs no model inference. The separate
`team_settings_browser_test.cjs` is also a real browser suite, invoked through
the opt-in `TestQuestionBrowser` entry; it is not a pure Node quick check.

The fixture loads the actual host and provider stylesheets in their production
order. At 1280px and 375px widths it checks 50 cards with long filenames: the
creation root has no internal vertical scrollbar, every removal action and
Create session can be reached by scrolling the page, and the conversation root
retains its 12rem cap. It verifies full total size before completion
acknowledgement, removal/restoration, file 51 rejection and exact-body recovery
of a 50-file request after response loss. These assertions need the matching
provider source with the shared selection summary; syntax checks alone do not
establish layout acceptance.

## Native provider acceptance

After committed quick checks and independent review, set `LIVE_PROVIDER_PACKAGE`
to the real final built package and `LIVE_SWITCH_CODEX` to its exact raw native.
The two `live_switch_integration` selectors `TestNativeProviderRouting` and
`TestNativeScopedProviderSwitch` validate package provenance, native SHA and fixed
App Server argv before using private state and distinct synthetic endpoint/token
pairs. Run them sequentially with `-count=1`, outer bounds 180s/300s. Routing keeps
the restart/legacy-compensation matrix and an explicit strict `[]` control; its
fake dev-session helper supplies no terminal evidence. Scoped acceptance checks
A→B→A on unchanged root/member IDs, queue/goal/continuation and permissions,
unrelated active-turn identity, child inheritance and no switch-time daemon
restart. Its separate cold B restart keeps default A and uses no provider override;
actual B traffic must establish the saved route. Routing is bounded to 90s and
scoped to 180s, with bounded phase waits and joined owned process groups. Failed
private receipts are retained and their path logged. Compile/list and metadata-only
checks do not execute these native scenarios. Production managed-policy and real
terminal restoration proof belong to the session-creation VM check.

## Native monitor acceptance

After committed quick checks and independent final review, set
`LIVE_MONITOR_PACKAGE` to the exact built managed candidate package and
`LIVE_SWITCH_CODEX` to its pinned native executable. The monitor fixture requires
the real candidate catalog and utility files; a temporary catalog cannot replace
that package. The generic default has no `teamConfig`; build the owning configured
workspace's managed package instead, with its runtime input bound to the exact
reviewed source. Retain the source/input graph and package/native store paths
without writing lockfiles or selecting the candidate package. Run from the runtime
repository:

```sh
nix develop --command go -C portal test -mod=readonly \
  -tags live_switch_integration -run '^TestNativeMonitorOperation$' \
  -v -count=1 ./cmd/workspace-portal
```

The fixture uses private workspaces, state, sockets and a synthetic Responses
backend. Its model calls execute disposable shell commands through the real App
Server, then report through the candidate MCP and CLI. The ordinary portal worker
must produce actual lead input and a matching native receipt after the original
lead turn ends. It also checks steering, immutable reports, cancellation,
uncertain receipts, portal/native restart and evidence resolution. This command
does not select a workspace package or access registered sessions.
Successful per-case receipt evidence is retained in the test log.

## Bounded maintenance acceptance

The tagged `TestNativeWorkspaceBoundedPause` fixture uses `LIVE_SWITCH_CODEX`
for the exact pinned native executable. It exercises ordinary conditional pause,
turn interruption, durable queue/goal retention, an owned local TERM/restart and
stable explicit resume against a synthetic Responses backend. Its 90-second
case bound is separate from the one-second pause grace used by the fixture.

Compile only, without fixture execution:

```sh
nix develop --command env GOWORK=off go -C portal test -mod=readonly \
  -tags=live_switch_integration -run '^$' ./cmd/workspace-portal
```

Run actual native cases only after committed source, focused checks, independent
review and a separately authorized execution batch. The local stop/restart is
an injected activation boundary; it does not verify the Ruby worker, real
systemd/profile activation, actual client restoration or all activation-failure
paths. The `session-creation` NixOS test exercises those boundaries in its
disposable VM, using the public switch, status and recovery commands. It repackages
the same native executable under distinct store paths to trigger an ordinary
native restart. The fixture checks equal executable hashes and exact source and
target identities. Its maintenance cases cover cooperative completion, deadline
interruption, user Stop suppression, and failed target startup followed by
recovery of the same operation.

```sh
nix build --print-build-logs .#checks.x86_64-linux.session-creation
```

Both fixtures check queued input by its submission identity and text. Input may
remain queued or appear in saved conversation history after normal dispatch;
an empty queue alone is not evidence of preservation. The VM delays target
activation with a fixture-only router service hold for passive checks before
resume. Its startup fault affects only the disposable native service.
Independent monitor acceptance and ordinary lifecycle/recovery checks remain
separate. Syntax checks, evaluation and compilation do not establish execution
of either acceptance fixture.
