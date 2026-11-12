package piquestions

import (
	"context"
	"os"
	"slices"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

func (s *FileStore) readRecord(ctx context.Context, ref agentbackend.ConversationRef) (Record, error) {
	r := fresh(ref)
	if s == nil {
		return r, ErrUnavailable
	}
	if err := s.validate(ctx, ref, nil); err != nil {
		return r, err
	}
	if err := s.file(ref).Read(ctx, &r); err != nil && !os.IsNotExist(err) {
		return r, err
	}
	return r, validRecord(r, ref)
}

// Questions returns detached immutable publication and response evidence. It
// never probes a worker or turns missing state into a persisted record.
func (s *FileStore) Questions(ctx context.Context, ref agentbackend.ConversationRef) ([]QuestionRecord, error) {
	r, err := s.readRecord(ctx, ref)
	if err != nil {
		return nil, err
	}
	out := slices.Clone(r.Questions)
	for i := range out {
		out[i].Questions = slices.Clone(out[i].Questions)
		for j := range out[i].Questions {
			out[i].Questions[j].Options = slices.Clone(out[i].Questions[j].Options)
		}
		out[i].Responses = slices.Clone(out[i].Responses)
		for j := range out[i].Responses {
			out[i].Responses[j].Input.Answers = slices.Clone(out[i].Responses[j].Input.Answers)
		}
	}
	return out, nil
}
