package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

func TestPlanDecisionRejectsHistoricalProposals(t *testing.T) {
	for _, latest := range []string{"", "ordinary", "empty", "failed", "interrupted"} {
		for _, action := range []string{"same", "new"} {
			t.Run(latest+"/"+action, func(t *testing.T) {
				server := newTestServer(t)
				defer server.Close()
				prepareInteractiveConversation(t, server, "source")
				controller := &browserContractCodex{transcript: codex.Transcript{
					ThreadID: "thread-1", Status: "idle", LatestTurnID: latest, CollaborationMode: "plan",
					Entries: []codex.TranscriptEntry{{TurnID: "old", TurnStatus: "completed", Kind: "plan", Text: "Earlier plan"}},
				}}
				if latest == "ordinary" {
					controller.transcript.Entries = append(controller.transcript.Entries,
						codex.TranscriptEntry{TurnID: latest, TurnStatus: "completed", Kind: "agentMessage", Text: "An ordinary reply"})
				}
				if latest == "failed" || latest == "interrupted" {
					controller.transcript.Entries = append(controller.transcript.Entries,
						codex.TranscriptEntry{TurnID: latest, TurnStatus: latest, Kind: "plan", Text: "Unfinished plan"})
				}
				server.config.Codex = controller
				body := fmt.Sprintf(`{"action":%q,"planTurnId":"old","planSha256":%q,"planText":"Earlier plan","clientUserMessageId":"00000000-0000-4000-8000-000000000001","name":"stale-plan","creationDate":"2026-09-13"}`, action, planDigest("Earlier plan"))
				response := postCreation(t, server, "/api/sessions/source/implement-plan", body, "application/json")
				if action == "same" {
					if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "stale") {
						t.Fatalf("response: %d %s", response.Code, response.Body.String())
					}
				} else {
					if response.Code != http.StatusAccepted {
						t.Fatalf("acceptance: %d %s", response.Code, response.Body.String())
					}
					receipt := awaitCreation(t, server, "2026-09-13-stale-plan")
					if receipt.Validated || !strings.Contains(receipt.Error, "stale") {
						t.Fatalf("creation: %#v", receipt)
					}
				}
				if controller.sendCount != 0 || controller.messageID != "" || controller.settings.CollaborationMode != "" {
					t.Fatal("stale plan changed the conversation")
				}
			})
		}
	}
}

func TestImplementPlanRecoversReceiptAfterConversationAdvances(t *testing.T) {
	server := newTestServer(t)
	const clientID = "00000000-0000-4000-8000-000000000001"
	controller := &browserContractCodex{
		transcript: codex.Transcript{ThreadID: "thread-1", LatestTurnID: "implementation", Status: "active", CollaborationMode: "default"},
		message:    "Implement the plan.", messageID: clientID, actionContext: "plan:" + planDigest("Earlier plan"), sendCount: 1,
	}
	server.config.Codex = controller
	body := fmt.Sprintf(`{"action":"same","planTurnId":"old","planSha256":%q,"clientUserMessageId":%q}`, planDigest("Earlier plan"), clientID)
	response := httptest.NewRecorder()
	server.implementPlan(response, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)), &session.Summary{Manifest: session.Manifest{
		Slug: "example", Codex: session.Codex{ThreadID: "thread-1"},
	}})
	if response.Code != http.StatusAccepted || controller.sendCount != 1 || controller.settings.CollaborationMode != "" {
		t.Fatalf("receipt recovery: %d %s, sends=%d, settings=%#v", response.Code, response.Body.String(), controller.sendCount, controller.settings)
	}
}
