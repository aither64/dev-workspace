package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/aither64/codex-web/codex"
)

func scopedPermissionTestThread() providerThread {
	thread := providerThread{ID: "retained", Slug: "idle", Cwd: "/workspace/work/idle", Settings: codex.ThreadSettings{Model: "retained-model", ReasoningEffort: "low"}, Original: map[string]any{
		"thread": map[string]any{"id": "retained"}, "cwd": "/workspace/work/idle", "model": "retained-model", "modelProvider": "pool-a", "reasoningEffort": "low", "approvalPolicy": "never", "approvalsReviewer": "user", "runtimeWorkspaceRoots": []any{"/workspace"}, "activePermissionProfile": nil,
		"sandbox": map[string]any{"type": "workspaceWrite", "writableRoots": []any{"/workspace", "/tmp"}, "networkAccess": false, "excludeTmpdirEnvVar": false, "excludeSlashTmp": false},
	}}
	thread.ScopedPermissions = &scopedPermissionAuthority{thread.ID, thread.Cwd, []string{"/workspace"}, compiledWorkspacePolicy([]string{"/workspace", "/workspace", "/tmp"})}
	return thread
}

func TestScopedPermissionAuthorityPreservesAuthoredTmpdirBothDirections(t *testing.T) {
	thread := scopedPermissionTestThread()
	before, _ := json.Marshal(thread.Original)
	for _, target := range []string{"pool-b", "pool-a"} {
		params, err := providerResumeParams(thread, target)
		if err != nil {
			t.Fatal(err)
		}
		policy := params["config"].(map[string]any)["sandbox_workspace_write"].(map[string]any)
		if policy["exclude_tmpdir_env_var"] != true || policy["exclude_slash_tmp"] != false || policy["network_access"] != false || !reflect.DeepEqual(policy["writable_roots"], []string{"/tmp", "/workspace"}) {
			t.Fatalf("compiled authority lost: %#v", policy)
		}
		if params["modelProvider"] != target || !reflect.DeepEqual(params["runtimeWorkspaceRoots"], thread.Original["runtimeWorkspaceRoots"]) {
			t.Fatal("routing or runtime roots changed")
		}
	}
	after, _ := json.Marshal(thread.Original)
	if string(before) != string(after) {
		t.Fatal("native response was rewritten")
	}
	thread.ScopedPermissions = nil
	params, err := providerResumeParams(thread, "pool-b")
	if err != nil {
		t.Fatal(err)
	}
	if params["config"].(map[string]any)["sandbox_workspace_write"].(map[string]any)["exclude_tmpdir_env_var"] != false {
		t.Fatal("legacy schema-1 behavior was upgraded")
	}
}

func TestScopedPermissionAuthorityRejectsForeignOrUnsupportedEvidence(t *testing.T) {
	for _, change := range []string{"missing", "foreign thread", "foreign cwd", "foreign roots", "extra rule", "missing protection", "changed kind", "changed network"} {
		t.Run(change, func(t *testing.T) {
			thread := scopedPermissionTestThread()
			switch change {
			case "missing":
				thread.ScopedPermissions = nil
			case "foreign thread":
				thread.ScopedPermissions.ThreadID = "other"
			case "foreign cwd":
				thread.ScopedPermissions.Cwd = "/elsewhere"
			case "foreign roots":
				thread.ScopedPermissions.RuntimeWorkspaceRoots = []string{"/elsewhere"}
			case "changed kind":
				thread.ScopedPermissions.Profile = map[string]any{"type": "disabled"}
			case "changed network":
				thread.ScopedPermissions.Profile["network"] = "enabled"
			default:
				fs := thread.ScopedPermissions.Profile["file_system"].(map[string]any)
				entries := fs["entries"].([]any)
				if change == "extra rule" {
					entries = append(entries, map[string]any{"path": map[string]any{"type": "path", "path": "/private"}, "access": "read"})
				} else {
					entries = entries[:len(entries)-1]
				}
				fs["entries"] = entries
			}
			if _, err := scopedPermissionProjection(thread); err == nil {
				t.Fatal("unsafe authority accepted")
			}
		})
	}
}

func TestScopedPermissionAuthorityDoesNotReplaceBuiltinOrReadOnlyHandling(t *testing.T) {
	for _, kind := range []string{"builtin", "read-only", "disabled", "custom named"} {
		t.Run(kind, func(t *testing.T) {
			thread := scopedPermissionTestThread()
			switch kind {
			case "builtin":
				thread.Original["activePermissionProfile"] = map[string]any{"id": ":workspace"}
			case "read-only":
				thread.Original["sandbox"] = map[string]any{"type": "readOnly", "networkAccess": false}
			case "disabled":
				thread.Original["sandbox"] = map[string]any{"type": "dangerFullAccess"}
			case "custom named":
				thread.Original["activePermissionProfile"] = map[string]any{"id": "custom"}
			}
			if _, err := scopedPermissionProjection(thread); err == nil {
				t.Fatal("inapplicable compiled authority accepted")
			}
			thread.ScopedPermissions = nil
			required, err := needsScopedPermissionAuthority(thread)
			if kind == "custom named" {
				if err == nil {
					t.Fatal("custom named policy accepted")
				}
				return
			}
			if err != nil || required {
				t.Fatalf("existing policy path changed: %v %v", required, err)
			}
			params, err := providerResumeParams(thread, "pool-b")
			if err != nil {
				t.Fatal(err)
			}
			if kind == "builtin" && (params["permissions"] != ":workspace" || params["sandbox"] != nil) {
				t.Fatal("builtin permission identity changed")
			}
		})
	}
}

