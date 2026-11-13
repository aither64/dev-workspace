package piruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"slices"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

// Tool policy is independent of provider settings and submission identity. These
// types extend Controller optionally; their presence advertises no capability.
type ToolPolicyTarget struct {
	Mode                      string
	Revision                  uint64
	Digest                    string
	Active, RegisteredCeiling []string
}
type ToolPolicyState = ToolPolicyTarget
type ToolPolicyProof struct {
	Policy                       ToolPolicyState
	Generation                   string
	Active, Callable, Registered []string
	Applied                      bool
}
type ToolPolicyResult struct {
	Outcome string
	Proof   ToolPolicyProof
}
type ToolPolicyController interface {
	InspectToolPolicy(context.Context, agentbackend.ExecutionHandle) (ToolPolicyProof, error)
	ApplyToolPolicy(context.Context, agentbackend.ExecutionHandle, uint64, ToolPolicyTarget) (ToolPolicyResult, error)
}

const MaxToolPolicyRevision uint64 = (1 << 53) - 1

var toolNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,127}$`)
var planTools = map[string]bool{"read": true, "grep": true, "find": true, "ls": true, "request_user_input_async": true, "publish_plan": true}

func validToolNames(names []string) bool {
	if names == nil || len(names) > 128 {
		return false
	}
	for i, name := range names {
		if !toolNamePattern.MatchString(name) || (i > 0 && names[i-1] >= name) {
			return false
		}
	}
	return true
}

// ToolPolicyDigest uses the frozen ordered-array JSON encoding, without newline.
// Revision is CAS state and intentionally absent from these bytes.
func ToolPolicyDigest(target ToolPolicyTarget) string {
	data, _ := json.Marshal([]any{"pi-tool-policy", 1, target.Mode, target.Active, target.RegisteredCeiling})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func ValidateToolPolicy(target ToolPolicyTarget) error {
	if (target.Mode != "plan" && target.Mode != "default") || target.Revision == 0 || target.Revision > MaxToolPolicyRevision || !validToolNames(target.Active) || !validToolNames(target.RegisteredCeiling) || target.Digest != ToolPolicyDigest(target) {
		return ErrScope
	}
	for _, name := range target.Active {
		if !slices.Contains(target.RegisteredCeiling, name) || (target.Mode == "plan" && !planTools[name]) {
			return ErrScope
		}
	}
	return nil
}
func sameToolPolicy(a, b ToolPolicyTarget) bool {
	return a.Mode == b.Mode && a.Revision == b.Revision && a.Digest == b.Digest && slices.Equal(a.Active, b.Active) && slices.Equal(a.RegisteredCeiling, b.RegisteredCeiling)
}

// Positive proof must be live, exact and within the trusted registered ceiling.
// Hook/host-instruction proof is produced by the worker policy module; Applied
// is accepted only together with all of these observations and durable state.
func ValidateToolPolicyProof(proof ToolPolicyProof, target ToolPolicyTarget, generation string) error {
	if generation == "" || ValidateToolPolicy(target) != nil || !proof.Applied || proof.Generation != generation || !sameToolPolicy(proof.Policy, target) || !validToolNames(proof.Active) || !validToolNames(proof.Callable) || !validToolNames(proof.Registered) || !slices.Equal(proof.Active, target.Active) || !slices.Equal(proof.Callable, target.Active) {
		return ErrHeld
	}
	for _, name := range proof.Registered {
		if !slices.Contains(target.RegisteredCeiling, name) {
			return ErrHeld
		}
	}
	return nil
}

// The optional methods reuse the authenticated supervisor socket. No worker is
// loaded or settings changed by either transport path. Dispatch registration and
// schema-2 mapping integration remain owned by the supervisor's source window.
type toolPolicyRequest struct {
	Handle           agentbackend.ExecutionHandle
	ExpectedRevision uint64
	Target           *ToolPolicyTarget
}

func validToolPolicyRevision(revision uint64) bool {
	return revision > 0 && revision <= MaxToolPolicyRevision
}
func (m *Manager) toolPolicyCall(ctx context.Context, handle agentbackend.ExecutionHandle, operation string, body toolPolicyRequest, result any) error {
	if err := validateHandle(handle); err != nil {
		return err
	}
	if _, err := sessionDirectory(handle.Conversation.Session); err != nil {
		return err
	}
	record, err := m.record(handle.Conversation.Session)
	if err != nil {
		return err
	}
	if record.Runtime.Generation != handle.Generation {
		return ErrScope
	}
	body.Handle = handle
	return m.call(ctx, record.Runtime, operation, body, result)
}
func (m *Manager) InspectToolPolicy(ctx context.Context, handle agentbackend.ExecutionHandle) (ToolPolicyProof, error) {
	var proof ToolPolicyProof
	err := m.toolPolicyCall(ctx, handle, "tool_policy_inspect", toolPolicyRequest{}, &proof)
	if err != nil {
		return proof, err
	}
	if proof.Applied && ValidateToolPolicyProof(proof, proof.Policy, handle.Generation) != nil {
		return ToolPolicyProof{}, ErrHeld
	}
	return proof, nil
}
func (m *Manager) ApplyToolPolicy(ctx context.Context, handle agentbackend.ExecutionHandle, expected uint64, target ToolPolicyTarget) (ToolPolicyResult, error) {
	result := ToolPolicyResult{Outcome: "rejected"}
	if !validToolPolicyRevision(expected) || ValidateToolPolicy(target) != nil {
		return result, ErrScope
	}
	err := m.toolPolicyCall(ctx, handle, "tool_policy_apply", toolPolicyRequest{ExpectedRevision: expected, Target: &target}, &result)
	if err != nil {
		return result, err
	}
	switch result.Outcome {
	case "applied":
		if ValidateToolPolicyProof(result.Proof, target, handle.Generation) != nil {
			return ToolPolicyResult{Outcome: "unknown"}, ErrHeld
		}
	case "rejected", "unknown":
		if result.Outcome == "unknown" && result.Proof.Applied {
			return ToolPolicyResult{Outcome: "unknown"}, ErrHeld
		}
	default:
		return ToolPolicyResult{Outcome: "unknown"}, ErrHeld
	}
	return result, nil
}

// validateInitialToolPolicyReplay is deliberately not an initializer. Existing
// creation/native proof is still required by Create; pending state never grants
// authority to repeat setup or reconstruct an unprovable initial policy.
func validateInitialToolPolicyReplay(schema int, current, pending, requested *ToolPolicyTarget) error {
	if schema != 1 && schema != 2 {
		return ErrScope
	}
	if pending != nil {
		return ErrHeld
	}
	if (current == nil) != (requested == nil) {
		return ErrConflict
	}
	if current == nil {
		return nil
	}
	if schema != 2 {
		return ErrUnavailable
	}
	if ValidateToolPolicy(*current) != nil || ValidateToolPolicy(*requested) != nil || requested.Revision != 1 {
		return ErrScope
	}
	if current.Revision > 1 {
		return ErrUnavailable
	}
	if !sameToolPolicy(*current, *requested) {
		return ErrConflict
	}
	return nil
}

func cloneToolPolicy(target ToolPolicyTarget) ToolPolicyTarget {
	target.Active = append([]string{}, target.Active...)
	target.RegisteredCeiling = append([]string{}, target.RegisteredCeiling...)
	return target
}

// policyReadyLocked observes existing ownership/holds. It never loads a worker,
// saves a record, clears pending state or asks the native worker to repair it.
func (s *supervisor) policyReadyLocked(c *conversation, handle agentbackend.ExecutionHandle) error {
	if c.record.Schema != 2 || c.record.OwnerKind != "" || c.record.Utility != nil {
		return ErrUnavailable
	}
	if c.record.HeldReason != "" || c.record.PendingSettings != nil || c.record.PendingToolPolicy != nil {
		return ErrHeld
	}
	if c.record.ToolPolicy == nil {
		return ErrUnavailable
	}
	if c.worker == nil || !c.record.Loaded {
		return ErrUnavailable
	}
	if c.record.Generation != handle.Generation || c.record.Conversation != handle.Conversation {
		return ErrScope
	}
	c.activeMu.Lock()
	callbacksHeld := c.callbackUnknown
	c.activeMu.Unlock()
	held, err := c.receipts.held()
	if err != nil || held || callbacksHeld {
		return ErrHeld
	}
	s.callbackMu.Lock()
	toolsReady := true
	for _, name := range c.hostTools {
		if s.attachment == nil || !slices.Contains(s.attachment.tools, name) {
			toolsReady = false
			break
		}
	}
	s.callbackMu.Unlock()
	if !toolsReady {
		return ErrUnavailable
	}
	check := func() (bool, error) { return s.unexpectedChildrenFor(c.record.WorkerPID) }
	if s.childrenCheck != nil {
		check = s.childrenCheck
	}
	if unknown, err := check(); err != nil || unknown {
		return ErrHeld
	}
	return nil
}

func (s *supervisor) inspectToolPolicyLocked(ctx context.Context, c *conversation, handle agentbackend.ExecutionHandle) (ToolPolicyProof, error) {
	if err := s.policyReadyLocked(c, handle); err != nil {
		return ToolPolicyProof{}, err
	}
	var proof ToolPolicyProof
	if err := c.worker.Call(ctx, "tool_policy_inspect", struct{}{}, &proof); err != nil {
		return ToolPolicyProof{}, err
	}
	if ValidateToolPolicyProof(proof, *c.record.ToolPolicy, handle.Generation) != nil {
		return ToolPolicyProof{}, ErrHeld
	}
	return proof, nil
}

func (s *supervisor) inspectToolPolicy(ctx context.Context, handle agentbackend.ExecutionHandle) (ToolPolicyProof, error) {
	if err := s.verifyHandle(handle); err != nil {
		return ToolPolicyProof{}, err
	}
	c, err := s.member(handle.Conversation)
	if err != nil {
		return ToolPolicyProof{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return s.inspectToolPolicyLocked(ctx, c, handle)
}

func (s *supervisor) applyToolPolicy(ctx context.Context, handle agentbackend.ExecutionHandle, expected uint64, target ToolPolicyTarget) (ToolPolicyResult, error) {
	result := ToolPolicyResult{Outcome: "rejected"}
	if err := s.verifyHandle(handle); err != nil {
		return result, err
	}
	if !validToolPolicyRevision(expected) || ValidateToolPolicy(target) != nil {
		return result, ErrScope
	}
	c, err := s.member(handle.Conversation)
	if err != nil {
		return result, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := s.policyReadyLocked(c, handle); err != nil {
		return result, err
	}
	c.activeMu.Lock()
	callbacksActive := c.callbackCount != 0
	c.activeMu.Unlock()
	if callbacksActive {
		return result, ErrHeld
	}
	current := cloneToolPolicy(*c.record.ToolPolicy)
	if expected != current.Revision || !slices.Equal(target.RegisteredCeiling, current.RegisteredCeiling) {
		return result, ErrConflict
	}
	duplicate := sameToolPolicy(target, current)
	if !duplicate && (current.Revision == MaxToolPolicyRevision || target.Revision != current.Revision+1) {
		return result, ErrConflict
	}
	var snapshot workerSnapshot
	if err := c.worker.Call(ctx, "snapshot", struct{}{}, &snapshot); err != nil {
		return result, err
	}
	if snapshot.NativeID != handle.Conversation.NativeID || snapshot.Model != c.record.Settings.Model || snapshot.Provider != c.record.Settings.Provider || snapshot.Thinking != c.record.Settings.Thinking {
		return result, ErrScope
	}
	if snapshot.Busy || snapshot.Held {
		return result, ErrHeld
	}
	proof, err := s.inspectToolPolicyLocked(ctx, c, handle)
	if err != nil {
		return result, err
	}
	if duplicate {
		return ToolPolicyResult{Outcome: "applied", Proof: proof}, nil
	}
	target = cloneToolPolicy(target)
	c.record.PendingToolPolicy = &target
	if err := c.save(); err != nil {
		return ToolPolicyResult{Outcome: "unknown"}, err
	}
	var native ToolPolicyResult
	if err := c.worker.Call(ctx, "tool_policy_apply", target, &native); err != nil {
		holdConversation(c)
		return ToolPolicyResult{Outcome: "unknown"}, err
	}
	if native.Outcome != "applied" && native.Outcome != "rejected" || native.Outcome == "applied" && ValidateToolPolicyProof(native.Proof, target, handle.Generation) != nil {
		holdConversation(c)
		return ToolPolicyResult{Outcome: "unknown"}, ErrHeld
	}
	if native.Outcome == "applied" {
		c.record.ToolPolicy = &target
	}
	c.record.PendingToolPolicy = nil
	if err := c.save(); err != nil {
		// A failed completion is not positive durable proof. Restore the prior
		// applied policy and pending intent, including a best-effort disk fence.
		c.record.ToolPolicy = &current
		c.record.PendingToolPolicy = &target
		restoreErr := c.save()
		holdConversation(c)
		return ToolPolicyResult{Outcome: "unknown"}, errors.Join(err, restoreErr)
	}
	if native.Outcome == "rejected" {
		return result, nil
	}
	return ToolPolicyResult{Outcome: "applied", Proof: native.Proof}, nil
}
