package session

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

const workflowMetadataFixture = `{"unrelated":{"allowed":true},"piQuestionsSchema":1,"piPlanSchema":1,"piToolPolicy":{"policy":1,"conversationSchema":2},"hostCallbacks":1,"linearLineage":1,"workflowObservations":1}`

func TestPiWorkflowContractMatchesRuntimeMetadataSource(t *testing.T) {
	source, err := os.ReadFile("runtime-contract.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(source, runtimeContractJSON) {
		t.Fatal("embedded runtime metadata differs from the package source")
	}
	if _, err := DecodePiWorkflowContract(source); err != nil {
		t.Fatalf("runtime metadata lacks the complete workflow contract: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(source, &fields); err != nil {
		t.Fatal(err)
	}
	team, err := DecodePiTeamRecordsContract(fields["piTeamRecords"])
	if err != nil || team != PiTeamRecords() {
		t.Fatalf("runtime metadata changed the retained team contract: %+v %v", team, err)
	}
}

func TestDecodePiWorkflowContractRequiresCompleteWholeMetadata(t *testing.T) {
	if _, err := DecodePiWorkflowContract([]byte(workflowMetadataFixture)); err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(workflowMetadataFixture), &fields); err != nil {
		t.Fatal(err)
	}
	for name := range fields {
		if name == "unrelated" {
			continue
		}
		for _, bad := range []string{"null", "false", `"1"`, "1.0", "0", "3", `{}`, `{"policy":1,"conversationSchema":2,"extra":1}`} {
			copy := map[string]json.RawMessage{}
			for k, v := range fields {
				copy[k] = v
			}
			copy[name] = json.RawMessage(bad)
			data, _ := json.Marshal(copy)
			if _, err := DecodePiWorkflowContract(data); err == nil {
				t.Fatalf("accepted %s=%s", name, bad)
			}
		}
		copy := map[string]json.RawMessage{}
		for k, v := range fields {
			if k != name {
				copy[k] = v
			}
		}
		data, _ := json.Marshal(copy)
		if _, err := DecodePiWorkflowContract(data); err == nil {
			t.Fatal("accepted absent", name)
		}
	}
	for _, bad := range []string{"null", "[]", "{}", workflowMetadataFixture + "{}", strings.Repeat(" ", 65537), strings.Replace(workflowMetadataFixture, `"policy":1`, `"policy":1,"policy":1`, 1), strings.Replace(workflowMetadataFixture, `"hostCallbacks":1`, `"hostCallbacks":1,"hostCallbacks":1`, 1), strings.Replace(workflowMetadataFixture, `"conversationSchema":2`, `"conversationSchema":null`, 1)} {
		if _, err := DecodePiWorkflowContract([]byte(bad)); err == nil {
			t.Fatal("accepted invalid metadata", bad[:min(len(bad), 100)])
		}
	}
	for _, field := range []string{"policy", "conversationSchema"} {
		for _, bad := range []string{"null", "false", `"1"`, "1.0", "0", "3"} {
			var nested map[string]json.RawMessage
			if err := json.Unmarshal(fields["piToolPolicy"], &nested); err != nil {
				t.Fatal(err)
			}
			nested[field] = json.RawMessage(bad)
			copy := map[string]json.RawMessage{}
			for k, v := range fields {
				copy[k] = v
			}
			copy["piToolPolicy"], _ = json.Marshal(nested)
			data, _ := json.Marshal(copy)
			if _, err := DecodePiWorkflowContract(data); err == nil {
				t.Fatal("accepted unsupported nested policy", field, bad)
			}
		}
	}
}
