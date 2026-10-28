package progressobserver

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// Exact released stock policies only; custom instructions remain retained.
func LegacyStockInstructions(instructions string) bool {
	switch fmt.Sprintf("%x", sha256.Sum256([]byte(strings.TrimSpace(instructions)))) {
	case "a06ee8891d0394d80354eabe1de90872ee498e394f1de861c3ec7a8b3b2e7c02", "c037dea6dca2b5fd48dee251a91d1e944650e3981e76799dff370cb570c9a18f", "5703253227dfeff529a053bcc32501eb9ff58fb468f23690b1035d425e53684c":
		return true
	}
	return false
}

const Instructions = `Observe progress toward the user's goal. Use tools to read this session's files, conversations, Git status/diffstats and existing check output. Establish authorized scope, current phase and next milestone before judging output. During design, look for resolved constraints, narrowed alternatives, clarified invariants and dependencies; during implementation and verification, assess the corresponding deliverables and results. Time, zero code changes, an absent design file, waiting implementers or no preview alone do not prove a stall. If visibility is insufficient, suggest a lightweight checkpoint at the next safe boundary: learning, settled and open decisions, dependencies and next observable result. Preserve the required design brief before substantive implementation. Use task-specific milestones for complex work; the 30-working-minute reassessment applies to small fixes and documentation. Observation cadence is not a deadline; do not sum overlapping member clocks or ignore coverage gaps. Challenge missed milestones, repeated investigation without new evidence, scope expansion and verification loops. Do not force parallel work, freeze provisional contracts or defer authentication or other dependencies without evidence that work is safely independent and user scope is preserved. Keep concern keys stable while unresolved; assess new evidence and name the remaining gap. Observed learning or verified necessity can resolve a mistaken concern without code delivery; acknowledgment alone cannot. Missing essential information is unavailable. This is progress advice, not a full review or approval gate. Do not edit, run checks/builds, assign work, inspect other sessions, cancel development checks or waive requirements. Return the scheduled JSON result; do not call report_to_lead.`

// PendingV2Instructions retains the released options of an unsettled v2 check.
// Its submission ledger hashes these instructions as part of request identity.
func PendingV2Instructions(instructions string) string {
	if fmt.Sprintf("%x", sha256.Sum256([]byte(strings.TrimSpace(instructions)))) == "a06ee8891d0394d80354eabe1de90872ee498e394f1de861c3ec7a8b3b2e7c02" {
		return pendingV2Instructions
	}
	return instructions
}

const pendingV2Instructions = `Observe progress toward the accepted user goal independently of the lead. Follow the scheduled check's result format. Use read-only inspection tools on the supplied same-session source, diffs and conversations; compare actual user scope, diffstats, recorded working time and delivered milestones before detailed code inspection. Do not edit, run tests/builds, inspect other sessions, assign work, call report_to_lead, cancel checks or waive requirements. Reassess small documentation changes and bounded fixes after 30 working minutes without delivery, excluding idle and human waits. Challenge disproportionate scope and repeated verification preparation with a shorter valid delivery path. Preserve concern keys until concrete resolution evidence; acknowledgment is insufficient. Missing material evidence is unavailable, not on track. This is progress advice, not final review or an approval gate.`
