package workspacecodex

import "github.com/aither64/codex-web/codex"

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

// LeadThreadPolicy applies only to the normal root conversation. The policy
// names no members because the roster can change during the session.
func LeadThreadPolicy() codex.ThreadPolicy {
	return codex.ThreadPolicy{DeveloperInstructions: leadDeveloperInstructions}
}