func TestScopedPermissionAuthorityReadsBoundNativeChronology(t *testing.T) {
	thread := scopedPermissionTestThread()
	for _, change := range []string{"none", "turn context", "foreign id", "foreign cwd", "stale provider", "stale roots", "missing compiled"} {
		t.Run(change, func(t *testing.T) {
			meta := map[string]any{"id": thread.ID, "cwd": thread.Cwd}
			settings := map[string]any{"cwd": thread.Cwd, "model": "retained-model", "model_provider_id": "pool-a", "reasoning_effort": "low", "approval_policy": "never", "approvals_reviewer": "user", "runtime_workspace_roots": []any{"/workspace"}, "permission_profile": thread.ScopedPermissions.Profile}
			switch change {
			case "foreign id":
				meta["id"] = "other"
			case "foreign cwd":
				meta["cwd"] = "/other"
			case "stale provider":
				settings["model_provider_id"] = "pool-b"
			case "stale roots":
				settings["runtime_workspace_roots"] = []any{"/other"}
			case "missing compiled":
				delete(settings, "permission_profile")
			}
			path := filepath.Join(t.TempDir(), "rollout.jsonl")
			file, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, record := range []any{map[string]any{"type": "session_meta", "payload": meta}, map[string]any{"type": "event_msg", "payload": map[string]any{"type": "thread_settings_applied", "thread_settings": settings}}} {
				if err := json.NewEncoder(file).Encode(record); err != nil {
					t.Fatal(err)
				}
			}
			if change == "turn context" {
				if err := json.NewEncoder(file).Encode(map[string]any{"type": "turn_context", "payload": map[string]any{"cwd": thread.Cwd, "model": "retained-model", "effort": "low", "approval_policy": "never", "permission_profile": thread.ScopedPermissions.Profile}}); err != nil {
					t.Fatal(err)
				}
			}
			file.Close()
			authority, err := readScopedPermissionAuthority(path, thread)
			if change == "none" || change == "turn context" {
				if err != nil || authority == nil {
					t.Fatalf("valid authority refused: %v", err)
				}
			} else if err == nil {
				t.Fatal("foreign/stale history accepted")
			}
		})
	}
}

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
		map[string]any{"type": "session_meta", "payload": map[string]any{"id": "retained", "cwd": "/workspace/work/session"}},
		map[string]any{"type": "turn_context", "payload": map[string]any{"cwd": "/workspace/work/session", "approval_policy": "never", "sandbox_policy": map[string]any{"type": "danger-full-access"}, "permission_profile": map[string]any{"type": "disabled"}}},
		map[string]any{"type": "event_msg", "payload": map[string]any{"type": "thread_settings_applied", "thread_id": "retained", "thread_settings": map[string]any{"cwd": "/workspace/work/session", "approval_policy": "on-request", "approvals_reviewer": "user", "permission_profile": profile, "runtime_workspace_roots": []any{"/workspace"}}}},
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
	params := map[string]any{"threadId": "retained", "cwd": "/workspace/work/session", "config": map[string]any{"model_reasoning_effort": "xhigh"}}
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

