# Pi worker conformance probe

This subtree records the finite P0 decision for adding opt-in Pi sessions. It
does not launch a usable worker, change the portal, select a session backend,
or install a service. Existing Codex execution remains unchanged.

The selected adapter is a **standalone SDK worker process for each loaded
lead/member**, beneath the eventual session supervisor. This preserves separate
worker processes. The SDK provides explicit canonical authentication and private
history/settings/resource injection. Stock RPC with a first-party extension also
works for the tested manual operations, but needs an authentication-file alias
and more extension/transport adaptation. Both alternatives remain in the test
fixture; neither is added to production packaging.

## Reproduce with pinned dependencies

[upstream.json](upstream.json) selects source revision
`a22b888a58fe55b668cc45b80749ec88d4eb4ddc`. Its own Nix, npm and installer locks
build the coherent CLI/SDK/dependency set. This is unreleased main source whose
manifests say 1.5.2, not an assertion of equivalence to release tag v1.5.2.
No ambient npm installation or patched Pi source is used.

Run the dependency build through the development verification watcher:

```sh
nix build --out-link /tmp/2026-11-11-pi-codex-migration-p0-dependency \
  --print-out-paths --print-build-logs \
  github:earendil-works/pi/a22b888a58fe55b668cc45b80749ec88d4eb4ddc#packages.x86_64-linux.default
```

Use its resulting store path for the focused API check, from this repository's
Nix environment. `package-api.mjs` loads public package exports from that output;
SDK child processes use the same Node executable embedded in the Pi CLI wrapper.
The harness uses only owned temporary homes, fake provider/authentication data
and `PI_OFFLINE=1`. It does not inherit the caller's real credential environment.
Keep the build's output link as a GC root until the probe and evidence handoff
finish. A successful `--no-link` build alone does not retain its dependency tree.

```sh
nix develop --command env PI_PROBE_PACKAGE=/nix/store/<built-pi-output> \
  node --test test/pi_runtime/probe.test.mjs
```

The separate finite rootless experiment is:

```sh
nix develop --command node test/pi_runtime/containment.mjs .
```

It creates two short-lived, uniquely named user units with a 20-second runtime
limit and five-second stop timeout. It makes no host configuration change. A
launcher timeout retains its exact unit and disk-root identity as Unknown;
inspect those identities before any cleanup. Successful finite commands finish
and join their children. Unit collection alone is not independent proof that a
cgroup became empty. The fixture is not a registered session service or a
replacement session supervisor.

## Supported API observations

The SDK fixture explicitly supplies `ModelRuntime.create({authPath,modelsPath})`,
`SettingsManager`, `SessionManager`, `DefaultResourceLoader` and custom tools.
Four loaded workers, a lead/member pair in each of two fake sessions, have
distinct process/session identities and private histories/settings/resources.
They share a canonical authentication directory and Pi's native credential
locking. A fake expired OAuth grant refreshes once across all four processes;
atomic replacement of the canonical auth file becomes visible without copying
credentials. No real provider login or network model request occurs.

The stock CLI comparison uses its supported RPC mode and first-party extension,
private `PI_CODING_AGENT_DIR`, explicit session storage and an `auth.json` symlink
to the canonical directory. Pi's native lock resolves the target. The test proves
that replacement is visible through the alias; it does not recommend a file bind
mount or independently copied rotating credential.

Both paths exercise send, streamed events, history, same-session model selection,
abort and restart. The SDK additionally exercises a different provider in the
same conversation, explicit versus non-persisted model defaults, trusted tool
binding, and process death after a tool effect but before its result. Mutable
settings changes stay local to the selected worker. RPC model selection does
not rewrite the worker's global model default.

## Readiness is three separate results

| Gate | P0 result | Remaining dependency |
| --- | --- | --- |
| Pilot adapter APIs | Fake SDK/RPC conformance passed; SDK selected. This is not a usable installed pilot. | Production caller/authority bindings, durable Unknown holds, capability refusals and H0 launch prerequisite must be implemented and reviewed. |
| Automation admission | Blocked for the tested ordinary prompt/RPC path. Tagged custom input is a separate, conditional composition candidate. | Fresh-session persistence and acknowledgement/restart proof beyond transport correlation. Keep dependent automated queue/team/fallback dispatch disabled until separately proved. |
| Launch containment | Rootless structural isolation passed; required host tool semantics failed. Launch-ready is not established. | Separate reviewed host-created namespace design/implementation; no unisolated fallback. |

The transport UUID is not passed as a native SDK request identity. RPC echoes
command IDs but does not persist them with native prompts. The tests lose an
acknowledgement, restart the worker, observe native history and verify the
transport UUID is absent. History can show work occurred; it does not prove
which stable external submission was accepted. The fixture does not resend
that operation. Production must retain Unknown rather than interpret missing
history as NotSubmitted or present a host outbox as Pi acceptance.

The bounded follow-up probes public `sendCustomMessage` with a custom type and
immutable submission ID/digest in `details`, using `triggerTurn`. This is not
equivalent to ordinary prompt correlation: tagged custom entries in an existing
persisted conversation can be inspected through public history. However a fresh
session initially persists only after a user or assistant message exists;
custom-only input does not meet that condition. A persisted custom tag therefore
cannot be assumed for the first input before inference. The fixture checks both
fresh and established conversations at the fake model boundary and preserves
Unknown on a crash. It does not inject a synthetic user prelude, write raw
JSONL, invoke private methods or claim native resume/deduplication guarantees.
These ordinary-prompt experiments also do not rule out a host witness binding
an immutable operation to an exact persisted native entry under exclusive
dispatch. That separate composition is not tested here; absence of a native
request UUID alone does not establish that supported admission is impossible.

A correlated explicit fixture refusal occurs before invoking Pi and can report
NotSubmitted. Completion of a manual call is evidence for that live invocation,
not a durable automation receipt. A tool effect before process death remains
Unknown even when the worker and transcript reopen. No arbitrary-tool
exactly-once claim follows from these checks.

## Rootless route and limits

The candidate is one user service with `Type=exec`, `Restart=no`,
`KillMode=control-group`, `PrivateUsers=yes` and `PrivateMounts=yes`. Owned disk
backing directories bind to `/tmp` and `/var/tmp`; `PrivateTmp` is unset so they
survive unit exit. Simultaneous session units get distinct mount namespaces and
cgroups, and child workers inherit their owning service's exact boundary. The
repository is readable and a local Nix daemon ping succeeds; daemon execution
is outside the workload's cgroup accounting.

On the tested host UID 1000 and GID 100 remain visible, but supplementary host
groups `[1,100,989]` appear as `[65534,100,65534]`. The exact host sudo wrapper
reaches password authentication outside the namespace; inside, it fails the
root-owner/setuid check before authentication. No password or privileged command
was executed. This does not preserve the required host tool semantics, so the
rootless route stops here. No alternative mapping, host helper installation,
privileged capability or host configuration modification is attempted.

Disk backing survival is proved only across finite unit exit, not supervisor
restart/rebind. These are Node child fixtures, not loaded Pi workers inside the
namespace; canonical-auth alias/locking checks run in the independent API
fixture. H0 must still verify combined launch, shared-auth directory access,
required tools, restart, cancellation and retained scratch with the actual
worker package. No installed integration or lifecycle acceptance is claimed.
