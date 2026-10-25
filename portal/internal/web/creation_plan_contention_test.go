package web

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aither64/codex-web/codex"
)

func TestAcceptedPlanWaitsForPortalConversationAndRevalidates(t *testing.T) {
	for _, outcome := range []string{"released", "replacement", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			f := newAcceptedForkFixture(t, 1)
			plan := "Preserve the exact accepted plan."
			f.server.config.Codex.(*creationTestCodex).transcript = codex.Transcript{ThreadID: "thread-1", LatestTurnID: "approved", Status: "idle", CollaborationMode: "plan", Model: "model-1", ReasoningEffort: "high",
				Entries: []codex.TranscriptEntry{{Kind: "plan", TurnID: "approved", TurnStatus: "completed", Text: plan}}}
			release := holdCreationConversation(t, f.server, "source")
			defer release()
			const destination = "2026-10-24-plan"
			response := postCreation(t, f.server, "/api/sessions/source/implement-plan",
				fmt.Sprintf(`{"action":"new","name":"plan","creationDate":"2026-10-24","provider":"openai","model":"model-1","reasoningEffort":"high","planTurnId":"approved","planSha256":%q}`, planDigest(plan)), "application/json")
			if response.Code != http.StatusAccepted {
				t.Fatal(response.Code, response.Body.String())
			}
			accepted, _ := f.server.currentCreation(destination)
			barrier := f.sourceBarrier(t)
			if barrier.event.Device != f.device || barrier.event.Inode != f.inode {
				t.Fatal("Ruby source verifier contended on a different lock")
			}
			barrier.release(t)
			deadline := time.Now().Add(3 * time.Second)
			for f.count(t, "source_waiting") == 0 && time.Now().Before(deadline) {
				if current, _ := f.server.currentCreation(destination); current.State != "running" {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			current, _ := f.server.currentCreation(destination)
			if f.count(t, "source_waiting") != 1 || current.State != "running" {
				t.Fatalf("accepted plan refused portal SH: %+v events=%s", current, f.events(t))
			}
			f.assertDestinationUnlocked(t, destination)
			f.assertNoNativeEffects(t)
			if outcome == "replacement" {
				f.replaceSource(t)
			} else if outcome == "cancelled" {
				f.server.cancelOperations()
				f.wait(t)
				f.assertNoNativeEffects(t)
				return
			}
			release()
			f.wait(t)
			finished, _ := f.server.currentCreation(destination)
			f.assertSameRequest(t, accepted, finished)
			if finished.Attempt != accepted.Attempt {
				t.Fatal("waiting turned into a new creation attempt")
			}
			if outcome == "replacement" {
				if finished.State != "failed" || !strings.Contains(finished.Error, "source session changed") {
					t.Fatalf("replacement source adopted: %+v", finished)
				}
				f.assertNoNativeEffects(t)
				return
			}
			if finished.State != "ready" || finished.Error != "" || f.server.proveCreation(finished) != nil {
				t.Fatalf("accepted plan did not finish its real CLI: %+v events=%s", finished, f.events(t))
			}
			if finished.Goal != planCreationGoal("source", plan) || f.count(t, "native_start") != 1 || f.count(t, "native_send") != 1 || f.count(t, "tmux_create") != 1 || f.count(t, "source_refused") != 0 {
				t.Fatalf("plan goal or native effects changed: %+v events=%s", finished, f.events(t))
			}
		})
	}
}

func TestAcceptedPlanJournalRecoveryDoesNotNeedSource(t *testing.T) {
	f := newAcceptedForkFixture(t, 0)
	f.config["failTerminal"] = true
	f.writeConfig(t)
	plan := "Retain this approved request through interrupted terminal startup."
	f.server.config.Codex.(*creationTestCodex).transcript = codex.Transcript{ThreadID: "thread-1", LatestTurnID: "approved", Status: "idle", CollaborationMode: "plan", Model: "model-1", ReasoningEffort: "high",
		Entries: []codex.TranscriptEntry{{Kind: "plan", TurnID: "approved", TurnStatus: "completed", Text: plan}}}
	response := postCreation(t, f.server, "/api/sessions/source/implement-plan",
		fmt.Sprintf(`{"action":"new","name":"plan","creationDate":"2026-10-24","provider":"openai","model":"model-1","reasoningEffort":"high","planTurnId":"approved","planSha256":%q}`, planDigest(plan)), "application/json")
	if response.Code != http.StatusAccepted {
		t.Fatal(response.Code, response.Body.String())
	}
	f.wait(t)
	failed, _ := f.server.currentCreation("2026-10-24-plan")
	if failed.State != "failed" || !strings.Contains(failed.Error, "fixture terminal interruption") || f.count(t, "native_start") != 1 || f.count(t, "native_send") != 0 {
		t.Fatalf("missing real journal-bound interruption: %+v events=%s", failed, f.events(t))
	}
	if err := os.Rename(filepath.Join(f.server.config.Workspace, "work", "source"), filepath.Join(f.server.config.Workspace, "retained-source")); err != nil {
		t.Fatal(err)
	}
	f.config["failTerminal"] = false
	f.writeConfig(t)
	f.retry(t, failed, http.StatusAccepted)
	f.wait(t)
	ready, _ := f.server.currentCreation(failed.Request.Slug)
	f.assertSameRequest(t, failed, ready)
	if ready.State != "ready" || ready.Attempt != failed.Attempt+1 || f.server.proveCreation(ready) != nil || f.count(t, "source_attempt") != 1 || f.count(t, "native_start") != 1 || f.count(t, "native_send") != 1 {
		t.Fatalf("recovery lost its accepted plan or required the missing source: %+v events=%s", ready, f.events(t))
	}
}
