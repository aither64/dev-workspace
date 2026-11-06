package turnretry

import (
	"context"
	"errors"
	"os"
)

// ProofFailure keeps a safe stage separate from errors that may contain native
// response text. Logs publish only Diagnostic, never the underlying message.
type ProofFailure struct {
	Stage string
	Err   error
}

func (e *ProofFailure) Error() string { return e.Stage + ": " + e.Err.Error() }
func (e *ProofFailure) Unwrap() error { return e.Err }

func Diagnostic(err error) string {
	var proof *ProofFailure
	if errors.As(err, &proof) {
		switch proof.Stage {
		case "binding_unavailable", "history_read_failed", "history_invalid", "input_unmatched", "context_read_failed":
			return proof.Stage
		}
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, ErrProofIncomplete):
		return "proof_pending"
	case errors.Is(err, ErrMissingProof):
		return "settings_unproven"
	case errors.Is(err, ErrPolicyUnavailable):
		return "policy_unavailable"
	case errors.Is(err, os.ErrPermission):
		return "storage_permission"
	default:
		return "observation_or_admission_unavailable"
	}
}
