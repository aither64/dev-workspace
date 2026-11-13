package session

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

const nativeRoutingMetadataFixture = `{"unrelated":{"allowed":true},"piQuestionsSchema":1,"piPlanSchema":1,"piToolPolicy":{"policy":1,"conversationSchema":2},"hostCallbacks":1,"linearLineage":1,"workflowObservations":1,"piNativeRouting":{"policy":1,"recordSchema":3}}`

func TestDecodePiNativeRoutingContractSelectsWholeBoundedMetadata(t *testing.T) {
	data := []byte(nativeRoutingMetadataFixture)
	before := append([]byte(nil), data...)
	contract, err := DecodePiNativeRoutingContract(data)
	if err != nil {
		t.Fatal(err)
	}
	workflow, err := DecodePiWorkflowContract(data)
	if err != nil || contract != (PiNativeRoutingContract{Workflow: workflow, Policy: 1, RecordSchema: 3}) {
		t.Fatal("native selection omitted owning workflow prerequisites", contract, err)
	}
	contract.Workflow.PiQuestionsSchema = 0
	again, err := DecodePiNativeRoutingContract(data)
	if err != nil || again.Workflow != workflow || !bytes.Equal(data, before) {
		t.Fatal("selection shared mutable state or changed input", err)
	}
	padded := append(append([]byte(nil), data...), bytes.Repeat([]byte(" "), (64<<10)-len(data))...)
	if _, err := DecodePiNativeRoutingContract(padded); err != nil {
		t.Fatal("exact size boundary refused", err)
	}
	if _, err := DecodePiNativeRoutingContract(append(padded, ' ')); err == nil {
		t.Fatal("metadata beyond 64 KiB accepted")
	}
}

func TestDecodePiNativeRoutingContractRequiresEveryExactPrerequisite(t *testing.T) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(nativeRoutingMetadataFixture), &fields); err != nil {
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
		if result, err := DecodePiNativeRoutingContract(data); err == nil || result != (PiNativeRoutingContract{}) {
			t.Fatalf("unsupported metadata returned a selection: %s: %#v %v", data, result, err)
		}
	}
	for name := range fields {
		if name == "unrelated" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			missing := clone()
			delete(missing, name)
			refuse(t, missing)
			for _, invalid := range []string{"null", "false", `"1"`, "1.0", "1e0", "0", "-1", "4", "[]", "{}"} {
				copy := clone()
				copy[name] = json.RawMessage(invalid)
				refuse(t, copy)
			}
		})
	}
	for name, keys := range map[string][]string{"piToolPolicy": {"policy", "conversationSchema"}, "piNativeRouting": {"policy", "recordSchema"}} {
		for _, key := range keys {
			var nested map[string]json.RawMessage
			if err := json.Unmarshal(fields[name], &nested); err != nil {
				t.Fatal(err)
			}
			delete(nested, key)
			copy := clone()
			copy[name], _ = json.Marshal(nested)
			refuse(t, copy)
			for _, invalid := range []string{"null", "false", `"1"`, "1.0", "1e0", "0", "-1", "4", "[]", "{}"} {
				if err := json.Unmarshal(fields[name], &nested); err != nil {
					t.Fatal(err)
				}
				nested[key] = json.RawMessage(invalid)
				copy[name], _ = json.Marshal(nested)
				refuse(t, copy)
			}
		}
		copy := clone()
		copy[name] = json.RawMessage(strings.TrimSuffix(string(fields[name]), "}") + `,"extra":1}`)
		refuse(t, copy)
	}
}

func TestDecodePiNativeRoutingContractRefusesAmbiguousEncodingAndShape(t *testing.T) {
	for _, invalid := range []string{
		"", "null", "[]", "{}", nativeRoutingMetadataFixture + "{}",
		`{"piNativeRouting":{"policy":1,"recordSchema":3}}`,
		strings.Replace(nativeRoutingMetadataFixture, `"recordSchema":3`, `"recordSchema":2`, 1),
		strings.Replace(nativeRoutingMetadataFixture, `"recordSchema":3`, `"recordSchema":3,"recordSchema":3`, 1),
		strings.Replace(nativeRoutingMetadataFixture, `"piNativeRouting":`, `"piNativeRouting":{},"piNativeRouting":`, 1),
		strings.Replace(nativeRoutingMetadataFixture, `"allowed":true`, `"allowed":true,"allowed":true`, 1),
		strings.Replace(nativeRoutingMetadataFixture, `"recordSchema":3`, `"RecordSchema":3`, 1),
		strings.Replace(nativeRoutingMetadataFixture, `"piNativeRouting":`, `"PiNativeRouting":`, 1),
	} {
		if _, err := DecodePiNativeRoutingContract([]byte(invalid)); err == nil {
			t.Fatal("ambiguous metadata accepted", invalid)
		}
	}
	if _, err := DecodePiNativeRoutingContract(append([]byte(nativeRoutingMetadataFixture), 0xff)); err == nil {
		t.Fatal("invalid UTF-8 accepted")
	}
}
