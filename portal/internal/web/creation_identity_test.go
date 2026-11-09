package web

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

// Serialize the production authorities into the exact helper used by the VM.
func TestCreationReadyIdentityContract(t *testing.T) {
	slug, receiptID, thread := "2026-11-10-before", strings.Repeat("a", 64), "fixture-root"
	goal := "Synthetic identity contract"
	status := preparationStatus{RequestID: preparationTestID(251), ReceiptID: receiptID, Slug: slug, State: "ready"}
	receipt := creationReceipt{Schema: 3, Workspace: "/fixture", ReceiptID: receiptID, State: "ready", Validated: true,
		Goal: goal, Model: "gpt-5.5", Effort: "high", Request: creationRequest{Kind: "new", Slug: slug},
		DirectTeam: &teamruntime.Preset{ID: "development", CatalogDigest: "catalog", TeamDigest: "team"}}
	complete := creationEvidence{Schema: 3, Workspace: "/fixture", Slug: slug, ReceiptID: receiptID, ThreadID: thread,
		GoalSHA256: creationGoalDigest(goal), Model: "gpt-5.5", Effort: "high", TrackingDevice: 1, TrackingInode: 2,
		Team: "development", CatalogDigest: "catalog", TeamDigest: "team"}
	manifest := session.Manifest{Slug: slug, Codex: session.Codex{ThreadID: thread},
		Creation: session.Creation{State: "ready", InitialGoalSent: true, GoalSHA256: creationGoalDigest(goal)}}
	original := map[string]any{"status": status, "receipt": receipt, "completion": complete, "manifest": manifest,
		"tracking": map[string]any{"cwd": "/fixture/work/" + slug, "device": 1, "inode": 2},
		"expected": map[string]any{"workspace": "/fixture", "cwd": "/fixture/work/" + slug, "slug": slug,
			"requestId": status.RequestID, "receiptId": receiptID, "model": "gpt-5.5", "effort": "high"}}
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"ready-status-without-threadId", nil},
		{"missing-completion", func(v map[string]any) { delete(v, "completion") }},
		{"nonready-status", func(v map[string]any) { v["status"].(map[string]any)["state"] = "running" }},
		{"nonready-receipt", func(v map[string]any) { v["receipt"].(map[string]any)["state"] = "running" }},
		{"wrong-receipt", func(v map[string]any) { v["completion"].(map[string]any)["receiptId"] = "wrong" }},
		{"wrong-slug", func(v map[string]any) { v["completion"].(map[string]any)["slug"] = "wrong" }},
		{"wrong-workspace", func(v map[string]any) { v["completion"].(map[string]any)["workspace"] = "/other" }},
		{"wrong-root", func(v map[string]any) { v["tracking"].(map[string]any)["cwd"] = "/other" }},
		{"changed-pair", func(v map[string]any) { v["completion"].(map[string]any)["effort"] = "xhigh" }},
		{"missing-thread", func(v map[string]any) { delete(v["completion"].(map[string]any), "threadId") }},
		{"conflicting-manifest", func(v map[string]any) { v["manifest"].(map[string]any)["codex"].(map[string]any)["threadId"] = "other" }},
		{"nonready-manifest", func(v map[string]any) {
			v["manifest"].(map[string]any)["creation"].(map[string]any)["state"] = "starting"
		}},
		{"unsent-goal", func(v map[string]any) {
			v["manifest"].(map[string]any)["creation"].(map[string]any)["initialGoalSent"] = false
		}},
		{"changed-team", func(v map[string]any) { v["completion"].(map[string]any)["teamDigest"] = "other" }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			var data map[string]any
			if err := json.Unmarshal(encoded, &data); err != nil {
				t.Fatal(err)
			}
			if _, exists := data["status"].(map[string]any)["threadId"]; exists {
				t.Fatal("production status invented threadId")
			}
			if test.mutate != nil {
				test.mutate(data)
			}
			input := filepath.Join(t.TempDir(), "authorities.json")
			payload, _ := json.Marshal(data)
			if err := os.WriteFile(input, payload, 0600); err != nil {
				t.Fatal(err)
			}
			command := exec.Command("python3", "../../../test/creation_transport.py", "ready-identity", input)
			output, err := command.CombinedOutput()
			if test.mutate != nil {
				if err == nil {
					t.Fatalf("invalid authority accepted: %s", output)
				}
				var refusal struct {
					Refused bool `json:"refused"`
				}
				if json.Unmarshal(output, &refusal) != nil || !refusal.Refused {
					t.Fatalf("helper did not refuse the authority: %v %s", err, output)
				}
				return
			}
			if err != nil {
				t.Fatalf("ready identity: %v %s", err, output)
			}
			var binding map[string]any
			if err := json.Unmarshal(output, &binding); err != nil {
				t.Fatal(err)
			}
			if binding["threadId"] != thread || binding["requestId"] != status.RequestID || binding["receiptId"] != receiptID {
				t.Fatalf("wrong binding: %s", output)
			}
		})
	}
}
