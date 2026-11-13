package piquestions

import (
	"context"
	"os"
	"testing"
)

func TestQuestionsReadDetachesNestedEvidenceAndRefusesMalformedState(t *testing.T) {
	ctx := context.Background()
	s, c := fixture(t)
	path, err := s.file(c.Handle.Conversation).Path()
	if err != nil {
		t.Fatal(err)
	}
	if qs, err := s.Questions(ctx, c.Handle.Conversation); err != nil || len(qs) != 0 {
		t.Fatal(qs, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("read created state", err)
	}
	q, err := s.Publish(ctx, c, PublishInput{Questions: []Question{{Title: "Which?", Options: []Option{{Label: "One"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	in := ResponseInput{ResponseID: UUID(), QuestionID: q.ID, QuestionRevision: q.Revision, Answers: []Answer{{QuestionID: "0", ChoiceID: "0", Text: "saved"}}}
	if _, err := s.AcceptResponse(ctx, c.Handle.Conversation, in); err != nil {
		t.Fatal(err)
	}
	qs, err := s.Questions(ctx, c.Handle.Conversation)
	if err != nil {
		t.Fatal(err)
	}
	qs[0].Questions[0].Title = "changed"
	qs[0].Questions[0].Options[0].Label = "changed"
	qs[0].Responses[0].Input.Answers[0].Text = "changed"
	qs[0].Caller.NativeToolCallID = "changed"
	again, err := s.Questions(ctx, c.Handle.Conversation)
	if err != nil || again[0].Questions[0].Title != "Which?" || again[0].Questions[0].Options[0].Label != "One" || again[0].Responses[0].Input.Answers[0].Text != "saved" || again[0].Caller != c {
		t.Fatal(again, err)
	}
	if err := os.WriteFile(path, []byte(`{"schema":1,"unknown":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Questions(ctx, c.Handle.Conversation); err == nil {
		t.Fatal("malformed state became empty")
	}
}
