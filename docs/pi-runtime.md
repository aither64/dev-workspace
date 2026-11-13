# Pi SDK runtime

The Pi adapter separates conversation input from runtime and host authority.
`piruntime.Controller` takes trusted session and member identities; its nullable
conversation operation slots do not grant permission to use them. The host
launcher supplies the session process boundary described in
[Pi session host launcher](pi-runtime-host.md). Each loaded conversation uses a
separate SDK process, private history/settings/resources and the configured
canonical native authentication directory.

The manager verifies the private supervisor handshake against independent host
observations before loading a worker. Installed host permissions, trusted portal
composition and route admission are prerequisites for enabling Pi in the portal.
Source fixtures do not establish an installed usable session.

## Input acceptance and recovery

The adapter publishes a private, fsynced intent before dispatch. Ordinary text
input has one pending submission per worker. The SDK user event occurs before
native history append, so the adapter waits until the exact new entry can also
be read through an independent public SessionManager reopen. Repeated text is
correlated using the live event object and new entry lineage, never text alone.
The host then commits the native witness to its existing submission record
before returning Accepted.

Pi's native history writer closes these writes without calling `fsync`.
Independent reopen and the synced host receipt prove observed file persistence
and recovery from a worker crash; they do not prove that native history survives
a host power failure. If later verification finds missing history or an entry,
the operation remains held without repair or resend.

Records use schema 1 and retain the stable conversation, original operation ID,
action, full immutable input/settings digest and dispatch generation. Generation
is evidence and does not partition duplicate detection. IDs accept exactly
lowercase 32 hexadecimal characters or the lowercase 8-4-4-4-12 hexadecimal
spelling. Spellings are preserved; compact and hyphenated IDs are distinct.
Lookup and same-ID retries never dispatch again, including terminal NotSubmitted.
Changed input or settings under the same ID conflicts.

A crash before the committed witness can leave Unknown even if native input or
a tool effect exists. Missing acknowledgements after receipt commit are
recoverable by lookup. Unknown holds the affected conversation and requests its
exact worker abort; abort does not prove arbitrary external effects stopped.
SDK exceptions cannot establish NotSubmitted. The adapter makes no exactly-once
claim for external tools.

## Native identities and settings

A creation mapping reserves a native ID before SDK creation. Empty native
sessions may have no history file. Never submit synthetic input to force a
header, and never open a missing history path as recovery: the SDK can choose
a new identity. Creation verification is observational and requires the owned
mapping plus independent native history and the original Accepted receipt.
An initialized-only proof cannot publish a ready conversation.

HistoryRef is an opaque SHA-256 mapping locator, not persistence evidence. Its
versioned encoding is the JSON array `['pi-history',1,backend,workspace,slug,
sessionIncarnation,member,conversationIncarnation,nativeID]`, encoded by Go
encoding/json without a newline. A golden fixture fixes the bytes/hash.

EffectiveSettings contains resolved provider/account/model/thinking/policy
references, never tokens or browser-supplied endpoints. Revision is the target
revision: current+1 applies a mutation; identical full settings at the same
revision replay the prior result; conflicting/skipped/lower revisions reject.
Configuration mutations are idle and serialized. Both current and requested
provider/account pairs must be in the immutable runtime account map. A switch
changes both provider and account reference; replacing the account under one
provider, or changing provider while retaining the account reference, refuses.
The public SDK changes model/thinking on the same native session with
persist:false. The runtime fsyncs its owned effective settings; native global defaults
and other members remain unchanged. Outcomes distinguish applied, rejected before
mutation, and unknown after possible mutation. A pending or uncertain settings
change holds further input and configuration; it never rolls back by guessing.

The SDK runs in the existing canonical `<workspace>/work/<slug>` directory.
Creation/load never creates a missing tracking directory. A crashed loaded
worker with persisted input remains held; loading history cannot resume its turn.

## Trusted startup and account attribution

Manager configuration requires explicit UserStateRoot, HostStateRoot and
AuthDirectory. The latter names an existing native Pi directory, including
its auth.json locks and atomic replacements. There is no directory inference,
credential copy, login or reuse of a rotating grant from another runtime.

AccountBindings is a sorted, nonempty map of native provider IDs to catalog
account references, bounded to 64 entries. Trusted catalog composition supplies
all selectable canonical pairs before startup. The logical credential-store
reference is canonical, with the native provider ID as credential key. Browser
and model input cannot add a binding. This proves configured store/key
attribution, not upstream account identity, entitlement or successful model access.

