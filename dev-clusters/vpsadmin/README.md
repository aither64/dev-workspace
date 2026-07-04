# vpsAdmin Dev Clusters

This directory provides workspace-local vpsAdmin development clusters selected
from feature worktrees under `worktrees/<slug>/`.

Runtime state, certificates, SSH keys, result links, and logs are stored under
`.dev-clusters/` at the workspace root and are intentionally not tracked by git.

## Basic Usage

```sh
dev-clusters/vpsadmin/bin/devcluster start 2026-05-29-security-advisories --topology dual
dev-clusters/vpsadmin/bin/devcluster urls 2026-05-29-security-advisories
dev-clusters/vpsadmin/bin/devcluster config 2026-05-29-security-advisories
dev-clusters/vpsadmin/bin/devcluster refresh 2026-05-29-security-advisories
dev-clusters/vpsadmin/bin/devcluster update 2026-05-29-security-advisories services
dev-clusters/vpsadmin/bin/devcluster ssh 2026-05-29-security-advisories services
dev-clusters/vpsadmin/bin/devcluster stop 2026-05-29-security-advisories
dev-clusters/vpsadmin/bin/devcluster gcroots --cleanup
```

When running inside a `dev-session` shell, `ssh` can use
`VPSFREE_DEV_SESSION_SLUG` automatically:

```sh
dev-clusters/vpsadmin/bin/devcluster ssh node1
dev-clusters/vpsadmin/bin/devcluster ssh services -- hostname
dev-clusters/vpsadmin/bin/devcluster ssh node1 -t -- bash -l
```

Topologies:

- `single`: services VM and one vpsAdminOS node.
- `dual`: services VM and two vpsAdminOS nodes.
- `storage`: services VM, two regular nodes, and one storage node.

Only one VPN-visible dev cluster should be active at a time. The cluster uses
the existing `aitherdev` dev-network names and IPs, especially
`webui.aitherdev.int.vpsfree.cz`. The `*-tmp.aitherdev.int.vpsfree.cz`
names are configured as secondary frontend entries for internal/maintenance
access tests.

Network modes:

- `bridge` is the default. It attaches VMs to `br0` and uses the predictable
  `172.16.106.*` dev addresses. This needs a usable `qemu-bridge-helper` or
  root privileges. On `aitherdev`, deploy the host configuration that provides
  `/run/wrappers/bin/qemu-bridge-helper` and keeps `/etc/qemu/bridge.conf`
  restricted to the allowed bridges.
- `local` runs without bridge privileges. VMs talk to each other on a QEMU
  socket network and expose host forwards on localhost:
  `10443` for HTTPS, `10022` for services SSH, `10122` for node1 SSH, `10222`
  for node2 SSH, and `10322` for storage1 SSH.

The bridge helper path defaults to `/run/wrappers/bin/qemu-bridge-helper`.
Override it with `VPSADMIN_DEVCLUSTER_BRIDGE_HELPER=/path/to/helper`, or set it
to an empty value to omit the QEMU `helper=` option.

`start` and `update` keep the built cluster config rooted at
`.dev-clusters/vpsadmin/clusters/<slug>/result-config` while the cluster is in
use. `stop` removes that root after the runner exits, and `reset` removes it
with the rest of the cluster state. Use `devcluster gcroots` to list retained
cluster config roots and `devcluster gcroots --cleanup` to remove roots for
stopped clusters left by older tooling.

Resolver behavior is configured in `config.json` under `resolver`. The default
mode, `cluster`, runs dnsmasq on the services VM, serves all devcluster host
records from the same config, and forwards other lookups to configurable
upstream nameservers. The default upstreams are the vpsFree.cz internal
resolvers `172.16.9.90` and `172.19.9.90`. Other supported modes are
`upstream`, which points every VM directly at `resolver.upstreamNameservers`;
`gateway`, which points every VM at `network.gateway`; and `none`, which leaves
the machine defaults alone.

## Configuration And Seed Data

Each cluster gets its own editable config at:

```sh
.dev-clusters/vpsadmin/clusters/<slug>/config.json
```

It is copied from `dev-clusters/vpsadmin/default-config.json` on first use and
is merged over the tracked defaults. Use it to change domains, service and node
IP addresses, topology membership, seeded users, resource packages, pool
settings, networks, IP addresses, and mail recipients.

The default seed creates:

