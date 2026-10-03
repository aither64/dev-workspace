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
