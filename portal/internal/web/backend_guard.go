package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aither64/dev-workspace/portal/internal/agentbackend"
	"github.com/aither64/dev-workspace/portal/internal/session"
)

// This conservative guard protects the existing Codex paths until complete Pi
// creation and runtime admission are implemented. It never adopts missing state
// as Codex and never invokes either native adapter or a recovery writer.
func (s *Server) requireCodexSessionBackend(slug string) error {
	if !session.ValidSlug(slug) {
		return errors.New("invalid session backend scope")
	}
	if s.config.UserStateRoot != "" {
		_, err := (session.BackendBindingStore{Root: s.config.UserStateRoot, Workspace: s.config.Workspace}).Load(slug)
		if err == nil {
			return fmt.Errorf("%w: Pi session cannot use a legacy Codex path", agentbackend.ErrBindingConflict)
		}
		if !errors.Is(err, agentbackend.ErrNotBound) {
			return err
		}
		if err := refusePiCreationRecord(s.creationPath(slug)); err != nil {
			return err
		}
	}
	return refusePiCreationRecord(filepath.Join(s.config.Workspace, "worktrees", ".locks", slug+".creation.json"))
}

func refusePiCreationRecord(path string) error {
	var record map[string]json.RawMessage
	if err := readCreationJSON(path, &record); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var schema int
	if json.Unmarshal(record["schema"], &schema) != nil || schema < 1 || schema > 3 || record["backend"] != nil {
		return fmt.Errorf("%w: complete schema-4 creation validation is unavailable", agentbackend.ErrUnsupportedBinding)
	}
	if raw, ok := record["request"]; ok {
		var request map[string]json.RawMessage
		if json.Unmarshal(raw, &request) != nil || request == nil {
			return errors.New("invalid creation request backend evidence")
		}
		if request["backend"] != nil {
			return agentbackend.ErrUnsupportedBinding
		}
	}
	return nil
}
