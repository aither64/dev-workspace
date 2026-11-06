package workspacecodex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/maintenance"
	"github.com/aither64/dev-workspace/portal/internal/session"
	"github.com/aither64/dev-workspace/portal/internal/userstate"
)

// Watches survive transport loss. Recheck private execution permission at the
// actual implicit resume boundary, without making recursive App Server calls.
func (c *Client) allowImplicitResume(thread string) bool {
	if c.maintenanceAdmission() != nil {
		return false
	}
	state, err := c.providerState()
	if err != nil {
		return false
	}
	if state != nil && state.Operation.Reserved() {
		owner, err := (session.RecoveryStore{Root: c.RecoveryRoot, Workspace: c.workspace}).ThreadOwner(thread)
		if err != nil || state.BlocksSession(owner.Slug, owner.Codex.ThreadID) {
			return false
		}
	}
	if c.RecoveryRoot == "" || c.workspace == "" {
		return true
	}
	return (session.RecoveryStore{Root: c.RecoveryRoot, Workspace: c.workspace}).AllowsImplicitResume(c.socket, thread)
}

func (c *Client) maintenanceProfile() string {
	if c.HostProfile != "" {
		return c.HostProfile
	}
	if profile := os.Getenv("DEV_WORKSPACES_PROFILE"); profile != "" {
		return profile
	}
	return filepath.Join(c.RecoveryRoot, "profile")
}
func (c *Client) maintenanceAdmission() error {
	return maintenance.RequireAdmission(c.RecoveryRoot, c.maintenanceProfile(), c.workspace)
}

// Direct team assignments also cross the cold boundary. The message must be
// durable before loading its recipient, including when native work is queued.
func (c *Client) sendWithRecovery(ctx context.Context, thread, text, id, action string, options codex.TurnOptions) (codex.SendReceipt, error) {
	if c.RecoveryRoot == "" || c.workspace == "" {
		return c.Client.SendWithOptions(ctx, thread, text, id, action, options)
	}
	metadata, err := c.ReadThreadMetadata(ctx, thread, true)
	if err != nil {
		return codex.SendReceipt{}, err
	}
	slug := filepath.Base(metadata.Cwd)
	if metadata.Cwd != filepath.Join(c.workspace, "work", slug) {
		return codex.SendReceipt{}, errors.New("assignment thread has an unexpected working directory")
	}
	store := session.RecoveryStore{Root: c.RecoveryRoot, Workspace: c.workspace}
	record, err := store.Load(slug)
	if errors.Is(err, os.ErrNotExist) {
		return c.Client.SendWithOptions(ctx, thread, text, id, action, options)
	}
	if err != nil {
		return codex.SendReceipt{}, err
	}
	summary, err := session.Find(c.workspace, slug)
	if err != nil || summary.Codex.ThreadID != record.ThreadID || record.SocketPath != c.socket {
		return codex.SendReceipt{}, errors.New("assignment recovery identity changed")
	}
	epoch, err := session.SocketIdentity(c.socket)
	if err != nil {
		return codex.SendReceipt{}, err
	}
	if record.Active(thread, epoch) {
		return c.Client.SendWithOptions(ctx, thread, text, id, action, options)
	}
	if receipt, found, err := c.Client.ReconcileSendWithOptions(ctx, thread, text, id, action, options); err != nil || found {
		return receipt, err
	}
	// Only the portal's mode-aware original path can settle this binding.
	saved, err := userstate.ReadDeferredSettings(userstate.WorkspaceDirectory(c.RecoveryRoot, "portal", c.workspace), slug, record.ThreadID, thread)
	if err != nil {
		return codex.SendReceipt{}, err
	}
	if saved != nil && saved.RequestID != "" {
		return codex.SendReceipt{}, &codex.MessageNotSentError{Err: errors.New("Recover the pending original input before assigning other work."), Retryable: false}
	}
	if len(options.AdditionalContext) != 0 {
		return codex.SendReceipt{}, errors.New("activate the conversation before sending additional turn context")
	}
	if err := c.Client.PrepareSendWithOptions(thread, text, id, action, false, options); err != nil {
		return codex.SendReceipt{}, err
	}
	entry, err := c.Client.QueueWithPolicy(ctx, thread, text, id, options.ThreadPolicy)
	if err != nil {
		return codex.SendReceipt{}, err
	}
	if err := c.ActivateThreadWithSettings(ctx, thread, codex.ThreadSettings{Model: options.Model, ReasoningEffort: options.ReasoningEffort, Policy: options.ThreadPolicy}); err != nil {
		return codex.SendReceipt{}, err
	}
	if err := store.Update(ctx, slug, func(current *session.Recovery) error {
		if current.ThreadID != record.ThreadID || current.SocketPath != c.socket {
			return errors.New("assignment recovery identity changed")
		}
		now, err := session.SocketIdentity(c.socket)
		if err != nil || now != epoch {
			return errors.New("App Server changed during assignment")
		}
		if current.SocketIdentity != epoch {
			current.ActiveThreads = []string{}
			current.Continuation = nil
		}
		current.SocketIdentity = epoch
		if thread == current.ThreadID {
			current.Continuation = nil
		}
		if !slices.Contains(current.ActiveThreads, thread) {
			current.ActiveThreads = append(current.ActiveThreads, thread)
		}
		return nil
	}); err != nil {
		return codex.SendReceipt{}, err
	}
	if err := c.completeColdSend(ctx, thread, action, entry, options, record, epoch); err != nil {
		return codex.SendReceipt{}, err
	}
	return codex.SendReceipt{ClientUserMessageID: id, QueuedSubmissionID: entry.ID}, nil
}

