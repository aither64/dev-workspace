package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/agentteams"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
)

func TestProviderResumePreservesPoliciesAndEnvironment(t *testing.T) {
	original := map[string]any{"runtimeWorkspaceRoots": []any{"/workspace"}, "approvalPolicy": "never", "approvalsReviewer": "user", "serviceTier": "fast", "sandbox": map[string]any{"type": "workspaceWrite", "writableRoots": []any{"/workspace"}, "networkAccess": true, "excludeSlashTmp": true}}
	thread := providerThread{ID: "thread-1", Cwd: "/workspace/work/example", Settings: codex.ThreadSettings{Model: "retained", ReasoningEffort: "low"}, Original: original, Environment: map[string]string{"DEV_SESSION_SLUG": "example"}}
	params, err := providerResumeParams(thread, "pool")
	if err != nil {
		t.Fatal(err)
	}
	if params["modelProvider"] != "pool" || params["model"] != "retained" || params["sandbox"] != "workspace-write" || params["approvalPolicy"] != "never" || !reflect.DeepEqual(params["runtimeWorkspaceRoots"], original["runtimeWorkspaceRoots"]) {
		t.Fatalf("retained settings changed: %#v", params)
	}
	config := params["config"].(map[string]any)
	if config["model_reasoning_effort"] != "low" || !reflect.DeepEqual(config["shell_environment_policy"].(map[string]any)["set"], thread.Environment) {
		t.Fatal("effort or environment changed")
	}
	if !reflect.DeepEqual(original["sandbox"], map[string]any{"type": "workspaceWrite", "writableRoots": []any{"/workspace"}, "networkAccess": true, "excludeSlashTmp": true}) {
		t.Fatal("snapshot was mutated")
	}
}

func TestProviderCaptureColdPolicyRefusesUnsupportedProfiles(t *testing.T) {
	for _, named := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "history.jsonl")
		payload := map[string]any{"approval_policy": "never", "approvals_reviewer": "user", "sandbox_policy": map[string]any{"type": "danger-full-access"}}
		if named {
			payload["active_permission_profile"] = map[string]any{"id": "custom"}
		}
		data, _ := json.Marshal(map[string]any{"type": "turn_context", "payload": payload})
		if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
		params := map[string]any{"config": map[string]any{"model_reasoning_effort": "low"}}
		err := preserveColdNativePolicy(path, params)
		if named {
			if err == nil {
				t.Fatal("unsupported profile accepted")
			}
			continue
		}
		if err != nil || params["sandbox"] != "danger-full-access" || params["approvalPolicy"] != "never" {
			t.Fatalf("cold policy changed: %v %#v", err, params)
		}
	}
}

func TestProviderResponseRequiresEffectiveRoutingAndRetainedEffort(t *testing.T) {
	thread := providerThread{ID: "thread-1", Cwd: "/workspace", Settings: codex.ThreadSettings{Model: "retained", ReasoningEffort: "low"}}
	result := map[string]any{"thread": map[string]any{"id": "thread-1", "modelProvider": "pool"}, "cwd": "/workspace", "model": "retained", "reasoningEffort": "low", "modelProvider": "openai"}
	if validateProviderResponse(thread, result, "pool") == nil {
		t.Fatal("historical provider accepted as effective routing")
	}
	result["modelProvider"] = "pool"
	if err := validateProviderResponse(thread, result, "pool"); err != nil {
		t.Fatal(err)
	}
	result["reasoningEffort"] = "xhigh"
	if validateProviderResponse(thread, result, "pool") == nil {
		t.Fatal("changed effort accepted")
	}
}

