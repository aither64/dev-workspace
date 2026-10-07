# Codex runtime package

`lib.mkCodexPackage { pkgs; codex; }` assembles a complete runtime package from
an already selected upstream Codex output. It copies the native executables
without rebuilding Rust, changing their ELF interpreter or RPATH, or stripping
store references. The supported host platform is `x86_64-linux`.

`lib.mkPackage` uses this helper once for the workspace's bundled Codex.
System configurations can pass their selected Codex output to the same helper.
Each consumer supplies its own `pkgs`, so equal Codex versions can have different
dependency closures.

## Upstream packaging and removal

The helper is a downstream workaround for missing complete-package metadata
and resources in the selected llm-agents output. It leaves Codex Rust code
unchanged. The complete-package layout is required by the upstream native daemon. See
[llm-agents issue #9887](https://github.com/numtide/llm-agents.nix/issues/9887),
[the proposed complete-layout packaging in PR #9889](https://github.com/numtide/llm-agents.nix/pull/9889)
and [Codex issue #48050](https://github.com/openai/codex/issues/48050).

Remove the helper only when the selected llm-agents Codex output itself provides
a complete, daemon-copyable package with materialized helpers. Both workspace
and system consumers must use that output directly and pass the existing
normal-startup, resume/fork, manifest/helper/copy and closure-retention checks.
Issue closure or a newer CLI version alone is insufficient. Migrate both
consumers before removing the helper. Preserve retained roots and their
dependencies, and keep the regression coverage.

## Runtime layout

The package has these paths:

```text
bin/codex
bin/codex-code-mode-host
bin/logs_client
share/                           upstream shell completions
libexec/codex/
  codex-package.json
  bin/codex                      native executable
  bin/codex-code-mode-host        native executable
  bin/logs_client                 native executable
  codex-path/rg
  codex-resources/bwrap
```

The public Codex launcher forwards arguments to this output's native entrypoint
and puts bubblewrap on `PATH`. The helper commands and completions keep their
existing public interfaces. Missing required executables fail assembly.

The manifest contains `layoutVersion = 1`, the input's `version`, the host Rust
`target`, `variant = "codex"`, `entrypoint = "bin/codex"`,
`resourcesDir = "codex-resources"` and `pathDir = "codex-path"`. This follows
the [upstream package layout](https://github.com/openai/codex/blob/rust-v0.160.0/scripts/codex_package/README.md).

The runtime root retains resources supplied by the input. File and directory
links are dereferenced during copying; no symlinks remain anywhere below
`libexec/codex`. Public links outside that root are allowed. Ripgrep and Linux
bubblewrap are regular files at the paths above so the native daemon can copy
the runtime root without links that escape its package.

## Daemon copies and dependency retention

The native daemon can copy the runtime root into
`$CODEX_HOME/packages/app-server-daemon/`. Those copied files still depend on
the Nix store: their ELF interpreter, libraries and embedded store references
remain intact. Nix scans the assembled output's references and retains those
dependencies as its closure. Copying files into `CODEX_HOME` does not create a
Nix GC root.

Workspace activation retains the selected assembled Codex output through
`~/.local/state/dev-workspaces/codex/current` and
`~/.local/state/dev-workspaces/codex/generations/<generation>`. A deployment
using a different user namespace has the same paths under its selected state
root. System generations independently retain the system Codex package and its
closure. Retain both sets of roots when using system and workspace commands.

Keep retained workspace Codex roots and system generations while a daemon copy
may depend on them. Before pruning a generation, establish that its closure is
unused or retain that exact assembled output through another GC root. Inspect
the output and its dependencies without running garbage collection:

```sh
assembled=$(readlink -f "$workspace_package/libexec/codex")
nix-store --query --references "$assembled"
nix-store --query --requisites "$assembled"
```

Here `workspace_package` is the selected workspace application output. For a
system-only consumer, inspect its assembled Codex output directly.

The package pin selects the workspace's explicit App Server and terminal pair.
Upstream's native daemon release selection and updater have their own state;
plain CLI startup can use a previously installed daemon release. Check that
daemon's version separately from the public command's `--version`. Assembly
does not make the daemon updater a permanently pinned Nix service.

## Verification and recovery

The `codex-package` flake check verifies the manifest, executable bytes and
native identity, completions, launcher targets and input/output version. It
also covers materializing file and directory links and rejecting a missing
required executable. Native daemon startup and persisted-state compatibility
need separate probes with a private disposable `CODEX_HOME`.

Workspace recovery remains forward-only: repeat an interrupted supported
`workspace-host switch` or select a corrected newer package. Preserve transition
journals and retained Codex roots. See the [portal recovery guide](workspace-portal.md#user-profile-state)
for the supported entry points. System recovery uses its retained system
generation; whether an older Codex can read newer state must be established
separately on disposable state before using that recovery path.

Workspace package switches compare the candidate's resolved native executable
and launch contract, rather than its CLI version string or package path. With
the live-switch contract, an unchanged native invocation preserves the running
App Server and terminal clients. The initial cutover from an older package and
any changed invocation require idle sessions; see
[switching while threads are active](workspace-portal.md#switching-while-codex-threads-are-active).

## Session naming release boundary

The portal's session namer uses a portal-owned, naming-only App Server with
exact `gpt-5.5`/`low`. The package installs the full unchanged original
`codex.src/codex-rs/models-manager/models.json` as
`share/workspace-portal/codex-models.json`. A startup CLI override selects the
native static models manager for this child; the ordinary App Server retains
its dynamic catalog. The helper checks the immutable file and startup
`sessionFlags` origin before inference. It does not patch Codex, edit model
entries or rewrite shared Codex home.
Global user instructions remain trusted serving-instance policy. File/network
action tools and project/workspace/team/skill injection must be excluded;
questions fail immediately. The supported Direct profile exposes no model tools.
CodeModeOnly models are unsupported because yielded cells can survive completed
turns without a supported scoped stop/join operation.

Before enabling a candidate deployment, run the codex-web protocol corpus and
fixed restriction-key/type validator against that candidate's generated App
Server schema and `core/config.schema.json`. After mandatory review, run the
tagged exact-binary mock-provider fixture described in codex-web's technical
reference. Record the exact native binary hash, assembled package/profile and
unchanged model metadata, actual advertised tool schemas, forbidden dispatch,
question rejection and ephemeral teardown evidence. Include the startup
catalog path/origin, local-only child launch and actual managed-service
start/loss/stop/forced-exit checks with ordinary-daemon continuity. The real
positive naming canary must use this same child profile and obtain a valid model
result within the ten-second budget. Missing prerequisites or
unsupported behavior block release. Deterministic fallback after a helper error
does not establish isolation.

The child reads the existing host-selected account/provider configuration and
uses the pinned version's local-only launch marker; credentials are not copied
into a new home or passed in argv. The portal owns its process group and join,
including early child exit and portal failure. Native ephemeral storage excludes
resumable conversations but its separate SQLite diagnostics can retain input
and identity in plaintext. The existing local logger remains enabled, and its
startup pruning does not guarantee an erasure deadline. See the
[preparation contract](session-preparations.md#naming-and-reservation) for runtime
ownership, naming-input bounds and native logging semantics.

MCP names are captured once per bounded naming call. Pause/stop naming through
the existing portal service or supported package transition before changing MCP
configuration. Finish that reconfiguration while no naming call is active;
fresh calls capture and disable all literal names then present. This support
condition does not include deliberate administrator reconfiguration during a
call. Use the existing service/package procedures, without a global config
lease or another process manager. Package-generation cancellation must prevent
the old worker from committing a fallback or reservation.
