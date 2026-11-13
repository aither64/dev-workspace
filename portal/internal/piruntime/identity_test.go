package piruntime

import (
	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"testing"
)

func TestHistoryReferenceGolden(t *testing.T) {
	ref := agentbackend.ConversationRef{Session: agentbackend.SessionRef{Workspace: "/workspace", Slug: "2026-11-11-example", Incarnation: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, Backend: agentbackend.Pi, Member: "lead", Incarnation: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", NativeID: "11111111-1111-4111-8111-111111111111"}
	const want = "945c3c7f31701ba0efc341967ef47fa5a8b4734b33a3b00bc2822c4f15a84789"
	if got := historyReference(ref); got != want {
		t.Fatal(got)
	}
	other := ref
	other.Member = "implementer1"
	if historyReference(other) == want {
		t.Fatal("member omitted")
	}
	other = ref
	other.NativeID = "22222222-2222-4222-8222-222222222222"
	if historyReference(other) == want {
		t.Fatal("native identity omitted")
	}
}
