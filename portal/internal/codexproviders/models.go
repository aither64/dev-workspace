package codexproviders

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/aither64/codex-web/codex"
	"github.com/aither64/dev-workspace/portal/internal/processgroup"
)

// ModelsReadError retains only finite diagnostic facts, never helper output or
// the underlying process error. Its public text stays unchanged.
type ModelsReadError struct {
	message, outcome string
	exitCode         int
}

func (e *ModelsReadError) Error() string { return e.message }

func (e *ModelsReadError) Diagnostic() (string, int) { return e.outcome, e.exitCode }

type modelOutput struct {
	buffer  bytes.Buffer
	limited bool
}

func (b *modelOutput) Bytes() []byte { return b.buffer.Bytes() }

func (b *modelOutput) Write(p []byte) (int, error) {
	if b.buffer.Len()+len(p) > 1024*1024 {
		b.limited = true
		return 0, errors.New("provider model catalog exceeds bounds")
	}
	return b.buffer.Write(p)
}

func modelProcessDiagnostic(err error, command *exec.Cmd, limited bool) (string, int) {
	exitCode := -1
	if command.ProcessState != nil {
		exitCode = command.ProcessState.ExitCode()
	}
	var exit *exec.ExitError
	var start *exec.Error
	var path *os.PathError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline", exitCode
	case errors.Is(err, context.Canceled):
		return "canceled", exitCode
	case limited:
		return "output_limit", exitCode
	case errors.As(err, &exit):
		return "exit", exitCode
	case errors.As(err, &start), errors.As(err, &path):
		return "start", exitCode
	default:
		return "unknown", exitCode
	}
}

// ReadModels is shared by CLI validation and the portal's cached reader. Only
// trusted runtime identities reach the model-only host helper.
func ReadModels(ctx context.Context, profile, workspace, name, provider string) ([]codex.Model, error) {
	if !filepath.IsAbs(profile) || !filepath.IsAbs(workspace) || !ValidID(name) || !ValidID(provider) {
		return nil, &ModelsReadError{"provider model catalog has an invalid identity", "identity", -1}
	}
	var out modelOutput
	command := exec.Command(filepath.Join(profile, "bin/workspace-host"), "provider", "models", name, provider)
	command.Dir, command.Stdout = workspace, &out
	if err := processgroup.Run(ctx, command); err != nil {
		outcome, exitCode := modelProcessDiagnostic(err, command, out.limited)
		return nil, &ModelsReadError{"selected provider model catalog is unavailable", outcome, exitCode}
	}
	var models []codex.Model
	if json.Unmarshal(out.Bytes(), &models) != nil {
		return nil, &ModelsReadError{"selected provider model catalog is invalid", "invalid", 0}
	}
	return models, nil
}