- one hypervisor pool on each regular node, using filesystem `tank/ct`;
- public and private IPv4 networks with allocatable addresses;
- two non-admin users, `test-user1` and `test-user2`;
- default VPS resource values and per-user resource packages;
- mail recipients for admin daily reports;
- vpsfree mail templates, when the matching worktree exists;
- Adminer database browser, exposed as
  `https://adminer.aitherdev.int.vpsfree.cz/`;
- vpsFree.cz web, exposed as `https://web-cs.aitherdev.int.vpsfree.cz/`
  and `https://web-en.aitherdev.int.vpsfree.cz/`, when the matching
  worktree exists;
- a vpsf-status instance on the services VM, exposed as
  `https://status.aitherdev.int.vpsfree.cz/`.

The plugin set is configured with `plugins.enabled`. The default value is
`"all"`, which enables every plugin directory bundled in the selected vpsAdmin
worktree. Set it to a JSON array such as `["webui", "payments"]` to test a
smaller set, or to `"none"` to disable plugins.

Regular devcluster nodes set nodectld's `zfs_send` and `zfs_recv` queue
`start_delay` to `nodectld.zfsTransferStartDelay`, which defaults to `0`.
This avoids production transfer pacing during manual migration, clone, backup,
and VPS replacement testing. Set it in a cluster config only when the delay
itself is what you need to test.

After a services seed has changed pool data, `devcluster refresh <slug>` prepares
the vpsAdmin pool working directories and default pool device grants on regular
nodes, then restarts nodectld so DB-seeded pools are usable by node transactions.
`start` and `update ... services` run the same refresh automatically.

Example local start:

```sh
dev-clusters/vpsadmin/bin/devcluster start 2026-05-29-security-advisories --topology single --network local
```

For browser testing in `local` mode, resolve the printed dev hostnames to
`127.0.0.1` and use port `10443`, for example
`https://webui.aitherdev.int.vpsfree.cz:10443/`.

## HTTPS

`start` ensures a certificate set exists. By default, it generates a
workspace-local CA and leaf certificate. To reuse an existing CA and server
certificate, import a directory containing `vpsadmin-ca.crt`,
`vpsadmin-ca.key`, `vpsadmin-cert.crt`, and `vpsadmin-cert.key`:

```sh
dev-clusters/vpsadmin/bin/devcluster cert import /path/to/certs
```

Set `VPSADMIN_DEVCLUSTER_CERT_IMPORT_DIR=/path/to/certs` to have `start`
import an existing certificate set automatically when the cluster has no
certificate yet. When the configured domains change, the helper reissues the
leaf certificate from the current CA. If the current CA key is encrypted and
cannot sign unattended, it falls back to a fresh workspace-local CA. Set
`VPSADMIN_DEVCLUSTER_CA_PASSPHRASE` before `start` or `update` to reissue from
an encrypted imported CA instead.

Use:

```sh
dev-clusters/vpsadmin/bin/devcluster cert show-ca
```

to print the CA certificate path and fingerprint for browser trust setup.

## Email

Outgoing vpsAdmin notification e-mail is captured by Mailpit on the services
VM. The Mailpit UI is exposed through the services nginx frontend at the HTTPS
URL printed by `devcluster urls`, currently
`https://mailpit.aitherdev.int.vpsfree.cz/`, and is protected with the
configured development basic-auth credentials. The raw Mailpit HTTP listener is
bound to `127.0.0.1` inside the services VM.

If `worktrees/<slug>/vpsfree-notification-templates` exists, its templates are
copied into the Nix closure and installed as managed notification templates by
the API service. Re-run:

```sh
dev-clusters/vpsadmin/bin/devcluster update <slug> services
```

after changing template files. Runtime virtiofs mounts cannot be added to an
already-running VM, so templates are intentionally closure-copied instead of
mounted live.

## Webhook Test Server

The services VM runs `vpsadmin-webhook-test-server.service` for notification
webhook testing. Use `http://127.0.0.1:18080/events` as the webhook URL from
vpsAdmin. The latest request is written to
`/tmp/vpsadmin-webhook-test/request.json` inside the services VM.

## SMS

The services VM runs `vpsfree-sms-gateway.service` with the gateway's fake
driver when the selected vpsAdmin checkout supports SMS notifications. vpsAdmin
uses `http://127.0.0.1:9876/v1/sms` as its SMS gateway, and the SMS dispatcher
is enabled by default. The seeded dev users have SMS notifications enabled so
receiver actions can be created and verified without production GSM modems.

