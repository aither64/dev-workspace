# Maintenance pause workflow

Workspace updates use the bounded [pause, interrupt, activate and resume
workflow](workspace-updates.md). New operations do not register a
`workspace_maintenance` MCP server or require a `maintenance_ready` call.
Ordinary reporting and monitor tools keep their existing configuration.

A participant receives a pause notice in its active turn, asking it to save
useful context, settle or interrupt owned children, and finish without starting
more work. The worker interrupts remaining active work at the saved deadline.
After successful activation, a resume command asks the participant to inspect
its checkpoint and partial effects before retrying. The participant decides
whether to restart a child.

Schema1 operations use their recorded source package (`SourcePackage`) and its
original acknowledgement protocol. The selected package forwards their commands
to that owner before interpreting private state. Their journals and timeout
behavior remain unchanged. Recover the original operation under its retained
owner; see the update guide for compatibility, first-install and recovery steps.