func TestColdPolicyProjectionDoesNotSelectProviderModelOrEffort(t *testing.T) {
	for _, kind := range []string{"workspace-write", "read-only"} {
		t.Run(kind, func(t *testing.T) {
			for _, tc := range []struct {
				name   string
				refuse bool
			}{
				{"settings", false}, {"turn", false}, {"equal settings after turn", false},
				{"newer policy", false}, {"newer roots", false}, {"empty roots", false},
				{"missing roots", true}, {"null roots", true}, {"foreign checkpoint", true},
				{"ownerless checkpoint", true}, {"foreign session", true}, {"missing session", true},
				{"duplicate session", true}, {"cwd without checkpoint", true}, {"missing compiled", true}, {"absent compiled", true},
				{"invalid approval", true}, {"invalid roots", true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					cwd := "/workspace/work/idle"
					meta := map[string]any{"type": "session_meta", "payload": map[string]any{"id": "retained", "cwd": cwd}}
					profile := compiledWorkspacePolicy([]string{"/workspace", "/workspace", "/tmp"})
					if kind == "read-only" {
						profile = map[string]any{"type": "managed", "network": "restricted", "file_system": map[string]any{"type": "restricted", "entries": []any{map[string]any{"path": map[string]any{"type": "special", "value": map[string]any{"kind": "root"}}, "access": "read"}}}}
					}
					settings := map[string]any{"cwd": cwd, "model": "retained-model", "model_provider_id": "pool-b", "reasoning_effort": "low",
						"approval_policy": "never", "approvals_reviewer": "user", "service_tier": "priority", "runtime_workspace_roots": []any{"/workspace"}, "permission_profile": profile}
					event := func(value map[string]any, owner string) any {
						return map[string]any{"type": "event_msg", "payload": map[string]any{"type": "thread_settings_applied", "thread_id": owner, "thread_settings": value}}
					}
					turn := map[string]any{"cwd": cwd, "approval_policy": "never", "permission_profile": profile,
						"sandbox_policy": map[string]any{"type": "workspace-write", "writable_roots": []string{"/workspace", "/tmp"}, "network_access": false, "exclude_tmpdir_env_var": false, "exclude_slash_tmp": false}}
					if kind == "read-only" {
						turn["sandbox_policy"] = map[string]any{"type": "read-only"}
					}
					records := []any{meta, event(settings, "retained")}
					wantRoots := []any{"/workspace"}
					wantWritable := []string{"/tmp", "/workspace"}
					switch tc.name {
					case "newer policy":
						turn["permission_profile"] = compiledWorkspacePolicy([]string{"/workspace", "/tmp", "/workspace/new"})
						wantWritable = []string{"/tmp", "/workspace", "/workspace/new"}
					case "newer roots", "empty roots", "missing roots", "null roots", "invalid roots", "foreign checkpoint", "ownerless checkpoint":
						newer := map[string]any{}
						for key, value := range settings {
							newer[key] = value
						}
						owner := "retained"
						switch tc.name {
						case "newer roots":
							wantRoots = []any{"/workspace/z", "/workspace/a"}
							newer["runtime_workspace_roots"] = wantRoots
						case "empty roots":
							wantRoots = []any{}
							newer["runtime_workspace_roots"] = wantRoots
						case "missing roots":
							delete(newer, "runtime_workspace_roots")
						case "null roots":
							newer["runtime_workspace_roots"] = nil
						case "invalid roots":
							newer["runtime_workspace_roots"] = []any{"relative"}
						case "foreign checkpoint":
							owner = "other"
						case "ownerless checkpoint":
							owner = ""
						}
						records = append(records, event(newer, owner))
					case "foreign session":
						meta["payload"].(map[string]any)["id"] = "other"
					case "missing session":
						records = records[1:]
					case "duplicate session":
						records = append(records, meta)
					case "cwd without checkpoint":
						turn["cwd"] = "/workspace/other"
					case "missing compiled":
						turn["permission_profile"] = nil
					case "absent compiled":
						delete(turn, "permission_profile")
					case "invalid approval":
						turn["approval_policy"] = "automatic"
					}
					if tc.name != "settings" {
						records = append(records, map[string]any{"type": "turn_context", "payload": turn})
					}
					if tc.name == "equal settings after turn" {
						records = append(records, event(settings, "retained"))
					}
					path := filepath.Join(t.TempDir(), "history.jsonl")
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
					params := map[string]any{"threadId": "retained", "cwd": cwd, "excludeTurns": true, "config": map[string]any{}}
					before, _ := json.Marshal(params)
					err = preserveColdNativePolicy(path, params)
					if tc.refuse {
						after, _ := json.Marshal(params)
						if err == nil || string(before) != string(after) {
							t.Fatalf("refusal changed request: %v %#v", err, params)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					// Exact shape forbids all routing/model/effort/auth fields.
					want := map[string]any{"threadId": "retained", "cwd": cwd, "excludeTurns": true, "sandbox": "workspace-write", "runtimeWorkspaceRoots": wantRoots,
						"approvalPolicy": "never", "approvalsReviewer": "user", "serviceTier": "priority", "config": map[string]any{"sandbox_workspace_write": map[string]any{
							"writable_roots": wantWritable, "network_access": false, "exclude_tmpdir_env_var": true, "exclude_slash_tmp": false}}}
					if kind == "read-only" && tc.name != "newer policy" {
						want["sandbox"] = "read-only"
						want["config"] = map[string]any{}
					}
					if !reflect.DeepEqual(params, want) {
						t.Fatalf("cold policy-only request changed: got=%#v want=%#v", params, want)
					}
				})
			}
		})
	}
}

func TestColdOwnerlessReadOnlyKeepsLegacyProjection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.jsonl")
	record := map[string]any{"type": "turn_context", "payload": map[string]any{
		"approval_policy": "never", "sandbox_policy": map[string]any{"type": "read-only"},
		"permission_profile": map[string]any{"type": "managed", "network": "restricted", "file_system": map[string]any{"type": "restricted", "entries": []any{map[string]any{"path": map[string]any{"type": "special", "value": map[string]any{"kind": "root"}}, "access": "read"}}}},
	}}
	data, _ := json.Marshal(record)
	if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	params := map[string]any{"config": map[string]any{}}
	if err := preserveColdNativePolicy(path, params); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(params, map[string]any{"config": map[string]any{}, "sandbox": "read-only", "approvalPolicy": "never"}) {
		t.Fatalf("legacy projection changed: %#v", params)
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
