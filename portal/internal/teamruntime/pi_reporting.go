package teamruntime

import "context"

// Report is explicitly invoked by the bound tool, never by turn completion.
func (d PiDelivery) Report(ctx context.Context, caller PiCaller, id, text string) (PiDeliveryResult, error) {
	return d.Deliver(ctx, caller, PiMessage{ID: id, To: "lead", Text: text})
}

func (d PiDelivery) Peer(ctx context.Context, caller PiCaller, message PiMessage) (PiDeliveryResult, error) {
	return d.Deliver(ctx, caller, message)
}
