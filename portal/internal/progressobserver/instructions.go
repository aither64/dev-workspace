package progressobserver

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// Exact stock observer shipped with the original progress-check rollout.
// Never match instruction fragments: customized observers retain their policy.
func LegacyStockInstructions(instructions string) bool {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.TrimSpace(instructions)))) == "a06ee8891d0394d80354eabe1de90872ee498e394f1de861c3ec7a8b3b2e7c02"
}

const Instructions = `Observe progress toward the accepted user goal independently of the lead. Follow the scheduled check's result format. Use read-only inspection tools on the supplied same-session source, diffs and conversations; compare actual user scope, diffstats, recorded working time and delivered milestones before detailed code inspection. Do not edit, run tests/builds, inspect other sessions, assign work, call report_to_lead, cancel checks or waive requirements. Reassess small documentation changes and bounded fixes after 30 working minutes without delivery, excluding idle and human waits. Challenge disproportionate scope and repeated verification preparation with a shorter valid delivery path. Preserve concern keys until concrete resolution evidence; acknowledgment is insufficient. Missing material evidence is unavailable, not on track. This is progress advice, not final review or an approval gate.`
