# Codex runtime package

Use the [runtime update skill](../skills/update-dev-workspace-codex/SKILL.md)
for a version upgrade. It delegates protocol and catalog maintenance to the
selected codex-web source's update skill.

`lib.mkCodexPackage { pkgs; codex; }` assembles a complete runtime package from
an already selected native Codex output. It copies the native executables
without rebuilding Rust, changing their ELF interpreter or RPATH, or stripping
store references. The supported host platform is `x86_64-linux`.

`lib.mkPackage` uses this helper once for the workspace's bundled Codex.
System configurations pass this flake's
`packages.x86_64-linux.codex-native` to the same helper.
Each consumer supplies its own `pkgs`, so equal Codex versions can have different
dependency closures.

## Native source and provider compatibility

`nix/codex-native.nix` owns the selected native build. Its fixed source receipt
pins an exact commit from `aither64/codex` above a stable upstream release,
its source and Cargo hashes, and the frozen llm-agents recipe. The builder uses
the recipe's `srcOverride` and `cargoVendor` interfaces. It retains the recipe's
daemon Nix-store patch, V8 archive/bindings, recursion and compiler settings,
manifest and resources. The fork already contains the provider compatibility
change; packaging must not apply that patch again.

The derivation runs focused provider and CLI tests in the release target after
building the production binaries. Only the test App Server enables debug
assertions, which upstream's managed-config and startup test hooks require.
Checks hash the production executables before and after testing and refuse
changed artifacts. These source tests supplement the separate behavioral
acceptance checks against the exact installed production executable.

The required `checks.x86_64-linux.codex-provider-enforcement` owns the twelve
provider-enforcement integration cases. It lists the exact Rust module's tests
before execution and requires all twelve names to pass with none ignored. The
check inherits the selected recipe/source/toolchain and anchors daemon path
substitution to the production native output. It builds only the integration
harness and test server/helpers, retains those artifacts and a result receipt,
and leaves the measured production native derivation unchanged. Its exported
`bin/codex-provider-enforcement RECEIPT_DIRECTORY` repeats only that fixed suite
with disposable local state. The test server enables debug assertions for the
upstream test hooks; it is not the production executable's behavioral proof.
After vendor/source lock consistency passes, the check prepares the release
tag's 171 stale local lock versions to match their inherited workspace version.
The guarded rewrite preserves external packages and all dependency edges; the
subsequent test build remains offline and locked. The original source and vendor
lockfiles remain unchanged.

The native producer retains one filename-suffixed command that matched zero
tests. It provides no coverage and is redundant with this required check.
Retaining it preserves the measured output's embedded daemon identity; remove
that command at the next necessary native-input or release rebuild. Raw native
or package build success alone cannot satisfy the provider-enforcement gate.
The existing fast CI includes the dedicated check through its flake enumeration.

The raw `codex-native` export is the sole native authority for the workspace,
system command and provider wrappers. Each consumer may assemble helpers from
its own `pkgs`, but must not select a second fork pin or copy the Rust recipe.
Workspace `share/dev-workspace/package.json` records the native output and its
source provenance. The native CLI version remains the upstream release version;
identify a downstream binary by its exact source, derivation and executable hash.

The shared workspace App Server starts with
`app-server --allow-host-provider-switching`. This host-only, default-off flag
allows concurrent explicit providers and the existing fenced close/reopen switch
under the same native thread IDs. It relaxes only current-provider-ID equality;
provider availability, managed selection/definition policy, authentication,
network and malformed-config checks remain required. It does not change a
loaded thread's route in place or rewrite saved history.

The workspace package declares this fixed server argv in
`codex.app_server_argv`. The launcher, candidate probe and immutable launch
marker use that exact candidate's declaration. A missing declaration in valid
legacy metadata means strict startup. Different server arguments, even with the
same native binary, require the ordinary idle transition. Registration `-c`
arguments, registration digests and retained startup grants keep their existing
meaning; candidate package/profile authority binds the fixed server arguments.
The Go package reader also recognizes this optional typed section when loading
managed or unmanaged registration metadata. A present section requires a known
argv list; optional native/provenance fields retain their declared string shapes.
Malformed or unknown fields still refuse registration. These fields do not enter
the registration-only digest or change retained startup-grant authority.
An unpatched executable override fails its actual-argument startup probe before
selection. Metadata alone does not establish native support.