The fake gateway stores its SQLite database in
`/var/lib/vpsfree-sms-gateway/gateway.db` inside the services VM. Inbound SMS
persistence is disabled by default. To disable the gateway, edit the cluster
config:

Inspect sent and queued fake-driver SMSes with `vpsfree-sms-gatewayctl`:

```sh
bin/devcluster ssh <slug> services -- vpsfree-sms-gatewayctl stats
bin/devcluster ssh <slug> services -- \
  vpsfree-sms-gatewayctl outbound list --source vpsadmin
```

Sent fake-driver SMSes show up as `sent` outbound messages with a `fake-*`
provider ID.

```json
{
  "sms": {
    "enable": false
  }
}
```

To opt into inbound persistence, keep the gateway enabled and set:

```json
{
  "sms": {
    "inbound": {
      "enable": true,
      "webhooks": []
    }
  }
}
```

## Telegram

Telegram notifications are enabled only when a bot token file exists on the
host:

```sh
mkdir -p .dev-clusters/vpsadmin/telegram
printf '%s\n' '<bot-token>' > .dev-clusters/vpsadmin/telegram/bot-token
printf '%s\n' 'vpsadmin_aitherdev_bot' > .dev-clusters/vpsadmin/telegram/bot-username
chmod 0600 .dev-clusters/vpsadmin/telegram/bot-token
```

`devcluster update <slug> services` copies the token into the services VM and
enables Telegram for the API, notification dispatcher, and Telegram receiver
services. Re-run it after creating, changing, or removing the token file in an
already-running cluster. When the token file exists before a fresh cluster is
started, startup automatically performs the services update after the cluster
becomes ready.

The bot username is not secret. It is used to show pairing links in vpsAdmin,
for example `https://t.me/vpsadmin_aitherdev_bot?start=<token>`. If you use a
different development bot, replace `vpsadmin_aitherdev_bot` with the username
shown by BotFather, without the leading `@`. BotFather shows it after bot
creation as the final `_bot` username and in the `t.me/<username>` link.

The default receive mode is polling. To test webhook mode, add this to the
cluster config:

```json
{
  "telegram": {
    "receiveMode": "webhook"
  }
}
```

Webhook mode also needs a secret token:

```sh
openssl rand -hex 32 > .dev-clusters/vpsadmin/telegram/webhook-secret
chmod 0600 .dev-clusters/vpsadmin/telegram/webhook-secret
```

The dev cluster registers the webhook URL on the API domain at
`https://api.aitherdev.int.vpsfree.cz/_telegram/webhook`.

## Database Browser

Adminer runs on the services VM and is exposed through the same nginx HTTPS
frontend as the Web UI, API, Mailpit, and status page. The default basic-auth
credentials are printed by `devcluster urls`.

Use `MySQL`, server `127.0.0.1`, user `vpsadmin`, and password
`testMariadbApiPassword` to browse the vpsAdmin database.

## Status Page

vpsf-status runs on the services VM and is exposed through the same nginx HTTPS
frontend as the Web UI, API, and Mailpit. If `worktrees/<slug>/vpsf-status`
exists, it is used as the source for the status package. If
`worktrees/<slug>/vpsadmin-go-client` exists, it is made available to that
package for local generated-client testing.

Re-run:

```sh
dev-clusters/vpsadmin/bin/devcluster update <slug> services
```

after changing vpsf-status or the generated Go client.

## Runtime Updates

The web UI is served from a live symlink tree backed by the selected vpsAdmin
worktree, with Composer/vendor dependencies coming from the Nix package.
Changes to existing PHP/templates/static files are visible after normal
PHP-FPM/nginx behavior.

When `worktrees/<slug>/web` exists, the vpsFree.cz web is served from a live
symlink tree backed by that worktree. Its generated `config.php` points to the
devcluster API.

Ruby services and system-level changes use:

```sh
dev-clusters/vpsadmin/bin/devcluster update <slug> services
dev-clusters/vpsadmin/bin/devcluster update <slug> node1
```

which rebuilds the machine config, copies the new closure to the running VM,
and runs `switch-to-configuration`.

When changing Ruby code packaged as gems, rebuild vpsAdmin packaged gems before
updating the cluster:

```sh
cd worktrees/<slug>/vpsadmin
nix develop -c rake vpsadmin:gems
```
