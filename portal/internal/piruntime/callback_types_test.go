package piruntime

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

func TestLinearSelectionCrossLanguageFixture(t *testing.T) {
	bytes, err := lineageBytes([]string{"a", "é", ""})
	if err != nil || hex.EncodeToString(bytes) != "00000003000000016100000002c3a900000000" {
		t.Fatal(bytes, err)
	}
	ref := agentbackend.ConversationRef{Session: agentbackend.SessionRef{Workspace: "/espace/é", Slug: "2026-11-11-fixture", Incarnation: strings.Repeat("a", 64)}, Backend: agentbackend.Pi, Member: "lead", Incarnation: strings.Repeat("b", 64), NativeID: "11111111-1111-4111-8111-111111111111"}
	handle := agentbackend.ExecutionHandle{Conversation: ref, Generation: strings.Repeat("c", 64)}
	subject := CallbackSubject{Kind: "conversation", Conversation: &handle}
	token, err := LinearSelectionToken(subject, "root")
	if err != nil || token != "linear-v1:285c02f4e5a7ed8e2396883d73ae191eaa9beef6e2b7218f8b6d85e374863d2c" {
		t.Fatal(token, err)
	}
	handle.Generation = strings.Repeat("d", 64)
	same, err := LinearSelectionToken(subject, "root")
	if err != nil || same != token {
		t.Fatal(same, err)
	}
	utility := CallbackSubject{Kind: "utility", Utility: &UtilityCallbackSubject{Session: ref.Session, MonitorOperationID: "22222222-2222-4222-8222-222222222222", Incarnation: ref.Incarnation, NativeID: ref.NativeID, Generation: handle.Generation}}
	other, err := LinearSelectionToken(utility, "root")
	if err != nil || other == token {
		t.Fatal(other, err)
	}
}
