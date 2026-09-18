# Ruby test layout

`dev_session_test.rb` and `workspace_host_test.rb` remain the complete-suite
entry points used by Nix and CI. They load their feature groups in a fixed
order, so existing commands continue to run the full suites.

`dev_session/` groups session lifecycle coverage by behavior: creation,
worktrees, deletion, archival, tmux, Codex runtime, and retained sessions.
`workspace_host/` groups registry, service, command, transition, and suspension
coverage. Run an individual file directly when working on that behavior.

`support/` contains the shared test cases and setup. `fixtures/` remains the
shared corpus. Keep reusable setup in `support/`; keep scenario assertions in
the feature file that owns the behavior.
