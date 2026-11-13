package piquestions

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

func TestQuestionOriginPreservesOrderBytesAndCanonicalIndexes(t *testing.T) {
	ctx := context.Background()
	s, c := fixture(t)
	q, err := s.Publish(ctx, c, PublishInput{[]Question{{Title: " é\n", Options: []Option{{Label: "One"}, {Label: "Two"}}}, {Title: "Second"}}})
	if err != nil {
		t.Fatal(err)
	}
	r := fresh(c.Handle.Conversation)
	if err = s.file(c.Handle.Conversation).Read(ctx, &r); err != nil {
		t.Fatal(err)
	}
	record := r.Questions[0]
	for _, raw := range []string{`{"questions":[{"title":" é\n","options":["One","Two"]},{"title":"Second"}]}`, `{"questions":[{"title":" \u00e9\n","options":["One","Two"]},{"title":"Second","options":[]}]}`} {
		if err = VerifyNativeArguments(record, []byte(raw)); err != nil {
			t.Fatal(raw, err)
		}
	}
	for _, raw := range []string{`{"questions":[{"title":"é\n","options":["One","Two"]},{"title":"Second"}]}`, `{"questions":[{"title":" é\n","options":["One","Two"]},{"title":"Second"}]}`, `{"questions":[{"title":" é\n","options":["Two","One"]},{"title":"Second"}]}`, `{"questions":[{"title":"Second"},{"title":" é\n","options":["One","Two"]}]}`, `{"questions":[{"title":" é\n","options":["One","Two"]},{"title":"Second","options":null}]}`, `{"questions":[{"title":" é\n","id":"0","options":["One","Two"]},{"title":"Second"}]}`, `{"questions":[{"title":"\ud800"}]}`} {
		if VerifyNativeArguments(record, []byte(raw)) == nil {
			t.Fatal("accepted altered native args", raw)
		}
	}
	record.Questions[0].ID = "99"
	if VerifyNativeArguments(record, []byte(`{"questions":[{"title":" é\n","options":["One","Two"]},{"title":"Second"}]}`)) == nil {
		t.Fatal("noncanonical saved IDs accepted")
	}
	record.Questions[0].ID = "0"
	record.Questions[0].Options[0].ID = "99"
	if VerifyNativeArguments(record, []byte(`{"questions":[{"title":" é\n","options":["One","Two"]},{"title":"Second"}]}`)) == nil {
		t.Fatal("noncanonical option indexes accepted")
	}
	_ = q
}
func TestQuestionCaptureOriginOutsideLockAndDuplicateWithoutReader(t *testing.T) {
	ctx := context.Background()
	s, c := fixture(t)
	q, err := s.Publish(ctx, c, PublishInput{[]Question{{Title: "Which?"}}})
	if err != nil {
		t.Fatal(err)
	}
	checks := 0
	wrapper := AdmittedStore{Store: s, Admit: func(ctx context.Context, _ agentbackend.ConversationRef, fn func(context.Context) error) error {
		return fn(ctx)
	}, Current: func(_ context.Context, c Caller) (Caller, error) { return c, nil }, VerifyOrigin: func(ctx context.Context, _ Caller, record QuestionRecord) error {
		checks++
		bounded, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		state := fresh(c.Handle.Conversation)
		if err := s.file(c.Handle.Conversation).Update(bounded, &state, func() error { return nil }); err != nil {
			return err
		}
		return VerifyNativeArguments(record, []byte(`{"questions":[{"title":"Which?"}]}`))
	}}
	in := ResponseInput{ResponseID: UUID(), QuestionID: q.ID, QuestionRevision: 1, Answers: []Answer{{QuestionID: "0", Text: "answer"}}}
	a, err := wrapper.AcceptResponse(ctx, c.Handle.Conversation, in)
	if err != nil || checks != 1 {
		t.Fatal(a, err, checks)
	}
	wrapper.Current = nil
	wrapper.VerifyOrigin = nil
	wrapper.Admit = nil
	again, err := wrapper.AcceptResponse(ctx, c.Handle.Conversation, in)
	if err != nil || again.Sequence != a.Sequence || checks != 1 {
		t.Fatal(again, err, checks)
	}
	in.ResponseID = UUID()
	in.SupersedesResponseID = a.ResponseID
	if _, err = wrapper.AcceptResponse(ctx, c.Handle.Conversation, in); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
}

