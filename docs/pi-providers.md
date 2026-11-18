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

The runtime's fixed `CanonicalCredentialRef`, `canonical`, names only its
explicitly configured `AuthDirectory/auth.json`. It is not a default pathname
or an operator-selected alias. The initial native runtime reads that file by
native provider ID. An available account mapping therefore also needs
`CredentialRef == canonical` and `CredentialKey == Provider.NativeID`; an
`AccountRef` label alone cannot select a different native credential.

`SelectableRuntimeAccounts(catalog, canonicalRef)` returns the sorted, unique
native-provider/account union for available model routes whose mappings satisfy
that rule, including eligible providers not used by initial primaries. It skips
unsupported or unverified rows without changing the catalog or its digest.
Malformed or ambiguous catalogs refuse; an empty union or more than 64 pairs
is unavailable. Trusted composition freezes this union in runtime startup
configuration. Browser arguments cannot expand it or restart the runtime to
add a requested account.

`ProjectRuntimeAccounts(catalog, targets, canonicalRef)` strictly validates the
exact frozen targets and returns their sorted, unique bindings. Unsupported
selected store/key/provider/model/thinking or route mappings refuse. With
fallback execution disabled, request admission checks the actual primary;
future activation must check every enabled physical target. This projection
does not truncate the original chain or waive full policy validation. An
unavailable inactive fallback remains draft and unselectable.

`Catalog.Public()` exposes labels, model capabilities and gate summaries. It
omits native configuration references, credential references/keys, auth kinds,
evidence bodies and raw provider errors. Account references are opaque configured
bindings, not proof of the upstream account identity.

Raw public gates alone do not make an action selectable. The portal must also
check the frozen supported map and complete policy/target admission. Keep
runtime support distinct from provider entitlement; skipping a row does not
establish a provider failure.

## Installed configuration

`LoadInstalled(packageRoot)` reads only
`share/dev-workspace/pi-providers.json` under the caller's admitted immutable
package root. It returns `PackageConfiguration{Catalog, Default}` from the
schema-1 envelope with the keys `schema`, `catalog` and `default`. The nested catalog
uses the existing catalog wire fields, without a digest field; the default is
the existing Selection shape with `chain`, `budget`, `fallback_on`. There is
no built-in provider/default, credential scan or alternate configuration path.

The whole file is limited to 1 MiB. The loader rejects missing, unknown or
duplicate fields, unsupported versions, null objects or defaults, wrong types,
trailing data and invalid catalog or selection content. It preserves the exact
nested catalog byte span for `DecodeCatalog` hashing, including its key order,
escaping and internal whitespace. It validates the default with the owning
selection validator at internal revision 1, without a fake session or member.
Each load owns its catalog slices and retains a separate default selection.
No partially validated configuration is returned.
Trusted typed callers can use `PackageConfiguration.Validate()` for the same
catalog and default checks. They must retain the original raw-byte catalog
digest; validation neither reconstructs those bytes nor creates a session identity.

A missing file returns an error wrapping `os.ErrNotExist`; malformed or
unsupported content is a separate refusal. The portal may disable Pi when the
optional file is absent while keeping its legacy runtime usable. It must not
replace malformed content with an inferred default or another catalog. Trusted
composition owns the production `builtin` support intersection, full-chain
canonical checks and saved team settings, then freezes the resulting selection.
Generic runtime configuration references remain opaque to this loader. Reading
the package does not initialize a runtime, authenticate or enable fallback.

## Immutable native endpoint composition

A downstream package can pass `piModelConfig` to `lib.mkPackage` to install
`share/dev-workspace/pi-models.json`. This optional native SDK input configures
one built-in OpenAI Responses endpoint:

```json
{"providers":{"openai":{"baseUrl":"https://gateway.example.test/v1","api":"openai-responses"}}}
```

The input must be a regular UTF-8 file of at most 64 KiB. It requires the keys
shown above and accepts the optional model overrides and bounded physical
models list below. Validation rejects
duplicate, missing, null or unknown fields and trailing input. The URL must use
HTTP or HTTPS, have a host, and contain no user information, query, fragment,
control characters or interpolation. It retains
the declared path. The file cannot contain credentials, headers, OAuth,
commands or custom providers.

Model IDs and capabilities come from the installed catalog and pinned native
built-in model data. An installed catalog must map the endpoint to a provider
with `native_id: openai` and `runtime_config_ref: builtin`, and a canonical
`openai` API-key account. Validation reads no credentials and does not establish
account access or model entitlement.

