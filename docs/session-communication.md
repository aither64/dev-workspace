# Communication between session leads

Leads in the same registered workspace can exchange messages after the user
approves the pair. Approval from either session allows both directions. Existing
leads handle cooperation; no additional team role is needed.

In the portal, open **Communication**, choose the other session and select
**Allow communication**. Either session can revoke access. The tab shows the
approval source, message history and delivery status. In the Codex tab, session
messages and communication tool calls use amber styling and a session label.
Choose **Sessions** to see only that traffic, or **All** to include it alongside
the rest of the conversation. Errors remain visible in every filter. A lead may also record
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
`--message-id ID` to retrieve a specific one. The portal displays the retained
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
and delivery options. After an uncertain result or restart, it reconciles the
same native receipt before retrying. Revocation cancels unsent messages; it
cannot recall an accepted message. An uncertain submission remains available
for reconciliation even after revocation, without authorizing a new send.

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
