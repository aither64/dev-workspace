package monitoroperation

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"time"

	"github.com/aither64/codex-web/codex"
)

// ProofObservation contains only local timing and typed/owning result classes.
// It is not persisted and grants no authority to alter or stop the proof.
type ProofObservation struct {
	Stage, MonitorID, EntryContext, Result string
	Started                                time.Time
	Duration, Remaining                    time.Duration
}

type ProofObserver func(ProofObservation)
type proofObserverKey struct{}

func WithProofObserver(ctx context.Context, observer ProofObserver) context.Context {
	return context.WithValue(ctx, proofObserverKey{}, observer)
}

// BeginProof observes the original context without replacing its deadline.
// Finish must receive the immediate error before wrapping/joining later causes.
func BeginProof(ctx context.Context, id, stage string) func(error, bool) {
	observer, _ := ctx.Value(proofObserverKey{}).(ProofObserver)
	if observer == nil {
		return func(error, bool) {}
	}
	row := ProofObservation{Stage: stage, MonitorID: id, Started: time.Now(), Remaining: -1, EntryContext: ProofErrorClass(ctx.Err())}
	if deadline, ok := ctx.Deadline(); ok {
		row.Remaining = deadline.Sub(row.Started)
		if row.Remaining < 0 {
			row.Remaining = 0
		}
	}
	return func(err error, mismatch bool) {
		row.Duration, row.Result = time.Since(row.Started), ProofErrorClass(err)
		if err == nil && mismatch {
			row.Result = "identity-mismatch"
		}
		observer(row)
	}
}

// Unknown native refusals stay other: error prose is never a classifier.
func ProofErrorClass(err error) string {
	if err == nil {
		return "success"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	var transport *codex.TransportError
	var network net.Error
	if errors.As(err, &transport) || errors.As(err, &network) {
		return "transport"
	}
	var syntax *json.SyntaxError
	var shape *json.UnmarshalTypeError
	if errors.As(err, &syntax) || errors.As(err, &shape) {
		return "decode"
	}
	if _, ok := codex.RPCErrorData(err); ok {
		return "rpc"
	}
	var detail *DiagnosticError
	if errors.As(err, &detail) && detail.Category == "identity-mismatch" && detail.Cause == nil {
		return "identity-mismatch"
	}
	return "other"
}