An optional `modelOverrides` object under `providers.openai` sets native
`samplingParams.parallel_tool_calls` to a boolean with one to sixteen overrides,
each naming an exact native model:

```json
"modelOverrides":{"gpt-6.1-sol":{"samplingParams":{"parallel_tool_calls":false}}}
```

Each model key must match an installed catalog model for the OpenAI provider.
Validation accepts `true` or `false` and rejects wildcards,
other model or sampling fields, and non-boolean values. Set the value required
by the selected endpoint. Omitting the override preserves the native SDK default;
omission is not a substitute for an explicitly required value.

The optional `providers.openai.models` list supplies up to 16 physical model
descriptors with unique IDs. Each descriptor contains `contextWindow`, `id`,
`input`, `name`, `reasoning`, `samplingParams` and `thinkingLevelMap`. The supported
descriptors accept text and image input, enable reasoning, and set
`parallel_tool_calls` to `false`. Their thinking map preserves `low`, `medium`,
`high`, `xhigh` and `max`; `off` and `minimal` are `null`, and `ultra` is unavailable.
Pricing and `maxTokens` are omitted and use SDK defaults. These descriptors do not
establish provider entitlement or prove every advertised capability against the
remote route.

The [retained catalog compatibility rules](pi-runtime.md#upgrading-a-ready-session)
explain supported additions and preservation of original digests and settings.

This setting controls the serialized provider request. Tool permissions and local
tool execution policy remain unchanged. Input validation and local transport
checks do not establish remote gateway compatibility.

The supervisor loads the fixed file from its admitted `launch.AppStorePath`,
including when a retained runtime uses an earlier package than the portal's
current profile. It passes that path through trusted worker initialization.
Browser input, environment variables and member settings cannot select the
endpoint. The installed file takes precedence over private `models.json`;
the runtime does not merge them. If the package has no optional file, the worker
keeps its existing private-model behavior. An explicitly supplied worker path
that is missing or invalid is rejected before native state creation or sending,
without falling back to the public OpenAI endpoint.

The SDK's writable `models-store.json` stays in each private agent directory.
The immutable input directory receives no cache or lock writes. Credentials stay
at the configured canonical `AuthDirectory/auth.json`, with native locking and
stored-auth checks. The catalog digest continues to hash the original catalog
bytes. The admitted immutable package, launch digest and generation bind the
endpoint bytes.

This input requires no record, receipt or wire migration. It does not
automatically rebind endpoints, switch accounts or retry requests. Existing
Accepted/Unknown ownership and guarded successor recovery continue to apply.

A package can contain the endpoint configuration without `piProviderConfig`;
Pi remains unavailable in that case. Credential provisioning, route/model
acceptance, and host and application activation are separate prerequisites.
A local synthetic Responses check verifies SDK routing and isolation. It does
not prove live gateway access or installed host behavior.

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

After validating the complete policy/catalog, `Resolve` also strictly projects
the primary's canonical account mapping. Its returned value carries private,
process-local validation provenance for the complete resolved scope, source,
selection, settings and digests. The provenance uses independent copies of
mutable slices. An ordinary value copy remains valid; modifying exported
fields or deserializing a fabricated result cannot retain that proof. `Apply`
checks it before calling Configure. It grants no authority or native readiness.
Persisted creation selections rebuild through normal `Resolve` with the exact
frozen catalog/policy, rather than serializing this proof or reading new defaults.

The portal's Pi model and reasoning editor uses the current admitted provider
catalog and frozen runtime account binding. It changes only the selected member's
primary model/thinking pair, preserving the provider, account, fallback chain and
other members. Only an explicit **Save** may initialize or change desired policy.
A missing policy can be initialized from the immutable policy recorded at creation
only while the member's complete current settings still match its initial settings.

When changing policy, the editor saves it by CAS, releases the policy lock, rereads
the desired revision and calls `Apply` once. Reads and reloads never apply desired
policy. The displayed selection comes from confirmed runtime settings; a rejected
or unknown Configure outcome does not confirm the requested change.

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

The portal supplies admission and rechecks the current desired digest before
`Apply`.
The caller supplies complete verified current runtime settings with positive
revision. Empty/unverified settings or revision zero refuse; runtime Create owns
initialization. A change targets current+1. The same revision with identical
complete settings calls Configure for verified
idempotent replay. Changed values at the same revision, skipped/lower revisions
and wrong conversation/member scope refuse. An unchanged provider/account pair
permits ordinary model/thinking changes. Changing only the provider or only
AccountRef refuses. Both may change after strict canonical target validation;
the runtime checks both old and new pairs against its frozen startup map,
current generation and idle/no-unresolved-operation state before native effects.
This uses the same conversation and private settings without changing global
defaults, copying credentials or sending input. Partial or uncertain native
model/thinking/settings mutation holds rather than claiming rollback.

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

## Pure native retry policy

`CompileNativePolicy(catalog, resolved, conversation, frozenBindings)` prepares
the separate native same-input policy. It requires intact `Resolve` provenance,
the original catalog digest, an explicitly admitted Pi conversation with matching
session/member/incarnation and native ID, complete chain availability and strict
canonical account projection for every target. All required pairs must be in
the sorted immutable runtime startup map. It preserves the original settings,
policy digest, chain order, limits and categories; no shortened chain or new
selection is used to compile a candidate. Returned target/category slices are
independent of the source values.

`PlanNativeRetry(policy, evidence)` is a pure decision over runtime-certified
observations. Its `allow`, `stop` or `held` result does not send input, mutate
settings, append history or charge an allowance. The runtime owns proof of the
original Accepted input, active generation, user/state/failed entries, actual
physical response, unchanged linear history and complete effect accounting.
The decider checks those required fields and certifications. Missing or
inconsistent proof, partial output, failed-response tool content, pending effects
or transport uncertainty holds. Cancellation, disabled or nonretryable failure,
and exhausted limits stop. Only enabled exact adapter-evidenced
`rate_limit_exceeded/429` or `overloaded/503` may advance.

An allowed decision selects exactly the next physical target and increments
chain index, extra attempts and hops once. This first path requires all three
counters to be equal and within the original finite limits. It permits no
same-target retry, skip, wraparound or reset after successful tool continuation.
The returned counters are prospective: the runtime must independently recheck
the active input and persist/sync its public native-branch charge before using
the target. Crash or uncertain charge stays held without refund or replay.

These pure functions use the runtime-owned structural policy/evidence types and
validators. They do not register an SDK router, implement a callback/writer or
prove native history/effect truth. Runtime candidate fixtures and the exact
production mapping/schema/reset/reload contract remain separate gates. The host
`PlanNext` rules are unchanged: Accepted still forbids host input redispatch.
Compiling or allowing a native candidate does not enable automatic fallback.

## Public SDK retry component

`pi-runtime/src/native-retry.mjs` exports `createNativeRetryRouting`. It registers
a private virtual model through the public SDK, selects it without
persisting shared defaults, and keeps virtual selection separate from physical
dispatch observations. Its policy, runtime/handle, retry request and decision
use the runtime types' exact exported Go field names. SDK route results use
`{model, thinkingLevel}`.

Before the single ordinary prompt, `beginInput` requires the complete original
SubmissionRef, generation, a fixed trusted deadline and an admission callback.
Initial routing waits within that deadline for the original Accepted receipt
and exact persisted user witness. Each continuation requires explicit trusted
effect accounting; retry also requires a trusted failure certifier and
PrepareNativeRetry adapter. A native user entry alone proves none of those host
facts. Missing authority holds, and the deadline is never renewed per retry.

The component independently reopens public native history and verifies its
complete linear branch, original user, latest exact routing state and failed
physical response. It observes streamed partial text/thinking and tool content.
Only the enabled exact error-code/status pairs described above can reach the
trusted retry gate. A refusal in model text does not authorize retry. Private native
retry limits must remain finite, provider retries zero, and compaction disabled;
direct and summary routing refuse.

Initial zero counters and each allowed next hop use public
`appendCustomEntry(VIRTUAL_MODEL_STATE_ENTRY, ...)`. Before returning a physical
target, the component independently reopens and compares the complete new
entry/state/parent and branch, then runs the existing history file and directory
sync barrier. It omits returned `state`, preventing the SDK's second append.
Successful tool continuation retains the original cumulative counters. Charge
uncertainty, cancellation or changed history holds without another target;
reopening never resumes or replays a charged input.

The finite pinned-SDK fixture uses two fake providers, at most three physical
targets and explicitly fake host admission/effect evidence. It checks native
continuation, one input, unchanged completed tool prefix, bounded cumulative
charges, refusal and process-death boundaries. This component has no production
worker registration, callback transport or mapping writer. Production still
needs authoritative original admission/current-generation/effect integration
and an original retry-deadline source; an acknowledgement or callback timeout
cannot substitute for that deadline. Physical provider support and live fallback
activation remain separate gates. Host `PlanNext` continues to forbid Accepted
redispatch.

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
