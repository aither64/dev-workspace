package maintenance

import (
	"context"
	"errors"
)

type ProofError struct{ Reason string }

func (e *ProofError) Error() string { return e.Reason }

type Controller struct {
	Store            Store
	WorkerInvocation string
}

func response(status, reason string) Response {
	return Response{Schema: TransportSchema, Status: status, Reason: reason}
}
func (c Controller) Status(id string) (Response, error) {
	o, err := c.Store.Load(id)
	if err != nil {
		return Response{}, err
	}
	o.Preparation = nil
	for i := range o.Participants {
		o.Participants[i].NoticeText = ""
		o.Participants[i].NoticeAction = ""
	}
	return Response{Schema: TransportSchema, Status: o.Phase, Operation: &o}, nil
}
func (c Controller) Cancel(ctx context.Context, id string) (Response, error) {
	late := false
	_, err := c.Store.Update(ctx, id, 0, func(o *Operation) error {
		if o.CutoverIntent {
			late = true
			return nil
		}
		if !o.Terminal() {
			o.CancelRequested = true
		}
		return nil
	})
	if late {
		return response("too-late", "source-shutdown-owned"), err
	}
	return response("cancelled", ""), err
}
func (c Controller) SuppressResume(ctx context.Context, r Request) (Response, error) {
	_, err := c.Store.Update(ctx, r.OperationID, 0, func(o *Operation) error {
		for i := range o.Participants {
			p := &o.Participants[i]
			if p.Thread == r.ThreadID {
				p.StopRevision++
				if p.ContinuationDisposition == "pending" {
					p.ContinuationDisposition = "suppressed"
				}
				p.Reason = "user-stop"
				return nil
			}
		}
		return errors.New("maintenance stop participant changed")
	})
	return response("suppressed", ""), err
}
func (c Controller) Handle(ctx context.Context, r Request, worker bool) (Response, error) {
	if r.Schema != TransportSchema || !uuid.MatchString(r.OperationID) {
		return Response{}, errors.New("invalid maintenance request")
	}
	switch r.Action {
	case "status":
		return c.Status(r.OperationID)
	case "cancel":
		return c.Cancel(ctx, r.OperationID)
	case "suppress-resume":
		return c.SuppressResume(ctx, r)
	case "prepare":
		if worker && r.Operation != nil && r.Operation.OperationID == r.OperationID {
			if err := c.Store.Create(ctx, *r.Operation); err != nil {
				return Response{}, err
			}
			return c.Status(r.OperationID)
		}
	}
	return Response{}, errors.New("unsupported maintenance control action")
}

// Stop before this transaction suppresses dispatch; Stop after it interrupts
// admitted work. Uncertain delivery is reconciled under the original ID.
func (c Controller) AdmitContinuation(ctx context.Context, id, thread, incarnation string) (Response, error) {
	status := "suppressed"
	_, err := c.Store.Update(ctx, id, 0, func(o *Operation) error {
		if o.Phase != "resuming" || (!o.ActivationIntent && !o.CancelRequested) {
			return errors.New("continuation phase changed")
		}
		for i := range o.Participants {
			p := &o.Participants[i]
			if p.Thread != thread {
				continue
			}
			if p.Incarnation != incarnation || p.StopRevision != 0 || p.ContinuationDisposition == "suppressed" {
				return nil
			}
			if p.ContinuationDisposition != "pending" {
				return errors.New("continuation already admitted")
			}
			p.ContinuationDisposition = "submitting"
			status = "admitted"
			return nil
		}
		return nil
	})
	return response(status, ""), err
}
