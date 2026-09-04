---
name: dev-session-handoff
description: Prepare a handoff for a vpsFree.cz development initiative and include its stable workspace portal URL after material changes, review checkpoints, or user-requested status updates.
---

# Development session handoff

Use the initiative tracked by the current process. When
`VPSFREE_DEV_SESSION_REQUIRE_RUNTIME=1`, use the configuration-owned
`workspace-dev-session`; otherwise use checkout-local `bin/dev-session`. Run
the applicable helper's `current` command and accept its result only when
`VPSFREE_DEV_SESSION_SLUG` has the same value, as required by the workspace
`AGENTS.md`. Do not use or modify another concurrent session. If no initiative
belongs to the process, create one before making changes or ask for the
intended slug when choosing one would change the task.

Before handing off material work:

1. Update `work/<slug>/state.md` with the current branches, test or CI results,
   blockers, and next operator action. Commit the checkpoint when the workspace
   rules require it.
2. Keep `portal.yml` free of secrets and transient files. Add only useful
   artifacts stored beneath the initiative tracking directory. Repository
   worktrees created with the applicable helper's `worktree add` command
   register themselves.
3. Get the stable link from the applicable helper's `url <slug> --as-is`
   command.
4. Put that link in the final handoff after the outcome and any action the user
   still owns. If the portal is not deployed or reachable yet, label the link as
   the post-deployment URL instead of omitting it.

Archived initiatives retain the same URL and become read-only in the portal.
