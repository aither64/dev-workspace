# Pi session host launcher

The optional NixOS host prerequisite gives each Pi development session one
system-service cgroup and exact mount namespace. The service runs one
supervisor as the configured workspace owner. The supervisor, loaded workers,
their tools and supervisor-started resources inherit this boundary. Services
launched with `dev-session service run` use separate systemd user units outside
it; see the [temporary-service path requirements](dev-sessions.md#temporary-services)
in `dev-sessions.md`. The portal remains outside it.
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
  requests/<request-uuid>/
    launch.json
    runtime-config.json
    package-root
  control.sock
  tmp/
  var-tmp/
```

The controller owns launch publication, the retained application GC root,
session authorization and profile admission. The initial request uses the
instance `package-root`. Each successor uses its own
`requests/<request-uuid>/package-root`, which must resolve to that request's exact
application package. A missing or mismatched successor root causes refusal;
the launcher does not fall back to the initial root. Controller admission and
the service entry use the same rule. The initial root remains unchanged for
retained history.

Scratch and credentials are separate: the host module leaves the canonical
writable credential directory visible, including its locks and atomic file
replacements. It never copies credentials.

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

An explicit retry of the original creation request may resume its saved initial
launch only when the helper returns a strictly validated `unknown_request` for
that exact instance and request. The creation owner first checks the unchanged
pre-native journal and binding and the absence of runtime authority. Under the
runtime lock, the Manager verifies the frozen launch, runtime configuration and
retained package root, then calls `start` once with the same request identity.
Ordinary `Ensure` and `Inspect` do not resume saved launches. A sudo refusal,
missing socket, cancellation or uncertain host intent does not prove absence.
The helper atomically checks and records intent before starting the native
service; a concurrent same-ID intent cannot cause a second native start.

After a controller-only forward update, recovery validates the selected portal
package separately from the original launch package. It retains the original
application path and profile token and requires byte-identical runtime, worker,
model, provider and capability files, with matching frozen team catalog and
instructions. Captured model and effort overrides remain unchanged. The portal
must still match the current selected profile. Missing, malformed or different
inputs keep recovery held. The launch stays unchanged and continues to select
the original package's supervisor and worker.

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

## Resume a retained successor before host intent

A failed upgrade can leave a successor saved in `starting` after its
predecessor has stopped, before the host records a start intent. The upgrade
owner may resume that launch only after a strictly decoded `unknown_request`
for the same successor. It verifies the saved request, predecessor generation,
frozen launch and configuration, package roots, unloaded members and settled
effects, then obtains fresh predecessor drain proof. Unknown or malformed
replies, sudo errors, a live predecessor or unproved drain keep recovery held.
A recorded host intent permits observation only, never another native start.
Ordinary `Ensure` and `Inspect` do not resume launches.

When the selected package cannot perform this continuation, use a corrected
candidate controller without selecting its profile. In the command below,
`B` is the candidate package and the host profile still selects the frozen
supervisor package `A`.

First install the corrected host helper and service entry together through
the normal host configuration, including the exact helper sudo rule and unit
`ExecStart`. Restart the existing portal service using the same selected
application package so its Manager resolves the new immutable helper path.
Leave the selected user profile and native session services unchanged.

Supply the same trusted paths used by the ordinary serving configuration:

```sh
"$B/bin/workspace-portal" pi-runtime resume-successor-start \
  --workspace "$workspace" --workspace-name "$workspace_name" \
  --user-state-root "$user_state_root" --authority-dir "$authority_dir" \
  --host-profile "$host_profile" --transition-lock "$transition_lock" \
  --registration-marker "$registration_marker" --codex-socket "$codex_socket" \
  --pi-auth-dir "$pi_auth_dir" --pi-host-state-root "$pi_host_state_root" \
  --slug "$slug" --request "$saved_request_uuid" \
  --expected-generation "$predecessor_generation"
```

The command loads `A`'s installed catalog and runtime configuration and
uses ordinary transition and session admission. It checks that the saved target,
profile identity and ready binding still match. It creates no portal server or
background workers. JSON output identifies the executing controller, selected
package and observed runtime status. Success covers the existing successor's
host start and handshake only: it does not attach host callbacks, load members
or publish runtime authority. If startup remains pending, inspect the same
retained request; an Unknown or held result requires diagnosis without changing
its identity or replaying input.

Once the handshake is ready, complete the same saved upgrade through the
ordinary `pi-runtime upgrade` CLI or HTTP route. That operation attaches host
callbacks, loads the retained members and publishes authority after verifying
the required proofs. It preserves the original native IDs, history, settings
and receipts. Keep the original upgrade UUID and package roots, and never
resend the initial input to recover an upgrade. Keep the corrected host
installed until the retained operation is settled; rolling it back early
reintroduces the successor-root refusal.

## Verification and installation boundaries

The host module installs these locking rules in `workspace-pi-host`, with a
sudo rule for that exact immutable helper path. A Manager retains the helper
path resolved at construction. Installed verification must initialize a
Manager against the intended host helper and confirm that resolved path.

The Pi template sets `restartIfChanged = false`, which renders
`X-RestartIfChanged=false`. A normal NixOS switch installs a changed entry
without restarting active Pi instances, including when the incoming template
first adds this flag. Only an admitted runtime-controller operation replaces
an invocation. Explicit stop and template removal remain outside this guarantee.

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