// Resume can leave a fresh accepted assignment at an idle queue head. Complete
// only that entry while the caller still holds its managed retry admission.
func (c *Client) completeColdSend(ctx context.Context, thread, action string, entry codex.QueueEntry, options codex.TurnOptions, original session.Recovery, epoch string) error {
	position := func(entries []codex.QueueEntry) (int, error) {
		found := -1
		for i, current := range entries {
			if current.ID != entry.ID && current.ClientUserMessageID != entry.ClientUserMessageID {
				continue
			}
			if current.ID != entry.ID || current.ClientUserMessageID != entry.ClientUserMessageID || current.Text != entry.Text || found != -1 {
				return -1, errors.New("cold assignment queue identity changed")
			}
			found = i
		}
		return found, nil
	}
	reconcile := func(entries []codex.QueueEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		at, err := position(entries)
		if err != nil {
			return err
		}
		if at >= 0 {
			return errors.New("cold assignment remains queued after start")
		}
		// Under managed admission no other producer can recreate this client ID.
		// Absence plus the SDK's exact queue/history proof establishes consumption.
		receipt, found, err := c.Client.ReconcileSendWithOptions(ctx, thread, entry.Text, entry.ClientUserMessageID, action, options)
		if err != nil {
			return err
		}
		if !found || receipt.ClientUserMessageID != entry.ClientUserMessageID {
			return errors.New("cold assignment execution is not reconciled")
		}
		return nil
	}
	entries, err := c.Client.ListQueue(ctx, thread)
	if err != nil {
		return err
	}
	at, err := position(entries)
	if err != nil {
		return err
	}
	if at < 0 {
		// This call already obtained an exact queue acknowledgement, activation
		// and checked permission. Preserve acceptance while history catches up;
		// the queued receipt does not claim execution or resolved lifecycle state.
		return ctx.Err()
	}
	if at > 0 {
		return nil
	}
	active, err := c.Client.ActiveTurnID(ctx, thread)
	if err != nil || active != "" {
		return err
	}
	if err := c.maintenanceAdmission(); err != nil {
		return err
	}
	summary, err := session.Find(c.workspace, original.Slug)
	if err != nil || summary.Archived || summary.Codex.ThreadID != original.ThreadID || summary.Codex.SocketPath != c.socket {
		return errors.New("cold assignment session identity changed")
	}
	current, err := (session.RecoveryStore{Root: c.RecoveryRoot, Workspace: c.workspace}).Load(original.Slug)
	if err != nil {
		return err
	}
	now, err := session.SocketIdentity(c.socket)
	if err != nil || now != epoch || current.ThreadID != original.ThreadID || current.SocketPath != c.socket || !current.Active(thread, now) {
		return errors.New("cold assignment execution permission changed")
	}
	// Calling the workspace wrapper would reacquire and cancel the held retry.
	if err := c.Client.StartQueueWithPolicy(ctx, thread, entry.ID, options.ThreadPolicy); err != nil {
		entries, readErr := c.Client.ListQueue(ctx, thread)
		if readErr != nil {
			return errors.Join(err, readErr)
		}
		if proofErr := reconcile(entries); proofErr != nil {
			return errors.Join(err, proofErr)
		}
		return nil
	}
	return nil
}
