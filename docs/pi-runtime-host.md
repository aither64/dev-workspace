# Pi session host launcher

The optional NixOS host prerequisite gives each Pi development session one
system-service cgroup and exact mount namespace. The service runs one
supervisor as the configured workspace owner; loaded workers, their tools and
registered Pi services inherit its boundary. The portal remains outside it.
This module installs host tools and policy. The application still comes from
the separately selected user profile.

Enable `services.dev-workspaces.piRuntime.enable` together with the existing
host module. It defaults to false. Disabled installations receive no Pi unit,
helper or sudo rule. The owner's configured home and primary group determine
the service identity; supplementary groups come from systemd's normal
initgroups setup. A stable, non-root configured UID is required.

`PrivateMounts=yes` and two fixed bind mounts create disk-backed `/tmp` and
`/var/tmp`. `PrivateUsers=no` preserves the host user namespace and normal
setuid/sudo semantics. The service grants no ambient mount capability and
does not set `NoNewPrivileges`. This follows the repository's trusted local
operator model. It does not contain an administrator who already controls the
host. Browser and model input remain subject to application authorization.

## Storage and application contract

`stateRoot` defaults to
`<configured-home>/.local/state/dev-workspaces/pi-runtime-host`. Overrides must
be normalized absolute paths without whitespace, colon or systemd `%`
specifiers. The runtime controller prepares owner-managed storage for a random
UUID retained for the entire session incarnation:

```text
<stateRoot>/instances/<instance-uuid>/
  launch.json
  launch.lock
  receipts/host.json
  package-root
  control.sock
  tmp/
  var-tmp/
```

The controller owns launch publication, the retained application GC root,
session authorization and profile admission. `package-root` must resolve to
the exact package in the request. Scratch and credentials are separate: the
host module leaves the canonical writable credential directory visible,
including its locks and atomic file replacements. It never copies credentials.

`launch.json` is UTF-8 JSON, bounded to 64 KiB. Unknown/duplicate keys, nulls,
wrong scalar types and trailing JSON refuse. Required fields are:

| Field | v1 value or type |
| --- | --- |
| `schema`, `host_launcher_contract` | Integer `1` |
| `scope.workspace` | Canonical existing absolute workspace path |
| `scope.slug` | Dated development-session slug |
| `scope.session_incarnation` | 64 lowercase hexadecimal characters |
| `scope.backend`, `scope.backend_contract` | `"pi"`, integer `1` |
| `scope.binding_revision` | Positive uint64 admitted binding revision |
| `instance_id`, `request_id` | Lowercase UUID |
| `generation` | 64 lowercase hexadecimal characters |
| `app_store_path` | Canonical `/nix/store/<hash>-<name>` package root |
| `profile_path` | Normalized absolute admitted profile path |
| `profile_generation` | Nonempty admitted profile-generation identity |
| `replaces_request_id` | Predecessor UUID, required only for a successor |

Strings are at most 4096 UTF-8 bytes and contain no control characters. The
digest is SHA-256 of the exact frozen file bytes, including whitespace and its
final newline. There is no digest field in the JSON. Reformatting changes the
identity. [The shared byte fixture](../test/fixtures/pi-host-launch-v1.json)
and its adjacent `.sha256` file are the cross-language test input.

The exact application must contain `share/dev-workspace/pi-runtime.json`:

```json
{"schema":1,"host_launcher_contract":1,"launch_request_schema":1,"handshake_schema":1,"supervisor_entrypoint":"bin/workspace-portal","supervisor_command":["pi-runtime","supervise"]}
```

These are fixed constants; the manifest cannot select arbitrary executable
arguments. The immutable host bootstrap runs after systemd drops privileges,
checks the request and capability, then directly execs:

```text
<app_store_path>/bin/workspace-portal pi-runtime supervise
  --launch-request <instance-directory>/launch.json
  --instance <instance-uuid> --request <request-uuid> --digest <sha256>
```

It never follows a moving profile to choose the executable. The application
must independently verify the same byte digest and handshake identity. The
launcher does not prove Pi prompt admission or restore uncertain tool effects.

## Private controller API and recovery

Only the installed exact-store `workspace-pi-host` parser is granted
noninteractive sudo. It accepts these fixed operations:

