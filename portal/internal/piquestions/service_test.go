package piquestions

import (
	"context"
	"errors"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

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
