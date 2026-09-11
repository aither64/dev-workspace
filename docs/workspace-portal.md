# Workspace portal

The workspace portal keeps long-running development sessions available through
both a browser and tmux. One installed package generation supplies the portal,
Codex CLI, session helper, user units and any configured extensions.

## Workspace configuration

Each workspace can contain `.dev-workspace.json`:

```json
{
  "schema": 2,
  "displayLabel": "Example development",
  "hostLabel": "build-host",
  "sshHost": "build-host.example.test",
  "portal": {
    "hostname": "workspace.example.test",
    "aliases": ["old-workspace.example.test"]
  },
  "developmentClusterProviders": ["service"]
}
```

The portal hostname and aliases are configuration, not application constants.
`workspace-host register` reads them when no command-line override is supplied.
The registry stores the resolved values so routing remains deterministic until
the next explicit registration update.

Provider IDs must exist in the immutable package extension catalog. Workspace
files cannot choose executable paths or labels.

## User-profile state

The default paths are:

- registry: `~/.config/dev-workspaces/registry.json`;
- installed profile and retained Codex roots:
  `~/.local/state/dev-workspaces/`;
- per-workspace sockets and runtime authority:
  `$XDG_RUNTIME_DIR/dev-workspaces/<name>/`;
- router socket: `/run/dev-workspaces/router.sock`.

The profile switcher retains the previous generation and its exact Codex build.
`workspace-host rollback` selects that pair together. Commands waiting on a
transition lock reject a generation change and must be run again.

## Extension catalog

Downstream flakes call `dev-workspace.lib.mkPackage` with one `extensions`
attribute set containing three fields:

```nix
extensions = {
  commands = { };
  skills = { };
  clusterProviders = { };
};
```

The fields are:

- `commands`, mapping command names to absolute executables;
- `skills`, mapping skill names to package directories;
- `clusterProviders`, mapping provider IDs to a label and helper executable.

All values that name files or directories must be immutable Nix store
references. The resulting package writes `share/dev-workspace/extensions.json`.
Activation uses only this catalog when linking commands and skills. Workspace
configuration is always read from the registered root's `.dev-workspace.json`.
Cluster helpers receive
the selected workspace through `DEVCLUSTER_WORKSPACE` and keep ownership of
their `status`, `reset`, `cleanup-paths` and `transition-adopt` protocols.

The package constructor also accepts `userNamespace` and `routerSocket` for a
deployment-specific compatibility generation. `userNamespace` selects the
default user config, state, runtime and profile paths, including portal
lifecycle receipts and deletion recovery, without changing the generic command
or environment-variable names. Keep the defaults for new deployments; use
overrides only while an existing installation is moving its state to the
generic namespace. `activationEnvironmentAliases` can name a
validated legacy activation marker when the installed generation must invoke
that compatibility package. The selected values are recorded in
`share/dev-workspace/package.json` so a deployment-specific migration can
verify both sides of a cutover before moving state.

## Host module

`nixosModules.host` configures the privileged nginx and TLS substrate. Its
defaults use:

- `/var/lib/dev-workspaces/password/password`;
- `/var/lib/dev-workspaces/auth/htpasswd`;
- `/var/lib/dev-workspaces/pki`;
- `/var/lib/dev-workspaces/tls`;
- `/var/lib/dev-workspaces/public/ca.pem`;
- `/run/lock/dev-workspace-substrate.lock`.

The local operator is trusted to administer the development host. The
root/user split assigns nginx and activation files to their system lifecycle;
it does not contain a compromised operator. Remote requests still require
input validation, authentication and authorization. This trust assumption does
not extend to projects developed in the workspace or their guests.

Persistent outputs stay below `/var/lib`, runtime paths below `/run`, and the
lock file directly below `/run/lock`. Configured output directories must be
distinct and mutually non-nested. Reconciliation refuses malformed or symlinked
configured files and directories. The `tls/current` generation link is intentional.
It serializes writers with `flock`, establishes expected owners and modes, and
publishes generated files and complete TLS pairs atomically. It does not inspect
physical ancestor relationships or attempt to defend against hostile local
mounts and hardlinks.

The shared password remains `root:workspace-portal-owner` with mode `0640`,
and the CA key remains `root:root` with mode `0600`. These defaults keep their
existing lifecycle and reduce accidental changes. The nginx group can read the
generated htpasswd and leaf key. Reconciliation preserves the CA and password,
checks certificate/key pairs and requested names, and verifies leaves using
only the local CA. A weekly system service renews certificates and reloads
nginx. Retained PKI state, TLS generations and migration markers are not removed.

The firewall stays closed unless `services.dev-workspaces.firewall.sourceRanges`
contains an allowed network.

## Deployment order

Deploy the host module before registering a workspace. Then build and select the
user-profile package, register the workspace, and verify the router and portal
units. A package transition must contain compatible cluster and runtime-authority
contracts whenever persistent state exists.

Namespace migrations are deployment-specific. Stop writers, preserve state
bytes and metadata, rewrite machine-consumed paths as one journaled operation,
then start the new generation. Rollback requires the reverse migration; old and
new namespaces must not run together.

## Development

Run the quick checks with:

```sh
nix flake check --no-build --show-trace
```

Run the complete suite with:

```sh
nix flake check --print-build-logs
```

The NixOS smoke test in `nix/tests/host-module-idempotency.nix` covers
activation, a stable rerun, nginx authentication and proxy access, renewal,
interrupted publication, malformed state, concurrency and configuration
rollback. CI runs it with KVM on `master` and manual dispatch. Dispatch feature
VM tests after mandatory review; package and focused checks run on every push
and pull request. Keep normal dependency-update CI below the 20-minute target
and inspect cold-run timings when changing its workload.
