# Workspace portal

The workspace portal keeps long-running development sessions available through
both a browser and tmux. One installed package generation supplies the portal,
Codex CLI, session helper, user units and any configured extensions.

## Workspace configuration

Each workspace can contain `.dev-workspace.json`:

```json
{
  "schema": 2,
  "displayLabel": "Example development",
  "hostLabel": "build-host",
  "sshHost": "build-host.example.test",
  "portal": {
    "hostname": "workspace.example.test",
    "aliases": ["old-workspace.example.test"]
  },
  "developmentClusterProviders": ["service"]
}
```

The portal hostname and aliases are configuration, not application constants.
`workspace-host register` reads them when no command-line override is supplied.
The registry stores the resolved values so routing remains deterministic until
the next explicit registration update.

The canonical hostname remains the portal's published base URL. At service
start, `workspace-host` passes only the registered aliases as additional exact
HTTPS origins. The portal permits browser conversation, upload and mutation
requests from that fixed set; it never derives trust from a request Host or
Origin header.

Provider IDs must exist in the immutable package extension catalog. Workspace
files cannot choose executable paths or labels.

## File links

Absolute file links in conversations and Markdown artifacts open a read-only
source viewer. The viewer shows line numbers and syntax highlighting. A link
ending in `:10`, `:10:5` or `#L10` selects line 10. Clicking a line number updates
the address; **Copy link** copies that address. Links without a line reference
open the beginning of the file.

Active repository links show the current worktree contents, including
uncommitted edits and newly staged files. The file must be tracked in a verified
session repository. After archival, the viewer reads the recorded final commit
and displays its revision. A link is a live reference, so later edits can move
the referenced line. The viewer reports a missing line instead of selecting a
different one.

Tracking files are limited to `plan.md`, `state.md` and artifacts declared in
`portal.yml`. Their links continue to work when tracking moves into `archive/`.
Untracked repository files, Git metadata, symlinks and other host files are not
available. Text previews are limited to 512 KiB and 12,000 lines. Markdown files
appear as source; binary files receive a notice instead of a text preview.

The viewer route is `/files/<slug>?repository=<id>&path=<relative-path>#L10`,
or `/files/<slug>?artifact=<relative-path>#L10` for a tracking artifact. The
repository ID is the same opaque ID used by repository review. The read-only
`GET /api/sessions/<slug>/file` endpoint accepts the same query parameters and
returns `path`, `repository` when applicable, `source`, the archived `revision`
when applicable, and a `content` object with the existing review preview fields.
Source values are `worktree`, `archive`, `artifact` and `archived-artifact`.
Existing URLs whose website path contains the absolute workspace file path
redirect to the viewer. The original conversation text remains unchanged.

## User-profile state

The default paths are:

- registry: `~/.config/dev-workspaces/registry.json`;
- installed profile and retained Codex roots:
  `~/.local/state/dev-workspaces/`;
- per-workspace sockets and runtime authority:
  `$XDG_RUNTIME_DIR/dev-workspaces/<name>/`;
- router socket: `/run/dev-workspaces/router.sock`.

Profile switches are forward-only. `workspace-host rollback` refuses to select
an older package; recover by repeating the same switch or selecting a newer
package. Commands waiting on a transition lock reject a generation change and
must be run again.

## Extension catalog

Downstream flakes call `dev-workspace.lib.mkPackage` with one `extensions`
attribute set containing three fields:

```nix
extensions = {
  commands = { };
  skills = { };
  clusterProviders = { };
};
```

The fields are:

- `commands`, mapping command names to absolute executables;
- `skills`, mapping skill names to package directories;
- `clusterProviders`, mapping provider IDs to a label and helper executable.

All values that name files or directories must be immutable Nix store
references. The resulting package writes `share/dev-workspace/extensions.json`.
Activation uses only this catalog when linking commands and skills. Workspace
configuration is always read from the registered root's `.dev-workspace.json`.
Cluster helpers receive
the selected workspace through `DEVCLUSTER_WORKSPACE` and keep ownership of
their `status`, `reset`, `cleanup-paths` and `transition-adopt` protocols.

The package constructor also accepts `userNamespace` and `routerSocket` for a
deployment-specific compatibility generation. `userNamespace` selects the
default user config, state, runtime and profile paths, including portal
lifecycle receipts and deletion recovery, without changing the generic command
or environment-variable names. Keep the defaults for new deployments; use
overrides only while an existing installation is moving its state to the
generic namespace. `activationEnvironmentAliases` can name a
validated legacy activation marker when the installed generation must invoke
that compatibility package. The selected values are recorded in
`share/dev-workspace/package.json` so a deployment-specific migration can
verify both sides of a cutover before moving state.

## Direct Codex thread teams

