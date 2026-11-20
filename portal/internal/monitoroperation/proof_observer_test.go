package monitoroperation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
)

func TestMonitorProofObserverTypedClasses(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{nil, "success"}, {context.DeadlineExceeded, "deadline"}, {context.Canceled, "canceled"},
		{&codex.TransportError{Err: errors.New("PRIVATE")}, "transport"}, {&net.OpError{Op: "PRIVATE"}, "transport"},
		{&json.SyntaxError{}, "decode"}, {&json.UnmarshalTypeError{}, "decode"},
		{errors.New("deadline transport decode PRIVATE"), "other"},
		{&DiagnosticError{Category: "identity-mismatch", Err: errors.New("PRIVATE")}, "identity-mismatch"},
	} {
		if got := ProofErrorClass(tc.err); got != tc.want {
			t.Fatalf("class %s != %s", got, tc.want)
		}
		if tc.err != nil && ProofErrorClass(fmt.Errorf("PRIVATE: %w", tc.err)) != tc.want {
			t.Fatal("wrapped type lost")
		}
	}
}

func TestMonitorProofObserverPreservesCooperativeContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	var rows []ProofObservation
	ctx = WithProofObserver(ctx, func(row ProofObservation) { rows = append(rows, row) })
	finish := BeginProof(ctx, "fixture", "native-thread")
	<-ctx.Done()
	// Local work may finish after cancellation; observation cannot relabel a
	// successful immediate result or forcibly return on the deadline.
	finish(nil, false)
	BeginProof(ctx, "fixture", "native-idle")(ctx.Err(), false)
	if len(rows) != 2 || rows[0].Result != "success" || rows[1].EntryContext != "deadline" || rows[1].Remaining != 0 || rows[1].Result != "deadline" {
		t.Fatal(rows)
	}
	BeginProof(context.Background(), "", "unused")(nil, false)
}
