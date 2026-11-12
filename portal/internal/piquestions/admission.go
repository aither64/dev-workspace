package piquestions

import (
	"context"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

type QuestionObservation struct {
	Current                                        agentbackend.ExecutionHandle
	BindingRevision, ObservedBindingRevision       uint64
	QuestionID                                     string
	Revision                                       uint64
	Caller                                         Caller
	Origin                                         piruntime.NativeOriginObservation
	CaptureAllowed                                 bool
	Callbacks, LinearLineage, GuardQuestionsSchema int
}
type QuestionObservations func(context.Context, agentbackend.ConversationRef) ([]QuestionObservation, error)

// AdmittedStore is the live HTTP capture composition. The raw FileStore remains
// a storage primitive for an already trusted publication/fake fixture context.
// No callback is swapped on the shared store or called under its file lock.
type AdmittedStore struct {
	Store        *FileStore
	Admit        func(context.Context, agentbackend.ConversationRef, func(context.Context) error) error
	Current      func(context.Context, Caller) (Caller, error)
	VerifyOrigin func(context.Context, Caller, QuestionRecord) error
	Observations QuestionObservations
}

func (s AdmittedStore) AcceptResponse(ctx context.Context, ref agentbackend.ConversationRef, in ResponseInput) (view ResponseView, err error) {
	if s.Store == nil {
		return view, ErrUnavailable
	}
	_, view, found, err := s.Store.responseSelection(ctx, ref, in)
	if found || err != nil {
		return view, err
	}
	if s.Admit == nil {
		return view, ErrUnavailable
	}
	err = s.Admit(ctx, ref, func(ctx context.Context) error {
		var e error
		view, e = s.Store.acceptResponse(ctx, ref, in, func(ctx context.Context, _ agentbackend.ConversationRef, q QuestionRecord) error {
			if s.Current == nil || s.VerifyOrigin == nil {
				return ErrUnavailable
			}
			current, err := s.Current(ctx, q.Caller)
			if err != nil {
				return err
			}
			if ValidateCaller(current) != nil || !sameCaller(current, q.Caller) {
				return ErrConflict
			}
			return s.VerifyOrigin(ctx, current, q)
		})
		return e
	})
	return
}
func (s AdmittedStore) Publish(ctx context.Context, c Caller, in PublishInput) (QuestionView, error) {
	if s.Store == nil {
		return QuestionView{}, ErrUnavailable
	}
	return s.Store.Publish(ctx, c, in)
}
func (s AdmittedStore) Close(ctx context.Context, c Caller, id string, revision uint64) (QuestionView, error) {
	if s.Store == nil {
		return QuestionView{}, ErrUnavailable
	}
	return s.Store.Close(ctx, c, id, revision)
}
func (s AdmittedStore) Snapshot(ctx context.Context, ref agentbackend.ConversationRef) (Snapshot, error) {
	if s.Store == nil {
		return Snapshot{}, ErrUnavailable
	}
	r, err := s.Store.readRecord(ctx, ref)
	if err != nil {
		return Snapshot{}, err
	}
	observed := map[string][]QuestionObservation{}
	if s.Observations != nil && s.Admit != nil && s.Current != nil && s.VerifyOrigin != nil {
		values, err := s.Observations(ctx, ref)
		if err == nil {
			for _, value := range values {
				observed[value.QuestionID] = append(observed[value.QuestionID], value)
			}
		}
	}
	out := Snapshot{Revision: r.Revision, Questions: []QuestionView{}}
	for _, q := range r.Questions {
		view := project(q)
		values := observed[q.ID]
		view.CanRespond = !q.Closed && len(values) == 1 && matchesQuestionObservation(ref, q, values[0])
		out.Questions = append(out.Questions, view)
	}
	return out, nil
}

func matchesQuestionObservation(ref agentbackend.ConversationRef, q QuestionRecord, o QuestionObservation) bool {
	if !o.CaptureAllowed || o.QuestionID != q.ID || o.Revision != q.Revision || o.Caller != q.Caller || o.Current.Conversation != ref || q.Caller.Handle.Conversation != ref || !Text(o.Current.Generation, 4096) || o.BindingRevision == 0 || o.BindingRevision != o.ObservedBindingRevision || o.Callbacks != 1 || o.LinearLineage != 1 || o.GuardQuestionsSchema != 1 || o.Origin.Handle != o.Current {
		return false
	}
	expected := piruntime.NativeOriginRef{AnchorEntryID: q.Caller.Lineage.AnchorEntryID, SelectionToken: q.Caller.Lineage.SelectionToken, NativeToolCallID: q.Caller.NativeToolCallID, Tool: "request_user_input_async"}
	return o.Origin.Origin == expected && VerifyNativeArguments(q, o.Origin.Arguments) == nil
}
func (s AdmittedStore) Response(ctx context.Context, ref agentbackend.ConversationRef, id string) (ResponseView, error) {
	if s.Store == nil {
		return ResponseView{}, ErrUnavailable
	}
	return s.Store.Response(ctx, ref, id)
}
