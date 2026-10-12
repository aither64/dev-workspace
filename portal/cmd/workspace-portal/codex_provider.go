package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/codexproviders"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/teamruntime"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
)

// This bounded helper is called by workspace-host under its transition lock.
// It never accepts an identity from the browser or writes provider state.
type providerThread struct {
	ID            string               `json:"id"`
	Slug          string               `json:"slug"`
	Cwd           string               `json:"cwd"`
	Settings      codex.ThreadSettings `json:"settings"`
	Original      map[string]any       `json:"original,omitempty"`
	Environment   map[string]string    `json:"environment,omitempty"`
	MemberAddress string               `json:"memberAddress,omitempty"`
}

type providerInventory struct {
	Schema    int              `json:"schema"`
	Workspace string           `json:"workspace"`
	Threads   []providerThread `json:"threads"`
	Loaded    []string         `json:"loaded"`
	Blockers  []string         `json:"blockers"`
}

func codexProviderCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("codex-provider requires a helper operation")
	}
	command := args[0]
	flags := flag.NewFlagSet("codex-provider", flag.ContinueOnError)
	workspace := flags.String("workspace", "", "registered workspace root")
	socket := flags.String("socket", "", "trusted native socket")
	stateRoot := flags.String("state-root", "", "private user state root")
	profile := flags.String("host-profile", "", "stable host profile")
	catalogPath := flags.String("catalog", codexproviders.CatalogPath(), "operator catalog path")
	snapshot := flags.String("snapshot", "", "private inventory file")
	target := flags.String("target", "", "configured provider identity")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected provider helper arguments")
	}
	if command == "catalog" {
		catalog, err := codexproviders.ReadCatalog(*catalogPath)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"catalog": catalog, "digest": catalog.Digest})
	}
	if !canonicalAbsolutePath(*workspace) || !canonicalAbsolutePath(*socket) || !canonicalAbsolutePath(*stateRoot) {
		return errors.New("provider helper requires canonical identities")
	}
	deadline := 3 * time.Minute
	if command == "health" {
		deadline = time.Second
	}
	ctx, stop := context.WithTimeout(context.Background(), deadline)
	defer stop()
	// Read-only inventory must not implicitly activate unloaded histories.
	identity := sha256.Sum256([]byte(*socket))
	client := &workspacecodex.Client{Client: codex.NewWithOptions(*socket, codex.ClientOptions{
		PreserveThreadInstructionsOnResume: true,
		AllowImplicitResume:                func(string) bool { return false },
		SubmissionLedgerPath:               filepath.Join(*stateRoot, "submissions", hex.EncodeToString(identity[:])+".json"),
		LegacySubmissionLedgerPath:         *socket + ".submission-attempts-v3.json",
	}), CodexHome: os.Getenv("DEV_WORKSPACE_CODEX_HOME")}
	// No watches or automatic question answers are installed. Omit the ordinary
	// routing resolver while inspecting or recovering a reserved operation.
	defer client.Close()
	if command == "health" {
		// Connect even when no retained threads were loaded before the restart.
		var loaded struct {
			Data *[]string `json:"data"`
		}
		if err := client.Request(ctx, "thread/loaded/list", map[string]any{}, &loaded); err != nil {
			return err
		}
		if loaded.Data == nil {
			return errors.New("native loaded thread inventory is absent")
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"schema": 1, "verified": true})
	}
	if command == "inventory" {
		inventory, err := buildProviderInventory(ctx, client, *workspace, *stateRoot, *socket, *profile)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(inventory)
	}
	info, err := os.Lstat(*snapshot)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 16*1024*1024 {
		return errors.New("provider snapshot is not a private bounded file")
	}
	data, err := os.ReadFile(*snapshot)
	if err != nil {
		return err
	}
	var inventory providerInventory
	if json.Unmarshal(data, &inventory) != nil || inventory.Schema != 1 || inventory.Workspace != *workspace || len(inventory.Threads) > 4096 {
		return errors.New("invalid provider snapshot")
	}
	if command == "preflight" {
		catalog, err := codexproviders.ReadCatalog(*catalogPath)
		if err != nil {
			return err
		}
		provider, err := catalog.Find(*target)
		if err != nil {
			return err
		}
		models := []string{}
		for _, thread := range inventory.Threads {
			models = append(models, thread.Settings.Model)
		}
		if provider.Kind == "codex-lb" {
			if err := codexproviders.ProbeModels(ctx, provider, models); err != nil {
				return err
			}
		} else {
			limits, err := client.ReadAccountRateLimits(ctx)
			if err != nil {
				return err
			}
			if limits.AccountID == nil || *limits.AccountID == "" {
				return errors.New("subscription authentication could not be verified")
			}
			available, err := client.ListModels(ctx)
			if err != nil {
				return err
			}
			for _, thread := range inventory.Threads {
				if _, err := workspacecodex.ResolveNewThreadSettings(available, thread.Settings); err != nil {
					return err
				}
			}
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"schema": 1, "verified": true})
	}
	if command == "capture" {
		for index := range inventory.Threads {
			thread := &inventory.Threads[index]
			var original map[string]any
			params := map[string]any{"threadId": thread.ID, "excludeTurns": true, "model": thread.Settings.Model, "config": map[string]any{"model_reasoning_effort": thread.Settings.ReasoningEffort}}
			metadata, err := client.ReadThreadMetadata(ctx, thread.ID, false)
			if err != nil {
				return err
			}
			if metadata.Status["type"] == "notLoaded" {
				if metadata.Path == nil {
					return errors.New("cold thread has no native settings history")
				}
				if err := preserveColdNativePolicy(*metadata.Path, params); err != nil {
					return err
				}
			}
			if err := client.Request(ctx, "thread/resume", params, &original); err != nil {
				return err
			}
			if err := validateProviderResponse(*thread, original, ""); err != nil {
				return err
			}
			if tier, retained := params["serviceTier"]; retained && !providerTierMatches(tier, original["serviceTier"]) {
				return errors.New("native cold resume changed the retained service tier")
			}
			if !supportedProviderPermissionProfile(original["activePermissionProfile"]) {
				return errors.New("named native permission profiles require explicit provider transition support")
			}
			thread.Original = original
			if err := validateProviderResumePayloads(*thread, *target); err != nil {
				return err
			}
		}
		return json.NewEncoder(os.Stdout).Encode(inventory)
	}
	if command != "resume" && command != "verify" {
		return errors.New("unknown provider helper operation")
	}
	if *target != "" {
		catalog, err := codexproviders.ReadCatalog(*catalogPath)
		if err != nil {
			return err
		}
		if _, err = catalog.Find(*target); err != nil {
			return err
		}
	}
	loaded := map[string]bool{}
	for _, id := range inventory.Loaded {
		loaded[id] = true
	}
	for _, thread := range inventory.Threads {
		if command == "verify" && !loaded[thread.ID] {
			continue
		}
		provider := *target
		if provider == "" {
			provider, _ = thread.Original["modelProvider"].(string)
		}
		params, err := providerResumeParams(thread, provider)
		if err != nil {
			return err
		}
		var result map[string]any
		if err := client.Request(ctx, "thread/resume", params, &result); err != nil {
			return err
		}
		if err := validateProviderResponse(thread, result, provider); err != nil {
			return err
		}
		// Restore settings not accepted as lifecycle parameters. Native update
		// retains the historical conversation and does not change routing.
		{
			update := map[string]any{"threadId": thread.ID}
			for _, key := range []string{"disabledPluginIds", "collaborationMode", "multiAgentMode"} {
				// Native cold resume already restores persisted settings. Sending
				// an unchanged collaboration mode would materialize a preset's
				// default instructions when its retained instructions were null.
				if value, ok := thread.Original[key]; ok && !reflect.DeepEqual(value, result[key]) {
					update[key] = value
				}
			}
			if len(update) > 1 {
				var restored map[string]any
				if err := client.Request(ctx, "thread/settings/update", update, &restored); err != nil {
					return err
				}
				// SettingsUpdateResponse is empty in the selected protocol. Read
				// the effective loaded settings through a second native resume.
				if err := client.Request(ctx, "thread/resume", params, &restored); err != nil {
					return err
				}
				if err := validateProviderResponse(thread, restored, provider); err != nil {
					return err
				}
				for key, value := range update {
					if key != "threadId" && !reflect.DeepEqual(value, restored[key]) {
						return fmt.Errorf("native provider settings did not retain %s", key)
					}
				}
			}
		}
		for _, key := range []string{"runtimeWorkspaceRoots", "approvalPolicy", "approvalsReviewer", "serviceTier", "sandbox", "activePermissionProfile"} {
			if key == "serviceTier" && providerTierMatches(thread.Original[key], result[key]) {
				continue
			}
			if !reflect.DeepEqual(thread.Original[key], result[key]) {
				return fmt.Errorf("provider transition changed retained %s for %s", key, thread.ID)
			}
		}
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"schema": 1, "verified": true})
}

