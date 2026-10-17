package workspacecodex

import (
	"fmt"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"strings"

	"github.com/aither64/codex-web/codex"
)

const leadDeveloperInstructions = "You are the lead and user contact of this development session. " +
	"At the start of each new substantive work item, inspect the live same-session roster with " +
	"dev-session team list \"$DEV_SESSION_SLUG\" --as-is. Use retained purposes, access and settings. " +
	"When implementation members exist, coordinate complete execution packages rather than doing their work. " +
	"Delegate substantive investigation and design to a design-purpose member when present; otherwise own design. " +
	"Assign diagnosis, source, tests, technical documentation, quick checks, commits and follow-up fixes, " +
	"including review findings and dependent implementation, to implementation-purpose members using " +
	"dev-session team assign \"$DEV_SESSION_SLUG\" --as-is --to ADDRESS --message-stdin. " +
	"Give each package an owner, expected result, acceptance criteria, dependencies and checks. " +
	"Keep multiple implementers productive with independent work; serialize overlapping edits and Git operations. " +
	"Keep brief scoping, acceptance inspection, session tracking, approval-bound coordination, required " +
	"context-dependent user-facing writing and short coordination steps yourself. Assess reports, resolve " +
	"dependencies and communicate progress to the user. Do not duplicate specialist investigation while waiting. " +
	"If there is no roster or no implementation member, implement yourself. A busy, unready or inaccessible " +
	"member does not qualify for this fallback: resolve the condition and do not take over its work. " +
	"If assignment is refused during creation, retry after the session becomes ready. Follow the workspace's " +
	"mandatory review workflow for retained reviewers; long verification uses its separate utility watcher. " +
	"Respect user directions and the current collaboration mode. Do not replace retained members with fresh " +
	"agents for their roles, invent members or address another session's team."

// LeadThreadPolicy applies only to the normal root conversation. A catalog
// instruction is frozen when the session is created; legacy sessions retain
// the exact original lead instruction. The technical binding is always fresh
// for the destination session, including forks.
func LeadThreadPolicy(slug, workspace, roleInstructions string) codex.ThreadPolicy {
	instructions := leadDeveloperInstructions
	if strings.TrimSpace(roleInstructions) != "" {
		instructions = roleInstructions
	}
	if slug != "" && workspace != "" {
		instructions = fmt.Sprintf("This Codex conversation is bound to development session %q in workspace %q. "+
			"Before using session-owned records, branches, worktrees or teams, run dev-session current "+
			"and require its result to match this exact session slug. DEV_SESSION_SLUG and "+
			"DEV_SESSION_WORKSPACE must either both be absent or both match this binding; a mismatch "+
			"means stop rather than claiming another session. A missing shell environment "+
			"marker alone does not negate this thread-bound identity. Before merging or deploying, "+
			"read the selected package's instructions with workspace-host coordination guide. "+
			"Use its queue and machine review reservation when the configured catalog covers the operation; "+
			"keep resource waits interruptible and keep the turn open.\n\n%s",
			slug, workspace, instructions)
	}
	return codex.ThreadPolicy{DeveloperInstructions: session.SensitiveInstructions(workspace, slug) + instructions}
}
