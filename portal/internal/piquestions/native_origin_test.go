package piquestions

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/piruntime"
)

type originReader func(context.Context, agentbackend.ExecutionHandle, piruntime.NativeOriginRef) (piruntime.NativeOriginObservation, error)

func (f originReader) InspectNativeOrigin(ctx context.Context, h agentbackend.ExecutionHandle, o piruntime.NativeOriginRef) (piruntime.NativeOriginObservation, error) {
	return f(ctx, h, o)
}

func TestNativeQuestionVerifierBindsCurrentHandleAndSavedOrigin(t *testing.T) {
	ctx := context.Background()
	_, saved := fixture(t)
	saved.Lineage.SelectionToken = "linear-v1:" + strings.Repeat("a", 64)
	current := saved
	current.Handle.Generation = "restarted"
	record := QuestionRecord{Caller: saved, Questions: []Question{{ID: "0", Title: "Which?", Options: []Option{{ID: "0", Label: "One"}}}}}
	args := json.RawMessage(`{"questions":[{"title":"Which?","options":["One"]}]}`)
	calls := 0
	reader := originReader(func(_ context.Context, h agentbackend.ExecutionHandle, origin piruntime.NativeOriginRef) (piruntime.NativeOriginObservation, error) {
		calls++
		if h != current.Handle || origin.AnchorEntryID != saved.Lineage.AnchorEntryID || origin.SelectionToken != saved.Lineage.SelectionToken || origin.NativeToolCallID != saved.NativeToolCallID || origin.Tool != "request_user_input_async" {
			t.Fatal(h, origin)
		}
		return piruntime.NativeOriginObservation{Handle: h, Origin: origin, Arguments: args}, nil
	})
	verifier := NativeOriginVerifier{Inspector: reader}
	if err := verifier.Verify(ctx, current, record); err != nil || calls != 1 || record.Caller != saved {
		t.Fatal(err, calls, record.Caller)
	}
	for _, change := range []func(*Caller){
		func(c *Caller) { c.Handle.Conversation.NativeID = "other" },
		func(c *Caller) { c.Handle.Conversation.Incarnation = strings.Repeat("c", 64) },
		func(c *Caller) { c.Lineage.AnchorEntryID = "other" },
		func(c *Caller) { c.Lineage.SelectionToken = "other" },
		func(c *Caller) { c.NativeToolCallID = "other" },
	} {
		changed := current
		change(&changed)
		if !errors.Is(verifier.Verify(ctx, changed, record), ErrConflict) || calls != 1 {
			t.Fatal("changed publication reached reader", changed, calls)
		}
	}
	for name, change := range map[string]func(*piruntime.NativeOriginObservation){
		"stale generation": func(o *piruntime.NativeOriginObservation) { o.Handle.Generation = saved.Handle.Generation },
		"wrong native ID":  func(o *piruntime.NativeOriginObservation) { o.Handle.Conversation.NativeID = "other" },
		"wrong anchor":     func(o *piruntime.NativeOriginObservation) { o.Origin.AnchorEntryID = "other" },
		"wrong token":      func(o *piruntime.NativeOriginObservation) { o.Origin.SelectionToken = "other" },
		"wrong call":       func(o *piruntime.NativeOriginObservation) { o.Origin.NativeToolCallID = "other" },
		"wrong tool":       func(o *piruntime.NativeOriginObservation) { o.Origin.Tool = "publish_plan" },
		"changed content": func(o *piruntime.NativeOriginObservation) {
			o.Arguments = json.RawMessage(`{"questions":[{"title":"Changed"}]}`)
		},
		"oversized args": func(o *piruntime.NativeOriginObservation) {
			o.Arguments = json.RawMessage(strings.Repeat(" ", NativeArgumentLimit+1))
		},
	} {
		t.Run(name, func(t *testing.T) {
			v := NativeOriginVerifier{Inspector: originReader(func(ctx context.Context, h agentbackend.ExecutionHandle, origin piruntime.NativeOriginRef) (piruntime.NativeOriginObservation, error) {
				o, err := reader(ctx, h, origin)
				change(&o)
				return o, err
			})}
			if !errors.Is(v.Verify(ctx, current, record), ErrConflict) {
				t.Fatal("accepted mismatched observation")
			}
		})
	}
	if !errors.Is((NativeOriginVerifier{}).Verify(ctx, current, record), ErrUnavailable) {
		t.Fatal("missing reader enabled origin")
	}
	for _, pair := range [][2]error{{piruntime.ErrScope, ErrConflict}, {piruntime.ErrConflict, ErrConflict}, {piruntime.ErrUnavailable, ErrUnavailable}, {context.Canceled, context.Canceled}} {
		v := NativeOriginVerifier{Inspector: originReader(func(context.Context, agentbackend.ExecutionHandle, piruntime.NativeOriginRef) (piruntime.NativeOriginObservation, error) {
			return piruntime.NativeOriginObservation{}, pair[0]
		})}
		if !errors.Is(v.Verify(ctx, current, record), pair[1]) {
			t.Fatal(pair)
		}
	}
}
