package workspacecodex

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// RequireThreadExecutionIdle is only for a host transition after all senders
// have drained. Durable unknown outcomes are retained; lifecycle idle remains
// strict. Callers must use a cold, unwatched ObserverOnly client.
func (c *Client) RequireThreadExecutionIdle(ctx context.Context, thread, cwd string) error {
	if !c.transitionProof {
		return errors.New("package transition requires a cold proof client")
	}
	metadata, err := c.ReadThreadMetadata(ctx, thread, true)
	if err != nil {
		return err
	}
	if metadata.Cwd != cwd || !workspaceThread(metadata.Source) {
		return errors.New("transition thread does not match the trusted workspace identity")
	}
	if len(metadata.Status) != 1 {
		return errors.New("native execution status has an unsupported shape")
	}
	switch metadata.Status["type"] {
	case "idle", "notLoaded":
	default:
		return fmt.Errorf("Codex thread %s has unconfirmed execution status %q", thread, metadata.Status["type"])
	}
	if err := c.RequireThreadTurnsIdle(ctx, thread); err != nil {
		return err
	}
	queued, err := c.ListQueue(ctx, thread)
	if err != nil {
		return err
	}
	if len(queued) != 0 {
		return errors.New("conversation has queued native work")
	}
	return c.RequireTransitionStorage(ctx, thread)
}

// The existing scoped reader validates the selected ledger/database and fences
// without interpreting an admission claim as sender liveness or rewriting rows.
func (c *Client) RequireTransitionStorage(ctx context.Context, thread string) error {
	if !c.transitionProof {
		return errors.New("package transition requires a cold proof client")
	}
	if c.RecoveryRoot != "" && c.RetryPolicyResolver != nil {
		scope, err := c.RetryScope(ctx, thread)
		if err != nil {
			return err
		}
		state, err := c.RetryStore().Load(scope)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && state.Owned() {
			return errors.New("conversation has pending automatic retry work")
		}
	}
	_, err := c.ExportSubmissionAttempts(thread)
	return err
}
