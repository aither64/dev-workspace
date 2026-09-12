# dev-workspace

`dev-workspace` manages persistent Codex development sessions. It provides the
session CLI, browser portal, user-profile runtime, workspace registry and an
extension interface for development-cluster helpers. The package pins
its own Codex build, so one profile generation always contains a tested portal
and App Server pair.

The repository was extracted from a larger development-workspace application.
Session records and local worktrees remain in each configured workspace.

## Nix interfaces

The flake exports these packages on `x86_64-linux`:

- `dev-workspace` contains the portal, lifecycle commands, user units and Codex
  runtime.
- `workspace-portal` and `workspace-host` are compatibility aliases for
  `dev-workspace`.

`nixosModules.host` configures a host-side nginx proxy, generated basic
authentication and a local certificate authority. It does not install or run
the user application. The firewall remains closed unless
`services.dev-workspaces.firewall.sourceRanges` contains an allowed network.

The local operator is trusted to administer the development host. Root owns
nginx credentials, TLS keys and activation state for their system lifecycle.
The application, sessions and repositories belong to the operator. This split
does not provide a security boundary against that operator; remote clients
remain untrusted.

Persistent state paths remain below `/var/lib`, the router socket below `/run`,
and the reconciliation lock directly below `/run/lock`. Configured output
directories must be distinct and mutually non-nested. Reconciliation checks
final output types, expected ownership and modes, preserves the CA and shared
password, and replaces generated files and TLS pairs atomically under one lock.
It validates leaf certificates against the local CA and configured names.
Weekly renewal keeps the nginx endpoint usable between deployments. Existing
PKI state, certificate generations and migration markers remain available for
rollback.
See `nixosConfigurations.example` for a minimal configuration.

Install the user-profile application from a checked-out source:

```sh
nix run .#workspace-host -- switch --source "$PWD"
```

After the first installation, run updates through the stable command from the
selected profile:

```sh
workspace-host switch --source "$PWD"
```

The switch command retains the previous profile generation and its Codex
runtime. Use `workspace-host rollback` to return to that pair.

## Workspace configuration

The package can be extended through `lib.mkPackage`. Extensions supply immutable
command, skill and cluster-provider catalogs. Workspace configuration selects
provider IDs; it cannot supply executable paths.

A workspace declares its portal identity and optional providers in
`.dev-workspace.json`:

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

Without this file, the portal uses generic labels, hides the SSH attach command
and enables no development-cluster providers. Registration flags can override
the configured hostname and aliases; the registry stores the resolved values.

## Codex limits

The portal sidebar shows the main Codex allowance, with percentage remaining
and reset times in the browser's local timezone. The weekly and 5-hour windows
appear only when Codex reports them. Model-specific allowances are excluded.
On narrow screens, select the compact limit indicator to open the details.

The session-independent `GET /api/codex-limits` endpoint reads the configured
App Server account and returns `windows` plus an `updatedAt` snapshot time.
Each window contains `usedPercent`, `windowDurationMins` and nullable `resetsAt`
(Unix seconds). Reads share a 30-second server cache. Open pages refresh every
minute while visible and when focused. A failed refresh keeps prior values
marked with their last update time.

## Session creation

Creating a session, forking a conversation, or implementing a plan in a new
session opens the destination page immediately. That page shows initialization
progress and elapsed time. Initialization continues when the page is closed.
After an interrupted attempt, use its retry button to continue the recorded
request. A plan session keeps the exact accepted plan and model settings.
Conversation controls become available when initialization is complete.

Completed creation receipts are kept in a bounded retry cache. When the cache
fills, the oldest completed receipts retire; unfinished attempts remain available
for recovery. An existing destination still prevents a later request from
replacing that session.

## Repository review

The Repositories tab lists local feature commits, including commits that have
not been pushed. Expand a commit message to read its body, select its subject
to review the commit, or choose **Review branch** for the whole comparison.
GitHub links and workflow status remain available separately.

The file list scrolls to each file's diff. Split view is the default; the portal
remembers the selected layout. Split view numbers both versions. Unified view
numbers the current file and shows deleted text in separate unnumbered blocks.
Unchanged sections can be expanded. Binary files, submodules, symlinks, mode
changes, and missing final newlines retain their metadata. Text previews are
limited to 512 KiB and 12,000 lines per version.

Active comparisons use the merge base with the locally available default branch.
Opening a history or branch comparison saves its exact base and head. After
integration or archival, the portal reuses the saved comparison for that exact
head. When no saved comparison exists, the original recorded base is labelled
as a fallback; it can include upstream commits after a rebase. An open review
keeps its revisions until explicitly refreshed.

Git supplies immutable objects through a bounded local reader. Read-only
CodeMirror Merge editors are loaded as needed from the portal's own assets.
The complete npm dependency graph is locked in `portal/review-ui/package-lock.json`;
Nix builds the bundle and includes dependency versions and license notices.

## Transcript controls

Every message and activity entry has a copy button beside its timestamp in
the bottom-right footer. Messages copy their original Markdown; commands copy
the command and full available output. File changes include paths and patches.
Collapsed entries copy all loaded content, and streaming entries copy the
content available when selected.

## Development

Run the package checks and host smoke test with:

```sh
nix flake check --print-build-logs
```

The package uses a Go portal, Ruby lifecycle helpers and shell-based cluster
launchers. The flake supplies their build and test dependencies.

CI runs package and focused checks on every push and pull request. On `master`
and manual dispatch, it also runs a small NixOS VM test for activation,
idempotency, nginx authentication, renewal, recovery and rollback. Dispatch
feature-branch VM tests after mandatory review. The VM job requires KVM
acceleration. Review cold-run timings when changing these checks; the target for
a normal dependency update is under 20 minutes.
