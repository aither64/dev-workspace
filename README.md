# dev-workspace

`dev-workspace` manages persistent Codex development sessions. It provides the
session CLI, browser portal, user-profile runtime, workspace registry and
optional vpsAdmin and vpsAdminOS development-cluster helpers. The package pins
its own Codex build, so one profile generation always contains a tested portal
and App Server pair.

The source history was extracted from
[`aither64/vpsfree-cz-workspace`](https://github.com/aither64/vpsfree-cz-workspace)
at commit `3580e60bb035c2d0ba5be6f0d2489bbbf30ded3d`. Session records and local
worktrees remain in the workspace repository and are not part of this package.

## Nix interfaces

The flake exports these packages on `x86_64-linux`:

- `dev-workspace` contains the portal, lifecycle commands, user units, Codex
  runtime and development-cluster providers.
- `dev-workspace-vpsfree` adds the vpsFree.cz KB helpers and packaged Codex
  skills.
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
directories instead of following them. Existing CA files must be unmounted,
single-link regular files with the documented root ownership and modes. Leaf
validation trusts only that local CA, never the host's default trust store.
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

Registration owns the local workspace path and portal hostname. A registered
workspace can add `.dev-workspace.json` at its root to select optional providers
and user-facing host details:

```json
{
  "schema": 1,
  "displayLabel": "Example development",
  "hostLabel": "build-host",
  "sshHost": "build-host.example.test",
  "developmentClusterProviders": ["vpsadmin"]
}
```

Without this file, the portal uses generic labels, hides the SSH attach command
and enables no development-cluster providers. The package ships the vpsAdmin
and vpsAdminOS providers; list `vpsadmin`, `vpsadminos`, both or neither.

## Development

Run the complete package checks with:

```sh
nix flake check --print-build-logs
```

The package uses a Go portal, Ruby lifecycle helpers and shell-based cluster
launchers. The flake supplies their build and test dependencies.
