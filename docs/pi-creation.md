# Pi creation authority

Pi creation accepts an explicit new-session request with `backend: pi`. It has
its own durable identity and recovery contract. Existing Codex sessions keep
their formats and creation path. Browser Pi creation remains unavailable until
host composition supplies the verified runtime, provider and launch dependencies.
A missing adapter refuses before publishing a request or invoking native code.

The [Pi runtime](pi-runtime.md) owns native history and input acceptance.
[Provider policy](pi-providers.md) owns selection validation and its digests.
The host creation owner combines those proofs with the records described here.

## Provider configuration

The portal loads optional `share/dev-workspace/pi-providers.json` once from its
admitted application package. The schema-1 object contains `catalog` and an
explicit `default` selection. The catalog digest covers the exact nested JSON
bytes. A missing file disables Pi; malformed content produces a Pi configuration
refusal while Codex remains usable. There is no alternate catalog path or inferred
provider default.

Production selection requires `RuntimeConfigRef: builtin`, available catalog
routes and canonical credential/account mappings for the complete selected
chain. The startup account map includes every selectable builtin canonical
provider, including providers outside the initial selection. Unsupported or
unverified routes remain unavailable without changing catalog gates or digests.
An unavailable selected fallback refuses the selection as a whole.

Initial policy uses revision 1 and the package's full default selection. Explicit
member overrides preserve each saved member's exact model and thinking level;
roles, access and instructions remain unchanged. Unknown saved settings refuse.
Creation freezes the complete validated policy and reserved member settings.
Recovery uses that retained selection and catalog digest rather than current
package defaults. Retained fallback intent does not enable automatic redispatch.

Package installation can check the same catalog and production-default rules:

```sh
workspace-portal pi-providers validate --package-root /nix/store/…-dev-workspace
```

The command returns nonzero for missing or invalid configuration. It reads the
fixed configuration file without constructing a runtime, inspecting auth or
host-launcher state, or calling a model. A successful check grants no native capability.
Serving constructs the real runtime Manager with explicit user-state, host-state
and auth paths and the frozen account map. It uses the installed host helper and
Unix transport, without launching a worker during construction. Missing roots or
helper, mismatched profile/package, or invalid static worker/host contracts leave
Pi unavailable while Codex remains usable. Auth validation checks directory
ownership/access and separation without reading credentials. Static checks do
not prove installed host permissions or native launch success.

The first launch uses the creation intent's original initial input UUID as its
runtime request ID in a separate namespace. A host launch receipt does not prove
input acceptance. The launch freezes the package, profile symlink identity and
initial binding revision. Recovery reads the actual saved request and reuses all
of its fields unchanged. Root/member readiness may advance the binding revision;
the launch keeps its original revision while live authority publishes the current
revision and exact returned generation. Recovery remains held if the profile is
replaced, the package or account map changes, a successor is saved, or the launch
record is missing after authority or native evidence exists. Configuration checks
never start or repair a missing runtime.

## Durable identity and live evidence

The durable selector is a private schema-1 file at
`UserStateRoot/session-backends/<workspace-sha256>/<slug>.json`. The hash is the
full lowercase SHA-256 of the canonical workspace path. The binding reserves the
session incarnation, original creation receipt, and every selected conversation's
address and incarnation. Native IDs are opaque adapter values. A conversation
stays `creating` until the host proves its retained native history and original
accepted input.

Schema-2 runtime authority is an ephemeral projection of the binding. Its
workspace, slug, session incarnation and binding revision must match the current
binding. A dispatch handle must also have the current generation and exact ready
conversation identity. The projection does not prove host process ownership,
roster access or native acceptance. Missing live state holds a Pi session; it
cannot select Codex. Malformed, conflicting or unsupported evidence refuses.

## Creation records

Schema 4 uses the existing creation paths:

| Record | Path | Retention |
| --- | --- | --- |
| Mutable portal receipt | `UserStateRoot/portal/<workspace-id>/creations/<slug>.json` | Bounded status/retry cache |
| Immutable request | `<slug>.<receiptId>.complete.json.request` beside the receipt | Durable binding lifetime |
| Completion proof | `<slug>.<receiptId>.complete.json` beside the receipt | Durable binding lifetime |
| Creation journal | `<workspace>/worktrees/.locks/<slug>.creation.json` | While Pi authority or dependent records remain |
| Tracking projection | `<workspace>/work/<slug>/portal.yml` | Normal tracking lifetime |

The receipt freezes the original request, removal-history epoch, receipt ID,
normalized goal, resolved direct team, session/member incarnations and initial
input ID. Its required `pi.selection` contains the exact catalog digest, complete
provider policy/default/overrides, ordered fallback intent and every reserved member's
resolved primary settings. The immutable request and journal copy the same value
as `provider_selection`; completion uses `providerSelection`. Recovery compares
full values through the provider validator against that exact catalog. A missing
or changed catalog holds the request instead of selecting current defaults.
Fallback intent is retained; executable fallback is unavailable.

Strict decoders reject incomplete shapes, duplicate or unknown fields, nulls,
unsupported schemas and contradictory identities. An earlier incomplete
schema-4 shape has no defaulting or conversion path. The journal and request stay
within the existing 64 KiB bound; oversized selections refuse before acceptance.

Pi tracking uses schema 4 with `schema`, `slug`, `backend`, `creation` and an
explicit `repositories` array. `sensitive` and `artifacts` are optional. Backend
is `{kind: pi, contract: 1, session_incarnation: ...}`. Creation contains only
`state`, `goal_sha256` and `initial_goal_sent`. The projection has no Codex,
tmux, native path, fork or archive fields and cannot replace the private binding.

## Publication and interruption

The creation owner publishes durable state in this order:

1. Save and sync the accepted receipt before acknowledgement or effects.
2. Save the immutable request, creating journal and revision-1 creating binding.
3. Prepare owned tracking through the Ruby tracking helpers. Reserve all selected
   members, including members that will remain held.
4. Save `native_create_started` and the exact execution generation before calling
   the released native create adapter. Save its exact native ID and history
   reference before the first input.
5. Submit the original goal once through the initialized worker's ordinary Send
   path, with the original initial input ID, context `initial` and `Retry=false`.
   Require the persistent native history and original Accepted receipt proof.
6. Publish the ready root binding, then the matching live projection. Complete the
   tracking projection, recheck native/input proof, and publish completion proof.
7. Save the retained ready journal, then the final ready portal receipt.

A crash before native-create intent can resume preparation under the same
receipt. Loss after that intent can leave native creation Unknown. A lost native
result or missing input receipt cannot cause another create or Send. Unknown and
terminal NotSubmitted input stay held. Recovery preserves all original IDs and
settings; it does not reopen missing native history, send a synthetic bootstrap,
replace the initial input ID or choose another backend. Package recovery keeps
the existing forward-only selection rule.

Go holds transition SH across each internal Ruby tracking child and releases its
creation/runtime ownership first. The child acquires creation then runtime EX,
rechecks package identity and the exact receipt/epoch/binding tuple, and uses the
existing tracking/goal/YAML authoring helpers. The internal phases are
`prepare-tracking` and `complete-tracking`; they cannot attach, select tmux or
initialize Codex. Completion writes only the tracking projection. Go reacquires
its locks and repeats the authoritative native proof before writing final
completion evidence or marking the journal ready.

## Retained root and member publication

Completion includes the original Accepted runtime `initialSubmissionDigest`. Pi cache
retirement removes only the mutable receipt. It retains the immutable request,
completion and ready journal. The host reconstructs the full original runtime
`SubmissionRef` and uses observational Lookup to recover its original Input and
Settings. It checks the goal hash, exact frozen settings, context `initial`,
`Retry=false`, full submission digest and native witness, then calls the current
CreationVerifier. It copies neither goal text nor a second input ledger into the
retained proof. Receipt absence permits only completed proof; it cannot resume a
creating request.