func TestProviderCaptureValidatesForwardAndCompensationPayloads(t *testing.T) {
	for _, failure := range []string{"missing effort", "missing original provider", "unsupported sandbox", "read-only networking", "changed retained sandbox"} {
		t.Run(failure, func(t *testing.T) {
			thread := providerThread{Settings: codex.ThreadSettings{Model: "retained", ReasoningEffort: "low"}, Original: map[string]any{"modelProvider": "openai", "sandbox": map[string]any{"type": "readOnly", "networkAccess": false}}}
			switch failure {
			case "missing effort":
				thread.Settings.ReasoningEffort = ""
			case "missing original provider":
				delete(thread.Original, "modelProvider")
			case "unsupported sandbox":
				thread.Original["sandbox"] = map[string]any{"type": "externalSandbox"}
			case "read-only networking":
				thread.Original["sandbox"] = map[string]any{"type": "readOnly", "networkAccess": true}
			case "changed retained sandbox":
				thread.Settings.Policy.Sandbox = "danger-full-access"
			}
			if validateProviderResumePayloads(thread, "pool") == nil {
				t.Fatal("unsupported pre-stop snapshot accepted")
			}
		})
	}
}

func TestProviderTransitionPreservesRetainedMemberPolicyAfterPackageUpgrade(t *testing.T) {
	workspace := t.TempDir()
	store, err := teamruntime.NewStore(t.TempDir(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	service := teamruntime.Service{Store: store, Workspace: workspace, Catalog: &agentteams.Catalog{SchemaVersion: 5}}
	for _, access := range []string{"read_only", "workspace_write"} {
		t.Run(access, func(t *testing.T) {
			role := "reviewer"
			if access == "workspace_write" {
				role = "implementer"
			}
			member := teamruntime.Member{Address: role + "0", Thread: "member-thread", Role: role, Behavior: role, Access: access}
			service.Catalog = &agentteams.Catalog{SchemaVersion: 5}
			legacy, err := service.MemberTurnPolicy("example", "root-thread", member)
			if err != nil {
				t.Fatal(err)
			}
			service.Catalog = &agentteams.Catalog{SchemaVersion: 6, ExecutionAccess: "full_access"}
			policy, err := service.MemberTurnPolicy("example", "root-thread", member)
			if err != nil || policy.Sandbox != "danger-full-access" || policy.ApprovalPolicy != "never" || policy.DeveloperInstructions != legacy.DeveloperInstructions {
				t.Fatalf("upgraded member policy: %#v, %v", policy, err)
			}
			for _, native := range []string{legacy.Sandbox, "danger-full-access"} {
				kind := map[string]string{"read-only": "readOnly", "workspace-write": "workspaceWrite", "danger-full-access": "dangerFullAccess"}[native]
				original := map[string]any{"modelProvider": "openai", "approvalPolicy": "on-request", "sandbox": map[string]any{"type": kind, "networkAccess": false}}
				thread := providerThread{ID: member.Thread, Cwd: workspace, MemberAddress: member.Address, Settings: codex.ThreadSettings{Model: "retained", ReasoningEffort: "low", Policy: policy}, Original: original}
				if err := validateProviderResumePayloads(thread, "pool"); err != nil {
					t.Fatal(err)
				}
				for _, provider := range []string{"pool", "openai"} {
					params, err := providerResumeParams(thread, provider)
					if err != nil || params["sandbox"] != native || params["approvalPolicy"] != original["approvalPolicy"] || params["modelProvider"] != provider {
						t.Fatalf("provider operation changed native execution policy: %#v, %v", params, err)
					}
				}
			}
		})
	}
}

func TestProviderBuiltInProfilePreservesEffectiveSandbox(t *testing.T) {
	for _, profile := range []any{nil, map[string]any{"id": ":workspace", "extends": nil}} {
		if !supportedProviderPermissionProfile(profile) {
			t.Fatalf("built-in refused: %#v", profile)
		}
	}
	for _, profile := range []any{"custom", map[string]any{"id": "custom"}, map[string]any{"id": ":unknown"}, map[string]any{"id": ":workspace", "extends": "custom"}} {
		if supportedProviderPermissionProfile(profile) {
			t.Fatalf("named profile accepted: %#v", profile)
		}
	}
	path := filepath.Join(t.TempDir(), "history.jsonl")
	record := `{"type":"turn_context","payload":{"active_permission_profile":{"id":":workspace"},"permission_profile":{"type":"managed"},"approval_policy":"never","sandbox_policy":{"type":"workspace-write","writable_roots":["/retained"],"network_access":true}}}`
	if err := os.WriteFile(path, []byte(record+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	params := map[string]any{"config": map[string]any{}}
	if err := preserveColdNativePolicy(path, params); err != nil {
		t.Fatal(err)
	}
	if params["permissions"] != ":workspace" || params["sandbox"] != nil || !reflect.DeepEqual(params["config"].(map[string]any)["sandbox_workspace_write"].(map[string]any)["writable_roots"], []any{"/retained"}) {
		t.Fatalf("built-in sandbox changed: %#v", params)
	}
}

func TestProviderColdSettingsUseNativeChronology(t *testing.T) {
	turn := map[string]any{"approval_policy": "never", "approvals_reviewer": "user", "sandbox_policy": map[string]any{"type": "danger-full-access"}, "permission_profile": map[string]any{"type": "disabled"}}
	readOnly := map[string]any{"type": "managed", "network": "restricted", "file_system": map[string]any{"type": "restricted", "entries": []any{map[string]any{"path": map[string]any{"type": "special", "value": map[string]any{"kind": "root"}}, "access": "read"}}}}
	updated := map[string]any{"approval_policy": "on-request", "approvals_reviewer": "auto_review", "permission_profile": readOnly, "active_permission_profile": map[string]any{"id": ":read-only"}, "service_tier": "flex"}
	turnRecord := map[string]any{"type": "turn_context", "payload": turn}
	settingsRecord := map[string]any{"type": "event_msg", "payload": map[string]any{"type": "thread_settings_applied", "thread_settings": updated}}
	for _, tc := range []struct {
		name                                           string
		records                                        []any
		approval, sandbox, permissions, tier, reviewer any
	}{
		{"idle update after turn", []any{turnRecord, settingsRecord}, "on-request", nil, ":read-only", "flex", "auto_review"},
		{"snapshot without turn", []any{settingsRecord}, "on-request", nil, ":read-only", "flex", "auto_review"},
		{"later turn wins policy", []any{settingsRecord, turnRecord}, "never", "danger-full-access", nil, "flex", "user"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "history.jsonl")
			var data []byte
			for _, record := range tc.records {
				line, _ := json.Marshal(record)
				data = append(append(data, line...), '\n')
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			params := map[string]any{"config": map[string]any{}}
			if err := preserveColdNativePolicy(path, params); err != nil {
				t.Fatal(err)
			}
			if params["approvalPolicy"] != tc.approval || params["sandbox"] != tc.sandbox || params["permissions"] != tc.permissions || params["serviceTier"] != tc.tier || params["approvalsReviewer"] != tc.reviewer {
				t.Fatalf("lost latest native settings: %#v", params)
			}
		})
	}
	updated["permission_profile"] = map[string]any{"type": "external", "network": "enabled"}
	path := filepath.Join(t.TempDir(), "unsupported.jsonl")
	data, _ := json.Marshal(settingsRecord)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	params := map[string]any{"config": map[string]any{}}
	if preserveColdNativePolicy(path, params) == nil || len(params) != 1 {
		t.Fatalf("unsupported snapshot modified a resume request: %#v", params)
	}
	updated["permission_profile"] = map[string]any{"type": "disabled"}
	updated["active_permission_profile"] = map[string]any{"id": ":danger-full-access"}
	updated["service_tier"] = "priority"
	data, _ = json.Marshal(settingsRecord)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := preserveColdNativePolicy(path, params); err != nil || params["serviceTier"] != "priority" {
		t.Fatalf("canonical Fast tier lost: %v %#v", err, params)
	}
}
