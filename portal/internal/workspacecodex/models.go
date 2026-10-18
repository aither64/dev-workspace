package workspacecodex

import (
	"context"
	"errors"
	"os"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/codexproviders"
)

// ModelsForDirectory uses the same trusted destination resolver as native
// creation. A selected provider can also be validated before its journal exists.
func (c *Client) ModelsForDirectory(ctx context.Context, cwd, profile, selected string) ([]codex.Model, error) {
	state, err := c.providerState()
	if err != nil {
		return nil, err
	}
	if state == nil {
		if selected != "" {
			return nil, errors.New("provider catalog is unavailable")
		}
		return c.Client.ListModels(ctx)
	}
	if selected == "" && cwd != "" {
		settings, err := c.providerRequestSettings(ctx, codex.LifecycleRequest{Method: "thread/start", Cwd: cwd})
		if err != nil {
			return nil, err
		}
		selected = settings.ModelProvider
	}
	if selected == "" {
		selected = state.Active
	}
	return codexproviders.ReadModels(ctx, profile, c.workspace, os.Getenv("DEV_WORKSPACE_NAME"), selected)
}
