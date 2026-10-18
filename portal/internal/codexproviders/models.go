package codexproviders

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/processgroup"
)

type modelOutput struct{ buffer bytes.Buffer }

func (b *modelOutput) Bytes() []byte { return b.buffer.Bytes() }

func (b *modelOutput) Write(p []byte) (int, error) {
	if b.buffer.Len()+len(p) > 1024*1024 {
		return 0, errors.New("provider model catalog exceeds bounds")
	}
	return b.buffer.Write(p)
}

// ReadModels is shared by CLI validation and the portal's cached reader. Only
// trusted runtime identities reach the model-only host helper.
func ReadModels(ctx context.Context, profile, workspace, name, provider string) ([]codex.Model, error) {
	if !filepath.IsAbs(profile) || !filepath.IsAbs(workspace) || !ValidID(name) || !ValidID(provider) {
		return nil, errors.New("provider model catalog has an invalid identity")
	}
	var out modelOutput
	command := exec.Command(filepath.Join(profile, "bin/workspace-host"), "provider", "models", name, provider)
	command.Dir, command.Stdout = workspace, &out
	if processgroup.Run(ctx, command) != nil {
		return nil, errors.New("selected provider model catalog is unavailable")
	}
	var models []codex.Model
	if json.Unmarshal(out.Bytes(), &models) != nil {
		return nil, errors.New("selected provider model catalog is invalid")
	}
	return models, nil
}
