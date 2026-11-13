package piplan

import (
	"context"
	"os"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piquestions"
)

func (s *FileStore) readRecord(ctx context.Context, ref agentbackend.ConversationRef) (Record, error) {
	r := fresh(ref)
	if s == nil {
		return r, piquestions.ErrUnavailable
	}
	if err := s.validate(ctx, ref, nil); err != nil {
		return r, err
	}
	if err := s.file(ref).Read(ctx, &r); err != nil && !os.IsNotExist(err) {
		return r, err
	}
	return r, validateRecord(r, ref)
}

// LatestProposal decodes one validated record without loading a worker. Proposal
// contains values only, so the returned copy shares no mutable store state.
func (s *FileStore) LatestProposal(ctx context.Context, ref agentbackend.ConversationRef) (*Proposal, error) {
	r, err := s.readRecord(ctx, ref)
	if err != nil || len(r.Proposals) == 0 {
		return nil, err
	}
	p := r.Proposals[len(r.Proposals)-1]
	return &p, nil
}