func buildProviderInventory(ctx context.Context, client *workspacecodex.Client, workspace, stateRoot, socket, profile string) (providerInventory, error) {
	result := providerInventory{Schema: 1, Workspace: workspace, Threads: []providerThread{}, Loaded: []string{}, Blockers: []string{}}
	summaries, err := session.List(workspace)
	if err != nil {
		return result, err
	}
	store, err := teamruntime.NewStore(stateRoot, workspace)
	if err != nil {
		return result, err
	}
	service := teamruntime.Service{Store: store, Client: client, Workspace: workspace, HostProfile: profile}
	seen := map[string]bool{}
	add := func(thread providerThread) error {
		if thread.ID == "" || seen[thread.ID] {
			return errors.New("provider inventory contains an ambiguous thread identity")
		}
		seen[thread.ID] = true
		metadata, err := client.ReadThreadMetadata(ctx, thread.ID, false)
		if err != nil {
			return err
		}
		if metadata.ID != thread.ID || metadata.Cwd != thread.Cwd || metadata.Source != "vscode" || metadata.Path == nil {
			return errors.New("provider thread ownership or materialization is unproved")
		}
		if err := client.RequireThreadIdle(ctx, thread.ID, thread.Cwd); err != nil {
			result.Blockers = append(result.Blockers, thread.Slug+": thread has active or unresolved work")
		}
		goal, err := client.HasActiveGoal(ctx, thread.ID)
		if err != nil {
			return err
		}
		if goal {
			result.Blockers = append(result.Blockers, thread.Slug+": thread has an active goal")
		}
		if thread.Settings.Model == "" {
			settings, err := client.ReadThreadSettings(ctx, thread.ID)
			if err != nil {
				return err
			}
			thread.Settings.Model = settings.Model
			thread.Settings.ReasoningEffort = settings.ReasoningEffort
		}
		result.Threads = append(result.Threads, thread)
		return nil
	}
	for _, summary := range summaries {
		if summary.Archived || summary.Codex.ThreadID == "" {
			continue
		}
		if summary.Codex.SocketPath != socket || !session.ValidSlug(summary.Slug) {
			return result, errors.New("session provider inventory has a foreign socket or slug")
		}
		if err := session.RequireRetainedReady(&summary, socket); err != nil {
			return result, err
		}
		cwd := filepath.Join(workspace, "work", summary.Slug)
		roster, err := store.Load(summary.Slug, summary.Codex.ThreadID)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return result, err
		}
		instructions := ""
		if roster != nil {
			instructions = roster.LeadInstructions
		}
		if err := add(providerThread{ID: summary.Codex.ThreadID, Slug: summary.Slug, Cwd: cwd, Settings: codex.ThreadSettings{Policy: workspacecodex.LeadThreadPolicy(summary.Slug, workspace, instructions)}}); err != nil {
			return result, err
		}
		if roster == nil {
			continue
		}
		for _, member := range roster.Members {
			if member.State == "removed" || member.State == "archived" {
				continue
			}
			if member.State != "ready" || member.RetireIntent != "" || member.Thread == "" {
				return result, errors.New("team member transition is unfinished")
			}
			policy, err := service.MemberTurnPolicy(summary.Slug, summary.Codex.ThreadID, member)
			if err != nil {
				return result, err
			}
			if err := add(providerThread{ID: member.Thread, Slug: summary.Slug, Cwd: cwd, MemberAddress: member.Address, Settings: codex.ThreadSettings{Model: member.Model, ReasoningEffort: member.Effort, Policy: policy}}); err != nil {
				return result, err
			}
		}
	}
	// Use the native loaded list without an application source filter, since
	// an unrelated native writer must block stopping the shared daemon.
	var loaded struct {
		Data *[]string `json:"data"`
	}
	if err := client.Request(ctx, "thread/loaded/list", map[string]any{}, &loaded); err != nil {
		return result, err
	}
	if loaded.Data == nil {
		return result, errors.New("native loaded thread inventory is absent")
	}
	for _, id := range *loaded.Data {
		result.Loaded = append(result.Loaded, id)
		if !seen[id] {
			result.Blockers = append(result.Blockers, "an unmanaged native thread is loaded")
		}
	}
	return result, nil
}

