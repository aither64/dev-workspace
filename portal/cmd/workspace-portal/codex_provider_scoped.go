package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/workspacecodex"
	"golang.org/x/sys/unix"
)

type scopedThreadIntent struct {
	Active       bool   `json:"active"`
	GoalActive   bool   `json:"goalActive"`
	GoalOwned    bool   `json:"goalOwned"`
	CloseIntent  bool   `json:"closeIntent"`
	Closed       bool   `json:"closed"`
	Verified     bool   `json:"verified"`
	Continued    bool   `json:"continued"`
	Continuation string `json:"continuation"`
	PauseTurnID  string `json:"pauseTurnId,omitempty"`
}

type scopedProviderSnapshot struct {
	providerInventory
	Operation string                         `json:"operation"`
	Target    string                         `json:"target"`
	Sessions  []string                       `json:"sessions"`
	Intents   map[string]*scopedThreadIntent `json:"intents"`
}

type scopedProviderProgress struct {
	Operation   string `json:"operation"`
	Stage       string `json:"stage"`
	Thread      string `json:"thread,omitempty"`
	ThreadLabel string `json:"threadLabel,omitempty"`
	Completed   int    `json:"completed"`
	Total       int    `json:"total"`
	UpdatedAt   string `json:"updatedAt"`
	Reason      string `json:"reason,omitempty"`
}

type scopedProviderFailure struct {
	Reason string
	Err    error
}

func (e *scopedProviderFailure) Error() string { return e.Err.Error() }
func (e *scopedProviderFailure) Unwrap() error { return e.Err }

func providerAtomicJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > 16*1024*1024 {
		return errors.New("provider snapshot exceeds bounds")
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".provider-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func scopedProviderCommand(ctx context.Context, client *workspacecodex.Client, action, workspace, stateRoot, socket, profile, authority, path, target, operation, scopes, rootsJSON string) (resultErr error) {
	if !canonicalAbsolutePath(authority) || !canonicalAbsolutePath(path) || !queueOperationID(operation) {
		return errors.New("scoped provider helper requires exact private identities")
	}
	if action == "scoped-prepare" {
		var stop context.CancelFunc
		ctx, stop = providerPreparationContext(ctx, path, operation)
		defer stop()
	}
	slugs := []string{}
	if scopes != "" {
		slugs = strings.Split(scopes, ",")
	}
	slices.Sort(slugs)
	scope := map[string]bool{}
	for _, slug := range slugs {
		if !session.ValidSlug(slug) || scope[slug] {
			return errors.New("invalid scoped provider session")
		}
		scope[slug] = true
	}
	var roots map[string]string
	if json.Unmarshal([]byte(rootsJSON), &roots) != nil || len(roots) != len(slugs) {
		return errors.New("scoped provider roots are absent or inconsistent")
	}
	progress := scopedProviderProgress{Operation: operation, Stage: strings.TrimPrefix(action, "scoped-")}
	labels := map[string]string{}
	writeProgress := func(stage, thread string, completed, total int) error {
		progress.Stage, progress.Thread, progress.Completed, progress.Total = stage, thread, completed, total
		progress.ThreadLabel = labels[thread]
		progress.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		return providerAtomicJSON(path+".progress", progress)
	}
	defer func() {
		if resultErr != nil {
			var failure *scopedProviderFailure
			if errors.As(resultErr, &failure) {
				progress.Reason = failure.Reason
			}
			// Stage + exact thread locate a failure without disclosing native
			// messages, endpoint addresses, input text or credentials.
			if progress.Reason == "" {
				progress.Reason = "native_" + progress.Stage + "_failed"
			}
			progress.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
			_ = providerAtomicJSON(path+".progress", progress)
		}
	}()
	if err := writeProgress("locking-sessions", "", 0, len(slugs)); err != nil {
		return err
	}
	for _, slug := range slugs {
		var lock *session.RuntimeLock
		for {
			var err error
			lock, err = session.LockRuntimeExclusive(authority, slug)
			if err == nil {
				break
			}
			if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
				return err
			}
			progress.Thread, progress.Reason = slug, "session_runtime_busy"
			progress.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
			if err := providerAtomicJSON(path+".progress", progress); err != nil {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
		progress.Reason = ""
		defer lock.Close()
		owner, err := session.PendingLifecycle(workspace, slug)
		if err != nil || owner != "" {
			progress.Thread, progress.Reason = slug, "session_lifecycle_unfinished"
			return errors.New("scoped provider session has an unfinished lifecycle operation")
		}
		summary, err := session.Find(workspace, slug)
		if err != nil || summary.Archived || roots[slug] == "" || summary.Codex.ThreadID != roots[slug] || summary.Codex.SocketPath != socket {
			progress.Thread, progress.Reason = slug, "session_identity_changed"
			return errors.New("scoped provider retained session root changed")
		}
	}
	if action == "scoped-prepare" {
		for _, slug := range slugs {
			if err := client.RetryStore().CancelSession(ctx, slug, roots[slug], socket, client.RetryBackend()); err != nil {
				return err
			}
		}
		inventory, err := buildProviderInventoryScope(ctx, client, workspace, stateRoot, socket, profile, scope)
		if err != nil {
			return err
		}
		snapshot := scopedProviderSnapshot{providerInventory: inventory, Operation: operation, Target: target, Sessions: slugs, Intents: map[string]*scopedThreadIntent{}}
		for index := range snapshot.Threads {
			thread := &snapshot.Threads[index]
			labels[thread.ID] = providerThreadLabel(*thread)
			if err := writeProgress("capturing-settings", thread.ID, index, len(snapshot.Threads)); err != nil {
				return err
			}
			turn, err := readScopedProviderTurn(ctx, client, thread.ID)
			if err != nil {
				return err
			}
			goal, err := client.HasActiveGoal(ctx, thread.ID)
			if err != nil {
				return err
			}
			if err := captureScopedProviderThread(ctx, client, thread, target); err != nil {
				return err
			}
			digest := sha256.Sum256([]byte(operation + ":" + thread.ID))
			identity := hex.EncodeToString(digest[:16])
			snapshot.Intents[thread.ID] = &scopedThreadIntent{Active: turn.Status == "inProgress", PauseTurnID: turn.ID, GoalActive: goal, GoalOwned: goal, Continuation: identity[0:8] + "-" + identity[8:12] + "-" + identity[12:16] + "-" + identity[16:20] + "-" + identity[20:32]}
		}
		return json.NewEncoder(os.Stdout).Encode(snapshot)
	}
	var snapshot scopedProviderSnapshot
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 16*1024*1024 {
		return errors.New("scoped provider snapshot is not private and bounded")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if json.Unmarshal(data, &snapshot) != nil || snapshot.Schema != 1 || snapshot.Workspace != workspace || snapshot.Operation != operation || snapshot.Target != target || !slices.Equal(snapshot.Sessions, slugs) || len(snapshot.Threads) > 4096 {
		return errors.New("scoped provider snapshot identity changed")
	}
	for _, thread := range snapshot.Threads {
		labels[thread.ID] = providerThreadLabel(thread)
		if !scope[thread.Slug] || thread.Cwd != filepath.Join(workspace, "work", thread.Slug) || snapshot.Intents[thread.ID] == nil {
			return errors.New("scoped provider thread has a foreign session")
		}
		if err := validateProviderResumePayloads(thread, target); err != nil {
			return err
		}
	}
	if action == "scoped-switch" {
		for index, thread := range snapshot.Threads {
			intent := snapshot.Intents[thread.ID]
			if intent.Verified {
				continue
			}
			if err := writeProgress("pausing", thread.ID, index, len(snapshot.Threads)); err != nil {
				return err
			}
			if !intent.Closed {
				if err := writeProgress("closing", thread.ID, index, len(snapshot.Threads)); err != nil {
					return err
				}
				archived, err := scopedThreadArchived(ctx, client, thread.ID)
				if err != nil {
					return err
				}
				if !archived {
					turn, err := readScopedProviderTurn(ctx, client, thread.ID)
					if err != nil {
						return err
					}
					// A failed archive can stop execution before moving the history.
					// Preserve the journaled active flag for that same stopped turn.
					if !intent.CloseIntent || turn.ID != intent.PauseTurnID || turn.Status == "inProgress" {
						goal, err := client.HasActiveGoal(ctx, thread.ID)
						if err != nil {
							return err
						}
						intent.GoalOwned = goal || (intent.GoalOwned && intent.PauseTurnID == turn.ID)
						intent.Active, intent.PauseTurnID = turn.Status == "inProgress", turn.ID
					}
					intent.CloseIntent = true
					if err := providerAtomicJSON(path, snapshot); err != nil {
						return err
					}
					var response any
					if err := client.Request(ctx, "thread/archive", map[string]any{"threadId": thread.ID}, &response); err != nil {
						return err
					}
				}
				intent.Closed = true
				if err := providerAtomicJSON(path, snapshot); err != nil {
					return err
				}
			}
			turn, err := readScopedProviderTurn(ctx, client, thread.ID)
			if err != nil {
				return err
			}
			if turn.ID != intent.PauseTurnID {
				// Goal completion may release ordinary queued work between capture
				// and shutdown. The old goal cannot own that newer source turn.
				intent.GoalOwned = false
			}
			intent.Active = turn.Status == "interrupted" && (intent.Active || turn.ID != intent.PauseTurnID)
			if err := providerAtomicJSON(path, snapshot); err != nil {
				return err
			}
		}
		// Native teardown stops the selected root's temporary descendants.
		// Only retained leads/members are reopened and checked for target routing.
		for index, thread := range snapshot.Threads {
			intent := snapshot.Intents[thread.ID]
			if err := writeProgress("reopening", thread.ID, index, len(snapshot.Threads)); err != nil {
				return err
			}
			archived, err := scopedThreadArchived(ctx, client, thread.ID)
			if err != nil {
				return err
			}
			if archived {
				var response any
				if err := client.Request(ctx, "thread/unarchive", map[string]any{"threadId": thread.ID}, &response); err != nil {
					return err
				}
			}
			params, err := providerResumeParams(thread, target)
			if err != nil {
				return err
			}
			var effective map[string]any
			if err := client.Request(ctx, "thread/resume", params, &effective); err != nil {
				return err
			}
			if err := validateProviderResponse(thread, effective, target); err != nil {
				return err
			}
			update := map[string]any{"threadId": thread.ID}
			for _, key := range []string{"disabledPluginIds", "collaborationMode", "multiAgentMode"} {
				if value, saved := thread.Original[key]; saved && !reflect.DeepEqual(value, effective[key]) {
					update[key] = value
				}
			}
			if len(update) > 1 {
				var response any
				if err := client.Request(ctx, "thread/settings/update", update, &response); err != nil {
					return err
				}
				if err := client.Request(ctx, "thread/resume", params, &effective); err != nil {
					return err
				}
				for key, value := range update {
					if key != "threadId" && !reflect.DeepEqual(value, effective[key]) {
						return fmt.Errorf("scoped provider changed retained %s", key)
					}
				}
			}
			for _, key := range []string{"runtimeWorkspaceRoots", "approvalPolicy", "approvalsReviewer", "sandbox", "activePermissionProfile", "serviceTier"} {
				if key == "serviceTier" && providerTierMatches(thread.Original[key], effective[key]) {
					continue
				}
				if !reflect.DeepEqual(thread.Original[key], effective[key]) {
					return fmt.Errorf("scoped provider changed retained %s", key)
				}
			}
			// Native goal reads cannot locate an unloaded archived history.
			// Cold resume restores current durable state; inspect it only now.
			intent.GoalActive, err = client.HasActiveGoal(ctx, thread.ID)
			if err != nil {
				return err
			}
			intent.Verified = true
			if err := providerAtomicJSON(path, snapshot); err != nil {
				return err
			}
		}
	} else if action == "scoped-continue" {
		for index, thread := range snapshot.Threads {
			intent := snapshot.Intents[thread.ID]
			if !intent.Verified {
				return errors.New("provider continuation requires effective routing proof")
			}
			if intent.Continued {
				continue
			}
			if err := writeProgress("resuming-work", thread.ID, index, len(snapshot.Threads)); err != nil {
				return err
			}
			// Native cold resume restores active goals from durable current
			// state. Do not replay goal/set: that could reactivate completed
			// work after an uncertain response. Non-goal work uses a receipt.
			if intent.Active && !intent.GoalOwned && !intent.GoalActive {
				if err := client.ConfirmRetryBinding(ctx, thread.ID, thread.Settings.Policy); err != nil {
					return err
				}
				if _, err := client.Send(ctx, thread.ID, "Continue the work interrupted by the provider switch. Check the last tool results before retrying any action. Ask again for any unanswered question or approval.", intent.Continuation, "provider-switch:"+operation); err != nil {
					return err
				}
			}

			intent.Continued = true
			if err := providerAtomicJSON(path, snapshot); err != nil {
				return err
			}
		}
	} else {
		return errors.New("unknown scoped provider operation")
	}
	if err := writeProgress("complete", "", len(snapshot.Threads), len(snapshot.Threads)); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"schema": 1, "verified": true})
}

