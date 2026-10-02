# Codex runtime package

`lib.mkCodexPackage { pkgs; codex; }` assembles a complete runtime package from
an already selected upstream Codex output. It copies the native executables
without rebuilding Rust, changing their ELF interpreter or RPATH, or stripping
store references. The supported host platform is `x86_64-linux`.

`lib.mkPackage` uses this helper once for the workspace's bundled Codex.
System configurations can pass their selected Codex output to the same helper.
Each consumer supplies its own `pkgs`, so equal Codex versions can have different
dependency closures.

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
