package piplan

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piquestions"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

type originReader func(context.Context, agentbackend.ExecutionHandle, piruntime.NativeOriginRef) (piruntime.NativeOriginObservation, error)

func (f originReader) InspectNativeOrigin(ctx context.Context, h agentbackend.ExecutionHandle, o piruntime.NativeOriginRef) (piruntime.NativeOriginObservation, error) {
	return f(ctx, h, o)
}

func TestNativePlanVerifierUsesOriginalTextAndCurrentGeneration(t *testing.T) {
	ctx := context.Background()
	_, saved := fixture(t)
	saved.Lineage.SelectionToken = "linear-v1:" + strings.Repeat("a", 64)
	current := saved
	current.Handle.Generation = "restarted"
	p := Proposal{Conversation: saved.Handle.Conversation, Caller: saved, NativeOrigin: NativeOrigin{EntryID: saved.Lineage.AnchorEntryID, Complete: true}, Text: "Plan\r\n", TextSHA256: Digest("Plan\r\n"), Complete: true}
	calls := 0
	reader := originReader(func(_ context.Context, h agentbackend.ExecutionHandle, o piruntime.NativeOriginRef) (piruntime.NativeOriginObservation, error) {
		calls++
		if h != current.Handle || o.AnchorEntryID != saved.Lineage.AnchorEntryID || o.SelectionToken != saved.Lineage.SelectionToken || o.NativeToolCallID != saved.NativeToolCallID || o.Tool != "publish_plan" {
			t.Fatal(h, o)
		}
		return piruntime.NativeOriginObservation{Handle: h, Origin: o, Arguments: json.RawMessage(`{"text":"Plan\r\n"}`)}, nil
	})
	v := NativeOriginVerifier{Inspector: reader}
	if err := v.Verify(ctx, current, p); err != nil || calls != 1 || p.Caller != saved {
		t.Fatal(err, calls, p.Caller)
	}
	for name, change := range map[string]func(*Proposal){
		"native ID":  func(p *Proposal) { p.Caller.Handle.Conversation.NativeID = "other" },
		"scope":      func(p *Proposal) { p.Conversation.NativeID = "other" },
		"anchor":     func(p *Proposal) { p.NativeOrigin.EntryID = "other" },
		"digest":     func(p *Proposal) { p.TextSHA256 = Digest("other") },
		"incomplete": func(p *Proposal) { p.Complete = false },
	} {
		t.Run(name, func(t *testing.T) {
			changed := p
			change(&changed)
			if !errors.Is(v.Verify(ctx, current, changed), piquestions.ErrConflict) {
				t.Fatal("changed plan accepted")
			}
		})
	}
	for name, change := range map[string]func(*piruntime.NativeOriginObservation){
		"generation": func(o *piruntime.NativeOriginObservation) { o.Handle.Generation = saved.Handle.Generation },
		"scope":      func(o *piruntime.NativeOriginObservation) { o.Handle.Conversation.NativeID = "other" },
		"anchor":     func(o *piruntime.NativeOriginObservation) { o.Origin.AnchorEntryID = "other" },
		"token":      func(o *piruntime.NativeOriginObservation) { o.Origin.SelectionToken = "other" },
		"tool call":  func(o *piruntime.NativeOriginObservation) { o.Origin.NativeToolCallID = "other" },
		"tool":       func(o *piruntime.NativeOriginObservation) { o.Origin.Tool = "request_user_input_async" },
		"text":       func(o *piruntime.NativeOriginObservation) { o.Arguments = json.RawMessage(`{"text":"Plan\n"}`) },
	} {
		t.Run(name, func(t *testing.T) {
			v := NativeOriginVerifier{Inspector: originReader(func(ctx context.Context, h agentbackend.ExecutionHandle, origin piruntime.NativeOriginRef) (piruntime.NativeOriginObservation, error) {
				o, err := reader(ctx, h, origin)
				change(&o)
				return o, err
			})}
			if !errors.Is(v.Verify(ctx, current, p), piquestions.ErrConflict) {
				t.Fatal("changed observation accepted")
			}
		})
	}
	if !errors.Is((NativeOriginVerifier{}).Verify(ctx, current, p), piquestions.ErrUnavailable) {
		t.Fatal("missing reader enabled plan")
	}
	for _, pair := range [][2]error{{piruntime.ErrScope, piquestions.ErrConflict}, {piruntime.ErrConflict, piquestions.ErrConflict}, {piruntime.ErrUnavailable, piquestions.ErrUnavailable}, {context.Canceled, context.Canceled}} {
		v := NativeOriginVerifier{Inspector: originReader(func(context.Context, agentbackend.ExecutionHandle, piruntime.NativeOriginRef) (piruntime.NativeOriginObservation, error) {
			return piruntime.NativeOriginObservation{}, pair[0]
		})}
		if !errors.Is(v.Verify(ctx, current, p), pair[1]) {
			t.Fatal(pair)
		}
	}
}
