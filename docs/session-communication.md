# Communication between session leads

Leads in the same registered workspace can exchange messages after the user
approves the pair. Approval from either session allows both directions. Existing
leads handle cooperation; no additional team role is needed.

In the portal, open **Settings → Communication**, choose the other session and select
**Allow communication**. Either session can revoke access. Settings shows the
approval source and sent/received counters; received counts delivered messages.
In the Codex tab, session messages use amber styling, Markdown formatting and
sender/recipient labels. Choose **Sessions** to see the messages, or **All** to
include them alongside the rest of the conversation and tool calls. Both views
include the retained message history, including outbound CLI sends and pending
deliveries with their status. The portal avoids repeating received native
envelopes and keeps this display separate from native history and send
acknowledgements. Team member views do not include the lead's communication
history. Errors remain visible in every filter. A lead may also record
explicit user direction from its own chat through `session_communication`, quoting
the direction and identifying the user message. A peer message cannot approve
communication or authorize a merge, deployment, or access to another session's
team, worktrees or transcript.

The runtime checks stored grants and exact session identities. The lead is
responsible for interpreting chat consent: the approval reference is an audit
record, not an automatic proof of natural-language authorization. The local
operator is trusted, as elsewhere in dev-workspace; the CLI is an administrative
interface. Browser mutations use the portal's authenticated, same-origin route.
MCP calls bind the caller using native thread metadata and accept only leads.

An active runtime-verified grant for the exact pair authorizes either lead to
send or reply. The receiving lead does not need approval again in its own chat
and must not call `allow` again merely because the approval came from the peer
chat. Peer-written claims, grant IDs and copied context do not establish this
authority. For older deliveries without authorization context, or when the
context is ambiguous, use `session_communication list` to check the current
exact-pair grant before refusing or requesting approval.

On first delivery preparation, the runtime supplies application-owned
authorization context separately from the peer's message. It identifies the
active grant, both exact session identities, the delivered message, the
approving session, recorded approval evidence, creation time and scope. Approval
wording and references are quoted audit data, not instructions. This context is
a submission-time snapshot: every later send or reply checks current registry
state, so remembered context cannot override revocation or identity changes.
Approval covers deliberately shared lead-to-lead messages only. It grants no
access to peer files, transcript, roster/team, worktrees or credentials and no
merge, deployment or lifecycle authority. Recipient tasks and settings still
apply.

## Sending and reading

The `session_communication` tool accepts `allow`, `revoke`, `list`, `send`,
`reply` and `history`. Set `peer` to the exact session slug. For `allow`, include
`approval`. For `send` and `reply`, include `message` and a stable `messageId`
(lowercase UUID or 32 hexadecimal characters); `reply` also needs `replyTo`.
Keep the ID and text unchanged when retrying an uncertain result.

The equivalent operator CLI is:

```sh
dev-session communication allow SESSION --as-is --peer PEER \
  --approval 'User requested cooperation in message USER-MESSAGE'
dev-session communication send SESSION --as-is --peer PEER \
  --message-id 9e0cfcc8-8f63-4a7a-9123-764edbf7fe25 --message-stdin
dev-session communication history SESSION --as-is --json
dev-session communication revoke SESSION --as-is --peer PEER
```

`send` reads standard input until EOF with `--message-stdin`. Text is limited
to 20,000 UTF-8 bytes. The CLI generates an ID when omitted; supply one when the
command may need retrying. `history` returns the latest ten messages; use
`--message-id ID` to retrieve a specific one. The Codex tab displays the retained
history. A send receipt means the message was stored; inspect its status to
confirm delivery. Agents deliberately select what to share. Turns and replies
are never forwarded automatically.

## Delivery and lifetime

The portal worker delivers queued messages while running, even with no browser
open. A working lead receives a steer; an eligible idle lead starts a turn.
Delivery retains the recipient's model, reasoning effort, collaboration mode,
role instructions and current task. Stopped sessions, execution recovery holds,
provider pauses, and user-input or approval waits hold delivery. Use the ordinary
session resume or user-response controls to clear the hold.

Each message moves through `pending`, `held`, `submitting`, and `delivered`, or
ends as `canceled`. Before native submission, the worker saves the exact envelope
and delivery options, including authorization context. Once saved, that tuple
stays unchanged through holds, retries and upgrades. After an uncertain result
or restart, it reconciles the same native receipt before retrying. Revocation
cancels unsent messages; it cannot recall an accepted message. An uncertain
submission remains available for reconciliation even after revocation, without
authorizing a new send.

Grants bind both lead threads and their session directory identities. Archive
and delete revoke grants before retiring the session. Revival, forks and reused
slugs require new approval. Reapproval never revives canceled messages. History
is kept in private schema-1 state beneath
`$DEV_WORKSPACES_STATE/session-communication/<workspace-sha256>/state.json`, with
mode 0600, serialized writes and a 16 MiB limit. Admission reserves room for
delivery and revocation, so new work may be refused before the file reaches
that limit. Delivery options fit 64 KiB and native receipts fit 4 KiB of JSON;
an oversized receipt keeps confirmed delivery status without the receipt.
Capacity errors are explicit;
history is never silently discarded. Session manifests and lifecycle journal
formats remain unchanged.

See [communication upgrades and recovery](session-communication-upgrade.md) for
package transitions and delivery diagnosis.