```text
workspace-pi-host start   --instance UUID --request UUID --digest SHA256
workspace-pi-host inspect --instance UUID --request UUID --digest SHA256
workspace-pi-host stop    --instance UUID --request UUID --digest SHA256
                         --boot-id UUID --invocation-id HEX32
```

There is no arbitrary unit name, command array, systemctl option, polkit
manage-units grant, daemon or reset command. The application controller must
resolve trusted session authority before invoking it. Whole-unit stop needs
its own authorized operation; individual worker cancellation uses the verified
supervisor channel.

Lock order is application lifecycle/package/session admission, then the
instance `launch.lock`. Publication fsyncs the request and releases its
exclusive lock before calling the helper. The helper holds a shared,
nonblocking lock for `inspect` and an exclusive, nonblocking lock for `start`
and `stop`. Inspect calls can overlap. Publication, start and stop require
exclusive access. If the lock is busy, retry the same operation. The
unprivileged bootstrap never waits on it. Freeze the descriptor while any
invocation, job or Unknown outcome exists.

The helper atomically fsyncs a root-owned mode-0600 `receipts/host.json` before
the native start call. It is bounded to 1 MiB and retains every request; full
capacity refuses without pruning. Same-ID start never calls native start
again. It reconciles the recorded invocation or returns Unknown, including a
crash between intent publication and the manager call. `inspect` does not
write receipts or repair state. Responses expose native execution observations
to the private controller, with outcomes `starting`, `running`, `stopped`,
`rejected` or `unknown`. Running means native service execution, not application
readiness: the controller must validate the supervisor handshake independently.

Stop checks both boot and invocation immediately before requesting native
stop. Positive drain requires a retained exact cgroup/invocation, no native
job, a terminal loaded unit, and an empty or absent owned cgroup. The cgroup's
`populated` observation covers descendants. Failed observation, changed boot,
replaced invocation or survivors keep recovery held; absent unit metadata alone
does not prove termination. An asynchronous stop can need same-ID reconciliation.

Terminal replay returns its saved result before looking at the current
descriptor, so replaying an old stop cannot affect a successor. A new request
needs a new execution generation, the exact current terminal predecessor,
unchanged session scope and freshly checked drain proof. Application holds for
unresolved tool effects remain separate. No helper operation deletes scratch,
receipts or GC roots, and no unit auto-restart creates another invocation.
After a host reboot or uncertain stop, retain the evidence and use the
application's forward recovery; do not edit receipts to clear the hold.

## Verification and installation boundaries

The host module installs these locking rules in `workspace-pi-host`, with a
sudo rule for that exact immutable helper path. A Manager retains the helper
path resolved at construction. Installed verification must initialize a
Manager against the intended host helper and confirm that resolved path.

The generated `workspace-pi-host` and `workspace-pi-session-entry` wrappers
set `NIX_SSL_CERT_FILE` and `SSL_CERT_FILE` to
`/etc/ssl/certs/ca-certificates.crt` in their sanitized environment. The
session entry passes these settings to the supervisor, which forwards them to
workers. Existing supervisors retain their CA settings, including for workers
they start after a host update.

The source suite `nix develop --command ruby test/pi_runtime_host_test.rb`
uses disposable fake manager/process/package inputs and never controls a live
manager. `nix/tests/pi-runtime-host.nix` defines a disposable installed VM check:
two concurrent units, inherited process boundaries, host UID/groups/userns,
sudo authentication boundary, stale stop, native drain and scratch rebind.
Run installed checks only after the intended changes are committed and reviewed,
using the project's verification watcher policy.

Module evaluation and fake tests cannot establish the deployed host's tool
permissions. A real installation requires separately reviewed host
configuration, compatible application capability/authority fences and explicit
activation authorization. Install the host prerequisite before the dependent
user-profile application. No application or host deployment follows from
building these candidates. Forward recovery retains both backends' state and
the exact live package closures; an older incompatible package is not a
rollback strategy.

Cgroup accounting includes local descendants. Shared Nix daemon builders,
remote tools, VMs and the portal remain external. Scratch directory usage is
not a quota and can miss deleted-but-open allocations.
