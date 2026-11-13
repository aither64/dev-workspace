package piruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

func receiptFixture(t *testing.T) (*receiptEngine, SubmissionRef, SendInput, EffectiveSettings) {
	t.Helper()
	conversation := agentbackend.ConversationRef{Session: agentbackend.SessionRef{Workspace: "/tmp", Slug: "2026-11-12-fixture", Incarnation: "session"}, Backend: agentbackend.Pi, Member: "lead", Incarnation: "member", NativeID: "native-session"}
	ref := SubmissionRef{Conversation: conversation, OperationID: "11111111-1111-4111-8111-111111111111", Action: "send"}
	input := SendInput{Message: "identical text"}
	settings := EffectiveSettings{Provider: "fake", Model: "model", Revision: 1}
	ref.Digest = SubmissionDigest(ref, input, settings)
	return &receiptEngine{root: filepath.Join(t.TempDir(), "submissions")}, ref, input, settings
}

func TestReceiptCrashBoundariesNeverRedispatch(t *testing.T) {
	for _, point := range []string{"intent", "dispatch", "native", "witness", "receipt"} {
		t.Run(point, func(t *testing.T) {
			engine, ref, input, settings := receiptFixture(t)
			calls, aborts := 0, 0
			engine.checkpoint = func(at string) error {
				if at == point {
					return errors.New("injected process loss")
				}
				return nil
			}
			dispatch := func(context.Context) (nativeResult, error) {
				calls++
				return nativeResult{Outcome: "Accepted", Witness: &workerWitness{SessionID: ref.Conversation.NativeID, EntryID: "entry", EffectiveMessage: input.Message}}, nil
			}
			if _, err := engine.send(context.Background(), ref, input, settings, "old-generation", dispatch, func() { aborts++ }); err == nil {
				t.Fatal("fault was not injected")
			}
			restarted := &receiptEngine{root: engine.root}
			status, err := restarted.lookup(ref)
			if err != nil {
				t.Fatal(err)
			}
			want := "Unknown"
			if point == "receipt" {
				want = "Accepted"
			}
			if status.Phase != want {
				t.Fatalf("got %s want %s", status.Phase, want)
			}
			before := calls
			replayed, err := restarted.send(context.Background(), ref, input, settings, "new-generation", dispatch, func() { aborts++ })
			if err != nil || replayed.Phase != want || calls != before {
				t.Fatalf("retry dispatched or changed outcome: %+v %v calls=%d", replayed, err, calls)
			}
			if replayed.Generation != "old-generation" {
				t.Fatal("generation made a new dedup namespace")
			}
			changed := input
			changed.Message = "changed"
			conflict := ref
			conflict.Digest = SubmissionDigest(conflict, changed, settings)
			if _, err := restarted.send(context.Background(), conflict, changed, settings, "new", dispatch, func() {}); !errors.Is(err, ErrConflict) {
				t.Fatalf("changed duplicate: %v", err)
			}
			if (point == "native" || point == "witness") && aborts != 1 {
				t.Fatal("uncertain affected worker was not aborted")
			}
		})
	}
}

func TestReceiptRejectsInvalidWitnessAndKeepsRefusalTerminal(t *testing.T) {
	engine, ref, input, settings := receiptFixture(t)
	status, err := engine.send(context.Background(), ref, input, settings, "generation", func(context.Context) (nativeResult, error) {
		return nativeResult{Outcome: "Accepted", Witness: &workerWitness{SessionID: "foreign", EntryID: "entry", EffectiveMessage: input.Message}}, nil
	}, func() {})
	if err != nil || status.Phase != "Unknown" {
		t.Fatalf("foreign witness %+v %v", status, err)
	}
	other := ref
	other.OperationID = "22222222-2222-4222-8222-222222222222"
	other.Digest = SubmissionDigest(other, input, settings)
	if _, err := engine.send(context.Background(), other, input, settings, "generation", func(context.Context) (nativeResult, error) {
		t.Fatal("held conversation dispatched")
		return nativeResult{}, nil
	}, func() {}); !errors.Is(err, ErrHeld) {
		t.Fatal(err)
	}
	engine, ref, input, settings = receiptFixture(t)
	calls := 0
	refuse := func(context.Context) (nativeResult, error) {
		calls++
		return nativeResult{Outcome: "NotSubmitted", Refusal: "worker busy before native call"}, nil
	}
	for i := 0; i < 2; i++ {
		status, err = engine.send(context.Background(), ref, input, settings, "generation", refuse, func() {})
		if err != nil || status.Phase != "NotSubmitted" {
			t.Fatal(status, err)
		}
	}
	if calls != 1 {
		t.Fatal("NotSubmitted was automatically resubmitted")
	}
}

func TestReceiptRetains32HexOperationIdentity(t *testing.T) {
	engine, ref, input, settings := receiptFixture(t)
	ref.OperationID = "0123456789abcdef0123456789abcdef"
	ref.Digest = SubmissionDigest(ref, input, settings)
	status, err := engine.send(context.Background(), ref, input, settings, "generation", func(context.Context) (nativeResult, error) {
		return nativeResult{Outcome: "NotSubmitted", Refusal: "before native call"}, nil
	}, func() {})
	if err != nil || status.Ref.OperationID != ref.OperationID {
		t.Fatal(status, err)
	}
	loaded, err := engine.lookup(ref)
	if err != nil || loaded.Ref.OperationID != ref.OperationID {
		t.Fatal(loaded, err)
	}
}

func TestOperationIDsKeepExactSpellingAndRejectPaths(t *testing.T) {
	engine, ref, input, settings := receiptFixture(t)
	compact := "0123456789abcdef0123456789abcdef"
	hyphenated := "01234567-89ab-cdef-0123-456789abcdef"
	calls := 0
	dispatch := func(context.Context) (nativeResult, error) {
		calls++
		return nativeResult{Outcome: "NotSubmitted", Refusal: "fixture"}, nil
	}
	for _, id := range []string{compact, hyphenated} {
		ref.OperationID = id
		ref.Digest = SubmissionDigest(ref, input, settings)
		for i := 0; i < 2; i++ {
			status, err := engine.send(context.Background(), ref, input, settings, "g", dispatch, func() {})
			if err != nil || status.Ref.OperationID != id {
				t.Fatal(status, err)
			}
		}
		if _, err := os.Stat(filepath.Join(engine.root, id+".json")); err != nil {
			t.Fatal(err)
		}
		changed := input
		changed.Message = "conflict"
		other := ref
		other.Digest = SubmissionDigest(other, changed, settings)
		if _, err := engine.send(context.Background(), other, changed, settings, "g", dispatch, func() {}); !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatal("distinct IDs aliased or duplicate dispatched", calls)
	}
	for _, id := range []string{strings.ToUpper(compact), "../" + compact, compact + "0", compact[:31], " " + hyphenated, hyphenated + " ", ""} {
		ref.OperationID = id
		ref.Digest = SubmissionDigest(ref, input, settings)
		if _, err := engine.lookup(ref); !errors.Is(err, ErrScope) {
			t.Fatal("lookup accepted invalid ID", id, err)
		}
		if _, err := engine.send(context.Background(), ref, input, settings, "g", dispatch, func() {}); !errors.Is(err, ErrScope) {
			t.Fatal("send accepted invalid ID", id, err)
		}
	}
	if calls != 2 {
		t.Fatal("invalid ID dispatched")
	}
}
