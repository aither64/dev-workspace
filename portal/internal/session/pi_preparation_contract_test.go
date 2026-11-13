package session

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestPiPreparationContractWholeMetadata(t *testing.T) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(roleInstructionsMetadataFixture), &fields); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"sessionBackendBindingSchema": "1", "sessionBackendBindingPolicy": "1", "runtimeAuthoritySchema": "2", "piCreationSchema": "4", "piPreparationSchema": "2"} {
		fields[key] = json.RawMessage(value)
	}
	good, _ := json.Marshal(fields)
	if value, err := DecodePiPreparationContract(good); err != nil || value.Schema != 2 {
		t.Fatal(value, err)
	}
	for key := range fields {
		if key == "unrelated" {
			continue
		}
		original := fields[key]
		for _, bad := range []json.RawMessage{nil, json.RawMessage(`null`), json.RawMessage(`false`), json.RawMessage(`"2"`), json.RawMessage(`2.0`), json.RawMessage(`0`)} {
			if bad == nil {
				delete(fields, key)
			} else {
				fields[key] = bad
			}
			data, _ := json.Marshal(fields)
			if value, err := DecodePiPreparationContract(data); err == nil || value != (PiPreparationContract{}) {
				t.Fatalf("partial metadata accepted at %s: %s", key, data)
			}
		}
		fields[key] = original
	}
	duplicate := strings.Replace(string(good), `"piPreparationSchema":2`, `"piPreparationSchema":2,"piPreparationSchema":2`, 1)
	for _, data := range []string{duplicate, string(good) + `{}`, string(good) + strings.Repeat(" ", 64<<10)} {
		if _, err := DecodePiPreparationContract([]byte(data)); err == nil {
			t.Fatal("ambiguous/oversized metadata accepted")
		}
	}
}

func TestPiPreparationContractMatchesRuntimeMetadata(t *testing.T) {
	source, err := os.ReadFile("runtime-contract.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(source, runtimeContractJSON) {
		t.Fatal("embedded/runtime metadata differs")
	}
	if contract, err := DecodePiPreparationContract(source); err != nil || contract.Schema != 2 {
		t.Fatal(contract, err)
	}
}
