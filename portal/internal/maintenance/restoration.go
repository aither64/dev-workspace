package maintenance

import (
	"context"
	"errors"
	"reflect"
	"slices"
)

// RestorationIntent is the finite last step of one update. Its owned effects
// and final outcome remain immutable across worker loss; the ordinary fence is
// opened only by FinishRestoration after the host confirms those effects.
type RestorationIntent struct {
	Outcome      string           `json:"outcome"`
	Package      string           `json:"package"`
	ProfileToken string           `json:"profileToken"`
	Portals      []string         `json:"portals"`
	Terminals    []TerminalIntent `json:"terminals"`
	Completed    bool             `json:"completed"`
}

func (c Controller) BeginRestoration(ctx context.Context, id string, intent RestorationIntent) (Response, error) {
	if intent.Completed || intent.ProfileToken == "" || intent.Package == "" || !slices.Contains([]string{"updated", "cancelled", "source-recovered"}, intent.Outcome) {
		return Response{}, errors.New("invalid restoration intent")
	}
	_, err := c.Store.Update(ctx, id, 0, func(o *Operation) error {
		if o.WorkerInvocation != "" && o.WorkerInvocation != c.WorkerInvocation {
			return errors.New("restoration worker changed")
		}
		expectedPackage := o.ActivationPackage
		if intent.Outcome == "cancelled" {
			if o.CutoverIntent || !o.CancelRequested {
				return errors.New("cancellation is not owned")
			}
			expectedPackage = o.SourcePackage
			if intent.ProfileToken != o.SourceProfileToken {
				return errors.New("cancellation profile changed")
			}
		} else if !o.ContinuationsSettled || !o.CutoverIntent || !o.ActivationIntent || o.SourceRecovered != (intent.Outcome == "source-recovered") {
			return errors.New("continuation obligations are not settled")
		}
		if intent.Package != expectedPackage {
			return errors.New("restoration package changed")
		}
		intent.Portals = append([]string{}, o.DrainedPortals...)
		intent.Terminals = append([]TerminalIntent{}, o.TerminalIntents...)
		if o.Restoration != nil {
			old := *o.Restoration
			incoming := intent
			if !reflect.DeepEqual(old, incoming) {
				return errors.New("restoration intent changed")
			}
			return nil
		}
		if o.Terminal() {
			return errors.New("operation already terminal")
		}
		o.Restoration = &intent
		o.Phase = "resuming"
		return nil
	})
	return response("restoring", ""), err
}
func (c Controller) FinishRestoration(ctx context.Context, id string, intent RestorationIntent) (Response, error) {
	status := ""
	_, err := c.Store.Update(ctx, id, 0, func(o *Operation) error {
		if o.WorkerInvocation != "" && o.WorkerInvocation != c.WorkerInvocation {
			return errors.New("restoration worker changed")
		}
		if o.Restoration == nil {
			return errors.New("restoration is not owned")
		}
		expected := *o.Restoration
		expected.Completed = false
		if !reflect.DeepEqual(expected, intent) {
			return errors.New("restoration receipt changed")
		}
		if o.Restoration.Completed {
			status = o.Phase
			return nil
		}
		if intent.Outcome == "cancelled" {
			if o.CutoverIntent || !o.CancelRequested {
				return errors.New("cancel restoration changed")
			}
			if !o.NoticeCleanupComplete || !o.ContinuationsSettled {
				return errors.New("notice cleanup remains unresolved")
			}
		} else if !o.ContinuationsSettled {
			return errors.New("continuations remain unresolved")
		}
		if !slices.Equal(o.DrainedPortals, intent.Portals) || !slices.Equal(o.TerminalIntents, intent.Terminals) {
			return errors.New("owned restoration set changed")
		}
		o.Restoration.Completed = true
		o.Outcome = ""
		o.Phase = "cancelled"
		if intent.Outcome != "cancelled" {
			o.Outcome = intent.Outcome
			if intent.Outcome == "updated" {
				o.Phase = "complete"
			}
		}
		o.Blockers = []string{}
		status = o.Phase
		return nil
	})
	return response(status, ""), err
}
