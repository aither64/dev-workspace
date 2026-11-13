package piruntime

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
)

func TestTaggedMappingPreservesRetainedShapeAndRefusesOwnerConfusion(t *testing.T) {
	_, utility, _, settings := utilityReceiptFixture(t)
	root := filepath.Join(t.TempDir(), "owned")
	ref := agentbackend.ConversationRef{Session: utility.Utility.Session, Backend: agentbackend.Pi, Member: "lead", Incarnation: utility.Utility.Incarnation, NativeID: utility.Utility.NativeID}
	record := conversationRecord{Schema: 1, CreationID: "11111111-1111-4111-8111-111111111111", Conversation: ref, HistoryRef: historyReference(ref), HistoryFile: filepath.Join(root, "history/native.jsonl"), Generation: strings.Repeat("c", 64), Settings: settings, InitialSettings: settings}
	data, _ := json.Marshal(record)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(data, &fields)
	if len(fields) != 14 || fields["Conversation"] == nil || fields["OwnerKind"] != nil || fields["Utility"] != nil {
		t.Fatal("retained schema shape changed", string(data))
	}
	if err := validateConversationRecord(record, ref, root); err != nil {
		t.Fatal(err)
	}
	utilityRecord := record
	utilityRecord.Schema = 2
	utilityRecord.OwnerKind = "utility"
	utilityRecord.Utility = &utility.Utility
	utilityRecord.Conversation = agentbackend.ConversationRef{}
	utilityRecord.HistoryRef = ""
	data, _ = json.Marshal(utilityRecord)
	_ = json.Unmarshal(data, &fields)
	var utilityFields map[string]json.RawMessage
	_ = json.Unmarshal(data, &utilityFields)
	if utilityFields["Conversation"] != nil || utilityFields["HistoryRef"] != nil || utilityFields["Utility"] == nil || utilityFields["OwnerKind"] == nil {
		t.Fatal("utility masqueraded as retained", string(data))
	}
	if err := validateUtilityRecord(utilityRecord, utility.Utility, root); err != nil {
		t.Fatal(err)
	}
	if err := validateConversationRecord(utilityRecord, ref, root); !errors.Is(err, ErrConflict) {
		t.Fatal("utility mapping accepted in member root", err)
	}
	if err := validateUtilityRecord(record, utility.Utility, root); !errors.Is(err, ErrConflict) {
		t.Fatal("member mapping accepted in utility root", err)
	}
	other := utility.Utility
	other.OperationID = "44444444-4444-4444-8444-444444444444"
	if err := validateUtilityRecord(utilityRecord, other, root); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	utilityRecord.HistoryFile = filepath.Join(root, "outside.jsonl")
	if err := validateUtilityRecord(utilityRecord, utility.Utility, root); !errors.Is(err, ErrScope) {
		t.Fatal(err)
	}
}