The manager freezes launch bytes and a per-request runtime-config.json with
scope, instance/request/generation, launch SHA-256, selected user/auth directories
and account_bindings. Their exact bytes and hashes survive interrupted preparation.
It reserves the instance/generation before preparation and records starting
before the host call. Same-ID reconciliation never calls native start again.
Missing or changed frozen configuration refuses. A different start request is
currently refused; no automatic successor or crash-resume policy is inferred.

Production workers match both account references before setup/configuration,
reject configured apiKey/headers/auth-source overrides in private models.json
and custom registered providers, then require the public runtime's stored-auth
source. Stored status alone is insufficient because it precedes configured
overrides in Pi. Clean custom endpoint/model definitions remain subject to
trusted catalog and adapter support. Fake runtimes are explicit source fixtures.

## History, cancellation and resources

Read/Page exposes escaped text from the selected native branch. Opaque cursors
bind the stable conversation, execution generation and full branch contents;
append, replacement, divergence or wrong scope makes an old cursor stale. Frames
are bounded: each page contains up to 50 native entries and 512 KiB of entry
JSON; an oversized individual entry is unavailable while its native original
remains intact. Watch invalidates a current snapshot and has no replay-log claim.

Interrupt requests the bound SDK abort. Native idle is not descendant-quiescence
proof. Unload requires idle/no unresolved receipts/settings and verifies the
exact worker/process-group exit plus session-cgroup inventory. Unattributed or
surviving descendants retain a durable hold, even if a later observation sees
no process; no implicit cleanup or recovery clears it.

Supervisor-started resources retain original operation/resource IDs and exact
PID/start/process-group identity. Same-ID starts do not execute again. A known
exit needs the retained wait result and descendant checks; uncertain effects
stay held. Metrics describe the session cgroup and retained scratch usage,
with explicit unavailable/external coverage. Usage is not a quota and shared
Nix daemon builders, remote tools/VMs and deleted-open files are excluded.
The external service registry must admit versioned Pi handles before using
these methods; compilation does not enable legacy service routes.

## Worker package boundary

The worker uses public SDK exports from one explicit immutable Pi package root.
It never resolves ambient npm modules or probe fixtures. Its module-relative closure
is worker.mjs, protocol.mjs, session.mjs, accounts.mjs and package.mjs. The private version-1
pipe protocol has bounded messages, correlation and exact execution identity.
The pre-initialization version/EOF smoke creates no SDK session and reads no
authentication data. Source-only fake providers are excluded from production.

The application's strict pi-worker.json parser owns validation of worker
protocol, immutable Node/entrypoint/Pi roots, duplicate/unknown/null/trailing
fields, filesystem kinds and access. This metadata contains no state/auth paths
or credentials. Host execution and a package build alone do not prove supervisor
readiness, native prompt admission or installed tool permissions.

## Packaged inputs and operator prerequisites

The application closure contains the exact unmodified upstream Pi SDK, its
compatible Node runtime and the fixed production worker modules. Startup uses
their absolute Nix-store paths; it does not resolve `pi`, `node` or SDK packages
through PATH, npm or a moving profile. No resource installation is needed at
runtime.

`share/dev-workspace/pi-runtime.json` publishes the fixed host-launcher-v1
entrypoint. Separate `share/dev-workspace/pi-worker.json` contains only
`schema`, `worker_protocol`, `node_executable`, `worker_entrypoint` and
`pi_package_root`. Both versions are 1. Credentials, provider endpoints and
machine-specific state paths are absent from these reusable manifests.
The `pi-worker` package/check is available independently of application launch.

Pi needs the separately installed [host prerequisite](pi-runtime-host.md).
The portal service also requires explicit `DEV_WORKSPACES_PI_AUTH_DIR` and
`DEV_WORKSPACES_PI_HOST_STATE_ROOT` settings, forwarded as `--pi-auth-dir` and
`--pi-host-state-root`. Neither has an application default. Set the latter to
the installed `services.dev-workspaces.piRuntime.stateRoot` exactly, including
any override. Configure these through the existing
`workspace-portal@<name>.service` environment/drop-in or a deployment-owned
equivalent. Missing or invalid configuration disables Pi while Codex remains
available.

Use one canonical native Pi credential directory, outside member histories,
settings and session scratch. Workers share the directory, including native
locks and atomic credential replacement; each keeps private histories,
settings, models and resources. An initial native Pi grant is an operator
prerequisite. Copying Codex's rotating grant does not replace that step.
The trusted manager resolves the existing user-state root once and freezes
it with the auth directory in each request's private runtime configuration;
workers do not infer those paths from their environment.

Package and fake-protocol checks establish immutable input resolution and
source contracts. Installed host UID/groups, sudo policy, exact namespace,
Nix access and canonical-auth visibility still need the reviewed installed
verification gate. Building a package does not activate it or enable Pi.