The Team tab and `dev-session team` control real, independent Codex threads.
The normal session conversation is always `lead` and remains the one available
through `dev-session attach`; member threads do not get tmux panes. Adding a
member creates its persistent Codex thread immediately, before it receives its
first assignment. Members use compact addresses such as `architect0`,
`implementer0`, and `reviewer0`. Addresses are never reused or renumbered.

The roster is a private, workspace-and-session-scoped record. It contains only
the root identity, member addresses, thread IDs, lifecycle state, and desired
next-turn model and reasoning settings. Message history stays in each Codex
thread. A team message resolves both addresses through the same roster, and the
member transcript API accepts a roster address rather than a raw thread ID.
There is no global message bus or cross-session member discovery.

The installed workspace policy supplies three starting teams: Solo (`solo`),
Full team (`delegated`), and Lead-designed team (`lead_designed`). Their role
lists and exact lead model and reasoning effort appear on the new-session
screen. Full team has `lead`, `architect0`, `implementer0`, and `reviewer0`;
Lead-designed team omits the architect. The selected team and its member
settings are saved with the creation receipt. Member threads are created before
the lead receives the initial request. A failed creation can be retried with
that saved selection.

After creation, the Team tab can add, remove, configure, assign work to, and
inspect the messages of an individual member. Model and reasoning settings
apply to the member's next turn. Removed members stay in a collapsed history
section. The Codex tab can switch between `lead` and ready members to show
each complete conversation; direct messages use the member's current role
policy and model settings. An assignment to a busy member uses App
Server's ordinary turn-steer behavior. Members send results and blocking
questions to `lead` through the package-owned `report_to_lead` tool. Each
message has a unique ID that the member reuses only if delivery needs a retry.
The member's work remains in its own transcript.

The new-session form labels presets by size and role counts, such as
`Full team (4): 1 lead, 1 architect, 1 implementer, 1 reviewer`. It also shows
a copyable `dev-session start` command that tracks the selected name, team,
model, and reasoning effort. From an interactive terminal, that command asks
for the initial request; portal uploads are not transferred to the CLI.

Removing a member is permanent for that roster address: archive/revive and
fork retain it as removed rather than making it active again. Session lifecycle
operations and team mutations share the runtime lock, so an active lifecycle
transition rejects an add, removal, configuration change, or assignment.

The CLI uses the same record:

```sh
dev-session team list example
dev-session team preset example delegated
dev-session team add example architect --model gpt-6-sol --effort xhigh
dev-session team configure example architect0 --model gpt-6-sol --effort high
dev-session team assign example --to implementer0 --message 'Implement the approved plan.'
```

Forking materializes a matching member thread for every source member. Archive
and delete archive member threads with the root; revive unarchives them. The
retired virtual-team ledger is ignored for existing idle sessions, which retain
their root conversation and begin with an empty direct roster.

## Installed team policy

An optional `teamConfig` in `lib.mkPackage` builds the installed team
catalog. The catalog supplies named presets and exact model, reasoning,
behavior, and access defaults; it is not a virtual-team dispatcher. The
package records its catalog digest, and the host launch marker binds it through
the registration-plan digest. No native child-agent capacity or per-role startup
arguments are required for direct threads.

At creation, the selected preset is copied into the private schema-3 receipt
and durable roster. That snapshot includes each member's address, model,
reasoning effort, behavior, and access. Retrying creation uses the snapshot,
even if a later package has different defaults. Team mutations remain
unavailable until the creation receipt is ready. If a member's App Server
start has an uncertain outcome, retry reconciles its registered App Server
project and refuses to start another thread when the result is ambiguous. Each
member's project is created with a durable idempotency key derived from the
workspace, session, lead thread, and member address. The roster stores the
project ID returned by App Server before attempting `thread/start`. A lost
`project/create` response can be retried with the same key; a lost
`thread/start` response is resolved by listing threads in that project. If
the project lookup or thread listing is unavailable, creation remains pending.
Fresh headless threads have no rollout until history is written, and App Server
can unload them when the creating client disconnects. Before marking a member
ready, the runtime injects one internal developer bootstrap item and verifies
its exact marker in that thread's rollout; this starts no model turn. An
uncertain injection is reconciled against that marker, never blindly repeated.
Fork destinations receive the same bootstrap before becoming ready.

The runtime applies the retained role instructions to each member thread.
Architects and reviewers use a read-only sandbox; implementers use
workspace-write. Ordinary portal reads leave the thread's persisted
instructions alone. Assignments reapply the retained policy and use a stable
message ID for retries. The browser retains an uncertain assignment ID across
reloads in the current tab until the request is confirmed. In the CLI, keep
the ID shown after an uncertain assignment failure and pass it with
`--message-id` when retrying.
Retry the same ID within the same installed package generation. A package
switch can change the retained assignment text or tool path, so an earlier
uncertain ID may be rejected rather than silently replayed. If that happens,
inspect the member transcript to determine whether the assignment arrived.
Send a new assignment with a new ID only after confirming it did not; if the
outcome remains uncertain, stop and investigate before resending.

