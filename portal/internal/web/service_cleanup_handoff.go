package web

import "github.com/aither64/dev-workspace/portal/internal/session"

func servicePreludePhase(phase string) bool {
	return phase == "services_pending" || phase == "services_stopped"
}

// A prelude can become its ordinary archive journal once. The captured phase
// and full prelude hash prove which evidence is being replaced; the accepted
// directory/root proof prevents the same slug from retargeting that receipt.
func lifecycleJournalEvidenceMatches(workspace string, operation lifecycleOperation, progress session.LifecycleProgress) bool {
	expected := operation.Options.JournalEvidence
	if progress.ServicePrelude && !servicePreludePhase(operation.Phase) && (operation.Options.JournalExpected || expected != "") {
		return false
	}
	if expected == progress.Evidence {
		return true
	}
	if !servicePreludePhase(operation.Phase) {
		return expected == ""
	}
	if expected == "" || progress.ServicePrelude || servicePreludePhase(progress.Phase) || operation.Kind != "archive" || progress.Operation != "archive" || operation.Options.JournalID == "" || operation.Options.JournalID != progress.JournalID || operation.Options.Mode != progress.Mode || !progress.RootRecorded {
		return false
	}
	if progress.ServicePreludeEvidence != "" && progress.ServicePreludeEvidence != expected {
		return false
	}
	target, location := operation.Options.JournalTargetID, operation.Options.JournalTargetLocation
	if target == "" {
		if operation.TargetIdentityVersion != lifecycleTargetIdentityVersion {
			return false
		}
		target, location = operation.Options.TargetID, operation.Options.TargetLocation
	}
	if !messageDigestPattern.MatchString(target) || location != "work" && location != "archive" {
		return false
	}
	summary, err := session.Find(workspace, operation.Slug)
	if err != nil || !observationRootMatches(summary.Codex.ThreadID, progress.RetainedThreadID) {
		return false
	}
	identity, err := lifecycleTargetIdentityAt(summary, location)
	return err == nil && identity == target
}