During preparation, the candidate's checker validates both the active predecessor
and the target. It resolves their exact executable identities and uses each
package's own server arguments. Candidate identity wins when the paths match,
including an executable override, so a stock override cannot satisfy a flagged
candidate's admission. Retained prepare callers need no new command option;
an unrelated diagnostic executable receives only the strict protocol probe.

Remote terminals retain their existing daemon connection and per-session provider
arguments. Naming and provider model-discovery children, standalone system Codex
and DeepSeek commands keep the strict default. In-process native servers do not
inherit the shared server's CLI option; verify actual child invocation and behavior
for each supported call path. Preserve the original provider-switch journals,
queues, goals, pending deliveries, IDs and forward recovery.

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
the [upstream package layout](https://github.com/openai/codex/blob/rust-v0.167.0/scripts/codex_package/README.md).

The runtime root retains resources supplied by the input. File and directory
links are dereferenced during copying; no symlinks remain anywhere below
`libexec/codex`. Public links outside that root are allowed. Ripgrep and Linux
bubblewrap are regular files at the paths above so the native daemon can copy
the runtime root without links that escape its package.

## Daemon selection and dependency retention

The selected llm-agents package patches native daemon installation to use its
original immutable Nix-store runtime in place. It disables upstream release
updates for that selection and attempts to register a native daemon GC root.
The assembled executable retains the original package's embedded store reference,
so Nix retains that package and its dependencies as part of the assembly closure.
The native daemon can therefore execute the original output rather than the
materialized assembly. Check its actual executable independently.

Retained daemons from older package generations can still use runtime copies
under `$CODEX_HOME/packages/app-server-daemon/`. These copies retain their ELF
interpreter, libraries and embedded store references. Copying files into
`CODEX_HOME` does not create a Nix GC root.

Workspace activation retains the selected assembled Codex output through
`~/.local/state/dev-workspaces/codex/current`. A deployment using a different
user namespace has the same path under its selected state root. Ordinary
application profile generations retain their bundled Codex outputs; system
generations independently retain the system Codex package and its closure.
The current root also protects an independently selected workspace Codex for
future restarts. Activation does not create a separate numeric Codex history.

On Linux, Nix discovers running store executables and their mapped/open store
dependencies through `/proc` and retains their reference closures. An idle
running process has this protection too. A copied executable outside the store
is not itself a root; its actual mapped/open store dependencies receive runtime
protection. Selected restart targets still need their current or ordinary
profile roots. Native daemon copies and their selection remain separately owned.

The [coordination collector](coordination-operations.md#payload-collection)
retires recognized legacy numeric Codex roots after pending recovery settles.
It unlinks redundant roots without deleting store contents, pruning native
copies, restarting daemons or waiting for old processes to exit. Ordinary Nix
GC determines which closures remain live. Inspect an assembled output and its
dependencies without running garbage collection:

```sh
assembled=$(readlink -f "$workspace_package/libexec/codex")
nix-store --query --references "$assembled"
nix-store --query --requisites "$assembled"
```

Here `workspace_package` is the selected workspace application output. For a
system-only consumer, inspect its assembled Codex output directly.

The package pin selects the workspace's explicit App Server and terminal pair.
Retained native daemon state or an already-running daemon can still select a
different executable. Check its version and store identity separately from the
public command's `--version`; assembly alone does not establish what is running.

## Verification and recovery

The `codex-package` flake check verifies the manifest, executable bytes and
native identity, completions, launcher targets and input/output version. It
also covers materializing file and directory links and rejecting a missing
required executable. Native daemon startup and persisted-state compatibility
need separate probes with a private disposable `CODEX_HOME`.

Workspace recovery remains forward-only: repeat an interrupted supported
`workspace-host switch` or select a corrected newer package. Preserve transition
journals, the current Codex root and required application profiles. Pending JSON
records the retry target but is not a GC root; existing selected-package and
prepared-operation protection retains that target. A valid pending transition
postpones legacy Codex root collection, and malformed pending state refuses
collection. See the [portal recovery guide](workspace-portal.md#user-profile-state)
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
exact Codex 0.167.0 and `gpt-6-luna`/`high` through codex-web's
`RunPromptedEphemeralTurn`. The package installs codex-web's derived Luna catalog
as `share/workspace-portal/codex-models.json`. The
[preparation contract](session-preparations.md#naming-and-reservation) defines
the fixed restrictions, disabled Responses Lite mode and startup
catalog checks, and links to codex-web's exact metadata contract. A startup CLI
override selects the native static models manager for this child; the ordinary
App Server retains its dynamic catalog. The helper checks the immutable file
and startup `sessionFlags` origin before inference. It does not patch Codex or
rewrite shared Codex home.
Naming instructions tell the model to treat the request as data and avoid tools,
files, commands, browsing, delegation and questions. The private call retains
its fixed restrictions and rejects interactive requests, but native tools can
remain available. The prompted contract makes no guarantee about native tool
registration, tool attempts or their effects. Global user instructions remain
trusted serving-instance policy. CodeModeOnly models are unsupported because
yielded cells can survive completed turns without a supported scoped stop/join
operation. Codex-web's strict `RunEphemeralTurn` API retains its separate
supported 0.163 contract. The supported 0.163, 0.164, 0.165 and 0.167
releases use identical immutable Luna catalog bytes, which do not establish
native version or tool isolation.

Naming startup also requires the ordinary App Server version supplied by the
host through `--codex-version` to be exactly `0.167.0`. A missing or mismatched
version leaves naming unavailable without running the bundled executable,
including its version probe. This prevents different native versions from
writing the shared Codex home during the supported
[staged package switch](workspace-portal.md#native-changing-transitions-and-unresolved-submissions).
The child's own exact-version probe remains required.

Before enabling a candidate deployment, run the codex-web protocol corpus and
fixed restriction-key/type validator against that candidate's generated App
Server schema and `core/config.schema.json`. After mandatory review, run the
tagged exact-binary mock-provider fixture described in codex-web's technical
reference. Record the exact native binary hash, assembled package/profile and
supported catalog metadata, actual advertised tool schemas, dispatch behavior,
question rejection and ephemeral teardown evidence. Include the startup
catalog path/origin, local-only child launch and actual managed-service
start/loss/stop/forced-exit checks with ordinary-daemon continuity when those
contracts change. Synthetic tests do not establish real-provider adherence to
the instructions or naming latency. A real positive naming canary is optional,
separately authorized validation using the same child, packaged instructions and
model within the thirty-second budget. Failed required checks or unsupported
runtime behavior block release. Naming errors stop preparation before reservation
and preserve the request for retry or a custom name.

The child reads the workspace-private account/provider configuration and
uses the pinned version's local-only launch marker; credentials are not copied
into a new home or passed in argv. The portal owns its process group and join,
including early child exit and portal failure. Native ephemeral storage excludes
resumable conversations but its separate SQLite diagnostics can retain input
and identity in plaintext. The existing local logger remains enabled, and its
periodic pruning does not guarantee an erasure deadline. See the
[preparation contract](session-preparations.md#naming-and-reservation) for runtime
ownership, naming-input bounds and native logging semantics.

MCP names are captured once per bounded naming call. Pause/stop naming through
the existing portal service or supported package transition before changing MCP
configuration. Finish that reconfiguration while no naming call is active;
fresh calls capture and disable all literal names then present. This support
condition does not include deliberate administrator reconfiguration during a
call. Use the existing service/package procedures, without a global config
lease or another process manager. Package-generation cancellation must prevent
the old worker from publishing a name or reservation.
