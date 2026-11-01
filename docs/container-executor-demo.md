# Container executor demonstration

This opt-in fixture checks whether one stock host Codex App Server can route
native tools to two stock exec-servers in rootless Podman containers. It uses a
local synthetic Responses backend and disposable state. It changes no installed
workspace settings or production code.

The fixture targets Codex 0.165.0 because it exercises that version's experimental
environment protocol. Compilation alone does not validate native tool dispatch;
run the integration command below to check it.

## Prerequisites and invocation

Run on a Linux Nix host with working rootless Podman, subordinate UID/GID ranges,
and an accessible `/nix/var/nix/daemon-socket/socket`. All selected executables
must already exist in `/nix/store`. The test does not pull or build an OS image,
rebuild Codex, create a development session, or contact a real model service.

Select the exact native executable, Bash, Git, Nix, Podman and its helper directory.
The following example uses the already installed outputs from the demonstration
host; other hosts must select their own existing outputs. `DEMO_CODEX` must remain
the raw 0.165.0 executable, rather than a launcher that injects live configuration.

```sh
export DEMO_CODEX=/nix/store/2rqwqchjlv6l2nw5l491pkhvrsijn50c-codex-package-0.165.0/libexec/codex/bin/codex
export DEMO_BASH=/nix/store/pwxj9ycwqd117yra6czifh3xfx5p88lh-bash-interactive-5.3p9/bin/bash
export DEMO_GIT=/nix/store/b1wn5clix1mz4vpfih7x9cnj9qfh7c6h-git-2.54.0/bin/git
export DEMO_NIX=/nix/store/z4wkkj3ncr69vgrb3020saqx1c8mfsjj-nix-2.34.9/bin/nix
export DEMO_PODMAN=/nix/store/3cgzffw8krnknnhnvz0b0fic9l9210zv-podman-5.8.8/bin/podman
export DEMO_PODMAN_HELPERS=/nix/store/8d6x0a7kc18vjjxg8swf0n81l5v8z4yp-podman-helper-binary-wrapper/bin
export CONTAINER_EXECUTOR_DEMO_LOG_DIR=/absolute/owned/verification-directory
export CONTAINER_EXECUTOR_DEMO=1
nix develop --command go -C portal test -mod=readonly \
  -tags=container_executor_demo ./cmd/workspace-portal \
  -run '^TestContainerExecutorDemo$' -count=1 -v -timeout=10m
```

Run the container integration only after independent review of the complete
committed fixture and quick checks. The fixture has a five-minute context and
bounded operation waits; the Go command has a ten-minute hard stop. Stop on the
first capability failure and retain the output instead of switching engines,
rebuilding native code or weakening routing assertions.

The cheap compile/skip check does not launch native or Podman:

```sh
env -u CONTAINER_EXECUTOR_DEMO nix develop --command go -C portal test \
  -mod=readonly -tags=container_executor_demo ./cmd/workspace-portal \
  -run '^TestContainerExecutorDemo$' -count=1 -timeout=45s
```

Capture the integration command's stdout/stderr in the owned verification
directory. The test prints its exact evidence subdirectory and native identity.
`native.log` captures native stdout/stderr; failed turn checks retain the last
RPC status and synthetic tool output in the test output. Backend responses
contain only fixture data. No real credentials are loaded, copied or refreshed.

## Execution boundary and assertions

The host process gets a private HOME, CODEX_HOME, configuration, native database,
socket and environment whitelist. Its only model provider is a loopback synthetic
Responses service with a dummy key. Optional plugin, remote-plugin and app
discovery is disabled. Threads use ordinary persisted history inside that private
home so the existing completion-history RPCs work. The whole home and database
are removed with the fixture. Each container has a minimal directory rootfs,
private writable home/cache/temp paths, a private working tree, no network and no
host home or container-control socket. Namespace root maps to the unprivileged
operator. The fixture uses a separate Podman store, runroot and runtime directory.
Read-only rootfs/store mounts are enforced by Podman; native tool sandbox policy
is `danger-full-access` within that outer container boundary.

