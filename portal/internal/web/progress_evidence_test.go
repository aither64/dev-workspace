package web

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/progressobserver"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

const releasedObserverInstructions = `Observe progress toward the accepted user goal independently of the lead. Read only the bounded evidence supplied for each scheduled check; do not run tools, edit files, inspect other sessions, assign work, cancel checks or waive requirements. Return the requested JSON result without calling report_to_lead. Healthy work stays quiet. Challenge repeated verification, evidence bookkeeping, or open-ended investigation only when concrete activity shows waste and you can recommend a shorter valid delivery path with a stopping condition. Respect user-authorized scope and required safety checks. A justified long check, waiting for a user, or lack of commits alone is not drift. Keep concern keys stable across checks. Acknowledgment alone does not resolve a concern; changed activity can. This is progress advice, not independent final change review or an approval gate.`

const releasedInspectionInstructions = `Observe progress toward the accepted user goal independently of the lead. Follow the scheduled check's result format. Use read-only inspection tools on the supplied same-session source, diffs and conversations; compare actual user scope, diffstats, recorded working time and delivered milestones before detailed code inspection. Do not edit, run tests/builds, inspect other sessions, assign work, call report_to_lead, cancel checks or waive requirements. Reassess small documentation changes and bounded fixes after 30 working minutes without delivery, excluding idle and human waits. Challenge disproportionate scope and repeated verification preparation with a shorter valid delivery path. Preserve concern keys until concrete resolution evidence; acknowledgment is insufficient. Missing material evidence is unavailable, not on track. This is progress advice, not final review or an approval gate.`

func TestProgressStockPolicyRefreshAndCustomSettings(t *testing.T) {
	for _, instructions := range []string{releasedObserverInstructions, releasedInspectionInstructions, releasedObserverInstructions + " Custom restriction.", progressobserver.Instructions} {
		s, c, summary, now := progressFixture(t)
		service, _ := s.teamService(summary.Slug)
		_, err := service.Store.Update(context.Background(), summary.Slug, summary.Codex.ThreadID, false, func(r *teamruntime.Roster) error { r.Members[0].Instructions = instructions; return nil })
		if err != nil {
			t.Fatal(err)
		}
		if err := s.stepProgressObserver(context.Background(), summary, now); err != nil {
			t.Fatal(err)
		}
		want, refreshes := instructions, 0
		if instructions == releasedObserverInstructions || instructions == releasedInspectionInstructions {
			want, refreshes = progressobserver.Instructions, 1
		}
		opts := c.sends[0].options
		if !strings.HasSuffix(opts.ThreadPolicy.DeveloperInstructions, want) || opts.Model != "retained-observer" || opts.ReasoningEffort != "xhigh" || len(c.refreshes) != refreshes {
			t.Fatal("stock update or custom settings lost", opts, c.refreshes)
		}
		roster, err := service.Store.Load(summary.Slug, summary.Codex.ThreadID)
		if err != nil || roster.Members[0].Instructions != want || roster.Members[0].Thread != "observer-thread" {
			t.Fatal("saved observer changed unexpectedly", roster, err)
		}
	}
}

