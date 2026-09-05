# vpsAdminOS Dev Clusters

This directory provides workspace-local vpsAdminOS-only development clusters.
It boots vpsAdminOS VMs directly, without vpsAdmin services, database, web UI,
DNS services, mail capture, or seeded API data.

Runtime state, SSH keys, result links, VM disks, sockets, and logs are stored
under `.dev-clusters/vpsadminos/` at the workspace root and are intentionally
not tracked by git.

## Basic Usage

```sh
dev-clusters/vpsadminos/bin/devcluster start 2026-05-31-example
dev-clusters/vpsadminos/bin/devcluster info 2026-05-31-example
dev-clusters/vpsadminos/bin/devcluster ssh 2026-05-31-example node1
dev-clusters/vpsadminos/bin/devcluster update 2026-05-31-example node1
dev-clusters/vpsadminos/bin/devcluster stop 2026-05-31-example
dev-clusters/vpsadminos/bin/devcluster gcroots --cleanup
```

When running inside a `dev-session` shell, `ssh` can use
`VPSFREE_DEV_SESSION_SLUG` automatically:

```sh
dev-clusters/vpsadminos/bin/devcluster ssh node1
dev-clusters/vpsadminos/bin/devcluster ssh node1 -- hostname
dev-clusters/vpsadminos/bin/devcluster ssh node1 -t -- bash -l
```

Topologies:

- `single`: one vpsAdminOS VM. This is the default.
- `dual`: two vpsAdminOS VMs.
- `triple`: three vpsAdminOS VMs.

Each VM boots with a persistent file-backed disk and an active `tank` pool, so
`osctl` can be used immediately after SSH login.

## Network Modes

`local` is the default. Each VM gets:

- `eth0`: QEMU user networking for internet access and SSH host forwarding;
- `eth1`: QEMU socket multicast networking for VM-to-VM traffic.

Local mode uses the configured `localNameservers` because QEMU's built-in
`10.0.2.3` resolver is not reliable in all vpsAdminOS test boots.

Default SSH forwards are:

- `node1`: `127.0.0.1:11122`
- `node2`: `127.0.0.1:11222`
- `node3`: `127.0.0.1:11322`

Example:

```sh
dev-clusters/vpsadminos/bin/devcluster start 2026-05-31-example --topology dual --network local
dev-clusters/vpsadminos/bin/devcluster ssh 2026-05-31-example node1
```

Some vpsAdminOS boots can spend several minutes without console output after
SeaBIOS. Use `--timeout seconds` when testing slower boots.

`bridge` attaches `eth1` to `br0`, assigns the configured `172.16.106.*`
addresses, and routes the VM through the bridge gateway. The current user needs
access to `/dev/kvm` and a usable QEMU bridge helper. The helper path defaults to
`/run/wrappers/bin/qemu-bridge-helper`; override it with:

```sh
VPSADMINOS_DEVCLUSTER_BRIDGE_HELPER=/path/to/helper \
  dev-clusters/vpsadminos/bin/devcluster start 2026-05-31-example --network bridge
```

Use an empty helper value to omit the QEMU `helper=` option.

## Configuration

Each cluster gets an editable config at:

```sh
.dev-clusters/vpsadminos/clusters/<slug>/config.json
```

It is copied from `dev-clusters/vpsadminos/default-config.json` on first use.
Use it to change node names, bridge/local IPs, SSH forward ports, memory, CPU,
disk sizes, topology membership, bridge name, gateway, local socket multicast
port, local resolvers, or upstream resolvers.

If `worktrees/<slug>/vpsadminos` exists, the cluster is built from that worktree.
Otherwise it is built from `repos/vpsadminos.git` `origin/staging`.

`start` and `update` keep the built cluster config rooted at
`.dev-clusters/vpsadminos/clusters/<slug>/result-config` while the cluster is in
use. `stop` removes that root after the runner exits, and `reset` removes it
with the rest of the cluster state. Use `devcluster gcroots` to list retained
cluster config roots and `devcluster gcroots --cleanup` to remove roots for
stopped clusters left by older tooling.

## Runtime Updates

After changing vpsAdminOS code or configuration, rebuild and switch a running VM:

```sh
dev-clusters/vpsadminos/bin/devcluster update <slug> node1
dev-clusters/vpsadminos/bin/devcluster update <slug> all
```

The update command copies the new system closure over SSH and runs
`switch-to-configuration switch` inside the selected VM.

Use `reset <slug>` to remove the per-slug VM state, including persistent disks.
