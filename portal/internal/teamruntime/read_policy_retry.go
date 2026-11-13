package teamruntime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/progressobserver"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

// readPolicyPredecessors reproduces only this release's package-owned changes.
// Role, access, MCP and session identity changes remain distinct policies.
func readPolicyPredecessors(workspace, slug string, current codex.ThreadPolicy) []codex.ThreadPolicy {
	policies := []codex.ThreadPolicy{current}
	previous := current
	instructions := current.DeveloperInstructions
	if sensitive := session.SensitiveInstructions(workspace, slug); sensitive != "" {
		if !strings.HasPrefix(instructions, sensitive) {
			return policies
		}
		previousSensitive := fmt.Sprintf("This is a sensitive session. Keep its plans, state, notes, review evidence and handoffs locally under %s. Never stage or commit these records, copy them into tracked notes or other sessions, or force-add ignored records. Maintain substantive local tracking without the ordinary initial/checkpoint/archive tracking commits. Project code retains its normal Git workflow. Pass this sensitivity instruction to any delegated agent.\n\n", filepath.Join(workspace, "work", slug))
		instructions = previousSensitive + strings.TrimPrefix(instructions, sensitive)
	}
	// Restrict substitutions to the technical binding, leaving role prose intact.
	start := strings.Index(instructions, "This Codex conversation is bound to development session ")
	if start < 0 {
		return policies
	}
	boundary := strings.Index(instructions[start:], "\n\n")
	if boundary < 0 {
		return policies
	}
	boundary += start
	binding, role := instructions[:boundary], instructions[boundary+2:]
	if !strings.Contains(binding, session.CrossSessionReadInstructions+" ") {
		return policies
	}
	binding = strings.Replace(binding, session.CrossSessionReadInstructions+" ", "", 1)
	binding = strings.Replace(binding, "Before working as this session,", "Before working on session files,", 1)
	previous.DeveloperInstructions = binding + "\n\n" + role
	policies = append(policies, previous)
	if oldRole, ok := progressobserver.PreviousReadInstructions(role); ok {
		previous.DeveloperInstructions = binding + "\n\n" + oldRole
		policies = append(policies, previous)
	}
	return policies
}

func SameMemberReadPolicy(workspace, slug string, original, current codex.ThreadPolicy) bool {
	for _, policy := range readPolicyPredecessors(workspace, slug, current) {
		if reflect.DeepEqual(original, policy) {
			return true
		}
	}
	return false
}

type readPolicyRetryClient interface {
	OriginalSendOptions(string, string, string, string) (codex.TurnOptions, bool, error)
	ReconcileSendWithOptions(context.Context, string, string, string, string, codex.TurnOptions) (codex.SendReceipt, bool, error)
}

// ResolveReadPolicyRetry keeps a reserved submission's complete original tuple.
// Compacted accepted attempts can only return a proven receipt, never dispatch
// reconstructed options. New IDs always use the current policy.
func ResolveReadPolicyRetry(ctx context.Context, client readPolicyRetryClient, workspace, slug, thread, text, id, action string, current codex.TurnOptions) (codex.TurnOptions, *codex.SendReceipt, error) {
	original, found, err := client.OriginalSendOptions(thread, text, id, action)
	if err == nil {
		if !found {
			return current, nil, nil
		}
		if !SameMemberReadPolicy(workspace, slug, original.ThreadPolicy, current.ThreadPolicy) {
			return codex.TurnOptions{}, nil, errors.New("member policy changed since this message was attempted")
		}
		return original, nil, nil
	}
	// The pinned SDK exposes this particular compacted-options condition as
	// an untyped error, after validating the exact ID/text/action identity.
	if err.Error() != "original message options are unavailable" {
		return codex.TurnOptions{}, nil, err
	}
	for _, policy := range readPolicyPredecessors(workspace, slug, current.ThreadPolicy) {
		candidate := current
		candidate.ThreadPolicy = policy
		receipt, accepted, reconcileErr := client.ReconcileSendWithOptions(ctx, thread, text, id, action, candidate)
		if reconcileErr == nil && accepted {
			return codex.TurnOptions{}, &receipt, nil
		}
		if reconcileErr != nil && reconcileErr.Error() != "message identity was reused for another action" {
			return codex.TurnOptions{}, nil, reconcileErr
		}
	}
	return codex.TurnOptions{}, nil, err
}
