package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/progressobserver"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

const releasedObserverInstructions = `Observe progress toward the accepted user goal independently of the lead. Read only the bounded evidence supplied for each scheduled check; do not run tools, edit files, inspect other sessions, assign work, cancel checks or waive requirements. Return the requested JSON result without calling report_to_lead. Healthy work stays quiet. Challenge repeated verification, evidence bookkeeping, or open-ended investigation only when concrete activity shows waste and you can recommend a shorter valid delivery path with a stopping condition. Respect user-authorized scope and required safety checks. A justified long check, waiting for a user, or lack of commits alone is not drift. Keep concern keys stable across checks. Acknowledgment alone does not resolve a concern; changed activity can. This is progress advice, not independent final change review or an approval gate.`

func TestProgressScheduledStockPolicyPreservesCustomSettings(t *testing.T) {
	for _, instructions := range []string{releasedObserverInstructions, releasedObserverInstructions + " Custom restriction."} {
		s, c, summary, now := progressFixture(t)
		service, err := s.teamService(summary.Slug)
		if err != nil {
			t.Fatal(err)
		}
		_, err = service.Store.Update(context.Background(), summary.Slug, summary.Codex.ThreadID, false, func(r *teamruntime.Roster) error {
			r.Members[0].Instructions = instructions
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.stepProgressObserver(context.Background(), summary, now); err != nil {
			t.Fatal(err)
		}
		opts := c.sends[0].options
		want := instructions
		if instructions == releasedObserverInstructions {
			want = progressobserver.Instructions
		}
		if !strings.HasSuffix(opts.ThreadPolicy.DeveloperInstructions, want) || opts.Model != "retained-observer" || opts.ReasoningEffort != "xhigh" {
			t.Fatal("stock update or custom settings lost", opts)
		}
		roster, err := service.Store.Load(summary.Slug, summary.Codex.ThreadID)
		if err != nil || roster.Members[0].Instructions != instructions {
			t.Fatal("saved roster changed", err)
		}
	}
}

func TestProgressEvidenceIncludesSteeringTimeAndSourceAccess(t *testing.T) {
	s, c, summary, now := progressFixture(t)
	lead := c.transcripts["thread-1"]
	lead.Entries = append(lead.Entries, codex.TranscriptEntry{Kind: "userMessage", Text: "Then investigate the CI failures and implement a small fix."})
	c.transcripts["thread-1"] = lead
	s.activity = &activityMonitor{clock: func() time.Time { return now }, snapshots: map[string]activityCachedSnapshot{"thread-1": {at: now, value: codex.ActivitySnapshot{ThreadID: "thread-1", CurrentTurnID: "work", CurrentState: "working", WorkingMS: 31 * 60000, IdleMS: 8 * 60 * 60000, WaitingMS: 60 * 60000, CoverageComplete: false, CoverageReason: "Earlier activity unavailable"}}}}
	if err := s.stepProgressObserver(context.Background(), summary, now); err != nil {
		t.Fatal(err)
	}
	state, _ := s.progressStore().Load(summary.Codex.ThreadID)
	if !state.Pending.EvidenceComplete {
		t.Fatal("available evidence was marked incomplete", state.Pending.Message)
	}
	message := c.sends[0].message
	for _, expected := range []string{"31.0 minutes", "coverage complete=false", "Then investigate the CI failures", c.rollout, "30 recorded working minutes", "Previous concern"} {
		if !strings.Contains(message, expected) {
			t.Fatalf("missing %q", expected)
		}
	}
	if strings.Contains(message, "571.0 minutes") {
		t.Fatal("idle/wait time counted")
	}
}

func TestProgressMissingEvidenceCannotClearConcern(t *testing.T) {
	s, c, summary, now := progressFixture(t)
	c.rollout = ""
	state, _ := s.progressStore().Load(summary.Codex.ThreadID)
	state.Concern = &progressobserver.Concern{Key: "scope", Summary: "New subsystem", Recommendation: "Simplify", Count: 1}
	if err := s.progressStore().Save(state); err != nil {
		t.Fatal(err)
	}
	if err := s.stepProgressObserver(context.Background(), summary, now); err != nil {
		t.Fatal(err)
	}
	progressComplete(c, `{"status":"on_track","evidence":"Looks fine","resolution":{"key":"scope","evidence":"Lead agrees"}}`)
	if err := s.stepProgressObserver(context.Background(), summary, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	state, _ = s.progressStore().Load(summary.Codex.ThreadID)
	if state.Concern == nil || state.Unavailable == "" {
		t.Fatal("missing evidence cleared concern", state)
	}
}