func validateProviderResponse(thread providerThread, result map[string]any, provider string) error {
	metadata, ok := result["thread"].(map[string]any)
	if !ok || metadata["id"] != thread.ID || result["cwd"] != thread.Cwd || result["model"] == nil || result["modelProvider"] == nil {
		return errors.New("native provider response has an unexpected identity")
	}
	if provider != "" && (result["modelProvider"] != provider || result["model"] != thread.Settings.Model || result["reasoningEffort"] != thread.Settings.ReasoningEffort) {
		return errors.New("effective provider/model/effort does not match the retained settings")
	}
	return nil
}

func providerResumeParams(thread providerThread, provider string) (map[string]any, error) {
	old := thread.Original
	if old == nil || provider == "" || thread.Settings.Model == "" || thread.Settings.ReasoningEffort == "" {
		return nil, errors.New("provider snapshot is incomplete")
	}
	config := map[string]any{"model_reasoning_effort": thread.Settings.ReasoningEffort,
		"shell_environment_policy": map[string]any{"set": thread.Environment}}
	params := map[string]any{"threadId": thread.ID, "excludeTurns": true, "modelProvider": provider, "model": thread.Settings.Model, "cwd": thread.Cwd, "runtimeWorkspaceRoots": old["runtimeWorkspaceRoots"], "approvalPolicy": old["approvalPolicy"], "approvalsReviewer": old["approvalsReviewer"], "config": config}
	// Native null explicitly clears a tier inherited from current config.
	params["serviceTier"] = old["serviceTier"]
	sandbox, ok := old["sandbox"].(map[string]any)
	if !ok {
		return nil, errors.New("native snapshot has no sandbox policy")
	}
	switch sandbox["type"] {
	case "dangerFullAccess":
		params["sandbox"] = "danger-full-access"
	case "readOnly":
		if sandbox["networkAccess"] == true {
			return nil, errors.New("custom read-only networking cannot be preserved")
		}
		if access, ok := sandbox["access"].(map[string]any); ok && access["type"] != "fullAccess" {
			return nil, errors.New("custom read-only filesystem access cannot be preserved")
		}
		params["sandbox"] = "read-only"
	case "workspaceWrite":
		params["sandbox"] = "workspace-write"
		policy := map[string]any{}
		for native, configKey := range map[string]string{"writableRoots": "writable_roots", "networkAccess": "network_access", "excludeTmpdirEnvVar": "exclude_tmpdir_env_var", "excludeSlashTmp": "exclude_slash_tmp"} {
			if value, ok := sandbox[native]; ok {
				policy[configKey] = value
			}
		}
		config["sandbox_workspace_write"] = policy
	default:
		return nil, errors.New("unsupported native sandbox policy")
	}
	policy := thread.Settings.Policy
	if policy.Sandbox != "" {
		if params["sandbox"] != policy.Sandbox {
			return nil, fmt.Errorf("retained team sandbox %s differs from effective native sandbox %v for %s", policy.Sandbox, params["sandbox"], thread.ID)
		}
	}
	if profile, ok := old["activePermissionProfile"].(map[string]any); ok {
		if !supportedProviderPermissionProfile(profile) {
			return nil, errors.New("unsupported native permission profile")
		}
		params["permissions"] = profile["id"]
		delete(params, "sandbox")
	}
	if policy.DeveloperInstructions != "" {
		params["developerInstructions"] = sessionLifecycleDeveloperInstructions + "\n\n" + policy.DeveloperInstructions
	}
	if server := policy.MCPServer; server != nil {
		config["mcp_servers"] = map[string]any{server.Name: map[string]any{"command": server.Command, "args": server.Args, "required": true, "enabled_tools": []string{server.Tool}, "tools": map[string]any{server.Tool: map[string]any{"approval_mode": "approve"}}}}
	}
	return params, nil
}

