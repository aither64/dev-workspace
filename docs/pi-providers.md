# Pi provider and member settings policy

`portal/internal/piproviders` validates trusted Pi provider catalogs, resolves
session defaults and exact member overrides, stores desired policy, and
coordinates the runtime's settings operation. It does not change Codex provider
state, issue model requests, read credentials, or activate automatic fallback.

## Catalog and account authority

Catalog schema 1 contains providers, accounts, physical models and route gates.
Composition supplies this trusted input; browser requests cannot supply URLs,
headers, credential paths or executables. `DecodeCatalog` hashes the exact input
bytes and rejects duplicate keys, unknown fields, missing fields, unsupported
schema/enums, controls, invalid UTF-8, duplicate mappings and dangling references.
Typed callers must supply the immutable input digest and pass `ValidateCatalog`.
Catalogs are limited to 1 MiB, 64 providers/accounts and 4096 models/route gates.
Identifiers use lowercase letters, digits, hyphens and underscores, up to 63
characters; model IDs retain their exact provider spelling. Strings are bounded
to 4096 UTF-8 bytes without controls.

A provider's `NativeID` resolves the physical Pi adapter and its
`RuntimeConfigRef` resolves trusted configuration. A model must explicitly have
`kind: physical`, supported thinking levels, input capabilities, tool capability
and capability evidence. Unknown or virtual targets are unavailable. The policy does
not silently clamp a requested thinking level or substitute a similarly named
model. Every selected provider/account/model tuple needs an `available` route
gate with a stable reason and nonsecret evidence reference. A catalog entry or
configured credential alone does not establish entitlement. An unverified
Daybreak route does not prevent use of unrelated available routes; selecting it
as a primary or fallback refuses the entire selection.

Accounts name opaque host configuration references. Their canonical identity is
the configured credential store/key. Duplicate account mappings and duplicate
native provider aliases refuse, so aliases cannot create extra refresh owners
or cycles. Workers sharing an OAuth grant must see the same canonical containing
auth directory, lock sidecars and credential replacement effects. Copies and
file-only bindings into independent lock directories are insufficient. Member
histories, model settings and resources remain private. This package never
imports a legacy Codex grant or coordinates its independent auth writer.

`Catalog.Public()` exposes labels, model capabilities and gate summaries. It
omits native configuration references, credential references/keys, auth kinds,
evidence bodies and raw provider errors. Account references are opaque configured
bindings, not proof of the upstream account identity.

## Desired policy and settings

A schema-1 policy contains the exact development-session identity, a document
revision, a versioned default selection and versioned overrides keyed by both
member address and conversation incarnation. A new incarnation inherits the
default rather than an old conversation's override. `WithOverride(..., nil)`
explicitly clears an override. Default changes affect inheriting members only;
there is no atomic all-member switch or global/pool endpoint mutation.

Each selection has a primary plus at most seven ordered fallbacks. Repeated
physical provider/account/model tuples refuse even when thinking differs.
Retry categories are explicit and unique. Extra-attempt and hop budgets default
to zero, are at most seven, and hops cannot exceed extra attempts.

`Resolve` returns the requested selection, its source and target Pi settings.
The settings policy revision is a content digest, separate from the policy
document CAS revision, backend binding revision and runtime settings revision. Its SHA-256
input is a fixed JSON array: schema, catalog digest, source, selected source
revision, ordered resolved native-provider/account/model/thinking tuples, budget
array, and sorted categories. Go `encoding/json` defines UTF-8/escaping and
decimal integer encoding; a byte fixture pins the representation. An unrelated
member override or settings revision does not change this member's digest.