func providerThreadLabel(thread providerThread) string {
	if thread.MemberAddress != "" {
		return thread.MemberAddress
	}
	return "Lead"
}

func queueOperationID(value string) bool {
	return len(value) == 36 && strings.Count(value, "-") == 4 && strings.Trim(value, "0123456789abcdef-") == ""
}

type scopedProviderTurn struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

func readScopedProviderTurn(ctx context.Context, client *workspacecodex.Client, thread string) (scopedProviderTurn, error) {
	var page struct {
		Data *[]scopedProviderTurn `json:"data"`
	}
	if err := client.Request(ctx, "thread/turns/list", map[string]any{"threadId": thread, "limit": 1, "sortDirection": "desc", "itemsView": "notLoaded"}, &page); err != nil {
		return scopedProviderTurn{}, err
	}
	if page.Data == nil || len(*page.Data) > 1 {
		return scopedProviderTurn{}, errors.New("native latest turn is absent or ambiguous")
	}
	if len(*page.Data) == 0 {
		return scopedProviderTurn{}, nil
	}
	return (*page.Data)[0], nil
}

func scopedThreadArchived(ctx context.Context, client *workspacecodex.Client, thread string) (bool, error) {
	var response struct {
		Thread struct {
			ID   string  `json:"id"`
			Path *string `json:"path"`
		} `json:"thread"`
	}
	if err := client.Request(ctx, "thread/read", map[string]any{"threadId": thread, "excludeTurns": true}, &response); err != nil {
		return false, err
	}
	if response.Thread.ID != thread || response.Thread.Path == nil {
		return false, errors.New("native close state has no materialized identity")
	}
	return filepath.Base(filepath.Dir(*response.Thread.Path)) == "archived_sessions", nil
}

