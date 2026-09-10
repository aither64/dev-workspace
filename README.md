# dev-workspace

`dev-workspace` manages persistent Codex development sessions. It provides the
session CLI, browser portal, user-profile runtime, workspace registry and an
extension interface for development-cluster helpers. The package pins
its own Codex build, so one profile generation always contains a tested portal
and App Server pair.

The repository was extracted from a larger development-workspace application.
Session records and local worktrees remain in each configured workspace.

## Nix interfaces

The flake exports these packages on `x86_64-linux`:

- `dev-workspace` contains the portal, lifecycle commands, user units and Codex
  runtime.
- `workspace-portal` and `workspace-host` are compatibility aliases for
  `dev-workspace`.

`nixosModules.host` configures a host-side nginx proxy, generated basic
authentication and a local certificate authority. It does not install or run
the user application. The firewall remains closed unless
`services.dev-workspaces.firewall.sourceRanges` contains an allowed network.
Persistent state paths must remain below root-controlled `/var/lib`; the
runtime router socket must remain below `/run`, and the reconciliation lock is
created directly below `/run/lock`. Every configured output has a separate
owning directory; those directories must be distinct and mutually non-nested.
Reconciliation rejects unexpected physical path relationships, symlinked
components and mounts on its internal `authority`, `pairs` and selected-pair
directories instead of following them. Existing `ca-key.pem` and `ca.pem`
files must be unmounted, single-link regular files owned by `root:root`, with
modes `0600` and `0644`, respectively. Leaf validation trusts only that local
CA, never the host's default trust store.
See `nixosConfigurations.example` for a minimal configuration.

Install the user-profile application from a checked-out source:

```sh
nix run .#workspace-host -- switch --source "$PWD"
```

After the first installation, run updates through the stable command from the
selected profile:

```sh
workspace-host switch --source "$PWD"
```

The switch command retains the previous profile generation and its Codex
runtime. Use `workspace-host rollback` to return to that pair.

## Workspace configuration

The package can be extended through `lib.mkPackage`. Extensions supply immutable
command, skill and cluster-provider catalogs. Workspace configuration selects
provider IDs; it cannot supply executable paths.

A workspace declares its portal identity and optional providers in
`.dev-workspace.json`:

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

Without this file, the portal uses generic labels, hides the SSH attach command
and enables no development-cluster providers. Registration flags can override
the configured hostname and aliases; the registry stores the resolved values.

## Development

Run the complete package checks with:

```sh
nix flake check --print-build-logs
```

The package uses a Go portal, Ruby lifecycle helpers and shell-based cluster
launchers. The flake supplies their build and test dependencies.
