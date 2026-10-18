package main

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
)

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
