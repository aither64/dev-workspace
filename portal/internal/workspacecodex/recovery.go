package workspacecodex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

// Watches survive transport loss. Recheck private execution permission at the
// actual implicit resume boundary, without making recursive App Server calls.
func (c *Client) allowImplicitResume(thread string) bool {
	if c.RecoveryRoot == "" || c.workspace == "" {
		return true
	}
	return (session.RecoveryStore{Root: c.RecoveryRoot, Workspace: c.workspace}).AllowsImplicitResume(c.socket, thread)
}

// Direct team assignments also cross the cold boundary. The message must be
// durable before loading its recipient, including when native work is queued.
func (c *Client) SendWithOptions(ctx context.Context, thread, text, id, action string, options codex.TurnOptions) (codex.SendReceipt, error) {
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
	if len(options.AdditionalContext) != 0 {
		return codex.SendReceipt{}, errors.New("activate the conversation before sending additional turn context")
	}
	if err := c.Client.PrepareSendWithOptions(thread, text, id, action, false, options); err != nil {
		return codex.SendReceipt{}, err
	}
	entry, err := c.Client.Queue(ctx, thread, text, id)
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
	return codex.SendReceipt{ClientUserMessageID: id, QueuedSubmissionID: entry.ID}, nil
}