// Capture proves both directions while the original shared writer is alive.
func validateProviderResumePayloads(thread providerThread, target string) error {
	original, _ := thread.Original["modelProvider"].(string)
	for _, provider := range []string{target, original} {
		if _, err := providerResumeParams(thread, provider); err != nil {
			return err
		}
	}
	return nil
}

func supportedProviderPermissionProfile(value any) bool {
	if value == nil {
		return true
	}
	// These are the selected native version's three built-in identities.
	if profile, ok := value.(map[string]any); ok {
		id, _ := profile["id"].(string)
		return (id == ":workspace" || id == ":read-only" || id == ":danger-full-access") && profile["extends"] == nil
	}
	return false
}

// Follow native 0.160.0's chronology: idle settings snapshots can supersede
// turn contexts. Tier is snapshot-owned and is absent from turn contexts.
func preserveColdNativePolicy(path string, params map[string]any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 32*1024*1024)
	var retained, lastTurn map[string]any
	var reviewer, tier any
	var snapshotSeen, latestSnapshot bool
	for scanner.Scan() {
		line := scanner.Bytes()
		if !bytes.Contains(line, []byte("turn_context")) && !bytes.Contains(line, []byte("thread_settings_applied")) {
			continue
		}
		var record struct {
			Type    string         `json:"type"`
			Payload map[string]any `json:"payload"`
		}
		if json.Unmarshal(line, &record) != nil {
			return errors.New("native settings history is malformed")
		}
		if record.Type == "turn_context" {
			retained = record.Payload
			lastTurn = record.Payload
			latestSnapshot = false
		} else if record.Type == "event_msg" && record.Payload["type"] == "thread_settings_applied" {
			settings, ok := record.Payload["thread_settings"].(map[string]any)
			if !ok {
				return errors.New("native settings snapshot is malformed")
			}
			retained = settings
			tier = settings["service_tier"]
			snapshotSeen, latestSnapshot = true, true
		} else {
			continue
		}
		if value := retained["approvals_reviewer"]; value != nil {
			reviewer = value
		}
	}
	if scanner.Err() != nil {
		return errors.New("native settings history exceeds supported record bounds")
	}
	if retained == nil {
		return nil
	}
	// permission_profile holds compiled permission data; the identity is a
	// separate sidecar in the selected native TurnContextItem format.
	profile := retained["active_permission_profile"]
	if !supportedProviderPermissionProfile(profile) {
		return errors.New("named cold permission profiles require explicit transition support")
	}
	sandbox, ok := retained["sandbox_policy"].(map[string]any)
	if latestSnapshot {
		compiled, valid := retained["permission_profile"].(map[string]any)
		if !valid {
			return errors.New("cold settings snapshot has no permission policy")
		}
		switch {
		case compiled["type"] == "disabled":
			sandbox = map[string]any{"type": "danger-full-access"}
		case lastTurn != nil && reflect.DeepEqual(compiled, lastTurn["permission_profile"]):
			sandbox, ok = lastTurn["sandbox_policy"].(map[string]any)
			if !ok {
				return errors.New("cold native sandbox policy is missing")
			}
		case isColdReadOnlyProfile(compiled):
			sandbox = map[string]any{"type": "read-only"}
		default:
			return errors.New("updated cold filesystem permissions cannot be preserved")
		}
		ok = true
	}
	if !ok {
		return errors.New("cold native sandbox policy is missing")
	}
	kind, ok := sandbox["type"].(string)
	if !ok {
		return errors.New("cold native sandbox kind is missing")
	}
	// Validate all forms before modifying the caller's request.
	approval, ok := retained["approval_policy"].(string)
	if !ok || (approval != "never" && approval != "on-request" && approval != "on-failure" && approval != "untrusted") {
		return errors.New("custom cold approval policies are unsupported")
	}
	if reviewer != nil && reviewer != "user" && reviewer != "auto_review" && reviewer != "guardian_subagent" {
		return errors.New("cold approval reviewer is unsupported")
	}
	if tier != nil && tier != "priority" && tier != "flex" && tier != "default" {
		return errors.New("cold service tier is unsupported")
	}
	config := map[string]any{}
	policyParams := map[string]any{}
	switch kind {
	case "danger-full-access":
		policyParams["sandbox"] = kind
	case "read-only":
		if sandbox["network_access"] == true {
			return errors.New("custom cold read-only networking is unsupported")
		}
		if access, ok := sandbox["access"].(map[string]any); ok && access["type"] != "full-access" {
			return errors.New("custom cold read-only filesystem access is unsupported")
		}
		policyParams["sandbox"] = kind
	case "workspace-write":
		policyParams["sandbox"] = kind
		policy := map[string]any{}
		for _, key := range []string{"writable_roots", "network_access", "exclude_tmpdir_env_var", "exclude_slash_tmp"} {
			if value, ok := sandbox[key]; ok {
				policy[key] = value
			}
		}
		config["sandbox_workspace_write"] = policy
	default:
		return errors.New("cold native sandbox policy cannot be preserved")
	}
	if profile, ok := profile.(map[string]any); ok {
		policyParams["permissions"] = profile["id"]
		delete(policyParams, "sandbox")
	}
	for key, value := range policyParams {
		params[key] = value
	}
	params["approvalPolicy"] = approval
	if reviewer != nil {
		params["approvalsReviewer"] = reviewer
	}
	if snapshotSeen {
		params["serviceTier"] = tier
	}
	for key, value := range config {
		params["config"].(map[string]any)[key] = value
	}
	return nil
}

func isColdReadOnlyProfile(profile map[string]any) bool {
	// This exact compiled built-in is representable through the legacy API.
	return reflect.DeepEqual(profile, map[string]any{
		"type": "managed", "network": "restricted",
		"file_system": map[string]any{"type": "restricted", "entries": []any{
			map[string]any{"path": map[string]any{"type": "special", "value": map[string]any{"kind": "root"}}, "access": "read"},
		}},
	})
}

func providerTierMatches(wanted, actual any) bool {
	// Native's explicit null means the default tier; both spellings have the
	// same scheduling semantics. Nondefault tiers must match exactly.
	return reflect.DeepEqual(wanted, actual) || ((wanted == nil || wanted == "default") && (actual == nil || actual == "default"))
}
