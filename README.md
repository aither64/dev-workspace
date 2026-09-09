# dev-workspace

`dev-workspace` manages persistent Codex development sessions. It provides the
session CLI, browser portal, user-profile runtime, workspace registry and
optional vpsAdmin and vpsAdminOS development-cluster helpers.

The source history was extracted from
[`aither64/vpsfree-cz-workspace`](https://github.com/aither64/vpsfree-cz-workspace)
at commit `3580e60bb035c2d0ba5be6f0d2489bbbf30ded3d`. Session records and local
worktrees remain in the workspace repository and are not part of this package.

The current tree is the extraction baseline. The public package and module
interfaces are developed on the `2026-09-09-workspace-components` branch.

## Development

Run the complete package checks with:

```sh
nix flake check --print-build-logs
```

The package uses a Go portal, Ruby lifecycle helpers and shell-based cluster
launchers. The flake supplies their build and test dependencies.
