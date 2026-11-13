package session

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

const roleInstructionsMetadataFixture = `{"unrelated":{"allowed":true},"piQuestionsSchema":1,"piPlanSchema":1,"piToolPolicy":{"policy":1,"conversationSchema":2},"hostCallbacks":1,"linearLineage":1,"workflowObservations":1,"piRoleInstructions":1}`

func TestPiRoleInstructionsContractMatchesRuntimeMetadataSource(t *testing.T) {
	source, err := os.ReadFile("runtime-contract.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(source, runtimeContractJSON) {
		t.Fatal("embedded runtime metadata differs from the package source")
	}
	contract, err := DecodePiRoleInstructionsContract(source)
	if err != nil {
		t.Fatalf("runtime metadata lacks the complete role instructions contract: %v", err)
	}
	workflow, err := DecodePiWorkflowContract(source)
	if err != nil || contract != (PiRoleInstructionsContract{Workflow: workflow, Version: 1}) {
		t.Fatal("runtime metadata changed the role instructions contract", contract, err)
	}
}

func TestDecodePiRoleInstructionsContractSelectsWholeBoundedMetadata(t *testing.T) {
	data := []byte(roleInstructionsMetadataFixture)
	before := append([]byte(nil), data...)
	contract, err := DecodePiRoleInstructionsContract(data)
	if err != nil {
		t.Fatal(err)
	}
	workflow, err := DecodePiWorkflowContract(data)
	if err != nil || contract != (PiRoleInstructionsContract{Workflow: workflow, Version: 1}) {
		t.Fatal("instruction selection omitted owning workflow prerequisites", contract, err)
	}
	contract.Workflow.PiQuestionsSchema = 0
	again, err := DecodePiRoleInstructionsContract(data)
	if err != nil || again.Workflow != workflow || !bytes.Equal(data, before) {
		t.Fatal("selection shared mutable state or changed input", err)
	}
	padded := append(append([]byte(nil), data...), bytes.Repeat([]byte(" "), (64<<10)-len(data))...)
	if _, err := DecodePiRoleInstructionsContract(padded); err != nil {
		t.Fatal("exact size boundary refused", err)
	}
	if _, err := DecodePiRoleInstructionsContract(append(padded, ' ')); err == nil {
		t.Fatal("metadata beyond 64 KiB accepted")
	}
}

func TestDecodePiRoleInstructionsContractRequiresEveryExactPrerequisite(t *testing.T) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(roleInstructionsMetadataFixture), &fields); err != nil {
		t.Fatal(err)
	}
	clone := func() map[string]json.RawMessage {
		copy := map[string]json.RawMessage{}
		for name, value := range fields {
			copy[name] = value
		}
		return copy
	}
	refuse := func(t *testing.T, value any) {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if result, err := DecodePiRoleInstructionsContract(data); err == nil || result != (PiRoleInstructionsContract{}) {
			t.Fatalf("unsupported metadata returned a selection: %s: %#v %v", data, result, err)
		}
	}
	invalidValues := []string{"null", "false", `"1"`, "1.0", "1e0", "0", "-1", "2", "[]", "{}"}
	for name := range fields {
		if name == "unrelated" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			missing := clone()
			delete(missing, name)
			refuse(t, missing)
			for _, invalid := range invalidValues {
				copy := clone()
				copy[name] = json.RawMessage(invalid)
				refuse(t, copy)
			}
		})
	}
	for _, key := range []string{"policy", "conversationSchema"} {
		t.Run("piToolPolicy/"+key, func(t *testing.T) {
			var nested map[string]json.RawMessage
			if err := json.Unmarshal(fields["piToolPolicy"], &nested); err != nil {
				t.Fatal(err)
			}
			delete(nested, key)
			copy := clone()
			copy["piToolPolicy"], _ = json.Marshal(nested)
			refuse(t, copy)
			for _, invalid := range append(append([]string(nil), invalidValues...), "3") {
				if err := json.Unmarshal(fields["piToolPolicy"], &nested); err != nil {
					t.Fatal(err)
				}
				// conversationSchema is exactly 2; policy is exactly 1.
				if key == "conversationSchema" && invalid == "2" {
					invalid = "1"
				}
				nested[key] = json.RawMessage(invalid)
				copy["piToolPolicy"], _ = json.Marshal(nested)
				refuse(t, copy)
			}
		})
	}
}

func TestDecodePiRoleInstructionsContractRefusesAmbiguousEncodingAndShape(t *testing.T) {
	for _, invalid := range []string{
		"", "null", "[]", "{}", roleInstructionsMetadataFixture + "{}",
		`{"piRoleInstructions":1}`,
		`{"piNativeRouting":{"policy":1,"recordSchema":3},"piRoleInstructions":1}`,
		strings.Replace(roleInstructionsMetadataFixture, `"piRoleInstructions":1`, `"piRoleInstructions":1,"piRoleInstructions":1`, 1),
		strings.Replace(roleInstructionsMetadataFixture, `"piRoleInstructions":1`, `"piRoleInstructions":1,"piRole\u0049nstructions":1`, 1),
		strings.Replace(roleInstructionsMetadataFixture, `"piRoleInstructions":1`, `"PiRoleInstructions":1`, 1),
		strings.Replace(roleInstructionsMetadataFixture, `"policy":1`, `"policy":1,"policy":1`, 1),
		strings.Replace(roleInstructionsMetadataFixture, `"policy":1`, `"Policy":1`, 1),
		strings.Replace(roleInstructionsMetadataFixture, `"conversationSchema":2`, `"conversationSchema":2,"extra":1`, 1),
		strings.Replace(roleInstructionsMetadataFixture, `"allowed":true`, `"allowed":true,"allowed":true`, 1),
		strings.Replace(roleInstructionsMetadataFixture, `"allowed":true`, `"allowed":null`, 1),
		strings.Replace(roleInstructionsMetadataFixture, `"allowed"`, "\"\xff\"", 1),
	} {
		if result, err := DecodePiRoleInstructionsContract([]byte(invalid)); err == nil || result != (PiRoleInstructionsContract{}) {
			t.Fatal("ambiguous metadata accepted", invalid, result, err)
		}
	}
}
