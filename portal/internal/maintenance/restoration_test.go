package maintenance

import (
	"context"
	"testing"
)

func TestRestorationKeepsAdmissionClosedUntilExplicitConfirmedOutcome(t *testing.T) {
	for _, outcome := range []string{"cancelled", "updated", "source-recovered"} {
		t.Run(outcome, func(t *testing.T) {
			c, _ := fixture(t)
			ctx := context.Background()
			pkg := "/candidate"
			_, e := c.Store.Update(ctx, testID, 0, func(o *Operation) error {
				o.DrainedPortals = []string{"portal-owned"}
				o.ContinuationsSettled = true
				o.NoticeCleanupComplete = true
				if outcome == "cancelled" {
					o.CancelRequested = true
					pkg = o.SourcePackage
				} else {
					o.CutoverIntent = true
					o.ActivationIntent = true
					o.ActivationPackage = pkg
					o.ActivationNative = "/native"
					o.ActivationPlansDigest = "plans"
					o.SourceRecovered = outcome == "source-recovered"
				}
				return nil
			})
			if e != nil {
				t.Fatal(e)
			}
			intent := RestorationIntent{Outcome: outcome, Package: pkg, ProfileToken: "source"}
			if _, e = c.BeginRestoration(ctx, testID, intent); e != nil {
				t.Fatal(e)
			}
			saved, e := c.Store.Load(testID)
			if e != nil || saved.Terminal() || saved.Restoration.Completed {
				t.Fatalf("%+v %v", saved, e)
			}
			workspace := saved.Services[0].Workspace
			if e = RequireAdmission(c.Store.Root, c.Store.Profile, workspace); e == nil {
				t.Fatal("restoration opened admission")
			}
			intent = *saved.Restoration
			// A worker crash/error after any subset of effects preserves the same tail.
			_, e = c.Store.Update(ctx, testID, 0, func(o *Operation) error { o.Phase = "recovery-required"; return nil })
			if e != nil {
				t.Fatal(e)
			}
			fresh := Controller{Store: c.Store}
			if _, e = fresh.BeginRestoration(ctx, testID, intent); e != nil {
				t.Fatal(e)
			}
			changed := intent
			changed.ProfileToken = "replacement"
			if _, e = fresh.FinishRestoration(ctx, testID, changed); e == nil {
				t.Fatal("changed restoration finalized")
			}
			if _, e = fresh.FinishRestoration(ctx, testID, intent); e != nil {
				t.Fatal(e)
			}
			saved, _ = c.Store.Load(testID)
			if !saved.Terminal() || !saved.Restoration.Completed {
				t.Fatal("confirmed restoration not terminal")
			}
			if e = RequireAdmission(c.Store.Root, c.Store.Profile, workspace); e != nil {
				t.Fatal(e)
			}
			if _, e = fresh.FinishRestoration(ctx, testID, intent); e != nil {
				t.Fatal("final receipt retry", e)
			}
		})
	}
}
func TestCancelRestorationCanRestoreEffectsButCannotFinalizeUnresolvedNotice(t *testing.T) {
	c, _ := fixture(t)
	ctx := context.Background()
	_, _ = c.Cancel(ctx, testID)
	if _, e := c.BeginRestoration(ctx, testID, RestorationIntent{Outcome: "cancelled", Package: "/source", ProfileToken: "source"}); e != nil {
		t.Fatal(e)
	}
	o, _ := c.Store.Load(testID)
	intent := *o.Restoration
	if _, e := c.FinishRestoration(ctx, testID, intent); e == nil {
		t.Fatal("unresolved notice silently abandoned")
	}
	o, _ = c.Store.Load(testID)
	if o.Terminal() || o.Restoration.Completed {
		t.Fatal("failed cleanup opened admission")
	}
}