Private `environments.toml` declares two stdio commands that start the containers
and run `codex exec-server --listen stdio`. `default = "none"` and
`include_local = false` prevent implicit host execution. Every successful thread
is explicitly bound through experimental `thread/start.environments`.

The synthetic backend emits real native tool calls. The test checks:

- `exec_command` reads a different marker at the same absolute path in each
  container and writes only that container's file. `write_stdin` completes a
  native interactive command in the first container.
- Freeform `apply_patch` modifies the selected container's file. `view_image`
  returns that container's red or blue image to the backend. Host and sibling
  sentinels catch accidental routing to the wrong root.
- Containers have separate mount, PID and user namespaces from the host.
- `/nix/store` is readable and rejects writes. The exact host daemon socket is
  the shared-service exception: the only requested daemon operation is
  `nix store ping --store daemon`. A read-only socket bind does not make the
  daemon's protocol read-only; this fixture does not establish a sandbox for
  arbitrary Nix daemon clients. It performs no build, GC or store mutation.
- A newly created disposable bare Git repository is shared writable. Each
  private Git worktree has a distinct absolute path, identical in host metadata
  and its container. Native commands write private files and an object to the
  shared bare repository; the host verifies the object. No canonical repository
  or global Git prune/GC is used.
- An unknown environment and the loss of the second owned executor reject or
  wait within a bounded call. Neither may execute on the host or sibling. The
  backend emits each requested call once and never replays it onto a new target.

The fixture joins its host App Server, removes only exact owned container names
from its private Podman store, and verifies no containers remain there before
removing its temporary root. That root is created directly beneath `/tmp` so a
Nix shell's temporary-directory cleanup cannot remove recovery state. If teardown
is uncertain it retains that root and reports its exact path. Evidence remains in
the selected log directory. This
cleanup never changes development-session lifecycle state or stops live services.

## Limits and follow-up gates

The optional cold-resume release gate extends this same disposable fixture.
Set `CONTAINER_EXECUTOR_COLD_RESUME=1` in addition to the invocation above. After
the baseline tool assertions, it restarts only its private host App Server with
the same HOME/database and static environment configuration, resumes the alpha
thread, and attempts a native sentinel command without a later turn rebind.
The thread must already expose the alpha environment and execute only there.
Missing binding or wrong-root execution writes `cold-resume-gate.json` with
`supported: false` and `releaseGate: "FAIL"`, then fails the Go test. A reproduced
native limitation is a failed product gate, not a passing release test.

This is the prerequisite to proving queue/goal continuation ordering. If it
fails, stop before expanding fork, child, dynamic registration or mixed-mode
cases: those cannot repair a missing cold binding. The selected protocol exposes
environment selection on thread/turn start, but not thread/resume; its generic
resume config bag has no supported per-thread execution-environment selector.
No global-default swap or post-resume rebind is substituted. Actual results are
recorded per run; source inspection is not a passing native test.

The fixture exercises execution without distributing subscription credentials to
executors. It does not validate production OAuth, subscription refresh, an
authenticated external provider, host MCP routing, native child inheritance,
terminal placement, cgroup attribution, pressure handling or discard policy.
Fixture data is small; existing read-only Nix store paths are not copied.

The baseline does not test resume or fork. Cold native resume can release queued
messages or goals before a client rebinds the next turn, and `thread/resume` has
no direct environment selector in this protocol. Production recovery must restore
the persisted environment before automatic work is admitted, or defer activation.
Default-none/local-disabled is necessary but does not prove safe recovery.
Maintenance-only admission with no queue/goal release is a separate future gate.

The environment API is experimental. A successful run validates only the selected
binary and asserted tools; it does not establish universal tool routing or
mixed-version compatibility. No persistent format, installed profile or runtime
policy changes are part of this demonstration.
