package agentteams

import (
	"encoding/json"
	"strings"
	"testing"
)

func directCreationSnapshot(withInstructions bool) map[string]any {
	member := map[string]any{
		"role": "scribe", "address": "scribe0", "model": "model-1", "reasoningEffort": "high",
		"behavior": "general", "access": "read_only",
	}
	team := map[string]any{
		"id": "custom", "name": "Custom team", "description": "A lead and scribe",
		"catalogDigest": strings.Repeat("a", 64), "teamDigest": strings.Repeat("b", 64),
		"leadModel": "model-1", "leadEffort": "high", "roles": []string{"lead", "scribe0"},
		"members": []any{member},
	}
	if withInstructions {
		team["leadInstructions"] = "Coordinate the team.\nWait for the first request."
		member["purpose"] = "general"
		member["instructions"] = "Summarize the assigned material."
	} else {
		member["role"] = "reviewer"
		member["address"] = "reviewer0"
		member["behavior"] = "reviewer"
		team["roles"] = []string{"lead", "reviewer0"}
	}
	return team
}

func TestDirectCreationSnapshotAcceptsOldAndPromptBearingTeams(t *testing.T) {
	for _, withInstructions := range []bool{false, true} {
		data, err := json.Marshal(directCreationSnapshot(withInstructions))
		if err != nil {
			t.Fatal(err)
		}
		if err := validateDirectCreationTeam(data, "model-1", "high"); err != nil {
			t.Fatalf("snapshot with instructions=%t: %v", withInstructions, err)
		}
	}
}

func TestDirectCreationSnapshotRejectsMixedAndInvalidPromptFields(t *testing.T) {
	tests := map[string]func(map[string]any){
		"missing member instructions": func(team map[string]any) {
			delete(team["members"].([]any)[0].(map[string]any), "instructions")
		},
		"wrong purpose": func(team map[string]any) {
			team["members"].([]any)[0].(map[string]any)["purpose"] = "review"
		},
		"empty lead instructions": func(team map[string]any) {
			team["leadInstructions"] = ""
		},
		"unknown member field": func(team map[string]any) {
			team["members"].([]any)[0].(map[string]any)["other"] = true
		},
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			team := directCreationSnapshot(true)
			change(team)
			data, err := json.Marshal(team)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateDirectCreationTeam(data, "model-1", "high"); err == nil {
				t.Fatal("accepted a malformed frozen prompt snapshot")
			}
		})
	}
}
