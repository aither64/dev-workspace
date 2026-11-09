package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
)

// Scoped schema 2 retains authored compiled permissions independently of the
// native response's accessibility-based legacy sandbox projection.
type scopedPermissionAuthority struct {
	ThreadID              string         `json:"threadId"`
	Cwd                   string         `json:"cwd"`
	RuntimeWorkspaceRoots []string       `json:"runtimeWorkspaceRoots"`
	Profile               map[string]any `json:"profile"`
}

func providerRuntimeRoots(value any) ([]string, error) {
	values, ok := value.([]any)
	if !ok || len(values) > 4096 {
		return nil, errors.New("native runtime workspace roots are invalid")
	}
	roots := make([]string, len(values))
	for index, value := range values {
		root, ok := value.(string)
		if !ok || !canonicalAbsolutePath(root) {
			return nil, errors.New("native runtime workspace root is invalid")
		}
		roots[index] = root
	}
	return roots, nil
}

func needsScopedPermissionAuthority(thread providerThread) (bool, error) {
	if !supportedProviderPermissionProfile(thread.Original["activePermissionProfile"]) {
		return false, errors.New("unsupported native permission profile")
	}
	if thread.Original["activePermissionProfile"] != nil {
		return false, nil
	}
	sandbox, ok := thread.Original["sandbox"].(map[string]any)
	if !ok {
		return false, errors.New("native snapshot has no sandbox policy")
	}
	return sandbox["type"] == "workspaceWrite", nil
}

func scopedPermissionProjection(thread providerThread) (map[string]any, error) {
	authority := thread.ScopedPermissions
	required, err := needsScopedPermissionAuthority(thread)
	if err != nil || !required || authority == nil || authority.ThreadID != thread.ID || authority.Cwd != thread.Cwd || !canonicalAbsolutePath(thread.Cwd) {
		return nil, errors.New("scoped permission authority is absent, foreign or inapplicable")
	}

	data, err := json.Marshal(authority)
	if err != nil || len(data) > 16*1024*1024 {
		return nil, errors.New("scoped permission authority exceeds snapshot bounds")
	}
	roots, err := providerRuntimeRoots(thread.Original["runtimeWorkspaceRoots"])
	if err != nil || !slices.Equal(roots, authority.RuntimeWorkspaceRoots) {
		return nil, errors.New("scoped permission runtime roots changed")
	}
	projection, err := coldWorkspaceSandbox(authority.Profile, thread.Cwd)
	if err != nil {
		return nil, err
	}
	sandbox := thread.Original["sandbox"].(map[string]any)
	if sandbox["networkAccess"] != projection["network_access"] {
		return nil, errors.New("scoped compiled network policy differs from native settings")
	}

	return projection, nil
}

func readScopedPermissionAuthority(path string, thread providerThread) (*scopedPermissionAuthority, error) {
	history, err := readNativePolicyHistory(path, true)
	if err != nil {
		return nil, err
	}
	settings := history.Retained
	effort := settings["reasoning_effort"]
	if effort == nil {
		effort = settings["effort"]
	}
	provider := settings["model_provider_id"]
	if provider == nil {
		provider = history.Settings["model_provider_id"]
	}
	if history.ThreadID != thread.ID || history.Cwd != thread.Cwd || settings == nil || settings["cwd"] != thread.Cwd || settings["model"] != thread.Settings.Model || effort != thread.Settings.ReasoningEffort || provider != thread.Original["modelProvider"] || settings["approval_policy"] != thread.Original["approvalPolicy"] || !reflect.DeepEqual(settings["active_permission_profile"], thread.Original["activePermissionProfile"]) {
		return nil, errors.New("scoped native permission history is absent, stale or foreign")
	}
	rootValues := settings["runtime_workspace_roots"]
	if rootValues == nil {
		rootValues = history.Settings["runtime_workspace_roots"]
	}
	roots, err := providerRuntimeRoots(rootValues)
	if err != nil {
		return nil, err
	}
	profile, ok := settings["permission_profile"].(map[string]any)
	if !ok {
		return nil, errors.New("scoped native compiled permissions are absent")
	}
	authority := &scopedPermissionAuthority{thread.ID, thread.Cwd, roots, profile}
	thread.ScopedPermissions = authority
	if _, err := scopedPermissionProjection(thread); err != nil {
		return nil, err
	}
	return authority, nil
}