The report tool is bound to the workspace, session, lead thread, member address,
and exact member thread. A new or forked thread receives the tool after its ID
is known. Before each assignment, App Server resumes the member thread with
that binding, including for members created by an earlier package. No roster
format change is needed. The helper checks the current roster identity before
sending. A missing or mismatched binding fails without sending to `lead`.
The helper exposes only `report_to_lead`. It passes the report through stdin to
the installed session command, which checks that the lead thread still belongs
to this session under the session lock. Neither command puts the report text in
its process arguments. Use a fresh message ID for each report, and reuse that
ID only when retrying delivery of the same report.

A fork requires all source members to have finished creation, preserves the
source member snapshot, and refuses an interrupted retry if that roster
changes. Because App Server forks have no project identity,
an uncertain member-fork response is not retried automatically. Archive,
revive, and delete follow the root session lifecycle. Session quiescence checks
the lead and every ready member before archive or a package transition;
deletion moves the retired roster into private recovery storage so the slug
can be reused. Existing idle
virtual-team sessions keep their lead
conversation; the old virtual ledger is not read to construct a direct roster.
Operators can add direct members after those sessions restart.

The catalog defines a verification watcher separately from the team. The
site selects its model and effort for each fresh, operation-scoped subagent;
it is not a persistent team member.

## Host module

`nixosModules.host` configures the privileged nginx and TLS substrate. Its
defaults use:

- `/var/lib/dev-workspaces/password/password`;
- `/var/lib/dev-workspaces/auth/htpasswd`;
- `/var/lib/dev-workspaces/pki`;
- `/var/lib/dev-workspaces/tls`;
- `/var/lib/dev-workspaces/public/ca.pem`;
- `/run/lock/dev-workspace-substrate.lock`.

The local operator is trusted to administer the development host. The
root/user split assigns nginx and activation files to their system lifecycle;
it does not contain a compromised operator. Remote requests still require
input validation, authentication and authorization. This trust assumption does
not extend to projects developed in the workspace or their guests.

Persistent outputs stay below `/var/lib`, runtime paths below `/run`, and the
lock file directly below `/run/lock`. Configured output directories must be
distinct and mutually non-nested. Reconciliation refuses malformed or symlinked
configured files and directories. The `tls/current` generation link is intentional.
It serializes writers with `flock`, establishes expected owners and modes, and
publishes generated files and complete TLS pairs atomically. It does not inspect
physical ancestor relationships or attempt to defend against hostile local
mounts and hardlinks.

The shared password remains `root:workspace-portal-owner` with mode `0640`,
and the CA key remains `root:root` with mode `0600`. These defaults keep their
existing lifecycle and reduce accidental changes. The nginx group can read the
generated htpasswd and leaf key. Reconciliation preserves the CA and password,
checks certificate/key pairs and requested names, and verifies leaves using
only the local CA. A weekly system service renews certificates and reloads
nginx. Retained PKI state, TLS generations and migration markers are not removed.

The firewall stays closed unless `services.dev-workspaces.firewall.sourceRanges`
contains an allowed network.

## Deployment order

Deploy the host module before registering a workspace. Then build and select the
user-profile package, register the workspace, and verify the router and portal
units. A package transition must contain compatible cluster and runtime-authority
contracts whenever persistent state exists.

Namespace migrations are deployment-specific. Stop writers, preserve state
bytes and metadata, rewrite machine-consumed paths as one journaled operation,
then start the new generation. Rollback requires the reverse migration; old and
new namespaces must not run together.

## Development

Run the quick checks with:

```sh
nix flake check --no-build --show-trace
```

Run the complete suite with:

```sh
nix flake check --print-build-logs
```

The NixOS smoke test in `nix/tests/host-module-idempotency.nix` covers
activation, a stable rerun, nginx authentication and proxy access, renewal,
interrupted publication, malformed state, concurrency and configuration
rollback. CI runs it with KVM on `master` and manual dispatch. Dispatch feature
VM tests after mandatory review; package and focused checks run on every push
and pull request. Keep normal dependency-update CI below the 20-minute target
and inspect cold-run timings when changing its workload.

### Cluster provider status and shutdown contract

`runtime-contract.json` publishes the provider contract. A successful `status`
command exits zero and prints its validated JSON state. Exit code
`clusterProvider.statusBusyExitCode` (75) is reserved for a concurrent transition:
providers return it promptly without a status document. The portal keeps the
last valid state and displays a local update notice. Other failures indicate
unavailable status. Readiness and credentials are replaced by the next complete
status response, including an empty response after reset.

The portal gives a release/reset operation
`clusterProvider.releaseTimeoutSeconds` (180 seconds). Providers must budget
guest shutdown, forced reaping, runner cleanup, and command completion within
that ceiling. Providers own their internal shutdown budgets and test them against
the runtime contract. These fields are additive package metadata; they do not
change persisted cluster state or its transition policy.
