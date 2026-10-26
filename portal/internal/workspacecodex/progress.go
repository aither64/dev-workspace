package workspacecodex

import (
	"context"
	"errors"
	"strings"

	"github.com/aither64/codex-web/codex"
)

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
			return codex.SendReceipt{}, errors.New("observer has pending manual retry work")
		}
		// Hold the same retry admission lock across dispatch. A background retry
		// cannot acquire this observer between the ownership check and submission.
		ctx = context.WithValue(ctx, retryTransactionKey{}, tx)
	}
	return c.sendWithRecovery(ctx, thread, text, id, action, options)
}