func TestProgressFailedRefreshRetainsPolicyAndDoesNotSubmit(t *testing.T) {
	s, c, summary, now := progressFixture(t)
	service, _ := s.teamService(summary.Slug)
	_, err := service.Store.Update(context.Background(), summary.Slug, summary.Codex.ThreadID, false, func(r *teamruntime.Roster) error {
		r.Members[0].Instructions = releasedObserverInstructions
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c.refreshError = errors.New("observer still loaded")
	if err := s.stepProgressObserver(context.Background(), summary, now); err == nil {
		t.Fatal("failed refresh accepted")
	}
	roster, _ := service.Store.Load(summary.Slug, summary.Codex.ThreadID)
	state, _ := s.progressStore().Load(summary.Codex.ThreadID)
	if len(c.sends) != 0 || state.Pending != nil || roster.Members[0].Instructions != releasedObserverInstructions {
		t.Fatal("failed refresh lost retry or submitted")
	}
	c.refreshError = nil
	if err := s.stepProgressObserver(context.Background(), summary, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(c.sends) != 1 {
		t.Fatal("refresh could not retry")
	}
}

func TestProgressContextIsCompactAndDoesNotCopyContent(t *testing.T) {
	s, c, summary, now := progressFixture(t)
	service, _ := s.teamService(summary.Slug)
	_, err := service.Store.Update(context.Background(), summary.Slug, summary.Codex.ThreadID, false, func(roster *teamruntime.Roster) error {
		for _, address := range []string{"architect0", "implementer0", "implementer1", "reviewer0"} {
			member := roster.Members[0]
			member.Role, member.Address, member.Thread = address[:len(address)-1], address, address+"-thread"
			member.Index = uint64(address[len(address)-1] - '0')
			roster.Members = append(roster.Members, member)
			c.transcripts[member.Thread] = codex.Transcript{ThreadID: member.Thread, Status: "idle"}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	marker := strings.Repeat("PRIVATE_RECORD_CONTENT ", 10000)
	for _, name := range []string{"plan.md", "state.md"} {
		if err := os.WriteFile(filepath.Join(c.cwd, name), []byte("---\nlifecycle: active\n---\n"+marker), 0600); err != nil {
			t.Fatal(err)
		}
	}
	lead := c.transcripts["thread-1"]
	lead.Entries = append(lead.Entries, codex.TranscriptEntry{Kind: "userMessage", Text: marker})
	c.transcripts["thread-1"] = lead
	s.activity = &activityMonitor{clock: func() time.Time { return now }, snapshots: map[string]activityCachedSnapshot{"thread-1": {at: now, value: codex.ActivitySnapshot{ThreadID: "thread-1", CurrentTurnID: "work", CurrentState: "working", WorkingMS: 31 * 60000, IdleMS: 8 * 60 * 60000, WaitingMS: 60 * 60000, CoverageComplete: false, CoverageReason: "Earlier activity unavailable"}}}}
	for thread := range c.transcripts {
		if thread != "thread-1" {
			s.activity.snapshots[thread] = activityCachedSnapshot{at: now, value: codex.ActivitySnapshot{ThreadID: thread, CurrentState: "idle", CoverageComplete: true}}
		}
	}
	if err := s.stepProgressObserver(context.Background(), summary, now); err != nil {
		t.Fatal(err)
	}
	message := c.sends[0].message
	for _, expected := range []string{"31.0 minutes", "coverage complete=false", c.rollout, c.cwd, "portal.yml", "Previous concern", "Git diffstats"} {
		if !strings.Contains(message, expected) {
			t.Fatalf("missing %q", expected)
		}
	}
	if len(message) > 4096 || strings.Contains(message, "PRIVATE_RECORD_CONTENT") || strings.Contains(message, "571.0 minutes") {
		t.Fatal("oversized or copied context", len(message))
	}
	t.Logf("six-member team check prompt: %d bytes", len(message))
}

func TestProgressMissingOptionalContextDoesNotOverrideInspectedResult(t *testing.T) {
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
	progressComplete(c, `{"status":"on_track","evidence":"Read the original request and current git diff: the optional subsystem is removed.","resolution":{"key":"scope","evidence":"Git diff now contains only the requested documentation."}}`)
	if err := s.stepProgressObserver(context.Background(), summary, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	state, _ = s.progressStore().Load(summary.Codex.ThreadID)
	if state.Concern != nil || state.Unavailable != "" {
		t.Fatal("optional context overrode observed result", state)
	}
}

func TestProgressPendingV2RetainsReleasedSubmissionOptions(t *testing.T) {
	for _, instructions := range []string{releasedObserverInstructions, releasedInspectionInstructions + "\n", "Custom observer policy"} {
		s, c, summary, now := progressFixture(t)
		service, _ := s.teamService(summary.Slug)
		roster, err := service.Store.Update(context.Background(), summary.Slug, summary.Codex.ThreadID, false, func(r *teamruntime.Roster) error {
			r.Members[0].Instructions = instructions
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		policy, err := service.MemberTurnPolicy(summary.Slug, summary.Codex.ThreadID, roster.Members[0])
		if err != nil {
			t.Fatal(err)
		}
		// These are the options hashed by the released submission ledger.
		if instructions == releasedObserverInstructions {
			policy.DeveloperInstructions = strings.TrimSuffix(policy.DeveloperInstructions, instructions) + releasedInspectionInstructions
		}
		want := codex.TurnOptions{Model: "retained-observer", ReasoningEffort: "xhigh", ThreadPolicy: policy}
		state, _ := s.progressStore().Load(summary.Codex.ThreadID)
		state.Pending = &progressobserver.Pending{ID: "released-prepared", Thread: "observer-thread", Address: "observer0", Model: want.Model, Effort: want.ReasoningEffort, Message: "Progress check v2. Original request", StartedAt: now}
		if err := s.progressStore().Save(state); err != nil {
			t.Fatal(err)
		}
		if err := s.stepProgressObserver(context.Background(), summary, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if len(c.sends) != 1 || c.sends[0].id != state.Pending.ID || c.sends[0].message != state.Pending.Message || !reflect.DeepEqual(c.sends[0].options, want) {
			t.Fatal("released submission identity changed", c.sends)
		}
		roster, err = service.Store.Load(summary.Slug, summary.Codex.ThreadID)
		if err != nil || len(c.refreshes) != 0 || roster.Members[0].Instructions != instructions {
			t.Fatal("pending check refreshed stock policy", roster, err)
		}
	}
}
