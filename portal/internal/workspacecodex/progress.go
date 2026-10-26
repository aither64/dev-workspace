package workspacecodex

import (
	"context"
	"errors"
	"strings"

	"github.com/aither64/codex-web/codex"
)

// This refusal proves that no progress submission was attempted.
var ErrProgressCheckDeferred = errors.New("observer has pending manual retry work")

// SendProgressCheck retains ordinary submission/recovery identity but deliberately
// creates no automatic-retry admission snapshot. Only the progress scheduler may
// decide to start another observation, under its activity gate and deadline.
// Manually requested observer assignments continue through SendWithOptions.
func (c *Client) SendProgressCheck(ctx context.Context, thread, text, id, action string, options codex.TurnOptions) (codex.SendReceipt, error) {
	if !strings.HasPrefix(action, "progress:") {
		return codex.SendReceipt{}, errors.New("invalid progress submission context")
	}
	tx, _, err := c.retryAdmission(ctx, thread)
	if err != nil {
		return codex.SendReceipt{}, err
	}
	if tx != nil {
		defer tx.Close()
		if tx.State.Owned() {
			return codex.SendReceipt{}, ErrProgressCheckDeferred
		}
		// Hold the same retry admission lock across dispatch. A background retry
		// cannot acquire this observer between the ownership check and submission.
		ctx = context.WithValue(ctx, retryTransactionKey{}, tx)
	}
	return c.sendWithRecovery(ctx, thread, text, id, action, options)
}

// RefreshProgressPolicy changes a stock observer's instructions without changing
// its identity/history or restarting the shared daemon. Warm resume keeps its old
// instructions; append the replacement as a native developer item instead.
// The caller holds session/roster locks and has no pending scheduled submission.
func (c *Client) RefreshProgressPolicy(ctx context.Context, thread, cwd string, settings codex.ThreadSettings) error {
	tx, _, err := c.retryAdmission(ctx, thread)
	if err != nil {
		return err
	}
	if tx != nil {
		defer tx.Close()
		if tx.State.Owned() {
			return ErrProgressCheckDeferred
		}
	}
	if err := c.Client.RequireThreadIdle(ctx, thread, cwd); err != nil {
		return err
	}
	if err := c.Client.ActivateThreadWithSettings(ctx, thread, settings); err != nil {
		return err
	}
	// Unsubscribe has a 30-minute native grace period. A developer item applies
	// immediately to this loaded thread and persists across later resumes. A
	// retry after a lost reply may repeat the same instruction, which is harmless.
	item := map[string]any{"type": "message", "role": "developer", "content": []map[string]any{{"type": "input_text", "text": settings.Policy.DeveloperInstructions}}}
	return c.Request(ctx, "thread/inject_items", map[string]any{"threadId": thread, "items": []any{item}}, nil)
}
