package session

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPiTeamRecordsContractRequiresCompleteExactSupport(t *testing.T) {
	valid := `{"policy":1,"roster":2,"monitorOperation":2,"communication":2,"progressObserver":2,"utilityReceipt":2}`
	if _, err := DecodePiTeamRecordsContract([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	if PiTeamRecords() != (PiTeamRecordsContract{Policy: 1, Roster: 2, MonitorOperation: 2, Communication: 2, ProgressObserver: 2, UtilityReceipt: 2}) {
		t.Fatal("published codec contract differs from implemented readers")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(valid), &fields); err != nil {
		t.Fatal(err)
	}
	for field := range fields {
		for _, replacement := range []string{"null", "false", `"2"`, "2.0", "0", "3"} {
			changed := map[string]json.RawMessage{}
			for key, value := range fields {
				changed[key] = value
			}
			changed[field] = json.RawMessage(replacement)
			data, _ := json.Marshal(changed)
			if _, err := DecodePiTeamRecordsContract(data); err == nil {
				t.Fatalf("accepted %s=%s", field, replacement)
			}
		}
		copy := map[string]json.RawMessage{}
		for key, value := range fields {
			if key != field {
				copy[key] = value
			}
		}
		data, _ := json.Marshal(copy)
		if _, err := DecodePiTeamRecordsContract(data); err == nil {
			t.Fatalf("accepted missing %s", field)
		}
	}
	for _, invalid := range []string{"null", "[]", "{}", valid + "{}", strings.Replace(valid, `"policy":1`, `"policy":1,"policy":1`, 1), strings.Replace(valid, `"policy":1`, `"unknown":1,"policy":1`, 1)} {
		if _, err := DecodePiTeamRecordsContract([]byte(invalid)); err == nil {
			t.Fatalf("accepted ambiguous/unsupported map: %s", invalid)
		}
	}
}
