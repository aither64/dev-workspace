# Host backend identities and readers

`agentbackend` is a standard-library-only identity leaf. Native adapters can
consume its session, conversation and execution references without importing
the session store or either native runtime. Its typed errors distinguish absent,
unsupported, conflicting, unready and stale evidence through `errors.Is`.

The adjacent `session` package owns the read APIs:

- `BackendBindingStore.Load` reads Pi binding schema 1 from
  `<UserStateRoot>/session-backends/<full lowercase workspace SHA256>/<slug>.json`.
  The hash covers the canonical workspace's UTF-8 bytes. The durable binding
  selects Pi even when runtime files are absent.
- `LoadPiRuntimeAuthority` reads Pi projection schema 2 from the existing
  `<AuthorityDir>/<slug>.json`. `ValidateBinding` checks the durable tuple and
  revision. A live projection is derived evidence, not the backend selector.
- `SelectBackend` consumes typed, validated host evidence. Existing Codex
  readers remain responsible for legacy validation. Positive legacy evidence
  selects Codex; absence alone does not. An orphan Pi projection, mixed legacy
  and Pi evidence or a mismatched durable tuple refuses selection.

Readers require private owner-UID mode-0700 directories and mode-0600 regular
files, bounded strict JSON and exact identity fields. Only genuine missing state
wraps `ErrNotBound`; callers must propagate other loader errors before selection.
Reads create no directories, records, locks or identities and change no bytes.

`BackendBinding.Resolve` requires a ready binding and addressed conversation.
`PiRuntimeAuthority.ValidateHandle` additionally checks a ready projection, the
current generation and the exact stable conversation reference. Session and
conversation incarnations survive worker restarts; execution generations do not.
Submission identity uses the stable reference plus the original action, input ID
and payload/options digest. A new generation cannot authorize resubmission of an
unknown request. These types do not implement a submission ledger or establish
native acceptance.

Selection and handle validation grant no roster permission, native history
ownership or execution authority. Callers must separately verify current host
generation, lifecycle/admission, member authorization and native process proof.
A creating conversation cannot dispatch, whether its native ID is absent or
already allocated. No reader advertises creation or execution support.

This reader boundary preserves schema-1 Codex bytes, receipts and recovery.
It supplies no writer, converter or migration. Pi creation and writes require
the separate durable package/stale-wrapper fences and creation-receipt ordering;
schema-2 reader refusal alone cannot prevent an older blind writer overwriting
runtime state. No live profile, lifecycle or native operation is needed to check
this boundary. Run its disposable fixtures from the repository Nix environment:

```sh
nix develop --command go -C portal test -mod=readonly \
  ./internal/agentbackend ./internal/session
```

The host/CLI guard continuation reads the same durable binding namespace through
`libexec/workspace-session-backends.rb`. Legacy writers, lifecycle commands and
web recovery/creation paths refuse Pi evidence before native or tracking effects,
and repeat the decisive proof after waiting for admission. Host preparation,
activation and restoration scan durable bindings as well as accepted creation
receipts and retained journals; loss of runtime files does not clear that gate.
Existing lock order and generation checks remain in place.

The runtime contract advertises binding schema 1/policy 1, authority schema 2 and
`piCreationSchema: 4`. The guard validates the complete schema-4 creation receipt,
immutable request, journal and completion shapes, including their agreement with
the durable binding. The guard refuses incomplete or contradictory records;
matching target metadata alone cannot make a Pi-bearing transition succeed. A binding with neither
retained creation fence is a conflict. See
[Pi creation package compatibility](../../../docs/pi-creation.md#package-compatibility)
for the retained-record and target-package requirements.

Predecessor packages still reject schema-4 records before their ready-state skip,
including candidate activation preflight. The actual ready journal, immutable
request and completion proof remain required after receipt-cache retirement.
Keep those records for retained Pi dependencies; a metadata key or sidecar cannot
retrofit old binaries. Compatibility checks neither repair state nor grant native
dispatch. Recovery stays with the original creation and receipt owner.

Optional trusted startup configuration forwards `DEV_WORKSPACES_PI_AUTH_DIR`
verbatim through `--pi-auth-dir` to `web.Config.PiAuthDirectory`. There is no
default, credential read or login at startup. This field grants no launch
support; a Pi adapter must validate its configured directory before loading a
worker. Missing or invalid Pi configuration must leave Codex available.
The launcher state root is also explicit: `DEV_WORKSPACES_PI_HOST_STATE_ROOT`
passes through `--pi-host-state-root` to `web.Config.PiHostStateRoot`. It has no
default and does not alter the frozen H0 launch request.

Run Ruby fixtures through `test/workspace_host_test.rb` or
`test/dev_session_test.rb`, using Minitest's `--name` filter for focused cases.
Individual component files rely on fake classes loaded by those suite entries.
For tmux fixtures, use `nix develop --command env TMPDIR=/tmp ruby ...`: the
extra Nix shell temporary-directory prefix can exceed the Unix socket path
limit. Each fixture still creates and removes its own disposable directory.
