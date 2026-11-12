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
