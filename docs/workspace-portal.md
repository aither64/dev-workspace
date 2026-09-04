# Workspace portal

The workspace portal is the browser view for development initiatives:

```text
https://vpsfree-cz-workspace.aitherdev.int.vpsfree.cz/
```

It lists active and archived initiatives that contain `portal.yml`. An
initiative page shows GitHub comparisons, recent workflow runs, the plan,
current state, curated artifacts, and its Codex conversation. An active
conversation can receive messages, be interrupted, answer Codex questions, and
handle supported command or file-change approvals.

The portal is not a shell or a general file browser. It exposes `plan.md`,
`state.md`, and files explicitly listed in `portal.yml`. Curated artifacts are
limited to plain text, Markdown, JSON, YAML, and passive raster images. Active
formats such as HTML, SVG, JavaScript, XHTML, and XML are refused.

## Starting and attaching to sessions

The New session form accepts a short name and an initial request. It runs the
packaged `dev-session`, journals creation before changing initiative state,
creates a persisted Codex thread, and sends the request as the first turn. A
retry uses the same dated slug and must contain the same request. Completed
requests replay their existing result instead of creating another session or
turn.

Codex receives the workspace instructions and selects affected repositories.
The terminal and browser share one App Server thread and a dedicated tmux
server at `/run/vpsfree-workspace-tmux/tmux.sock`. Use the immutable host
wrapper for terminal actions:

```sh
dev-session attach 2026-09-03-example
dev-session stop 2026-09-03-example
dev-session url 2026-09-03-example
```

The full dated slug is accepted directly. You can also use a short name when it
matches exactly one session.

The page shows attach and conversation controls only when host-only authority
under `/run/vpsfree-workspace-authority` is ready and matches the live tmux
session, App Server socket, and persisted thread working directory. Stopped,
archived, forged, or stale sessions remain passive. A verified persisted thread
can still provide a read-only transcript after stop, reboot, completion, or
archival.

Terminal and browser sessions use the App Server socket at
`/run/vpsfree-workspace-codex/app-server.sock`. The App Server runs the Codex
package selected by `vpsfree-cz-configuration`. The workspace package is
contract-tested against that same Codex during the aitherdev build, so an
incompatible experimental protocol change fails the build instead of requiring
a separate production pin. Stored client versions are diagnostic history and
do not invalidate sessions after a compatible upgrade.

`stop`, `remove`, and `finalize` quiesce the managed terminal client and refuse
an active turn. If tmux disappears before authority cleanup, inspect the
session and remove only a validated, idle stale record:

```sh
dev-session recover-stale 2026-09-03-example
```

The local journal provides at-most-once initial-goal delivery. Codex does not
accept caller-supplied idempotency keys for thread or turn creation, so the
helper durably records an initial submission attempt in a temporary schema-2
manifest before calling `turn/start`. A retry against the same unmaterialized
thread fails closed; it does not rely on a briefly stale idle status. Once
history materializes, the retry requires exactly one matching initial user
message. If an App Server restart removes the recorded memory-only thread,
creation-only reconciliation can select or create one exact fresh replacement
and clear the attempt marker.
A crash between recording the attempt and sending it can therefore require an
App Server restart before retrying.

A lost `thread/start` response can race the App Server's in-memory registration
and leave an unprompted orphan. Recovery refuses multiple visible candidates;
restarting the App Server clears such memory-only orphans. This does not resend
the initial goal because initial-goal delivery has its own durable attempt
marker. Completion removes the marker and writes the ordinary schema-1
manifest, so completed sessions remain readable after a rollback. A previous
portal version intentionally rejects an in-flight schema-2 creation; redeploy
the current version to reconcile it.

## Portal manifest

`dev-session start` creates `work/<slug>/portal.yml`. Worktree creation records
the repository identity, branch, GitHub repository, default branch, and initial
commit. Cleanup records final commits, and finalization preserves the same URL
under `archive/<slug>`. The example is the stable schema written after creation;
schema 2 exists only while an initial goal has an unresolved delivery outcome.