func TestMissingQuestionOriginHoldsNewSendButNotOriginalLookup(t *testing.T) {
	ctx := context.Background()
	s, c := fixture(t)
	q, err := s.Publish(ctx, c, PublishInput{[]Question{{Title: "Question"}}})
	if err != nil {
		t.Fatal(err)
	}
	input := ResponseInput{ResponseID: UUID(), QuestionID: q.ID, QuestionRevision: 1, Answers: []Answer{{QuestionID: "0", Text: "answer"}}}
	if _, err = s.AcceptResponse(ctx, c.Handle.Conversation, input); err != nil {
		t.Fatal(err)
	}
	lookups := 0
	svc := Service{Store: s, Admit: func(ctx context.Context, _ agentbackend.ConversationRef, fn func(context.Context) error) error {
		return fn(ctx)
	}, Dispatch: Dispatch{Current: func(_ context.Context, c Caller) (Caller, error) { return c, nil }, Lookup: func(_ context.Context, _ Caller, d Delivery) (string, error) {
		lookups++
		if d.OperationID != input.ResponseID || d.ReceiptRef != "original" {
			t.Fatal(d)
		}
		return "Unknown", nil
	}}}
	if err = svc.DeliverNext(ctx, c.Handle.Conversation); !errors.Is(err, ErrUnavailable) || lookups != 0 {
		t.Fatal(err, lookups)
	}
	if err = s.setDelivery(ctx, c.Handle.Conversation, input.ResponseID, func(d *Delivery) error { d.Phase = "handoff"; d.ReceiptRef = "original"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err = svc.DeliverNext(ctx, c.Handle.Conversation); err != nil || lookups != 1 {
		t.Fatal(err, lookups)
	}
}
func TestQuestionCaptureRechecksClosureAfterOriginInspection(t *testing.T) {
	ctx := context.Background()
	s, c := fixture(t)
	q, err := s.Publish(ctx, c, PublishInput{[]Question{{Title: "Question"}}})
	if err != nil {
		t.Fatal(err)
	}
	wrapper := AdmittedStore{Store: s, Admit: func(ctx context.Context, _ agentbackend.ConversationRef, fn func(context.Context) error) error {
		return fn(ctx)
	}, Current: func(_ context.Context, c Caller) (Caller, error) { return c, nil }, VerifyOrigin: func(ctx context.Context, c Caller, q QuestionRecord) error {
		_, err := s.Close(ctx, c, q.ID, q.Revision)
		return err
	}}
	input := ResponseInput{ResponseID: UUID(), QuestionID: q.ID, QuestionRevision: 1, Answers: []Answer{{QuestionID: "0", Text: "answer"}}}
	if _, err = wrapper.AcceptResponse(ctx, c.Handle.Conversation, input); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if _, err = s.Response(ctx, c.Handle.Conversation, input.ResponseID); !errors.Is(err, ErrNotFound) {
		t.Fatal("captured after closure", err)
	}
}

func TestOriginJSONUnicodeAndDuplicateFields(t *testing.T) {
	record := QuestionRecord{Questions: []Question{{ID: "0", Title: "😀", Options: []Option{}}}}
	if VerifyNativeArguments(record, []byte(`{"questions":[{"title":"\ud83d\ude00"}]}`)) != nil {
		t.Fatal("valid surrogate pair rejected")
	}
	for _, raw := range []string{`{"questions":[{"title":"\ud83d"}]}`, `{"questions":[{"title":"\ude00"}]}`, `{"questions":[{"title":"bad","title":"😀"}]}`, `{"questions":[{"title":"😀","options":[null]}]}`} {
		if VerifyNativeArguments(record, []byte(raw)) == nil {
			t.Fatal(raw)
		}
	}
}