func verifyScopedPermissionAuthority(ctx context.Context, client *workspacecodex.Client, thread providerThread, effective map[string]any) error {
	var response struct {
		Thread struct {
			ID, Cwd, ModelProvider string
			Path                   *string
		} `json:"thread"`
	}
	if err := client.Request(ctx, "thread/read", map[string]any{"threadId": thread.ID, "excludeTurns": true}, &response); err != nil {
		return err
	}
	if response.Thread.ID != thread.ID || response.Thread.Cwd != thread.Cwd || response.Thread.Path == nil || response.Thread.ModelProvider != effective["modelProvider"] {
		return errors.New("scoped permission verification has a foreign history")
	}
	after := thread
	after.Original = effective
	authority, err := readScopedPermissionAuthority(*response.Thread.Path, after)
	if err != nil {
		return err
	}
	after.ScopedPermissions = authority
	want, err := scopedPermissionProjection(thread)
	if err != nil {
		return err
	}
	got, err := scopedPermissionProjection(after)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(want, got) {
		return errors.New("scoped provider changed retained compiled permissions")
	}
	return nil
}

// Cold settings snapshots contain compiled permissions rather than a legacy
// sandbox. Recognize the selected native version's workspace-write projection,
// including its automatically protected metadata. Never discard an extra rule
// merely because the legacy sandbox API cannot describe it.
func coldWorkspaceSandbox(profile map[string]any, cwd string) (map[string]any, error) {
	unsupported := errors.New("updated cold filesystem permissions cannot be preserved")
	if !canonicalAbsolutePath(cwd) || profile["type"] != "managed" || !onlyPolicyKeys(profile, "type", "file_system", "network") {
		return nil, unsupported
	}
	network, ok := profile["network"].(string)
	if !ok || (network != "restricted" && network != "enabled") {
		return nil, unsupported
	}
	fs, ok := profile["file_system"].(map[string]any)
	if !ok || fs["type"] != "restricted" || !onlyPolicyKeys(fs, "type", "entries") {
		return nil, unsupported
	}
	entries, ok := fs["entries"].([]any)
	if !ok || len(entries) > 4096 {
		return nil, unsupported
	}
	actual := map[string]bool{}
	roots := []string{}
	slashTmp, tmpdir, writableCwd := false, false, false
	for _, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok || !onlyPolicyKeys(entry, "path", "access", "missing_path_behavior") {
			return nil, unsupported
		}
		path, ok := entry["path"].(map[string]any)
		if !ok {
			return nil, unsupported
		}
		encoded, err := json.Marshal(entry)
		if err != nil {
			return nil, unsupported
		}
		actual[string(encoded)] = true
		if entry["access"] != "write" {
			continue
		}
		if entry["missing_path_behavior"] != nil {
			return nil, unsupported
		}
		switch path["type"] {
		case "path":
			root, ok := path["path"].(string)
			if !ok || !canonicalAbsolutePath(root) || root == "/" || !onlyPolicyKeys(path, "type", "path") {
				return nil, unsupported
			}
			if !slices.Contains(roots, root) {
				roots = append(roots, root)
			}
			writableCwd = writableCwd || cwd == root || strings.HasPrefix(cwd, root+string(filepath.Separator))
		case "special":
			value, ok := path["value"].(map[string]any)
			if !ok || !onlyPolicyKeys(path, "type", "value") || !onlyPolicyKeys(value, "kind") {
				return nil, unsupported
			}
			switch value["kind"] {
			case "slash_tmp":
				slashTmp = true
			case "tmpdir":
				tmpdir = true
			default:
				return nil, unsupported
			}
		default:
			return nil, unsupported
		}
	}
	if !writableCwd {
		return nil, unsupported
	}
	expected := map[string]bool{}
	add := func(path map[string]any, access string, skip bool) {
		entry := map[string]any{"path": path, "access": access}
		if skip {
			entry["missing_path_behavior"] = "skip"
		}
		encoded, _ := json.Marshal(entry)
		expected[string(encoded)] = true
	}
	special := func(kind string) map[string]any {
		return map[string]any{"type": "special", "value": map[string]any{"kind": kind}}
	}
	add(special("root"), "read", false)
	if slashTmp {
		add(special("slash_tmp"), "write", false)
	}
	if tmpdir {
		add(special("tmpdir"), "write", false)
	}
	slices.Sort(roots)
	for _, root := range roots {
		add(map[string]any{"type": "path", "path": root}, "write", false)
		for _, name := range []string{".git", ".agents", ".codex", ".aws"} {
			add(map[string]any{"type": "path", "path": filepath.Join(root, name)}, "read", true)
		}
	}
	if len(actual) != len(expected) {
		return nil, unsupported
	}
	for entry := range actual {
		if !expected[entry] {
			return nil, unsupported
		}
	}
	return map[string]any{"type": "workspace-write", "writable_roots": roots, "network_access": network == "enabled", "exclude_tmpdir_env_var": !tmpdir, "exclude_slash_tmp": !slashTmp}, nil
}

func onlyPolicyKeys(value map[string]any, allowed ...string) bool {
	for key := range value {
		if !slices.Contains(allowed, key) {
			return false
		}
	}
	return true
}