Initial completion must match the current binding revision exactly. Later
reserved-member publications may advance that revision. The original completion
remains valid only with the same creation, removal epoch, full frozen options,
reserved identities/order, ready lead native/history/input and tracking identity.
The current live projection must still match the current binding exactly.

`PublishMemberReady` is a host composition port. Callers pass references and hold
no transition/runtime/team locks. The port acquires transition SH, runtime EX and
team admission, verifies completed root proof, and checks the actual retained
member's access, settings, incarnation and original assignment proof. Create's
returned ID or `Persisted=false` cannot make a member ready. Member initialization
uses its first real authorized assignment; publication sends no prompt.

The port writes the binding CAS before its same-generation live projection. A
projection failure returns the durable binding with `Complete=false`; dispatch
stays held. An exact replay can finish that projection without another revision
increment. This recovery exception belongs only to the publication owner after
it proves the exact prior member CAS. Ordinary readers and dispatch still require
the exact current projection. Fresh utilities have no retained member binding.

## Package compatibility

The host inventories the canonical durable binding roots, receipt namespace and
retained journals independently of ephemeral runtime files. Present Pi
projections must match their current bindings. Target packages need exact integer
contracts `sessionBackendBindingSchema: 1`, `sessionBackendBindingPolicy: 1`,
`runtimeAuthoritySchema: 2` and `piCreationSchema: 4`. Those declarations describe
implemented formats and refusal rules; they grant no launch capability.

Predecessor packages reject schema-4 receipts and journals, including ready
journals and candidate-entry preflight. The actual retained journal therefore
remains a downgrade fence after receipt-cache retirement. A metadata flag cannot
make an old parser understand Pi authority. Legacy writers and lifecycle paths
refuse Pi records before changing their bytes or invoking Codex.

Retained team records also require the target's complete `piTeamRecords` map:

```json
{"policy":1,"roster":2,"monitorOperation":2,"communication":2,"progressObserver":2,"utilityReceipt":2}
```

The preflight refuses a missing, null or incomplete map, extra fields, or values
that differ from those required. This map describes codec support; it does not
register team writers or native tools.
The existing host preflight invokes the proven executing package's private
`workspace-portal pi-team-preflight` reader with the explicit state root and all
registered workspace roots. The candidate supplies compatibility metadata only.
The reader takes no domain/runtime locks, creates no files and starts no worker.
It checks the actual roster, monitor, communication, observer and utility codecs,
including terminal, revoked, canceled and held records. It refuses unknown
namespaces, malformed records, invalid private-file permissions and conflicting
identities.
The scan permits at most 1024 workspaces and 65536 records; output is bounded to
4 MiB and contains only source and session/member identity constraints.

Every dependent Pi record keeps its original ready schema-4 journal, immutable
request and completion proof, even when its binding and mutable cache are absent.
The host matches session/lead identity, reserved member incarnation and creation
receipt references to that exact proof. An unrelated journal cannot cover a
record. Schema-2 communication with only legacy endpoints or empty arrays has no
provable Pi fence owner and refuses; the preflight never downgrades or repairs it.
Legacy communication that preserves retained Pi endpoints keeps their obligations.

Member initial input IDs are structural retention claims validated by the owning
codec. Later member native IDs must match a current binding's nonempty value;
after binding retirement they remain structural claims tied to the reserved
member tuple. The root proof contains neither of those member fields. Duplicate
claims must agree. Ready publication and native recovery still require their full
mapping, input receipt and native witness proofs. Utility retention separately
matches the original monitor launch, mapping and receipts, including their exact
settings, input/context/digest and original generation. Compatibility never rewrites
that generation or resends input.

Installed native creation additionally needs verified host-launcher ownership
and permissions, runtime create/history/input evidence and the trusted canonical
provider/account map.
The optional startup auth and host-state paths are explicit trusted configuration
without guessed defaults. See [Pi host launcher](pi-runtime-host.md) for the
execution boundary. Native successor start, Pi lifecycle and fresh utility
binding require their separately supported contracts.
