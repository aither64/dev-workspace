package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func compiledWorkspacePolicy(roots []string) map[string]any {
	entries := []any{
		map[string]any{"path": map[string]any{"type": "special", "value": map[string]any{"kind": "root"}}, "access": "read"},
		map[string]any{"path": map[string]any{"type": "special", "value": map[string]any{"kind": "slash_tmp"}}, "access": "write"},
	}
	for _, root := range roots {
		entries = append(entries, map[string]any{"path": map[string]any{"type": "path", "path": root}, "access": "write"})
		for _, name := range []string{".git", ".agents", ".codex", ".aws"} {
			entries = append(entries, map[string]any{"path": map[string]any{"type": "path", "path": filepath.Join(root, name)}, "access": "read", "missing_path_behavior": "skip"})
		}
	}
	return map[string]any{"type": "managed", "network": "restricted", "file_system": map[string]any{"type": "restricted", "entries": entries}}
}

func TestColdWorkspaceSnapshotReconstructsLatestPermissions(t *testing.T) {
	profile := compiledWorkspacePolicy([]string{"/workspace", "/workspace", "/tmp"})
	entries := profile["file_system"].(map[string]any)["entries"].([]any)
	for left, right := 0, len(entries)-1; left < right; left, right = left+1, right-1 {
		entries[left], entries[right] = entries[right], entries[left]
	}
	path := filepath.Join(t.TempDir(), "history.jsonl")
	records := []any{
		map[string]any{"type": "turn_context", "payload": map[string]any{"approval_policy": "never", "sandbox_policy": map[string]any{"type": "danger-full-access"}, "permission_profile": map[string]any{"type": "disabled"}}},
		map[string]any{"type": "event_msg", "payload": map[string]any{"type": "thread_settings_applied", "thread_settings": map[string]any{"approval_policy": "on-request", "approvals_reviewer": "user", "permission_profile": profile, "runtime_workspace_roots": []any{"/workspace"}}}},
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if err := json.NewEncoder(file).Encode(record); err != nil {
			t.Fatal(err)
		}
	}
	file.Close()
	params := map[string]any{"cwd": "/workspace/work/session", "config": map[string]any{"model_reasoning_effort": "xhigh"}}
	if err := preserveColdNativePolicy(path, params); err != nil {
		t.Fatal(err)
	}
	if params["sandbox"] != "workspace-write" || params["approvalPolicy"] != "on-request" {
		t.Fatalf("older policy substituted: %#v", params)
	}
	if !reflect.DeepEqual(params["runtimeWorkspaceRoots"], []any{"/workspace"}) {
		t.Fatalf("runtime roots changed: %#v", params)
	}
	got := params["config"].(map[string]any)["sandbox_workspace_write"]
	want := map[string]any{"writable_roots": []string{"/tmp", "/workspace"}, "network_access": false, "exclude_tmpdir_env_var": true, "exclude_slash_tmp": false}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("latest compiled policy changed: %#v", got)
	}
}

func TestColdWorkspaceSnapshotNeverDropsRestrictions(t *testing.T) {
	for _, change := range []string{"deny", "extra read", "missing protection", "glob", "root write", "outside cwd", "custom missing behavior", "unknown filesystem option"} {
		t.Run(change, func(t *testing.T) {
			profile := compiledWorkspacePolicy([]string{"/workspace", "/tmp"})
			fs := profile["file_system"].(map[string]any)
			entries := fs["entries"].([]any)
			switch change {
			case "deny":
				entries = append(entries, map[string]any{"path": map[string]any{"type": "path", "path": "/secret"}, "access": "deny"})
			case "extra read":
				entries = append(entries, map[string]any{"path": map[string]any{"type": "path", "path": "/workspace/private"}, "access": "read"})
			case "missing protection":
				entries = entries[:len(entries)-1]
			case "glob":
				entries = append(entries, map[string]any{"path": map[string]any{"type": "glob_pattern", "pattern": "**/.env"}, "access": "deny"})
			case "root write":
				entries[0].(map[string]any)["access"] = "write"
			case "outside cwd":
				profile = compiledWorkspacePolicy([]string{"/outside"})
				fs = profile["file_system"].(map[string]any)
				entries = fs["entries"].([]any)
			case "custom missing behavior":
				entries[3].(map[string]any)["missing_path_behavior"] = "different"
			case "unknown filesystem option":
				fs["glob_scan_max_depth"] = float64(3)
			}
			fs["entries"] = entries
			if _, err := coldWorkspaceSandbox(profile, "/workspace/work/session"); err == nil {
				t.Fatalf("unrepresentable %s accepted", change)
			}
		})
	}
}
