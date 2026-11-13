package piquestions

import (
	"context"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

// AdmittedStore is the live HTTP capture composition. The raw FileStore remains
// a storage primitive for an already trusted publication/fake fixture context.
// No callback is swapped on the shared store or called under its file lock.
type AdmittedStore struct {
	Store        *FileStore
	Admit        func(context.Context, agentbackend.ConversationRef, func(context.Context) error) error
	Current      func(context.Context, Caller) (Caller, error)
	VerifyOrigin func(context.Context, Caller, QuestionRecord) error
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
	return s.Store.Snapshot(ctx, ref)
}
func (s AdmittedStore) Response(ctx context.Context, ref agentbackend.ConversationRef, id string) (ResponseView, error) {
	if s.Store == nil {
		return ResponseView{}, ErrUnavailable
	}
	return s.Store.Response(ctx, ref, id)
}
