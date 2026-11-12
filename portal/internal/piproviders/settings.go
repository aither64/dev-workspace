package piproviders

import (
	"context"
	"errors"
	"math"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

// Apply is called under P2/P3 admission after rechecking the current desired
// policy digest. P3 is the final revision/idle/effect authority. No policy lock
// may be held across this call and no error triggers rollback or resubmission.
func Apply(ctx context.Context, c Configurer, h agentbackend.ExecutionHandle, current piruntime.EffectiveSettings, r Resolved) (piruntime.SettingsResult, error) {
	rejected := piruntime.SettingsResult{Requested: r.Settings, Effective: current, Outcome: "rejected"}
	if h.Conversation.Backend != agentbackend.Pi || h.Conversation.Session != r.Session || h.Conversation.Member != r.Member.Address || h.Conversation.Incarnation != r.Member.Incarnation || h.Generation == "" || h.Conversation.NativeID == "" {
		return rejected, ErrStalePolicy
	}
	if r.Settings.PolicyRevision != r.PolicyDigest || !digestPattern.MatchString(r.PolicyDigest) || r.Settings.Revision == 0 {
		return rejected, ErrInvalidPolicy
	}
	if current.AccountRef != "" && current.AccountRef != r.Settings.AccountRef {
		return rejected, ErrUnsupportedAccountSwitch
	}
	if r.Settings.Revision == current.Revision {
		if r.Settings != current {
			return rejected, ErrStalePolicy
		}
	} else if current.Revision == math.MaxUint64 || r.Settings.Revision != current.Revision+1 {
		return rejected, ErrStalePolicy
	}
	if c == nil {
		return rejected, ErrHeld
	}
	result, err := c.Configure(ctx, h, r.Settings)
	switch result.Outcome {
	case "applied":
		if err != nil || result.Requested != r.Settings || result.Effective.Revision != r.Settings.Revision || result.Effective.PolicyRevision != r.PolicyDigest || result.Effective.Provider != r.Settings.Provider || result.Effective.AccountRef != r.Settings.AccountRef || result.Effective.Model != r.Settings.Model || !textOK(result.Effective.Thinking) {
			result.Outcome = "unknown"
			return result, errors.Join(ErrHeld, err)
		}
		return result, nil
	case "rejected":
		return result, err
	case "unknown":
		return result, errors.Join(ErrHeld, err)
	default:
		result.Outcome = "unknown"
		return result, errors.Join(ErrHeld, err)
	}
}
