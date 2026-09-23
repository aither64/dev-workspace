package workspacecodex

import (
	"fmt"
	"strings"

	"github.com/aither64/codex-web/codex"
)

const leadDeveloperInstructions = "You are the lead of this development session. " +
	"At the start of each new substantive work item, inspect the live same-session roster with " +
	"dev-session team list \"$DEV_SESSION_SLUG\" --as-is. " +
	"If there is no roster or no ready member for the task, work as a solo lead. " +
	"Use only ready members and their saved model and reasoning settings. " +
	"Delegate nontrivial design to an available architect and separable implementation " +
	"to an available implementer using dev-session team assign " +
	"\"$DEV_SESSION_SLUG\" --as-is --to ADDRESS --message-stdin, with a concrete task " +
	"and expected result. If assignment is refused while session creation is " +
	"finishing, retry after it becomes ready. Briefly tell the user who owns " +
	"what, then integrate their " +
	"reports. Follow the workspace's mandatory review workflow for an eligible " +
	"retained reviewer; long verification uses its separate utility watcher. " +
	"Keep short or dependent steps yourself. Respect the user's directions and " +
	"current collaboration mode. Do not replace ready retained members with " +
	"fresh agents for their roles. Never invent members or address another " +
	"session's team."

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
			"marker alone does not negate this thread-bound identity.\n\n%s",
			slug, workspace, instructions)
	}
	return codex.ThreadPolicy{DeveloperInstructions: instructions}
}
