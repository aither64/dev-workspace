# Pi SDK runtime

The Pi adapter separates conversation input from runtime and host authority.
`piruntime.Controller` takes trusted session and member identities; its nullable
conversation operation slots do not grant permission to use them. The host
launcher supplies the session process boundary described in
[Pi session host launcher](pi-runtime-host.md). Each loaded conversation uses a
separate SDK process, private history/settings/resources and the configured
canonical native authentication directory.

The worker, protocol and receipt components are available independently. The
session supervisor and controller still need integration before this API can
launch a session.

## Input acceptance and recovery

The adapter publishes a private, fsynced intent before dispatch. Ordinary text
input has one pending submission per worker. The SDK user event occurs before
native history append, so the adapter waits until the exact new entry can also
be read through an independent public SessionManager reopen. Repeated text is
correlated using the live event object and new entry lineage, never text alone.
The host then commits the native witness to its existing submission record
before returning Accepted.

Records use schema 1 and retain the stable conversation, original operation ID,
action, full immutable input/settings digest and dispatch generation. Generation
is evidence and does not partition duplicate detection. IDs accept exactly
lowercase 32 hexadecimal characters or the lowercase 8-4-4-4-12 hexadecimal
spelling. Spellings are preserved; compact and hyphenated IDs are distinct.
Lookup and same-ID retries never dispatch again, including terminal NotSubmitted.
Changed input or settings under the same ID conflicts.

A crash before the committed witness can leave Unknown even if native input or
a tool effect exists. Missing acknowledgements after receipt commit are
recoverable by lookup. Unknown holds the affected conversation and requests its
exact worker abort; abort does not prove arbitrary external effects stopped.
SDK exceptions cannot establish NotSubmitted. The adapter makes no exactly-once
claim for external tools.

## Native identities and settings

A creation mapping reserves a native ID before SDK creation. Empty native
sessions may have no history file. Never submit synthetic input to force a
header, and never open a missing history path as recovery: the SDK can choose
a new identity. Creation verification is observational and requires the owned
mapping plus independent native history and the original Accepted receipt.
An initialized-only proof cannot publish a ready conversation.

HistoryRef is an opaque SHA-256 mapping locator, not persistence evidence. Its
versioned encoding is the JSON array `['pi-history',1,backend,workspace,slug,
sessionIncarnation,member,conversationIncarnation,nativeID]`, encoded by Go
encoding/json without a newline. A golden fixture fixes the bytes/hash.

EffectiveSettings contains resolved provider/account/model/thinking/policy
references, never tokens or browser-supplied endpoints. Revision is the target
revision: current+1 applies a mutation; identical full settings at the same
revision replay the prior result; conflicting/skipped/lower revisions reject.
Configuration is idle and serialized. Account changes require a separately
proved native account binding. Outcomes distinguish applied, rejected before
mutation, and unknown after possible mutation.

## Worker package boundary

The worker uses public SDK exports from one explicit immutable Pi package root.
It never resolves ambient npm modules or probe fixtures. Its module-relative closure
is worker.mjs, protocol.mjs, session.mjs and package.mjs. The private version-1
pipe protocol has bounded messages, correlation and exact execution identity.
The pre-initialization version/EOF smoke creates no SDK session and reads no
authentication data. Source-only fake providers are excluded from production.

The application's strict pi-worker.json parser owns validation of worker
protocol, immutable Node/entrypoint/Pi roots, duplicate/unknown/null/trailing
fields, filesystem kinds and access. This metadata contains no state/auth paths
or credentials. Host execution and a package build alone do not prove supervisor
readiness, native prompt admission or installed tool permissions.
