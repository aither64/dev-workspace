package agentteams

import (
	"errors"
	"strings"
)

// PiPolicy is a projection of already selected retained settings, not a new
// catalog selection or a permission grant. Pi has no implied OS sandbox.
type PiPolicy struct {
	Model, Effort, Behavior, Purpose, Instructions string
	SavedAccess, ExecutionAccess                   string
}

// ProjectPiPolicy preserves the saved role, including reviewer independence.
// Provider/model/thinking availability is validated separately by P4.
func ProjectPiPolicy(role Role, executionAccess string) (PiPolicy, error) {
	p := PiPolicy{Model: role.Model, Effort: role.Effort, Behavior: role.Behavior,
		Purpose: role.Purpose, Instructions: role.Instructions,
		SavedAccess: role.Access, ExecutionAccess: executionAccess}
	return p, p.Validate()
}

func (p PiPolicy) Validate() error {
	if !nonempty(p.Model, 4096) || !supportedEfforts[p.Effort] ||
		!validPurpose(p.Purpose, p.Behavior) || !nonempty(p.Instructions, 4096) ||
		strings.ContainsRune(p.Model+p.Instructions, 0) ||
		(p.SavedAccess != "read_only" && p.SavedAccess != "workspace_write") ||
		(p.ExecutionAccess != "" && p.ExecutionAccess != "full_access") ||
		(p.Purpose == "review" && p.SavedAccess != "read_only") ||
		(p.Purpose == "implementation" && p.SavedAccess != "workspace_write") {
		return errors.New("invalid retained Pi role policy")
	}
	return nil
}