func captureScopedProviderThread(ctx context.Context, client *workspacecodex.Client, thread *providerThread, target string) error {
	fail := func(reason string, err error) error { return &scopedProviderFailure{Reason: reason, Err: err} }
	var metadata struct {
		Thread struct {
			ID       string         `json:"id"`
			Cwd      string         `json:"cwd"`
			Path     *string        `json:"path"`
			Status   map[string]any `json:"status"`
			Provider string         `json:"modelProvider"`
		} `json:"thread"`
	}
	if err := client.Request(ctx, "thread/read", map[string]any{"threadId": thread.ID, "excludeTurns": true}, &metadata); err != nil {
		return fail("native_metadata_unavailable", err)
	}
	if metadata.Thread.ID != thread.ID || metadata.Thread.Cwd != thread.Cwd || metadata.Thread.Status == nil || metadata.Thread.Provider == "" {
		return fail("thread_identity_unproved", errors.New("native thread metadata has an unexpected identity"))
	}
	params := map[string]any{"threadId": thread.ID, "cwd": thread.Cwd, "excludeTurns": true, "modelProvider": metadata.Thread.Provider, "model": thread.Settings.Model, "config": map[string]any{"model_reasoning_effort": thread.Settings.ReasoningEffort}}
	if metadata.Thread.Status["type"] == "notLoaded" {
		if metadata.Thread.Path == nil {
			return errors.New("cold provider thread has no settings history")
		}
		if err := preserveColdNativePolicy(*metadata.Thread.Path, params); err != nil {
			return fail("cold_permissions_unsupported", err)
		}
	}
	if err := client.Request(ctx, "thread/resume", params, &thread.Original); err != nil {
		return fail("native_settings_resume_failed", err)
	}
	if err := validateProviderResponse(*thread, thread.Original, ""); err != nil {
		return err
	}
	if !supportedProviderPermissionProfile(thread.Original["activePermissionProfile"]) {
		return fail("named_permission_profile_unsupported", errors.New("cold provider transition cannot preserve this named permissions profile"))
	}
	if err := validateProviderResumePayloads(*thread, target); err != nil {
		return fail("retained_policy_mismatch", err)
	}
	return nil
}