`Store` owns `provider-policy.json` and `provider-policy.lock` beneath an
authorized `piruntime` session state root. Policy files are schema 1 and limited to
64 KiB, mode 0600 in a private 0700 directory. `Load` never creates or repairs
state; missing files remain distinct from malformed or unsupported state.
`CompareAndSwap` takes an exclusive file lock, rereads current state, checks the
document and affected selection revisions, and publishes a synced temporary
file through rename plus directory sync. Initial document/source revisions are
1. Unchanged sources retain their revisions; changed sources advance by one.
No policy lock is held while awaiting a worker. An error after rename may mean
publication occurred; reread before retrying CAS rather than assuming absence.

The portal supplies admission and rechecks the current desired digest before `Apply`.
The caller supplies verified current runtime settings. A change targets current+1;
same revision with identical complete settings calls Configure for verified
idempotent replay. Changed values at the same revision, skipped/lower revisions
and wrong conversation/member scope refuse. AccountRef changes refuse before
calling the worker until the runtime supports an exact account-bound switch.
The runtime makes the final idle, effect and revision checks.

Configure is called once. Applied results preserve requested versus effective
settings; rejected means no mutation, and unknown/empty/unrecognized outcomes
hold the runtime. An error accompanying an apparent applied result is unknown.
`Apply` never synthesizes rollback, clears receipts or resends input. Desired policy
remains separate from applied state, so partial member progress does not report
an all-member success. `Resolved.View` only reports effective settings and
physical dispatch when supplied verified observations; missing evidence stays
unknown. Physical observations name the actual provider/model/API/thinking and
timestamp, with account labelled as a configured binding.

## Non-executing fallback planning

`PlanNext` is pure. It validates the exact original submission/member scope,
policy digest, chain position, actual route and cumulative counters. It returns
`stop`, `held`, `exhausted`, or `candidate_execution_unavailable`. Even a
candidate has no sender, hidden operation ID or permission to resubmit input.

Accepted receipts categorically stop host redispatch. Unknown submission,
partial output and possible/observed effects hold. Positive no-submission and
no-effect evidence must come from the runtime, represented by `Effects: none` and a
nonempty `p3_not_submitted` evidence reference/time; empty fields prove nothing.
NotSubmitted is itself a terminal receipt, so identifying a candidate does not
change its frozen settings or make that receipt reusable for another attempt.

The initial exact evidence map recognizes `rate_limit_exceeded` with 429,
`overloaded` with 503, pre-submission transport `connection_refused` or
`dns_unavailable`, and selection-stage `model_unavailable` or
`configuration_unavailable`. All still need positive runtime evidence and an enabled
policy category. HTTP status or flattened text alone cannot establish safety.
Auth/refresh/storage errors, structured policy errors and unknown categories
hold; ordinary text refusals and cancellation stop. Structured policy handling
requires a separately accepted exact-code gate.

Budgets count cumulative extra attempts and hops for the original operation.
Transport retries, native retries and provider hops must consume the same
budget. Lookup, reconnect, successful tool continuation or settings changes
cannot reset it. No automatic execution is enabled until every retry layer is
observable and the runtime's bound native continuation gate and effect/budget
fixture pass. That continuation keeps the original Accepted receipt and user
entry; it is a separate execution contract from this host planner. Public Pi
virtual-model support alone does not enable it. The planner adds no classifier call,
compaction request, private router, worker hook or new submission ledger.

## Compatibility and verification

This is new Pi-only desired state. Existing Codex files, settings and recovery
contracts remain with their original owners. Unsupported Pi formats refuse;
they never fall through to Codex or reset defaults. A forward-compatible reader
is required once a policy is persisted; removing the package does not authorize
discarding that state. Real account/model availability and installed provider
configuration remain route-specific integration gates.

Focused fake tests run from `portal/` in the repository Nix shell:

```sh
go test -mod=readonly ./internal/piproviders
```

They cover catalog and policy rejection, inheritance/clear/incarnation isolation,
digest bytes, no-write reads/CAS, settings revision/outcome handling and terminal
fallback cases. They perform no login, credential read or model request. The
package expects application composition to supply the trusted catalog.