```yaml
schema: 1
slug: 2026-09-03-example
codex:
  thread_id: 01a00000-0000-0000-0000-000000000000
  socket_path: /run/vpsfree-workspace-codex/app-server.sock
  client_version: 0.152.1
creation:
  state: ready
  initial_goal_sent: true
  goal_sha256: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
repositories:
  - name: vpsadmin
    project: vpsadmin
    github: vpsfreecz/vpsadmin
    branch: 2026-09-03-example
    default_branch: master
    initial_base_sha: 0123456789abcdef0123456789abcdef01234567
artifacts:
  - label: Deployment notes
    path: deployment.md
```

The version is informational; thread ID, canonical working directory, App
Server endpoint, and live host authority control access. Do not list
credentials, private keys, caches, or bulk captures. Artifact paths cannot
leave the tracking directory, and symlinks are refused. Unknown fields,
explicit nulls, aliases, duplicate keys, tags, and multiple YAML documents are
invalid.

## Authentication and security boundary

nginx accepts HTTPS only from the WireGuard network and requires Basic
Authentication. The Go service listens on a permission-restricted Unix socket.
The portal cannot read nginx's password hash, TLS key, or CA key, and the socket
is not mounted into the development LXC.

The portal rejects mutations without the exact origin, uses a restrictive
content security policy and sanitized Markdown, never invokes Git while
rendering a page, and does not expose general filesystem, shell, or App Server
RPC access. Workspace manifests are display metadata; every mutation also
checks uid-private host authority against tmux and the App Server.

The Basic Auth username is `aither`. Its random plaintext password is stored at
`/home/aither/.local/state/vpsfree-workspace-portal/password`, readable only by
`aither`. NixOS activation derives the root-owned nginx hash from this file.
Neither value enters Git or the Nix store. Browsers commonly cache Basic Auth
credentials until their session is closed.

Portal access grants control comparable to an interactive Codex client running
as `aither`. Keep the password unique, keep the hostname VPN-only, and remove
the private CA from a lost device. Permission approval request types that
cannot be verified through the App Server remain terminal-only.

## Private CA and certificate renewal

The first aitherdev activation creates the CA and leaf directly in root-owned
`/var/lib/vpsfree-workspace-pki`, installs the nginx leaf pair, and exports the
public CA to the user-readable
`/var/lib/vpsfree-workspace-portal-public/ca.pem`.

The unencrypted CA key is protected by root ownership and mode `0600`; the
portal cannot read it. The nginx key is `root:nginx` mode `0640`. Certificates
and keys are switched as atomic versioned pairs. A weekly timer renews a leaf
with less than 30 days remaining, preserves the CA, and reloads nginx. Repeated
deployments validate and reuse existing state.

Copy only the public CA to VPN clients and trust it for TLS. Never copy the CA
key or server keys to a client.

Removing the NixOS configuration does not revoke the CA on clients. To
decommission the portal permanently, remove the CA from every client's trust
store, remove the internal DNS record, and delete the CA state on aitherdev.
If the CA key may have been exposed, stop the portal, remove client trust and
the DNS record, delete the compromised CA state, and redeploy to create a new
CA. Trust the replacement public CA only after checking it on aitherdev.

## Packaging and deployment

The workspace flake owns `packages.x86_64-linux.workspace-portal`.
`vpsfree-cz-configuration` consumes it through `aitherVpsfreeWorkspace`, whose
`nixpkgs` and `llm-agents` inputs follow the configuration's existing inputs.
Updating the ordinary `llm-agents` channel therefore updates terminal Codex,
the App Server, and the package's protocol contract test together.

Deploy aitherdev first, then deploy both internal DNS servers through normal
confctl generations. Host activation creates or reuses credentials and starts
the portal; no portal-specific rollout helper, NAR attestation, manual rollback
capture, or exclusive deployment procedure is needed. Ordinary NixOS
generation rollback removes the service while leaving reusable credentials on
disk.

The portal continues serving initiative status when GitHub or the App Server is
temporarily unavailable. Only the affected integration reports an error. The
browser displays the 20 most recent turns; attach from a terminal for older
conversation history.

## ChatGPT desktop and mobile access

The portal works in a macOS or mobile browser once the VPN is connected,
internal DNS resolves, and the private CA is trusted. It does not register its
sessions as native ChatGPT Remote sessions.

Separately, the ChatGPT macOS app can connect to aitherdev over SSH and start
its own remote Codex App Server. ChatGPT mobile can then control those native
remote chats through a paired desktop host. These are a separate session
population from the workspace portal.
