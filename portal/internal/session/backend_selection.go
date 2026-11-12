package session

import (
	"errors"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

// BackendEvidence contains already validated host records. LegacyConversation
// must come from the existing retained-root/member validation, not a browser
// native ID. It allows cold/archive evidence without inventing new provenance.
// Loader errors must be propagated before selection; only ErrNotBound means an
// absent Pi record. This function never parses legacy manifests or receipts.
type BackendEvidence struct {
	PiBinding          *BackendBinding
	PiAuthority        *PiRuntimeAuthority
	LegacyAuthority    *RuntimeAuthority
	LegacyConversation *agentbackend.ConversationRef
}

// BackendSelection reports a backend, not support or permission to execute.
// In particular, a Pi binding without live authority remains Pi and held.
type BackendSelection struct {
	Kind     agentbackend.Kind
	Evidence BackendEvidence
}

func SelectBackend(workspace, slug string, evidence BackendEvidence) (BackendSelection, error) {
	if !backendCanonicalPath(workspace) || !backendSlug(slug) {
		return BackendSelection{}, errors.New("invalid backend selection scope")
	}
	if evidence.LegacyAuthority != nil {
		if err := evidence.LegacyAuthority.Validate(slug, workspace); err != nil {
			return BackendSelection{}, err
		}
	}
	if legacy := evidence.LegacyConversation; legacy != nil {
		if legacy.Backend != agentbackend.Codex || legacy.Session.Workspace != workspace || legacy.Session.Slug != slug ||
			!backendScalar(legacy.NativeID) || !backendMember(legacy.Member) {
			return BackendSelection{}, agentbackend.ErrBindingConflict
		}
		if evidence.LegacyAuthority != nil && legacy.Member == "lead" && evidence.LegacyAuthority.CodexThreadID != "" &&
			evidence.LegacyAuthority.CodexThreadID != legacy.NativeID {
			return BackendSelection{}, agentbackend.ErrBindingConflict
		}
	}
	if evidence.PiBinding != nil {
		if err := evidence.PiBinding.Validate(workspace, slug); err != nil {
			return BackendSelection{}, err
		}
	}
	if evidence.PiAuthority != nil {
		if err := evidence.PiAuthority.validate(workspace, slug); err != nil {
			return BackendSelection{}, err
		}
	}
	hasLegacy := evidence.LegacyAuthority != nil || evidence.LegacyConversation != nil
	if evidence.PiBinding != nil {
		if hasLegacy {
			return BackendSelection{}, agentbackend.ErrBindingConflict
		}
		if evidence.PiAuthority != nil {
			if err := evidence.PiAuthority.ValidateBinding(*evidence.PiBinding); err != nil {
				return BackendSelection{}, err
			}
		}
		return BackendSelection{Kind: agentbackend.Pi, Evidence: evidence}, nil
	}
	if evidence.PiAuthority != nil {
		return BackendSelection{}, agentbackend.ErrBindingConflict
	}
	if hasLegacy {
		return BackendSelection{Kind: agentbackend.Codex, Evidence: evidence}, nil
	}
	return BackendSelection{}, agentbackend.ErrNotBound
}
