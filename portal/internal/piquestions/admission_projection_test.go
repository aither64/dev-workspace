package piquestions

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

func TestQuestionProjectionRequiresExactContentProofWithoutNativeCalls(t *testing.T) {
	ctx := context.Background()
	s, c := fixture(t)
	view, err := s.Publish(ctx, c, PublishInput{Questions: []Question{{Title: "Which?", Options: []Option{{Label: "One"}, {Label: "Two"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	current := c.Handle
	current.Generation = "restarted"
	o := QuestionObservation{Current: current, BindingRevision: 2, ObservedBindingRevision: 2, QuestionID: view.ID, Revision: view.Revision, Caller: c,
		Origin: piruntime.NativeOriginObservation{Handle: current, Origin: piruntime.NativeOriginRef{AnchorEntryID: c.Lineage.AnchorEntryID, SelectionToken: c.Lineage.SelectionToken, NativeToolCallID: c.NativeToolCallID, Tool: "request_user_input_async"}, Arguments: json.RawMessage(`{"questions":[{"title":"Which?","options":["One","Two"]}]}`)}, CaptureAllowed: true, Callbacks: 1, LinearLineage: 1, GuardQuestionsSchema: 1}
	values := []QuestionObservation{o}
	var observationErr error
	wrapper := AdmittedStore{Store: s, Observations: func(context.Context, agentbackend.ConversationRef) ([]QuestionObservation, error) {
		return values, observationErr
	},
		Admit: func(context.Context, agentbackend.ConversationRef, func(context.Context) error) error {
			t.Fatal("snapshot acquired mutation admission")
			return nil
		},
		Current: func(context.Context, Caller) (Caller, error) {
			t.Fatal("snapshot called current native authority")
			return Caller{}, nil
		},
		VerifyOrigin: func(context.Context, Caller, QuestionRecord) error {
			t.Fatal("snapshot inspected native origin")
			return nil
		}}
	check := func(want bool) {
		t.Helper()
		snapshot, err := wrapper.Snapshot(ctx, c.Handle.Conversation)
		if err != nil || snapshot.Questions[0].CanRespond != want || snapshot.Questions[0].Status != "pending" {
			t.Fatal(snapshot, err)
		}
	}
	check(true) // Busy capture needs origin/authority proof, not idle evidence.
	for _, change := range []func(*QuestionObservation){
		func(o *QuestionObservation) { o.QuestionID = "other" }, func(o *QuestionObservation) { o.Revision++ },
		func(o *QuestionObservation) { o.Caller.Handle.Generation = "changed saved caller" },
		func(o *QuestionObservation) { o.Current.Conversation.NativeID = "other" },
		func(o *QuestionObservation) { o.BindingRevision = 0 }, func(o *QuestionObservation) { o.ObservedBindingRevision++ },
		func(o *QuestionObservation) { o.Origin.Handle.Generation = "old" }, func(o *QuestionObservation) { o.Origin.Origin.AnchorEntryID = "other" },
		func(o *QuestionObservation) { o.Origin.Origin.NativeToolCallID = "other" }, func(o *QuestionObservation) { o.Origin.Origin.SelectionToken = "other" },
		func(o *QuestionObservation) { o.Origin.Origin.Tool = "publish_plan" },
		func(o *QuestionObservation) {
			o.Origin.Arguments = json.RawMessage(`{"questions":[{"title":"Which?","options":["Two","One"]}]}`)
		},
		func(o *QuestionObservation) { o.CaptureAllowed = false }, func(o *QuestionObservation) { o.Callbacks = 0 },
		func(o *QuestionObservation) { o.LinearLineage = 0 }, func(o *QuestionObservation) { o.GuardQuestionsSchema = 0 },
	} {
		changed := o
		change(&changed)
		values = []QuestionObservation{changed}
		check(false)
	}
	values = []QuestionObservation{o, o}
	check(false)
	values = []QuestionObservation{o}
	observationErr = errors.New("unavailable cache")
	check(false)
	observationErr = nil
	wrapper.Observations = nil
	check(false)
	wrapper.Observations = func(context.Context, agentbackend.ConversationRef) ([]QuestionObservation, error) {
		return []QuestionObservation{o}, nil
	}
	wrapper.VerifyOrigin = nil
	check(false)
	wrapper.VerifyOrigin = func(context.Context, Caller, QuestionRecord) error { return nil }
	if _, err := s.Close(ctx, c, view.ID, view.Revision); err != nil {
		t.Fatal(err)
	}
	closed, err := wrapper.Snapshot(ctx, c.Handle.Conversation)
	if err != nil || closed.Questions[0].CanRespond || closed.Questions[0].Status != "closed" {
		t.Fatal(closed, err)
	}
}
