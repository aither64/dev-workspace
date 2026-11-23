package piquestions

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

func TestDeliveryPreflightIsValidatedAndNeverAuthorizesDispatch(t *testing.T) {
	ctx := context.Background()
	s, c := fixture(t)
	ref := c.Handle.Conversation
	if needed, err := s.NeedsDelivery(ctx, ref); err != nil || needed {
		t.Fatal("missing store needs no delivery", needed, err)
	}
	q, err := s.Publish(ctx, c, PublishInput{[]Question{{Title: "Question"}}})
	if err != nil {
		t.Fatal(err)
	}
	in := ResponseInput{ResponseID: UUID(), QuestionID: q.ID, QuestionRevision: 1, Answers: []Answer{{QuestionID: "0", Text: "answer"}}}
	if _, err := s.AcceptResponse(ctx, ref, in); err != nil {
		t.Fatal(err)
	}
	if needed, err := s.NeedsDelivery(ctx, ref); err != nil || !needed {
		t.Fatal("pending intent not found", needed, err)
	}
	var admission sync.Mutex
	sends := 0
	svc := Service{Store: s, Admit: func(ctx context.Context, _ agentbackend.ConversationRef, run func(context.Context) error) error {
		admission.Lock()
		defer admission.Unlock()
		return run(ctx)
	}, Dispatch: Dispatch{Current: func(_ context.Context, caller Caller) (Caller, error) { return caller, nil },
		VerifyOrigin: func(context.Context, Caller, QuestionRecord) error { return nil },
		Ready:        func(context.Context, Caller) (bool, error) { return true, nil },
		Prepare:      func(context.Context, Caller, Delivery) (string, error) { return "original", nil },
		Send:         func(context.Context, Caller, Delivery) (string, error) { sends++; return "Accepted", nil },
		Lookup:       func(context.Context, Caller, Delivery) (string, error) { return "Accepted", nil }}}
	// Both drainers saw the positive hint before either entered exclusive
	// admission. Selection must be repeated there, so the second cannot send.
	results := make(chan error, 2)
	for range 2 {
		go func() { results <- svc.DeliverNext(ctx, ref) }()
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if sends != 1 {
		t.Fatal("preflight caused duplicate dispatch", sends)
	}
	if needed, err := s.NeedsDelivery(ctx, ref); err != nil || needed {
		t.Fatal("accepted response needs no delivery", needed, err)
	}
	s.Validate = func(context.Context, agentbackend.ConversationRef, *Caller) error { return ErrConflict }
	if _, err := s.NeedsDelivery(ctx, ref); !errors.Is(err, ErrConflict) {
		t.Fatal("preflight bypassed owning-record validation", err)
	}
}

func TestDeliveryPreflightDoesNotBypassFreshCallerOriginOrReadyChecks(t *testing.T) {
	for _, changed := range []string{"caller", "origin", "policy"} {
		t.Run(changed, func(t *testing.T) {
			ctx := context.Background()
			s, c := fixture(t)
			ref := c.Handle.Conversation
			q, err := s.Publish(ctx, c, PublishInput{[]Question{{Title: "Question"}}})
			if err != nil {
				t.Fatal(err)
			}
			in := ResponseInput{ResponseID: UUID(), QuestionID: q.ID, QuestionRevision: 1, Answers: []Answer{{QuestionID: "0", Text: "answer"}}}
			if _, err := s.AcceptResponse(ctx, ref, in); err != nil {
				t.Fatal(err)
			}
			if needed, err := s.NeedsDelivery(ctx, ref); err != nil || !needed {
				t.Fatal(needed, err)
			}
			svc := Service{Store: s, Admit: func(ctx context.Context, _ agentbackend.ConversationRef, run func(context.Context) error) error {
				return run(ctx)
			},
				Dispatch: Dispatch{Current: func(_ context.Context, caller Caller) (Caller, error) {
					if changed == "caller" {
						caller.Handle.Conversation.Incarnation = "changed"
					}
					return caller, nil
				}, VerifyOrigin: func(context.Context, Caller, QuestionRecord) error {
					if changed == "origin" {
						return ErrConflict
					}
					return nil
				}, Ready: func(context.Context, Caller) (bool, error) { return changed != "policy", nil },
					Prepare: func(context.Context, Caller, Delivery) (string, error) {
						t.Fatal("stale hint prepared input")
						return "", nil
					},
					Send: func(context.Context, Caller, Delivery) (string, error) {
						t.Fatal("stale hint sent input")
						return "", nil
					},
					Lookup: func(context.Context, Caller, Delivery) (string, error) { return "Unknown", nil }}}
			_ = svc.DeliverNext(ctx, ref)
		})
	}
}

func TestBusyAndUnknownNeverResend(t *testing.T) {
	for _, outcome := range []string{"Unknown", "NotSubmitted", "Accepted"} {
		t.Run(outcome, func(t *testing.T) {
			ctx := context.Background()
			s, c := fixture(t)
			q, err := s.Publish(ctx, c, PublishInput{[]Question{{Title: "Question"}}})
			if err != nil {
				t.Fatal(err)
			}
			in := ResponseInput{ResponseID: UUID(), QuestionID: q.ID, QuestionRevision: 1, Answers: []Answer{{QuestionID: "0", Text: "/a literal answer"}}}
			if _, err = s.AcceptResponse(ctx, c.Handle.Conversation, in); err != nil {
				t.Fatal(err)
			}
			ready := false
			sends := 0
			svc := Service{Store: s, Admit: func(ctx context.Context, _ agentbackend.ConversationRef, fn func(context.Context) error) error {
				return fn(ctx)
			}, Dispatch: Dispatch{VerifyOrigin: func(context.Context, Caller, QuestionRecord) error { return nil }, Current: func(_ context.Context, c Caller) (Caller, error) { return c, nil }, Ready: func(context.Context, Caller) (bool, error) { return ready, nil }, Prepare: func(context.Context, Caller, Delivery) (string, error) { return "original-receipt", nil }, Send: func(_ context.Context, _ Caller, d Delivery) (string, error) {
				sends++
				if d.OperationID != in.ResponseID || d.ReceiptRef != "original-receipt" {
					t.Fatal(d)
				}
				return outcome, nil
			}, Lookup: func(context.Context, Caller, Delivery) (string, error) { return outcome, nil }}}
			if err = svc.DeliverNext(ctx, c.Handle.Conversation); err != nil || sends != 0 {
				t.Fatal(err, sends)
			}
			ready = true
			for range 3 {
				if err = svc.DeliverNext(ctx, c.Handle.Conversation); err != nil {
					t.Fatal(err)
				}
			}
			if sends != 1 {
				t.Fatal(sends)
			}
			_, err = s.Response(ctx, c.Handle.Conversation, in.ResponseID)
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestMissingDispatchIsUnsupported(t *testing.T) {
	svc := Service{}
	if !errors.Is(svc.DeliverNext(context.Background(), agentbackend.ConversationRef{}), ErrUnavailable) {
		t.Fatal("missing support accepted")
	}
}

func TestPersistedHandoffAfterCrashUsesLookupOnly(t *testing.T) {
	ctx := context.Background()
	s, c := fixture(t)
	q, err := s.Publish(ctx, c, PublishInput{[]Question{{Title: "Question"}}})
	if err != nil {
		t.Fatal(err)
	}
	in := ResponseInput{ResponseID: UUID(), QuestionID: q.ID, QuestionRevision: 1, Answers: []Answer{{QuestionID: "0", Text: "answer"}}}
	if _, err = s.AcceptResponse(ctx, c.Handle.Conversation, in); err != nil {
		t.Fatal(err)
	}
	if err = s.setDelivery(ctx, c.Handle.Conversation, in.ResponseID, func(d *Delivery) error { d.Phase = "handoff"; d.ReceiptRef = "original"; return nil }); err != nil {
		t.Fatal(err)
	}
	lookups := 0
	svc := Service{Store: s, Admit: func(ctx context.Context, _ agentbackend.ConversationRef, fn func(context.Context) error) error {
		return fn(ctx)
	}, Dispatch: Dispatch{VerifyOrigin: func(context.Context, Caller, QuestionRecord) error { return nil }, Current: func(_ context.Context, c Caller) (Caller, error) { return c, nil }, Ready: func(context.Context, Caller) (bool, error) {
		t.Fatal("readiness after uncertain handoff")
		return false, nil
	}, Prepare: func(context.Context, Caller, Delivery) (string, error) { t.Fatal("second prepare"); return "", nil }, Send: func(context.Context, Caller, Delivery) (string, error) { t.Fatal("resend"); return "", nil }, Lookup: func(_ context.Context, _ Caller, d Delivery) (string, error) {
		lookups++
		if d.ReceiptRef != "original" {
			t.Fatal(d)
		}
		return "Unknown", nil
	}}}
	if err = svc.DeliverNext(ctx, c.Handle.Conversation); err != nil || lookups != 1 {
		t.Fatal(err, lookups)
	}
}
